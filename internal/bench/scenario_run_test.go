package bench_test

import (
	"testing"
	"time"

	"github.com/aditya-shantanu/ai-agent-service/internal/bench"
)

func TestBaselineScenario(t *testing.T) {
	f := newFakeGateway()
	env, srv := newFakeEnv(f)
	defer srv.Close()
	env.Iterations = 3

	res := bench.Baseline{}.Run(t.Context(), env)
	res.Summary = bench.Summarize(res.SamplesMS)

	if res.Failed {
		t.Fatalf("failed: %s", res.FailReason)
	}
	if len(res.SamplesMS) != 3 || len(res.Errors) != 0 {
		t.Fatalf("samples %d errors %v", len(res.SamplesMS), res.Errors)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.users) != 0 {
		t.Errorf("user not cleaned up: %v", f.users)
	}
}

func TestBaselineSetsExemption(t *testing.T) {
	f := newFakeGateway()
	env, srv := newFakeEnv(f)
	defer srv.Close()
	env.Iterations = 1

	sawExempt := false
	f.onExempt = func(exempt bool) { sawExempt = sawExempt || exempt }
	if res := (bench.Baseline{}).Run(t.Context(), env); res.Failed {
		t.Fatalf("failed: %s", res.FailReason)
	}
	if !sawExempt {
		t.Error("baseline scenario never set suspend-exempt — idle sweeper could poison the reference numbers")
	}
}

func TestResumeScenario(t *testing.T) {
	const wakeDelay = 60 * time.Millisecond
	f := newFakeGateway()
	f.wakeDelay = wakeDelay
	env, srv := newFakeEnv(f)
	defer srv.Close()
	env.Iterations = 2 // iteration 1 becomes the concurrent-herd check

	res := bench.Resume{}.Run(t.Context(), env)
	res.Summary = bench.Summarize(res.SamplesMS)

	if res.Failed {
		t.Fatalf("failed: %s", res.FailReason)
	}
	if len(res.SamplesMS) != 2 {
		t.Fatalf("samples = %v, errors = %v", res.SamplesMS, res.Errors)
	}
	for i, s := range res.SamplesMS {
		if s < float64(wakeDelay/time.Millisecond) {
			t.Errorf("sample %d = %vms — did not include the wake hold", i, s)
		}
	}
	if len(res.Extra["warmProbeMS"]) != 2 {
		t.Errorf("warm re-probes = %v", res.Extra["warmProbeMS"])
	}
	if len(res.Extra["concurrentWakeMS"]) != 3 {
		t.Errorf("concurrent wake probes = %v", res.Extra["concurrentWakeMS"])
	}
	if len(res.Errors) != 0 {
		t.Errorf("errors = %v", res.Errors)
	}
}

func TestResumeScenario503Retry(t *testing.T) {
	f := newFakeGateway()
	f.wakeDelay = 20 * time.Millisecond
	f.fail503 = 1
	f.retryAfter = "1"
	env, srv := newFakeEnv(f)
	defer srv.Close()
	env.Iterations = 1

	res := bench.Resume{}.Run(t.Context(), env)

	if res.Failed {
		t.Fatalf("failed: %s", res.FailReason)
	}
	// The 503 is an error event, the retried success lands in
	// wakeWithRetryMS, and the primary samples stay clean.
	if len(res.Errors) != 1 || res.Errors[0].Status != 503 || res.Errors[0].RetryAfter != "1" {
		t.Fatalf("errors = %v", res.Errors)
	}
	if len(res.SamplesMS) != 0 {
		t.Errorf("503 iteration must not produce a primary sample: %v", res.SamplesMS)
	}
	if got := res.Extra["wakeWithRetryMS"]; len(got) != 1 || got[0] < 1000 {
		t.Errorf("wakeWithRetryMS = %v (must include the honored Retry-After)", got)
	}
}

func TestNewAgentWarmScenario(t *testing.T) {
	f := newFakeGateway()
	env, srv := newFakeEnv(f)
	defer srv.Close()
	env.Iterations = 2

	res := bench.NewAgentWarm{}.Run(t.Context(), env)
	if res.Failed {
		t.Fatalf("failed: %s", res.FailReason)
	}
	if len(res.SamplesMS) != 2 || len(res.Errors) != 0 {
		t.Fatalf("samples %v errors %v", res.SamplesMS, res.Errors)
	}
}

func TestNewAgentWarmColdFallback(t *testing.T) {
	f := newFakeGateway()
	f.sandboxName = "hermes-alone-xyz" // not a pool sandbox: pool was empty
	env, srv := newFakeEnv(f)
	defer srv.Close()
	env.Iterations = 1

	res := bench.NewAgentWarm{}.Run(t.Context(), env)
	if res.Failed {
		t.Fatalf("failed: %s", res.FailReason)
	}
	if len(res.SamplesMS) != 0 {
		t.Errorf("cold fallback polluted warm samples: %v", res.SamplesMS)
	}
	if len(res.Extra["coldFallbackMS"]) != 1 {
		t.Errorf("coldFallbackMS = %v", res.Extra["coldFallbackMS"])
	}
}

func TestNewAgentColdSkipsWithoutOptIn(t *testing.T) {
	f := newFakeGateway()
	env, srv := newFakeEnv(f)
	defer srv.Close()

	res := bench.NewAgentCold{AllowDrain: false}.Run(t.Context(), env)
	if !res.Skipped {
		t.Fatalf("must skip without -allow-pool-drain: %+v", res)
	}
	res = bench.NewAgentCold{AllowDrain: true}.Run(t.Context(), env)
	if !res.Skipped {
		t.Fatalf("must skip without kube access: %+v", res)
	}
}
