package bench

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// Resume measures the cost-optimized return path: explicit suspend, then a
// timed proxied request whose wake-on-connect hold IS the user experience.
// This — minus the Baseline number — is the suspension UX tax.
type Resume struct{}

func (Resume) Name() string { return ScenarioResume }

func (Resume) Run(ctx context.Context, env *Env) *ScenarioResult {
	res := newResult(ScenarioResume)
	user := env.UserPrefix + "-resume"

	u, cleanup, err := setupUser(ctx, env, user)
	if err != nil {
		return res.fail("setup: %v", err)
	}
	defer cleanup()

	n := env.iters(5)
	res.Iterations = n
	for i := 0; i < n; i++ {
		if err := forceSuspend(ctx, env, user); err != nil {
			return res.fail("iteration %d: %v", i, err)
		}

		if i == n-1 && n > 1 {
			// Thundering-herd correctness: 3 simultaneous probes must all
			// succeed off ONE resume (per-user wake mutex). The slowest is
			// the sample — the worst experience within the herd.
			probes := concurrentProbes(ctx, env, user, u.Token, 3)
			worst := time.Duration(0)
			ok := true
			for _, p := range probes {
				if p.Status != 200 {
					res.addProbeError(i, p)
					ok = false
					continue
				}
				res.addExtra("concurrentWakeMS", p.Duration)
				if p.Duration > worst {
					worst = p.Duration
				}
			}
			if ok {
				res.addSample(worst)
			}
		} else {
			timedWakeProbe(ctx, env, res, i, user, u.Token)
		}

		// Post-wake steady-state check: the immediate re-probe must be fast
		// and clean (retryTransport should have absorbed the uvicorn bind).
		if p := env.Client.ProbeModels(ctx, user, u.Token); p.Status == 200 {
			res.addExtra("warmProbeMS", p.Duration)
		} else {
			p.Body = "post-wake re-probe: " + p.Body
			res.addProbeError(i, p)
		}

		if got, err := env.Client.GetUser(ctx, user); err == nil && got.LastWakeReason != "connect" {
			res.Errors = append(res.Errors, ErrorEvent{
				Iteration: i, Status: 200,
				Detail: fmt.Sprintf("lastWakeReason = %q, want connect", got.LastWakeReason),
			})
		}
	}
	return res
}

// forceSuspend suspends the user, waits for the state flip, then lets pod
// termination settle — the flip is async (s6 graceful shutdown), and timing
// a wake against a still-terminating pod measures a hybrid path.
func forceSuspend(ctx context.Context, env *Env, user string) error {
	if err := env.Client.Suspend(ctx, user); err != nil {
		return err
	}
	if _, err := env.Client.WaitState(ctx, user, "Suspended", 2*time.Minute); err != nil {
		return err
	}
	return sleepCtx(ctx, env.SuspendSettle)
}

// timedWakeProbe issues the timed wake request. A 503 is recorded as an
// error event (it must carry Retry-After — checked by the budget gate),
// honored, and retried up to 3 times; the wall-time through the successful
// retry lands in extra[wakeWithRetryMS], never in the primary samples.
func timedWakeProbe(ctx context.Context, env *Env, res *ScenarioResult, iter int, user, token string) {
	start := time.Now()
	for attempt := 0; attempt < 4; attempt++ {
		p := env.Client.ProbeModels(ctx, user, token)
		if p.Status == 200 {
			if attempt == 0 {
				res.addSample(p.Duration)
			} else {
				res.addExtra("wakeWithRetryMS", time.Since(start))
			}
			return
		}
		res.addProbeError(iter, p)
		if p.Status != 503 {
			return
		}
		wait := 10 * time.Second
		if s, err := strconv.Atoi(p.RetryAfter); err == nil && s > 0 {
			wait = time.Duration(s) * time.Second
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return
		}
	}
}

// ResumeTTFT measures the full-stack return experience: suspend, then a
// streamed chat turn — wake hold plus model first-token. Costs LLM credits.
type ResumeTTFT struct{}

func (ResumeTTFT) Name() string { return ScenarioResumeTTFT }

func (ResumeTTFT) Run(ctx context.Context, env *Env) *ScenarioResult {
	res := newResult(ScenarioResumeTTFT)
	user := env.UserPrefix + "-ttft-resume"

	u, cleanup, err := setupUser(ctx, env, user)
	if err != nil {
		return res.fail("setup: %v", err)
	}
	defer cleanup()

	n := env.iters(3)
	res.Iterations = n
	for i := 0; i < n; i++ {
		if err := forceSuspend(ctx, env, user); err != nil {
			return res.fail("iteration %d: %v", i, err)
		}
		t := env.Client.StreamChatTTFT(ctx, user, u.Token)
		if err := recordTTFT(res, i, t); err != nil {
			return res.fail("iteration %d: %v", i, err)
		}
	}
	return res
}
