# Cost-reduction roadmap ($/agent/month)

## Model assumptions

Every number in this document derives from these. All are editable fields
in `index.html` — change one and the whole model recomputes.

| | Assumption | Value |
|---|---|---|
| **Usage** | Interactions per agent | 10 / day |
| | Grouped into conversations | 3 / day (30 s gaps between messages) |
| | Work per interaction | 1 min |
| | Traffic window | 16 h / day |
| | Peak-over-mean concurrency | 2× |
| **Suspend / resume** | Suspend | 2,000 ms |
| | Resume | 10,000 ms |
| | Idle tail — isolated message | 15 s |
| | Idle tail — active conversation | 2 m (deployed adaptive policy) |
| **Per agent** | Requests / limits | 0.5 vCPU, 1 GiB / 2 vCPU, 2 GiB |
| | Persistent disk (PVC) | 2 GB — bills 24/7, even suspended |
| **Hardware** | Sandbox nodes | Spot `e2-custom-16-20480`, ~15% reserved |
| | Fixed overhead | 2 warm spares + on-demand system node + cluster fee |
| | Prices | GCP us-central1 list, Spot (as of 2026-07) |
| **Measured** | Warm adoption | ≤ 2 s |
| | Resume (observed) | 11–20 s |
| | Idle Hermes RSS | ~248 MiB (swap experiment) |

Scenario math from the model in this folder (defaults now reflect the
current deployed posture). History and remaining levers, in order of
leverage. Baseline before any of this: **$12.88/agent** (2×e2-standard-4
on-demand, 2 vCPU/2 GB requests).

## Done (2026-07-17) — deployed and e2e-validated

1. **Right-sized requests** — requests 500m/1Gi (limits 2 vCPU/2Gi) instead
   of 2/2 flat. Requests are what bind bin-packing; limits keep burst room.
   *(chart `sandbox.resources`)*
2. **Balanced machine shape** — sandbox nodes are `e2-custom-16-20480`
   (~1.25 GB/vCPU): enough RAM to cover GKE node reservations, none idle.
   1:4 "standard" shapes waste RAM dollars; 1:1 "highcpu" shapes go
   RAM-bound after reservations. *(terraform `sandbox_machine_type`)*
3. **Spot sandbox pool** — the platform is restart-tolerant *by design*
   (a preemption is just an unscheduled suspend; PVC survives, sessions
   resume, cron catches up), so sandboxes ride Spot (~70% off). Gateway +
   controllers stay on a small on-demand `system-pool`.
   *(terraform `sandbox-pool` + chart `sandbox.tolerations/nodeSelector`)*

Result: ~**$0.75–1.00/agent** at current single-node scale; marginal
$/agent → **~$0.50** as sandbox nodes are added (fixed costs amortize).

## TODO — next levers, in order

4. **Measure, then tighten requests further.** Instrument real usage
   (`kubectl top` / VPA recommendations on busy agents). If idle RSS is
   well under 1 Gi, dropping the RAM request directly multiplies density.
   Exit: a load test justifying the numbers in `values.yaml`.
5. ~~**Adaptive idle timeout.**~~ **DONE (2026-07-17, Level 1).** The
   sweeper now uses a 15 s base tail for isolated requests and a 2 m tail
   while a conversation is active (two activities within `activeTimeout`);
   conversations pay resume/tail once, not per message. Knobs:
   `idle.timeout` / `idle.activeTimeout`. Honest model effect with the
   deployed knobs (2 m active tail, 3 conversations/day, 30 s gaps): the
   plateau moves ~$0.34 → **~$0.31/agent** — the correction from the
   earlier ~$0.21 estimate is that conversations pay the *active* tail
   (120 s), not the base one. At these defaults Level 1 is mostly a UX
   win (no mid-conversation wakes) + it makes `activeTimeout` the direct
   cost dial: dropping it to 60 s prices at ~$0.28. Level 2 (in-pod busy
   probe) and level 3 (predictive/pre-warm) remain future work.
6. **GKE free tier / fee check.** The $0.10/h cluster fee is waived for one
   zonal cluster per billing account — this cluster qualifies. Confirm on
   the bill; if another cluster claims it, that's still ~$73/mo across all
   agents.
