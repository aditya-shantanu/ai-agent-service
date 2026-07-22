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
| | Resume | 20,000–24,000 ms measured on GKE under gVisor (runc was 10,700–13,600; 4,000 ms kind — deltas are PD attach + Sentry/import boot) |
| | Idle tail — isolated message | 15 s |
| | Idle tail — active conversation | 10 m on GKE (2 m kind) — deployed adaptive policy |
| **Per agent** | Requests / limits | 100m, 256 MiB / 2 vCPU, 2 GiB (measured; Burstable for swap) |
| | Persistent disk (PVC) | 2 GB — bills 24/7, even suspended |
| **Hardware** | Sandbox nodes | Spot `n2d-standard-8` + dedicated-LSSD swap (~62 agent slots) |
| | Fixed overhead | 2 warm spares + on-demand system node + cluster fee |
| | Prices | GCP us-central1 list, Spot (as of 2026-07) |
| **Measured** | Warm adoption | ≤ 2 s (unchanged under gVisor) |
| | Resume (observed) | ~20–24 s GKE under gVisor (~11–14 s runc; 4 s kind) |
| | Idle Hermes RSS | ~248 MiB runc / ~281–295 MiB pod-level under gVisor (+~15% Sentry+gofer) |
| | Density (PVC-backed, per node) | 62 agents, mixed-load clean (28 ms idle-cohort, PSI 0) — unchanged under gVisor (CPU-bound ceiling) |

Scenario math from the model in this folder (defaults now reflect the
current deployed posture). History and remaining levers, in order of
leverage. Baseline before any of this: **$12.88/agent** (2×e2-standard-4
on-demand, 2 vCPU/2 GB requests).

## Done (2026-07-17) — deployed and e2e-validated

1. **Right-sized requests** — requests 500m/1Gi (limits 2 vCPU/2Gi) instead
   of 2/2 flat. Requests are what bind bin-packing; limits keep burst room.
   *(chart `sandbox.resources`)*
2. **Balanced machine shape** — sandbox nodes were right-shaped to
   `e2-custom-16-20480` (~1.25 GB/vCPU): enough RAM to cover GKE node
   reservations, none idle. 1:4 "standard" shapes waste RAM dollars; 1:1
   "highcpu" shapes go RAM-bound after reservations. *(Superseded by the
   LSSD-swap `n2d-standard-8` pool below; the e2 shape survives only as the
   Terraform rollback pool.)*
3. **Spot sandbox pool** — the platform is restart-tolerant *by design*
   (a preemption is just an unscheduled suspend; PVC survives, sessions
   resume, cron catches up), so sandboxes ride Spot (~70% off). Gateway +
   controllers stay on a small on-demand `system-pool`.
   *(terraform `sandbox-pool` + chart `sandbox.tolerations/nodeSelector`)*

