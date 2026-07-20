package bench

import (
	"context"
	"fmt"
	"time"
)

// Baseline measures the reference UX: request latency against an
// always-alive (suspend-exempt) agent. Every other scenario's numbers are
// read as a delta against this one.
type Baseline struct{}

func (Baseline) Name() string { return ScenarioBaseline }

func (Baseline) Run(ctx context.Context, env *Env) *ScenarioResult {
	res := newResult(ScenarioBaseline)
	user := env.UserPrefix + "-baseline"

	u, cleanup, err := setupUser(ctx, env, user)
	if err != nil {
		return res.fail("setup: %v", err)
	}
	defer cleanup()

	// Exemption is load-bearing even though probes reset the idle clock:
	// any pause beyond the active window (2m kind) would let the sweeper
	// suspend the "always alive" reference mid-scenario and poison the
	// baseline with a multi-second wake sample.
	if err := env.Client.SetSuspendExempt(ctx, user, true); err != nil {
		return res.fail("set suspend-exempt: %v", err)
	}

	n := env.iters(20)
	res.Iterations = n
	for i := 0; i < n; i++ {
		if i > 0 && env.ProbeGap > 0 {
			if err := sleepCtx(ctx, env.ProbeGap); err != nil {
				return res.fail("interrupted: %v", err)
			}
		}
		p := env.Client.ProbeModels(ctx, user, u.Token)
		if p.Status == 200 {
			res.addSample(p.Duration)
		} else {
			res.addProbeError(i, p)
		}
	}
	return res
}

// BaselineTTFT measures time-to-first-token of a streamed one-token chat
// turn against an always-alive agent. Costs LLM credits.
type BaselineTTFT struct{}

func (BaselineTTFT) Name() string { return ScenarioBaselineTTFT }

func (BaselineTTFT) Run(ctx context.Context, env *Env) *ScenarioResult {
	res := newResult(ScenarioBaselineTTFT)
	user := env.UserPrefix + "-ttft-base"

	u, cleanup, err := setupUser(ctx, env, user)
	if err != nil {
		return res.fail("setup: %v", err)
	}
	defer cleanup()
	if err := env.Client.SetSuspendExempt(ctx, user, true); err != nil {
		return res.fail("set suspend-exempt: %v", err)
	}

	n := env.iters(3)
	res.Iterations = n
	for i := 0; i < n; i++ {
		t := env.Client.StreamChatTTFT(ctx, user, u.Token)
		if err := recordTTFT(res, i, t); err != nil {
			return res.fail("%v", err)
		}
		if err := sleepCtx(ctx, 2*time.Second); err != nil {
			return res.fail("interrupted: %v", err)
		}
	}
	return res
}

// recordTTFT files a TTFT outcome; a non-200 is fatal for the scenario
// (usually a missing provider key), never a latency sample.
func recordTTFT(res *ScenarioResult, iter int, t TTFT) error {
	if t.Status != 200 || t.Err != nil {
		res.Errors = append(res.Errors, ErrorEvent{Iteration: iter, Status: t.Status, Detail: t.Body})
		return errTTFT(t)
	}
	if !t.GotToken {
		res.Errors = append(res.Errors, ErrorEvent{Iteration: iter, Status: t.Status, Detail: "stream ended without a content token"})
		return errTTFT(t)
	}
	res.addSample(t.First)
	res.addExtra("totalMS", t.Total)
	return nil
}

func errTTFT(t TTFT) error {
	if t.Err != nil {
		return fmt.Errorf("chat stream failed: %w", t.Err)
	}
	if t.Status != 200 {
		return fmt.Errorf("chat turn failed (provider key loaded? run `make set-provider-key`): HTTP %d %s", t.Status, t.Body)
	}
	return fmt.Errorf("chat stream ended without a content token")
}
