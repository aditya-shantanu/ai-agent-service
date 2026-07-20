package bench

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Canonical scenario names (budget files key on these).
const (
	ScenarioNewWarm      = "new-agent-warm"
	ScenarioNewCold      = "new-agent-cold"
	ScenarioResume       = "resume-suspended"
	ScenarioBaseline     = "baseline-always-alive"
	ScenarioResumeTTFT   = "resume-suspended-ttft"
	ScenarioBaselineTTFT = "baseline-ttft"
)

// scenarioOrder fixes report ordering.
var scenarioOrder = []string{
	ScenarioBaseline, ScenarioResume, ScenarioNewWarm, ScenarioNewCold,
	ScenarioBaselineTTFT, ScenarioResumeTTFT,
}

// ErrorEvent records a non-200 or transport failure observed during a
// timed step. These are UX-correctness signals: the budget gate fails on
// anything except 503+Retry-After during a wake, which is merely counted.
type ErrorEvent struct {
	Iteration  int    `json:"iteration"`
	Status     int    `json:"status"` // 0 = transport error
	RetryAfter string `json:"retryAfter,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// ScenarioResult is the outcome of one scenario.
type ScenarioResult struct {
	Name       string               `json:"-"`
	Iterations int                  `json:"iterations"`
	SamplesMS  []float64            `json:"samplesMS,omitempty"`
	Summary    Summary              `json:"summary"`
	Extra      map[string][]float64 `json:"extra,omitempty"` // auxiliary series (warm re-probes, concurrent wakes, cold fallbacks)
	Errors     []ErrorEvent         `json:"errors"`
	Skipped    bool                 `json:"skipped,omitempty"`
	SkipReason string               `json:"skipReason,omitempty"`
	Failed     bool                 `json:"failed,omitempty"`
	FailReason string               `json:"failReason,omitempty"`
}

func newResult(name string) *ScenarioResult {
	return &ScenarioResult{Name: name, Errors: []ErrorEvent{}, Extra: map[string][]float64{}}
}

func (r *ScenarioResult) fail(format string, args ...any) *ScenarioResult {
	r.Failed = true
	r.FailReason = fmt.Sprintf(format, args...)
	r.Summary = Summarize(r.SamplesMS)
	return r
}

func (r *ScenarioResult) skip(reason string) *ScenarioResult {
	r.Skipped = true
	r.SkipReason = reason
	return r
}

func (r *ScenarioResult) addSample(d time.Duration) {
	r.SamplesMS = append(r.SamplesMS, ms(d))
}

func (r *ScenarioResult) addExtra(key string, d time.Duration) {
	r.Extra[key] = append(r.Extra[key], ms(d))
}

func (r *ScenarioResult) addProbeError(iter int, p Probe) {
	ev := ErrorEvent{Iteration: iter, Status: p.Status, RetryAfter: p.RetryAfter, Detail: p.Body}
	if p.Err != nil {
		ev.Detail = p.Err.Error()
	}
	r.Errors = append(r.Errors, ev)
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// Env is everything scenarios need to run.
type Env struct {
	Client        *Client
	Pool          *PoolControl // nil = no kube access: cold is skipped, warm falls back to fixed restock waits
	EnvName       string       // kind | gke
	UserPrefix    string
	SuspendSettle time.Duration // pod-termination settle after state flips to Suspended
	RestockWait   time.Duration // fixed warm-pool restock wait when Pool == nil
	ProbeGap      time.Duration // pause between baseline probes
	Iterations    int           // 0 = per-scenario defaults
	Logf          func(format string, args ...any)
}

func (e *Env) iters(def int) int {
	if e.Iterations > 0 {
		return e.Iterations
	}
	return def
}

func (e *Env) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

// Scenario is one benchmark scenario. Run must clean up its users even on
// failure (use context.WithoutCancel for teardown so Ctrl-C still cleans).
type Scenario interface {
	Name() string
	Run(ctx context.Context, env *Env) *ScenarioResult
}

// RunAll executes scenarios sequentially — parallel wakes would contend
// for node resources and pollute each other's latency samples.
func RunAll(ctx context.Context, env *Env, scenarios []Scenario) map[string]*ScenarioResult {
	out := make(map[string]*ScenarioResult, len(scenarios))
	for _, s := range scenarios {
		if ctx.Err() != nil {
			out[s.Name()] = newResult(s.Name()).skip("run interrupted")
			continue
		}
		env.logf("=== scenario %s", s.Name())
		res := s.Run(ctx, env)
		res.Summary = Summarize(res.SamplesMS)
		out[s.Name()] = res
	}
	return out
}

// setupUser creates a Ready user and verifies the data plane end-to-end
// with one untimed warm-up probe. The returned cleanup deletes the user
// on a fresh (cancellation-immune) context.
func setupUser(ctx context.Context, env *Env, name string) (*User, func(), error) {
	if err := env.Client.DeleteUserAndWait(ctx, name, 90*time.Second); err != nil {
		return nil, nil, fmt.Errorf("pre-clean %s: %w", name, err)
	}
	u, err := env.Client.CreateUser(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := env.Client.DeleteUserAndWait(cctx, name, 90*time.Second); err != nil {
			env.logf("cleanup %s: %v", name, err)
		}
	}
	if u.State != "Ready" {
		cleanup()
		return nil, nil, fmt.Errorf("user %s not Ready after create (state %s)", name, u.State)
	}
	if u.Token == "" {
		cleanup()
		return nil, nil, fmt.Errorf("user %s: no token returned (leftover claim from a previous run?)", name)
	}
	if p := env.Client.ProbeModels(ctx, name, u.Token); p.Status != 200 {
		cleanup()
		return nil, nil, fmt.Errorf("user %s: warm-up probe failed: HTTP %d %s %v", name, p.Status, p.Body, p.Err)
	}
	return u, cleanup, nil
}

// concurrentProbes fires n simultaneous proxied requests (thundering-herd
// wake: the gateway's per-user mutex must coalesce them into one resume).
func concurrentProbes(ctx context.Context, env *Env, user, token string, n int) []Probe {
	probes := make([]Probe, n)
	var wg sync.WaitGroup
	for i := range probes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			probes[i] = env.Client.ProbeModels(ctx, user, token)
		}(i)
	}
	wg.Wait()
	return probes
}