4. **Measured, right-sized requests** — 100m / 256 MiB (steady-state RSS
   is 248 MiB), limits 2 vCPU / 2 GiB. Answered by the swap experiment's
   direct measurement. *(was TODO #4)*
5. **Adaptive idle suspension** — see roadmap item below (Level 1 shipped).
6. **LSSD swap sandbox pool** — Spot `n2d-standard-8` + dedicated local-SSD
   swap; 62 PVC-backed agents/node (3.9×), swap as the safety net that
   makes 256 MiB requests burst-proof. *(see "Productionized" below)*

Result history: $12.88 → ~$0.75–1.00 (Spot/shape/requests) →
**floor ~$0.14/agent with the swap posture** (2026-07-17). Marginal slot
cost: $5.57 → $1.50.

## The cost/UX ledger

Every optimization bought cost at a price — usually latency. The baseline
truth: an always-on agent is strictly better UX (zero wake lag, no
preemption surprises) and costs ~$270/agent/month on this hardware. Each
row's UX price, honestly, with its mitigation:

| Optimization (in order applied) | Effect on $/agent/month | UX / other downside | Mitigation in place |
|---|---|---|---|
| **Suspend-when-idle** (the architecture) | 🟢 ↓ $270 → **$12.88** (21×) — big, but only the first step | **Cold wake: lag when returning after an absence** | Login sessions + memory survive, so lag is the *only* symptom; wake ≈ one LLM turn |
| **Warm pool** | ≈ neutral (~$3/mo spares) | ✅ none — signup drops to ~2 s (a UX *win*) | — |
| **Right-sized requests + balanced machine shape** | 🟢 ↓ → **$1.59** | Burst contention if many agents work at once (CPU throttling → slower replies at peak) | Limits keep 2 vCPU of burst headroom; mixed-load tested clean at 4× modeled peak |
| **Spot sandbox nodes** | 🟢 ↓ → **$0.75** | Rare, *unplanned* stall mid-session on preemption; an in-flight turn can be dropped | Hermes flags sessions `restart_interrupted` and auto-resumes; gateway/controllers stay on-demand |
| **Adaptive suspension** (15 s / 2 m windows) | ≈ neutral | ✅ conversations pay the cold wake once, not per message; returns after the window still hit it | `idle.activeTimeout` is a per-tier dial (see `../investigations/`) |
| **LSSD swap + measured requests** (100m/256Mi) | 🟢 ↓ → **$0.14** at-scale floor (slot $5.57 → $1.50, 3.9× density) | Swapped-agent wake +100–400 ms (measured; invisible vs LLM); *theoretical* thrash if far more agents go active than modeled | Mixed-load tested clean at 20% concurrent; PSI alerting is the open TODO |
| **Cron-aware wake** | ≈ neutral — *protects* the savings (jobs no longer force always-on pods) | Jobs can fire up to ~1 min late; jobs longer than the 2 m grace risk interruption | Hermes boot catch-up fires missed jobs once; `cron.grace` is a knob; Telegram users are exempt entirely |
| **Startup tuning** (aggressive readiness probe + GKE image streaming) | 🟢 ↓ slightly (shorter resumes = less pod-time) | ✅ cold resume 16–25 s → ~11–14 s on GKE (pre-gVisor; remaining chunk is PD attach) and 12 s → 4 s on kind | — |
| **Production active window 10 m** (GKE) | 🔴 ↑ ≈ **+$0.06** — accepted | ✅ most same-day returns land on a swapped-resident agent: **sub-second wake instead of cold resume** | Window is a per-tier values knob; swap pool makes residents cheap |
| **gVisor sandboxing** (GKE Sandbox) | ≈ neutral — no GCP charge, 62 slots/node unchanged (CPU-bound), swap still reclaims Sentry memfd memory | Cold resume **11–14 s → 20–24 s** (+Sentry boot & import gofer tax); +15% memory/agent eats *future* memory-bound squeeze headroom | Agents get kernel-syscall isolation on top of NetworkPolicy; the resume roadmap (stage-in/out storage, boot tuning) attacks the same delta |

The user-visible price of ~1000× cheaper agents is **one cold-start pause
per return-after-absence, and the occasional Spot hiccup**. Both shrink with
the roadmap (wider active windows now; stage-in/stage-out storage later
makes even cold wakes ~1–3 s). The suspension UX tax is now measured
continuously — `make bench` compares the resume path against an
always-alive baseline agent and gates on budgets (`../benchmarks/`).

Notes on the adaptive-suspension model effect (Level 1, deployed): with the
kind knobs (2 m active tail, 3 conversations/day, 30 s gaps) the *pre-swap*
plateau moved ~$0.34 → ~$0.31/agent — conversations pay the *active* tail
(120 s), not the base one. At those defaults Level 1 is mostly a UX win (no
mid-conversation wakes) and it makes `activeTimeout` the direct cost dial.
Level 2 (in-pod busy probe) and Level 3 (predictive/pre-warm) remain future
work. These dollar figures predate the swap posture; the current curve is
the table below.

## Next levers, in order

1. **GKE free tier / fee check.** The $0.10/h cluster fee is waived for one
   zonal cluster per billing account. Confirm on the bill; if another
   cluster claims it, that's still ~$73/mo across all agents.
2. **Committed-use discounts.** Once baseline usage is predictable: 1-yr
   (~37%) or 3-yr (~55%) CUD covering the system pool + the always-on
   fraction of the sandbox pool; Spot continues to cover burst.
3. **Faster resume — PARTIAL (2026-07-17).** Done: aggressive readiness
   probe (was adding 10–15 s of pure wait: kind resume 12 s → **4 s**; GKE
   16–25 s → **~11–14 s** pre-gVisor) and GKE image streaming on the
   swap pool (fast cold nodes). Remaining GKE chunk is **PD attach
   (~10 s)** — owned by the stage-in/stage-out storage design
   (`investigations/resume-latency-and-storage.md`), not by boot tuning.
   Resume latency is tracked continuously by `make bench`
   (`../benchmarks/`).
4. **Cluster autoscaler on the sandbox pool.** Fixed node counts pay for
   the peak all day. Autoscaling the Spot pool (min 1) trims the off-peak
   tail; pairs naturally with #2's CUD floor.
5. **Per-user cost attribution.** Not a reducer, but the prerequisite for
   pricing: meter pod-seconds + LLM tokens per user (the LLM key is
   platform-shared today; an LLM proxy with per-user keys/quotas is the
   end state — see README future work).
6. **Sub-100m CPU requests with swap-backed overcommit** — pushes past the
   62/node CPU ceiling; needs a busier mixed-load sweep first.

Re-derive any scenario by editing the fields in `index.html` — that is the
tool's job.

## Scale behavior: to a million agents

$/agent falls steeply while fixed costs (cluster fee, system pool, warm
spares) amortize, then **plateaus at the marginal cost** — with the current
posture that plateau is reached by ~10k agents:

Updated 2026-07-17 for the deployed swap posture (n2d-standard-8 Spot +
LSSD, 62 slots/node):

| agents | Spot nodes | clusters | $/month | $/agent |
|---|---|---|---|---|
| 100 | 1 | 1 | $223 | $2.23 |
| 1,000 | 1 | 1 | $295 | $0.30 |
| 10,000 | 7 | 1 | $1.6k | $0.16 |
| 100,000 | 68 | 1 | $14.5k | **$0.14** |
| 1,000,000 | 676 | ~10 | $144k | **$0.14** |

(Pre-swap posture for comparison: $0.34 plateau, $337k/mo at 1M.)

The plateau now decomposes as **~$0.063 compute** (duty-cycle share of a
$1.50 slot) + **$0.080 disk** — disk is the MAJORITY of the floor. The
next lever ranking flips accordingly: PVC cost (minimum-size rounding,
cheaper tiers, archival snapshots for dormant users) now outranks every
compute optimization. At ~$1.7M/yr spend for 1M agents, negotiated
discounts move every number.

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
`swapConfig` yet (pools are created by `hack/gke-swap-pool.sh` /
`hack/gke-gvisor-pool.sh`; see `terraform/README.md`).
Productionizing = swap-enabled Spot pool + requests ~100m/256Mi + a
mixed-load density sweep to find the real thrash cliff.

## Productionized (2026-07-17): swap pool is the deployed posture

Full-fidelity validation on `n2d-standard-8` Spot + dedicated-LSSD swap
(c4 was rejected: Hyperdisk-only, cannot attach existing pd-balanced user
PVCs; the dedicated-swap profile also requires ephemeral-storage LSSDs):

- **62 PVC-backed agents/node** (vs 16 before, 3.9×) — ceiling is the 100m
  CPU request, not memory or the ~128 disk-attach limit; swap is the
  safety net that makes 256Mi requests safe against burst overlap.
- **Mixed load (20% concurrently active): zero degradation** — idle cohort
  28 ms avg responses, memory PSI 0.00, load 0.33.
- Deployed: chart requests 100m/256Mi (Burstable — required for kubelet
  swap), sandboxes scheduled to `hermes-swap` pool
  (`hack/gke-swap-pool.sh`; gcloud-managed until the Terraform provider
  exposes `swapConfig`), warm spares migrated, e2e green.
- **New floor: ~$0.14/agent** ($93/mo node ÷ 62 slots × duty×peak + disk).
  Next squeeze documented: n2d-highcpu + swap-backed overcommit and/or
  smaller CPU requests push toward ~$0.10; disk is now ~60% of the floor.
- Rollback: flip `values-gke.yaml` selectors back to `hermes-sandbox`
  (the Terraform `sandbox-pool` is kept, idle, as an instant rollback
  target — ~$89/mo; remove it once confident in the swap posture).

## gVisor (GKE Sandbox) impact analysis (2026-07-17): floor unchanged at ~$0.14

Sandboxes moved to `hermes-gvisor-pool` — same shape and price as the swap
pool (Spot `n2d-standard-8` + dedicated-LSSD swap + image streaming), plus
`--sandbox type=gvisor`. **GKE Sandbox itself has no SKU — GCP charges
nothing extra for gVisor.** All numbers below measured on the live pool,
not modeled:

| Dimension | runc (before) | gVisor (measured) | $/agent effect |
|---|---|---|---|
| Node price | $93/mo Spot + LSSD | identical (same shape, no sandbox charge) | none |
| Slots/node | 62 (CPU-request-bound) | **62 — unchanged**: ceiling is the 100m CPU request (6.2 vCPU), not memory; GKE's gvisor RuntimeClass sets no pod `overhead`, so scheduler math is identical | none |
| Memory/agent | 248 MiB RSS | 281–295 MiB pod-level (+~15%); host-side Sentry+gofer ≈ 58 MiB/pod (12 runsc procs / 5 pods = 289 MiB) | none today — at 62 agents that's +~3.6 GB/node absorbed by the 318 GB swap headroom |
| Swap compat | measured 5 GB paged | **verified**: gVisor guest memory is memfd-backed ⇒ host-swappable; forced 24 GiB of demand → 5.1 GB paged to LSSD, all pods healthy, PSI ~0.3, idle-cohort 17–88 ms | none — the density mechanism survives |
| Resume | 11–14 s | **20–24 s** (+~9 s: PD attach unchanged ~6–8 s; container→Ready grew 2–3 s → ~8 s = Sentry boot + Python import gofer tax) | +~1% pod-time ≈ **+$0.001–0.003** — resume is paid 3×/day, sensitivity is cents by design |
| CPU / I/O | — | CPU loop parity (0.30 vs 0.31 s); metadata-heavy small-file I/O ~1.6× slower (0.33 vs 0.20 s per 6k ops); SQLite WAL commits *faster* (Sentry write-back caching) | none — workload is LLM-bound |
| Warm adoption | ≤2 s | ≤2 s (pods pre-exist; adoption is a label flip) | none |

**Bottom line: kernel-syscall isolation for ~free in dollars.** The floor
stays **~$0.14/agent**; the real prices are (a) cold resume grows one more
LLM-turn of dead air (20–24 s — same mitigation roadmap as before:
stage-in/stage-out storage attacks the PD-attach half, probe/boot tuning
the rest), and (b) **latent headroom loss**: the +15% memory/agent shrinks
the *future* memory-bound squeezes (the 198-agents/node no-PVC experiment
would land ~15% lower under gVisor, and the sub-100m-CPU-request lever
gets less room before the thrash cliff). Transitional cost only: the runc
`hermes-swap-pool` (~$93/mo) stays up as an instant rollback target —
remove it once confident, same playbook as the swap migration.

Caveats, honestly: GKE Sandbox blocks `kubectl port-forward` to sandboxed
pods (unused — the gateway proxies via the headless Service) and
container-level memory metrics (pod-level works — PSI/`memory.current`
alerting, the open TODO, is unaffected). gVisor's write-back caching means
an fsync ack does not guarantee host durability for *rootfs* writes
(in-memory overlay); Hermes state lives on the PVC and survived the full
e2e kill/recreate cycle, but a node crash mid-commit has a wider window
than runc — acceptable for conversation state, worth re-checking if we
ever store payments-grade data.
