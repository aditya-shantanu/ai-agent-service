# Cost calculator: $/agent/month on GKE

Open `index.html` in a browser (no build, no dependencies):

```sh
open costcalc/index.html
```

## What it answers

Given fixed hardware and the platform's suspend/resume behavior, how many
agents fit, and what does one agent cost per month? The headline tile is
**$/agent/month**, shown against the "always-on" baseline (what an agent
would cost if we never suspended) so the value of the suspend architecture
is visible. Two sweep charts show how **faster resume times** raise agent
capacity and cut $/agent on the same hardware — the question we'll re-ask
after each performance improvement (e.g. the Envoy data plane, image
pre-pull, smaller boot).

## The model

Per interaction, a pod is alive for:

```
resume + work + idle-timeout + suspend      (default: 10 + 60 + 60 + 2 = 132s)
```

- **pod-seconds/day/agent** = interactions/day × the above, plus optional
  cron wakes × (resume + cron-grace + suspend).
- **duty cycle** = pod-seconds/day ÷ active window (default 16 h — traffic
  isn't uniform over 24 h).
- **capacity** (concurrent pod slots) = usable vCPU or RAM (after system
  reserve) ÷ per-agent request, minus always-on warm-pool spares.
- **max agents** = capacity ÷ (duty × peak factor). Peak factor covers
  the reality that concurrency spikes above the mean.
- **cost/month** = node vCPU+RAM hourly × 730 + GKE cluster fee +
  per-agent PVC disk (PVCs bill even while suspended — every agent pays
  disk 24/7, which is exactly the architecture's bet: disk is ~100× cheaper
  than compute).
- **$/agent/month** = cost ÷ max agents.

Assumptions to know about: interactions are far enough apart that each pays
the full idle-timeout tail (bursty users would be cheaper); capacity is a
hard bin-pack on requests (no overcommit); node count is implied by the
vCPU/RAM totals you enter.

## Defaults (edit any field in the UI)

| Assumption | Default | Source |
|---|---|---|
| Per-agent | 2 vCPU / 2 GB RAM / 2 GB disk | stated requirement |
| Hardware | 8 vCPU / 32 GB (2× e2-standard-4) | current GKE cluster |
| Suspend / resume | 2 s / 10 s | to be measured (README validations: ~11–20 s observed resume) |
| Idle timeout | 60 s | platform default (`idle.timeout: 1m`) |
| Usage | 10 interactions/day × 1 min | stated assumption |
| Prices | e2 on-demand us-central1, $0.10/h cluster fee, $0.04/GB-mo PD | GCP list prices — edit for CUDs/spot |

## The insight the sweep makes visible

With defaults, per-interaction pod-time is 132 s, of which the **idle
timeout (60 s) is the largest platform-controlled share** — cutting resume
from 10 s to 0 s only shrinks pod-time by ~8%. Resume speed becomes a
first-order lever only after the idle timeout is tightened (or made
adaptive). The footer line under the charts recomputes this trade-off from
whatever values you've entered.
