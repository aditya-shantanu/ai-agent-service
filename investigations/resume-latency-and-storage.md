# Investigation: resume latency (PD attach) and the Filestore question

**Date:** 2026-07-17 · **Status:** analysis only — no changes made.
**Question asked:** resume takes 10s+, mostly PD attach. Can we use
[Filestore zonal](https://docs.cloud.google.com/filestore/docs/service-tiers-zonal)
instead? Why or why not, and what does it do to $/agent at scale?

## Where resume time goes

Measured evidence: resume on kind (no real block attach) is ~11–12 s; on
GKE 16–25 s. The delta — roughly **5–13 s — is PD attach/detach** plus
scheduling variance. The remainder (scheduling + Hermes s6/Python boot)
would be untouched by any storage change.

## Option A: Filestore (live NFS) — evaluated and REJECTED for direct use

**What it would fix.** NFS has no per-VM block attach: pods network-mount a
share near-instantly. It would also dissolve the ~128 disk-attach-per-VM
limit that eventually caps density.

**Why not — two decisive problems:**

1. **SQLite over NFS is a correctness hazard.** Hermes' core state
   (`state.db`) runs WAL journaling (`state.db-shm`/`-wal` are in the
   image's own boot chown list). WAL requires shared-memory mmap semantics
   that do not work over NFS; even non-WAL SQLite on NFS has a long history
   of lock-related corruption. Moving `/opt/data` onto NFS risks the one
   file that must never corrupt. Near-disqualifying by itself.
2. **The economics invert.** Filestore bills *provisioned instance*
   capacity: zonal ≈ $0.25–0.35/GiB-mo vs PD-standard $0.04 (6–8×), with a
   ~1 TiB minimum instance (~$300/mo before the first user). Disk is
   already ~60% of the $0.14 at-scale floor.

| Storage | Disk $/agent | Floor $/agent | Resume attach | Risk |
|---|---|---|---|---|
| PD today | $0.08 | **$0.14** | 5–13 s | baseline |
| Filestore, per-user shares | ~$0.50+ | ~$0.56 | ~0 s | SQLite/NFS + 4× cost |
| Filestore, one shared thin pool (users as subdirs, ~150–200 MB real usage) | ~$0.06 | ~$0.12 | ~0 s | SQLite/NFS, weak per-dir quotas, shared blast radius, NetworkPolicy egress change (Filestore IP is RFC1918, which sandbox egress deliberately blocks) |
| Stage-in/out (Option C) | ~$0.01 | ~$0.075 | ~0 s (+1–3 s staging) | sync-loss window, real engineering |

Note the only economically attractive Filestore shape (shared thin pool)
still carries the SQLite/NFS hazard — it doesn't escape problem 1.

## Option B: make attach latency RARE instead of fast (`idle.activeTimeout` dial)

Already shipped (adaptive suspension). An agent has three states: active
(RAM), **resident-idle** (pod alive, memory paged to local-SSD swap, ~zero
real CPU/RAM), suspended (pod gone, PD detached). Waking from
resident-idle is a sub-second page-in; from suspended, the full 16–25 s.
Raising `activeTimeout` keeps users in the fast tier between conversations.

**Benefits:** most 16–25 s waits become <1 s; zero engineering (one values
knob, per-tier capable); fewer suspend/resume cycles (less PD churn, cron
fires natively in-window).

**Drawback — the honest correction:** a swapped-out resident agent uses
almost no real resources, **but the scheduler still reserves its requests**
(100m/256Mi = 1/62 of a node). Bin-packing charges list price for parked
cars:

| `activeTimeout` | Resident time/day (3 conv) | At-scale floor | Wake UX |
|---|---|---|---|
| 2 m (today) | ~20 min | **$0.14** | most returns pay 16–25 s |
| 10 m | ~50 min | ~$0.20 | quick follow-ups instant |
| 30 m | ~1.9 h | ~$0.31 | most same-day returns instant |
| 60 m | ~3.2 h | ~$0.44 | nearly all returns instant |

Mitigations: sub-100m CPU requests (idle-resident true usage ≈ 0; swap is
the overcommit safety net — needs a density re-test) and per-tier windows
(paid 60 m / free 2 m). Likely sweet spot 10–15 m, pending real data on
inter-conversation gap distribution. Model it live: `costcalc/index.html`
(`activeTailS`).

Secondary drawbacks (minor): more resident pods ⇒ more users feel a Spot
preemption as a surprise wait; larger per-node blast radius.

## Option C: stage-in/stage-out (the storage endgame)

Keep SQLite on fast **local** storage (emptyDir on the node's LSSD); on
wake, stage state (~150 MB ⇒ 1–3 s) from a cheap cold store (GCS
$0.023/GiB, or a shared Filestore used only as an object shelf, never for
live SQLite); on suspend, stage out. Kills PD attach entirely, removes the
disk-attach density ceiling, drops disk to ~$0.01/agent → floor toward
**~$0.075**. Cost: real engineering — suspend must flush reliably, and a
Spot preemption between syncs loses the delta (the ~25 s preemption notice
usually covers a stage-out; "usually" must be designed for, e.g.
periodic incremental sync + journaled upload).

## Recommendation

1. **Now:** tune Option B (start 10–15 m; measure gap distribution to
   place the knob with data).
2. **Next storage project:** design Option C properly (alongside the cron
   and Envoy design docs) — it solves latency AND the disk-dominance of
   the cost floor in one move.
3. **Rejected:** live-NFS Filestore for `/opt/data` — SQLite/WAL
   correctness risk at any price point, and per-user shares also quadruple
   the floor.
