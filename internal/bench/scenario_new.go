package bench

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// warmPoolSandboxPrefix identifies sandboxes adopted from the warm pool:
// they keep their pool name (README decision 8).
const warmPoolSandboxPrefix = "hermes-pool-"

// NewAgentWarm measures signup UX with a stocked warm pool: the timed
// POST /api/v1/users blocks until adoption + Ready.
type NewAgentWarm struct{}

func (NewAgentWarm) Name() string { return ScenarioNewWarm }

func (NewAgentWarm) Run(ctx context.Context, env *Env) *ScenarioResult {
	res := newResult(ScenarioNewWarm)
	n := env.iters(5)
	res.Iterations = n

	for i := 0; i < n; i++ {
		user := fmt.Sprintf("%s-warm-%d", env.UserPrefix, i)
		if err := env.Client.DeleteUserAndWait(ctx, user, 90*time.Second); err != nil {
			return res.fail("pre-clean %s: %v", user, err)
		}

		start := time.Now()
		u, err := env.Client.CreateUser(ctx, user)
		elapsed := time.Since(start)
		if err != nil {
			res.Errors = append(res.Errors, ErrorEvent{Iteration: i, Detail: err.Error()})
			continue
		}
		switch {
		case u.State != "Ready":
			res.Errors = append(res.Errors, ErrorEvent{Iteration: i, Status: 201,
				Detail: fmt.Sprintf("state %s after create (provision timeout?)", u.State)})
		case strings.HasPrefix(u.SandboxName, warmPoolSandboxPrefix):
			res.addSample(elapsed)
		default:
			// Pool was empty — replenishment raced us. Report the cold
			// fallback separately rather than polluting the warm summary.
			env.logf("  iteration %d adopted no warm spare (sandbox %s) — recorded as coldFallbackMS", i, u.SandboxName)
			res.addExtra("coldFallbackMS", elapsed)
		}

		if err := env.Client.DeleteUserAndWait(ctx, user, 90*time.Second); err != nil {
			return res.fail("cleanup %s: %v", user, err)
		}
		if i < n-1 {
			if err := waitRestock(ctx, env); err != nil {
				return res.fail("waiting for warm-pool restock: %v", err)
			}
		}
	}
	return res
}

// waitRestock waits until the warm pool is back at its desired replica
// count (adoption consumes a spare; the controller replenishes async).
// Without kube access, falls back to a fixed wait.
func waitRestock(ctx context.Context, env *Env) error {
	if env.Pool == nil {
		env.logf("  no kube access; fixed restock wait %s", env.RestockWait)
		return sleepCtx(ctx, env.RestockWait)
	}
	return env.Pool.WaitRestocked(ctx, 5*time.Minute)
}

// NewAgentCold measures signup UX with a drained pool — the full cold
// provision (PVC create, image, sandbox boot). Drains deterministically by
// scaling the SandboxWarmPool to 0 (background replenishment makes
// saturation-by-concurrent-creates a coin flip per sample), so it requires
// kube access and the explicit -allow-pool-drain opt-in: while drained,
// real signups degrade to the cold path too.
type NewAgentCold struct {
	AllowDrain bool
}

func (NewAgentCold) Name() string { return ScenarioNewCold }

func (s NewAgentCold) Run(ctx context.Context, env *Env) *ScenarioResult {
	res := newResult(ScenarioNewCold)
	if !s.AllowDrain {
		return res.skip("pool drain not allowed (pass -allow-pool-drain)")
	}
	if env.Pool == nil {
		return res.skip("no kube access (need kubeconfig for -allow-pool-drain)")
	}

	restore, err := env.Pool.Drain(ctx)
	if err != nil {
		// Drain can fail after the replicas=0 patch — restore is valid
		// even then and MUST run, or the pool stays drained.
		if restore != nil {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 6*time.Minute)
			defer cancel()
			if rerr := restore(rctx); rerr != nil {
				env.logf("RESTORING WARM POOL FAILED after drain error: %v", rerr)
			}
		}
		return res.fail("drain warm pool: %v", err)
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 6*time.Minute)
		defer cancel()
		if err := restore(rctx); err != nil {
			env.logf("RESTORING WARM POOL FAILED: %v — fix with: kubectl -n %s patch sandboxwarmpool %s --type merge -p '{\"spec\":{\"replicas\":N}}' or helm upgrade",
				err, env.Pool.Namespace, env.Pool.PoolName)
			res.Errors = append(res.Errors, ErrorEvent{Detail: "warm pool restore failed: " + err.Error()})
		}
	}()

	n := env.iters(3) // each cold create churns a PVC (and a PD on GKE)
	res.Iterations = n
	for i := 0; i < n; i++ {
		user := fmt.Sprintf("%s-cold-%d", env.UserPrefix, i)
		if err := env.Client.DeleteUserAndWait(ctx, user, 90*time.Second); err != nil {
			return res.fail("pre-clean %s: %v", user, err)
		}

		start := time.Now()
		u, err := env.Client.CreateUser(ctx, user)
		elapsed := time.Since(start)
		if err != nil {
			res.Errors = append(res.Errors, ErrorEvent{Iteration: i, Detail: err.Error()})
		} else if u.State != "Ready" {
			res.Errors = append(res.Errors, ErrorEvent{Iteration: i, Status: 201,
				Detail: fmt.Sprintf("state %s after create (provision timeout?)", u.State)})
		} else if strings.HasPrefix(u.SandboxName, warmPoolSandboxPrefix) {
			res.Errors = append(res.Errors, ErrorEvent{Iteration: i, Status: 201,
				Detail: fmt.Sprintf("adopted warm spare %s during a drained-pool run", u.SandboxName)})
		} else {
			res.addSample(elapsed)
		}

		if err := env.Client.DeleteUserAndWait(ctx, user, 3*time.Minute); err != nil {
			return res.fail("cleanup %s: %v", user, err)
		}
	}
	return res
}
