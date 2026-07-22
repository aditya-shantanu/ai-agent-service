# Cost calculator: $/agent/month on GKE

Open `index.html` in a browser (no build, no dependencies):

```sh
open costcalc/index.html
```

## What it answers

Given fixed hardware and the platform's suspend/resume behavior, how many
agents fit, and what does one agent cost per month? Defaults are the
**golden scenario** — the currently deployed GKE posture — so what you see
on open is what production runs. Tiles:

- **$/agent/month** (hero) vs the always-on baseline,
- **$/agent at scale** — the marginal floor once fixed costs amortize
  (~10k+ agents),
- capacity, duty cycle, and cluster cost breakdown.

Charts: resume-time sensitivity sweeps (x-axis in **ms**) and the scale
curve (why $/agent falls with fleet size, then plateaus at the floor).

## The model (conversation-aware — matches the deployed adaptive policy)

Messages group into conversations; the pod stays up through
intra-conversation gaps, so **resume, idle tail and suspend are paid per
conversation, not per message**:

```
pod-time/conversation = resume + msgs*work + (msgs-1)*gap + active-tail + suspend
pod-time/day          = conversations * that (+ cron wakes)
duty cycle            = pod-time/day / active window
max agents            = (slots - spares) / (duty * peak factor)
$/agent/month         = (nodes + fixed + PVC disk) / max agents
floor $/agent         = slot-cost * duty * peak + disk     (fixed costs amortized)
```

## Defaults = deployed posture (all editable in the UI)

The model assumptions in `COST-REDUCTION.md` are the source of truth; this
table maps them to the calculator's fields.

| Assumption | Default | Source |
|---|---|---|
| Per-agent requests | 100m vCPU / 256 MiB (limits 2 / 2 GiB) | measured: steady-state RSS ~248 MiB runc / ~283 MiB pod-level under gVisor |
| Node | Spot `n2d-standard-8` + $36/mo LSSD (swap), **gVisor** (no extra charge) | deployed gVisor pool (`scripts/gke-gvisor-pool.sh`) |
| Resume / suspend | **22,000 ms / 2,000 ms** | measured 2026-07-17 under gVisor: GKE 20–24 s (runc was 10.7–13.6 s; kind 4.0 s — deltas are PD attach + Sentry/import boot) |
| Idle tails | 15 s isolated / **600 s** conversation | deployed adaptive policy (10 m GKE window) |
| Usage | 10 msgs/day in 3 conversations, 1 min work, 30 s gaps | stated assumptions |
| Traffic | 16 h window, 2× peak factor | assumption |
| Prices | GCP us-central1 Spot list (2026-07) | edit for CUDs/negotiated |

## How to read it

- The **at-scale floor tile** is the number scale buys you; the hero tile
  includes fixed costs at your entered hardware size.
- The floor currently reads **~$0.21**: that is the $0.14 swap-posture
  floor plus ~$0.06 deliberately spent on the 10-minute active window
  (sub-second wakes for same-day returns). Set the conversation tail back
  to 120 s to see the $0.14 economics.
- **Resume-time sensitivity is modest by design**: with adaptive
  suspension, resume is paid once per conversation (3×/day), so even
  halving it moves the floor by cents. The disk line ($0.08/agent) is now
  the biggest single component — see `COST-REDUCTION.md` for the lever
  ranking and `../investigations/resume-latency-and-storage.md` for the
  storage endgame that attacks both.

Full optimization history, assumptions and next levers:
[`COST-REDUCTION.md`](COST-REDUCTION.md).
