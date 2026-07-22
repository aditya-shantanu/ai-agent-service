# Validation log

A dated, historical record of what has been validated against real clusters
(kind and/or GKE) — nothing here is inferred. Numbers in each entry are what
was measured **at that time**; current latency numbers are tracked repeatably
by `make bench` (see `../benchmarks/README.md`).

## Capability checklist

| Capability | Evidence / entry point |
|---|---|
| Hermes image contract validated | `hermes-image.md`, `make validate-hermes-image` (5 checks) |
| Raw agent-sandbox layer bring-up | validated during bring-up with raw manifests (since retired; `make e2e` supersedes) |
| Control plane REST API | unit tests + live kind validation |
| Proxy + wake-on-connect + idle suspend | `make e2e`; resume ~4s kind / ~20-24s GKE+gVisor |
| Telegram token injection + suspend exemption | `make e2e` |
| Helm chart + full-loop e2e | `make e2e` — 11 checks, green on kind and GKE |
| GKE production deployment (DPv2, NetworkPolicy enforced) | `make deploy-gke` + e2e |
| Cron-aware wake (zero user traffic) | e2e check #9 |
| Infra as Terraform + Secret Manager keys | from-zero rebuild validated; ESO/Workload Identity sync |
| gVisor sandbox hardening (GKE Sandbox) | e2e 11/11 on the gVisor pool; swap density verified under gVisor |
| Cost posture (Spot, shape, adaptive suspension, LSSD swap) | $12.88 → $0.14-at-scale floor — `../costcalc/COST-REDUCTION.md` |
| UX/performance benchmark vs always-alive baseline | `make bench` / `make bench-gke` — budgets in `../benchmarks/` |

## Detailed validations

| Validation | Where |
|---|---|
| Hermes image env contract (auth gates, session-secret survival, telegram `.env` injection, non-root, restart persistence) — `make validate-hermes-image`, 5 checks | Docker |
| agent-sandbox layer (warm adoption ≤2s, env-claims rejected, NetworkPolicy enforced, suspend retains PVC+Service, exec injection) — via since-retired raw-manifest rehearsal scripts, all behaviors now re-verified continuously by `make e2e` | kind |
| Full platform loop (provision → proxy auth + both surfaces → idle suspend → wake-on-connect → session survival → telegram → cron wake with zero traffic → idempotent replay → cascade delete) — `make e2e`, 11 checks | kind + GKE |
| Multi-user concurrency (parallel warm/cold signups, concurrent traffic, cross-user 401 isolation, differential idle-suspend, transparent wake) — `make simulate-users` | kind |
| NetworkPolicy is the isolation boundary (unlabeled pods blocked, gateway label admitted) | kind (kube-network-policies) + GKE (Dataplane V2) |
| GKE production deploy (AR images, warm pool 72s to Ready from zero, e2e 11/11) — `make deploy-gke` | GKE |

## 2026-07-17 — Swap density + mixed load

62 PVC-backed agents on one n2d-standard-8 Spot node (3.9× the pre-swap
density), 20% concurrently active → idle cohort 28ms average response,
memory PSI 0.00. An earlier no-PVC rig reached 198 agents/node with ~29GB
paged to local SSD and 117–195ms swapped responses. (The one-off experiment
rigs were retired after the results were recorded in `../costcalc/`.)

## 2026-07-17 — gVisor (GKE Sandbox) migration

e2e 11/11 on the gVisor pool (warm adoption 2s, wake 20–24s, cron wake,
telegram exec-inject, cascade delete); s6 setuid bootstrap elevates
(`euid=0`) under GKE's managed runsc; forced 24 GiB memory pressure → 5.1 GB
of memfd sandbox memory paged to LSSD, all pods healthy, PSI ~0.3,
idle-cohort 17–88ms; CPU benchmark parity, small-file I/O 1.6×, +15% pod
memory.

## 2026-07-17 — From-zero Terraform rebuild

`terraform apply` (cluster/WI/AR/GSM/IAM) → full deploy chain → e2e 11/11 →
real Gemini chat → memory recalled across suspend/kill/wake in 25s (key
synced from Secret Manager via ESO/Workload Identity).

## 2026-07-17 — Durability deep-dive: does everything survive kill/recreate?

A dedicated two-cycle suspend/recreate test (explicit suspend, then two
autonomous cron wakes, zero user traffic):

- **Agent survives**: all three s6 services healthy after each recreation;
  a dashboard login session created *before* the first kill was still valid
  after it (users are never logged out by suspension).
- **Cron jobs continue across recreations** — a recurring `every 2m` job
  fired 3× across two kill cycles: once per cron-wake and once via the
  in-pod ticker while awake between cycles. The
  suspend→capture→wake→fire loop is self-sustaining. One-shot jobs wake
  exactly once, verified separately.
- **Skills / memory / filesystem changes survive byte-for-byte**: a planted
  custom skill, a `MEMORY.md` append, and a workspace file had identical
  md5 checksums after recreation. Everything under `/opt/data` (PVC) is
  durable; anything outside it dies with the pod, by design.

## 2026-07-17 — Real-LLM validation (Gemini via the .env flow)

With a real `GEMINI_API_KEY` loaded via `make set-provider-key`:

- **Live chat through the full stack** (proxy → Hermes API server → Gemini).
- **Agent memory survives the kill**: the agent saved a codeword to its
  persistent memory (verified on-PVC), the pod was suspended and deleted,
  and a single chat request against the dead sandbox woke it and answered
  correctly in 15 seconds total (wake + LLM).
- **Skills are actively loaded, not just stored**: a hand-planted skill file
  was found and used by the recreated pod's agent.
- Gap found and fixed during this validation: Hermes' seeded config defaults
  to an Anthropic model (404s on Gemini-only deployments) — the chart now
  pre-seeds `hermes.defaultModel` (default `google/gemini-flash-latest`) via
  an init container; a fresh user chats with zero manual setup.

## 2026-07-20 — UX benchmark: baseline vs cost-optimized lifecycle

First calibrated `hermes-bench` runs. kind: baseline probe p50 13–21ms,
resume p50 4.0–5.0s, warm signup 2.0s, cold signup 8.0s. GKE (gVisor):
baseline 172ms, resume p50 23.8s (suspension UX tax +23.6s), warm signup
2.1s. Zero wake errors; 3-way thundering-herd coalesced into a single
resume. Budgets recorded in `../benchmarks/budgets-*.yaml`.
