# Cost-reduction roadmap ($/agent/month)

**Model assumptions (every number below derives from these; all are
editable fields in `index.html`):** each agent gets 10 interactions/day
grouped into 3 conversations (30 s gaps between messages), each
interaction is 1 minute of agent work; traffic falls inside a 16 h/day
active window with a 2× peak-over-mean concurrency factor. Suspend takes
2,000 ms, resume 10,000 ms; the idle tail is 15 s after an isolated
message and 2 m while a conversation is active (deployed adaptive
policy). Per agent: 0.5 vCPU / 1 GiB requests (2 vCPU / 2 GiB limits) and
a 2 GB PVC that bills 24/7 even while suspended. Hardware: Spot
`e2-custom-16-20480` sandbox nodes (~15% reserved for system), 2 always-on
warm spares, one on-demand system node + cluster fee as fixed overhead.
Prices are GCP us-central1 list (Spot) as of 2026-07; measured inputs so
far: warm adoption ≤2 s, resume 11–20 s observed, idle Hermes RSS
~248 MiB.

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
