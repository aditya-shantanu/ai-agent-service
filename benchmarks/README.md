# UX / performance benchmark

`cmd/hermes-bench` measures what a user actually experiences at the
platform's two critical moments — **first contact** (new agent) and
**coming back** (resume) — and compares the cost-optimized lifecycle
against an **always-alive baseline** agent. It exists so cost work can be
evaluated as "saved $X/agent/month, cost Y seconds of UX" instead of by
feel. All timing is client-side wall-clock: the gateway exposes no timing
metadata, and client wall-time is the UX anyway.

## Running

```sh
make bench          # kind: full report + JSON snapshot
make bench-check    # kind: same, plus budget gate (exit 1 on violation)
make bench-gke      # GKE via the LoadBalancer (CHECK=1 / TTFT=1 / DRAIN=1)
TTFT=1 BENCH_ARGS="-scenarios resume,baseline" benchmarks/run.sh   # chat TTFT only
```

Requires a deployed hermes-service (`make dev` or `make deploy-gke`).
TTFT scenarios stream real chat turns — provider key required
(`make set-provider-key`) and a few LLM calls are spent per run.

## Scenarios

| Scenario | What it measures | How |
|---|---|---|
| `baseline-always-alive` | The reference UX: proxied request against a suspend-exempt agent | 20 timed `GET /u/{u}/v1/models` |
| `resume-suspended` | The cost-optimized return path | explicit suspend → settle → timed probe; the wake-on-connect hold IS the sample. Last iteration fires 3 concurrent probes (herd coalescing check) |
| `new-agent-warm` | Signup with a stocked warm pool | timed `POST /api/v1/users`; samples that missed the pool are reported separately as `coldFallbackMS` |
| `new-agent-cold` | Signup with the pool drained (full PVC + boot) | scales the SandboxWarmPool to 0 (restored afterwards); needs `-allow-pool-drain` |
| `baseline-ttft` / `resume-suspended-ttft` | Time-to-first-token of a streamed one-token chat turn | `--ttft` only |

Non-200 responses during timed steps are recorded as **error events**; the
gate fails on all of them except 503+`Retry-After` during a wake, which is
counted against `allowedWakeErrors`.

## The headline numbers

```
Suspension UX tax = resume-suspended p50 − baseline p50
Cold-create tax   = new-agent-cold p50 − new-agent-warm p50
```

The suspension tax is the single number future cost optimizations get
measured against (budget key `comparisons.suspendUXTaxP50Max`).

## Budgets & snapshots

- `benchmarks/budgets-kind.yaml`, `benchmarks/budgets-gke.yaml` — the per-environment
  latency contract. Only present keys are enforced; budgeted-but-skipped
  scenarios warn unless listed under `required:`. Numbers are PROVISIONAL
  until calibrated: after three green runs, tighten toward observed
  p50 × 1.5 / max × 2.
- `benchmarks/results/<env>-<timestamp>.json` (gitignored) — full snapshot with
  raw samples, comparisons, git commit and budget verdict. Keep interesting
  ones (before/after a cost change) by copying them elsewhere or attaching
  to the PR.

## Safety notes

- Benchmark users are named `bench-*`; every scenario deletes its users
  even on failure or Ctrl-C.
- The cold scenario restores the warm pool replicas itself, and
  `benchmarks/run.sh` re-restores in a trap; if both die, `helm upgrade`
  (or `kubectl -n hermes-users patch sandboxwarmpool hermes-pool --type
  merge -p '{"spec":{"replicas":N}}'`) puts it back.
- On GKE, draining the pool (DRAIN=1) degrades real signups for the
  duration of the cold scenario — coordinate accordingly.