7. **Committed-use discounts.** Once baseline usage is predictable: 1-yr
   (~37%) or 3-yr (~55%) CUD covering the system pool + the always-on
   fraction of the sandbox pool; Spot continues to cover burst.
8. **Faster resume (engineering, parked behind the Envoy plan).** Only ~8%
   of pod-time at a 60 s idle timeout — becomes first-order *after* #5.
   Levers: GKE image streaming, slimmer boot (skip services users don't
   use), and eventually pod checkpoint/restore. Re-run the calculator
   sweep after each improvement; the "resume time" charts exist precisely
   to track this.
9. **Cluster autoscaler on the sandbox pool.** Fixed node counts pay for
   the peak all day. Autoscaling the Spot pool (min 1) trims the off-peak
   tail; pairs naturally with #7's CUD floor.
10. **Per-user cost attribution.** Not a reducer, but the prerequisite for
    pricing: meter pod-seconds + LLM tokens per user (the LLM key is
    platform-shared today; an LLM proxy with per-user keys/quotas is the
    end state — see README future work).

Re-derive any scenario by editing the fields in `index.html` — that is the
tool's job.

## Scale behavior (asked 2026-07-17: "what about a million agents?")

$/agent falls steeply while fixed costs (cluster fee, system pool, warm
spares) amortize, then **plateaus at the marginal cost** — with the current
posture that plateau is reached by ~10k agents:

| agents | Spot nodes | clusters | $/month | $/agent |
|---|---|---|---|---|
| 100 | 1 | 1 | $230 | $2.30 |
| 1,000 | 3 | 1 | $481 | $0.48 |
| 10,000 | 29 | 1 | $3.5k | $0.35 |
| 100,000 | 287 | 1 | $33.7k | $0.34 |
| 1,000,000 | 2,865 | ~10 | $337k | **$0.34** |

The plateau decomposes as **$0.256 compute** (duty-cycle share of a Spot
slot) + **$0.080 disk** — so at scale the levers change: the idle timeout
(TODO #5) attacks the compute share, while **disk becomes the irreducible
per-agent floor** (scale-invariant; verify PD minimum-size rounding and
consider cheaper archival tiers for long-suspended users). At ~$4M/yr spend,
negotiated/committed discounts move every number.

Honesty about the model at 1M: it assumes linear infrastructure. Reality
adds: sharding across ~10+ clusters (1M Sandboxes+Claims+PVCs exceed a
single cluster's etcd object budget), the Envoy data plane becomes mandatory
(~46k concurrent connections vs the 1-replica gateway), Spot capacity
diversification across zones, and mass-wake herd management — all already
tracked in the README future-work section.

## Swap experiment results (2026-07-17, Spot c4-standard-8-lssd, 318GB LSSD swap)

Measured, not modeled — one 8 vCPU / 29 GB node running real Hermes agents
(same image/env as sandboxes, ephemeral storage, idle traffic):

| Metric | Result |
|---|---|
| Steady-state Hermes RSS | **248 MiB** (vs 1 GiB requested today — TODO #4 answered) |
| Healthy agents on ONE node | **198/200** (~1.8× the no-swap RAM ceiling of ~110) |
| Cold pages parked on SSD | ~29 GB, node RAM steady at ~1.7 GB available |
| Swapped-agent response time | 117 ms avg under load; 195 ms avg / 377 ms max calm |
| Load | settled to ~1.0 after mass-boot spike |

Cost read: ~$105/mo node ÷ 200 slots ≈ **$0.53/slot** vs $5.57/slot on the
swap-less pool → at-scale compute share $0.23 → ~$0.03; floor approaches
**~$0.10–0.12/agent** with disk now the dominant term.

Caveats before productionizing: pods ran without PVCs; agents were idle
(mixed-load thrash threshold NOT probed — we stopped at 200-healthy, the
CPU-request budget, not a swap failure); Terraform provider doesn't expose
`swapConfig` yet (experiment pool lives in `hack/swap-experiment/`).
Productionizing = swap-enabled Spot pool + requests ~100m/256Mi + a
mixed-load density sweep to find the real thrash cliff.
