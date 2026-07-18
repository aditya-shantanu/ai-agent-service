# Design decisions & caveats

Every load-bearing decision lives here. If you change one, update this list.

### Agent runtime

1. **Upstream Hermes image, unmodified, pinned** (`nousresearch/hermes-agent:v2026.7.7.2`).
   The upstream image already separates immutable code (`/opt/hermes`) from
   state (`HERMES_HOME=/opt/data` → our PVC), supervises the dashboard +
   messaging gateway via s6-overlay, and seeds config on first boot. A custom
   image bought us nothing. Full validated env contract: `docs/hermes-image.md`.
   *Caveat: image is amd64-only — Apple Silicon dev machines run it emulated
   (kind works via Rosetta binfmt); GKE amd64 default node pools are fine.*
2. **Two user surfaces per sandbox**: web dashboard (`:9119`, cookie-session
   auth via basic-auth login — the login form flows through our proxy
   untouched) and OpenAI-compatible API (`:8642`, per-request bearer; the
   proxy strips the user's platform token and injects the shared
   `API_SERVER_KEY` upstream). Both env-configured ⇒ warm-pool compatible.
3. **`HERMES_DASHBOARD_BASIC_AUTH_SECRET` must be set** (shared): dashboard
   session cookies are HMAC-signed with it. Without it every suspend/resume
   would log all users out. With it, sessions survive suspend/resume —
   verified by e2e check #7.
4. **Shared platform credentials inside sandboxes** (dashboard basic auth,
   `API_SERVER_KEY`, LLM provider keys are identical in every sandbox).
   Required for warm pools: pod env is baked before the user is known.
   *Caveat: the NetworkPolicy admitting only the gateway is therefore the
   real per-user isolation boundary — keep `networkPolicy.enabled: true`.
   Enforcement verified on kind (kube-network-policies) and on GKE
   Dataplane V2; verify on any other CNI before onboarding users. DNS egress
   must be port-53-to-anywhere, NOT a kube-dns podSelector: GKE runs
   NodeLocal DNSCache as a host-network daemon, and Cilium's host identity
   matches neither podSelectors nor ipBlocks — the selector-scoped rule
   silently breaks all sandbox DNS on GKE (found via a failing real-LLM
   call; fixed and validated).*
5. **The pod is the sandbox.** Hermes warns its terminal backend is
   "local/unsandboxed"; intentional here — an agent can only affect its own
   pod and PVC.

### Provisioning (agent-sandbox v0.5.2, pinned)

6. **SandboxClaim + SandboxWarmPool for fast first start.** Per-user claims
   keep `spec.env` and `spec.volumeClaimTemplates` empty — either would
   bypass the warm pool. Our template sets both injection policies to
   `Disallowed`, so such claims are **rejected outright**
   (`EnvVarsInjectionRejected`) instead of silently cold-starting. The only
   warm-compatible per-claim customization is `additionalPodMetadata`
   (pod labels must use the `sandbox.users.io` domain under default
   controller flags). Measured: warm adoption ≤2s; resume 4s on kind /
   ~11–14s on GKE (post probe-tuning + image streaming; GKE delta is PD
   attach — see `investigations/resume-latency-and-storage.md`).
7. **Never set claim `lifecycle`.** Every claim-expiry path deletes the
   Sandbox, which garbage-collects the user's PVC (their entire memory).
   User deletion = delete the claim (deliberate cascade: sandbox + PVC +
   claim-owned Secrets). Cost saving comes exclusively from
   `Sandbox.spec.operatingMode`, which no controller fights.
8. **Sandbox name ≠ claim name.** Warm-adopted sandboxes keep their
   pool-generated name. Always resolve
   `claim.status.sandbox.name → Sandbox → status.serviceFQDN`.
9. **Suspend deletes only the pod**; PVC and per-sandbox headless Service
   survive; resume reattaches the same PVCs. Pod-spec changes (e.g. a new
   provider-keys secret value) take effect on the next pod recreation —
   i.e. a suspend/resume cycle.

### Control plane

10. **No database.** The SandboxClaim *is* the user record; the per-user
    bearer token is stored only as a SHA-256 annotation on the claim
    (constant-time compare; raw token shown once at create/rotate); the
    Telegram token lives in a claim-owned Secret. `kubectl` is the admin UI
    of last resort.
11. **One Go binary, one replica.** Control plane + proxy + idle tracker
    share in-memory state (per-user wake mutexes, activity map). The
    Deployment uses `strategy: Recreate`. Scale-out path (sticky routing or
    moving last-activity to claim annotations + leader election) is a
    documented follow-up, not built.
12. **Custom application-level gateway instead of sandbox-router or a K8s
    Gateway/Ingress**: per-user authz, wake-on-request, and idle tracking are
    custom logic; a second hop buys nothing. Proxy techniques (dial-retry,
    `FlushInterval: -1`, Origin-strip on WebSocket upgrade) are borrowed from
    sandbox-router.
13. **Wake-on-connect holds the request** (up to `gateway.wakeTimeout`, 60s)
    rather than failing fast; observed resume is 4s (kind) to ~12s (GKE).
    Timeout → 503 +
    `Retry-After`. It is safe to flip `operatingMode` back to Running
    mid-suspension — never wait for `Suspended=True` first.
14. **Users with a Telegram token are exempt from idle suspend** by default
    (`idle.suspendTelegramUsers: false`): the bot long-polls from inside the
    sandbox and dies while suspended. Explicit `POST /suspend` still works.
    Webhook-mode Telegram through the gateway is the v2 fix.
15. **Telegram tokens are injected at runtime** (`pods/exec` → rewrite
    `$HERMES_HOME/.env` on the PVC → `s6-svc -r gateway-default`), because
    claim env is rejected by policy and template env is shared. The write
    lands on the PVC ⇒ survives suspend/resume without re-injection. Inputs
    are format-validated before touching a shell.
16. **Cron jobs wake suspended sandboxes** (`docs/cron-wake-design.md`):
    at suspend time the gateway reads the pod's `cron/jobs.json` (Hermes
    precomputes `next_run_at`) onto a claim annotation; a 30s waker loop
    resumes due sandboxes, execs `hermes cron tick`, and grants a 2m grace
    window the idle sweeper honors. One-shot jobs wake exactly once;
    recurring jobs re-capture at every suspend. Hermes' boot catch-up makes
    late wakes harmless (job fires once, no burst). Wake provenance is
    observable: `nextCronWake` + `lastWakeReason` (`connect|api|cron`) in
    the user status API.
17. **Gateway restart forgives idleness**: the in-memory activity map dies
    with the process, so after a gateway restart every user gets a fresh
    idle window (worst case: one extra `idle.timeout` of runtime). Suspended
    users stay suspended.
18. **Adaptive idle suspension (Level 1)**: 15s tail after an isolated
    request; 2m while a conversation is active (two activities within
    `idle.activeTimeout`); decays back automatically. Conversations pay
    resume+tail once, not per message; bookkeeping (first-sight, admin
    polling) never counts as activity.

### Packaging

19. **agent-sandbox is a documented prerequisite**, installed from its pinned
    release manifest (`sandbox-with-extensions.yaml`, v0.5.2 — `make
    sandbox-install`) — not vendored as a subchart (upstream chart is
    unpublished and drift-prone). Go module pin matches: `sigs.k8s.io/agent-sandbox v0.5.2`.
20. **Helm chart** (`charts/hermes-service`) carries gateway, SandboxTemplate,
    WarmPool, RBAC and secrets. Platform secrets are **generated on first
    install and preserved across upgrades** (`lookup`-based); bring your own
    via `secrets.*.existingSecret`. `storageClassName: ""` = cluster default
    (works on kind and GKE). Provider keys go in one Secret injected via
    `envFrom` — add any `*_API_KEY` without touching the chart.
21. Images for GKE live in Artifact Registry in `gke-ai-eco-dev`
    (`make images-push` builds amd64 and mirrors the pinned Hermes image).
22. **GCP infra is Terraform** (`terraform/`): cluster (DPv2 + Workload
    Identity), system node pool, Artifact Registry, the Secret Manager
    secret *container*, and ESO's IAM binding. Deliberately NOT Terraform:
    secret *values* (never in TF state — pushed from `.env` via
    `make gsm-push-key`) and the swap node pool (provider doesn't expose
    `swapConfig` yet — `hack/gke-swap-pool.sh`, idempotent via
    `make gke-swap-pool`).

### Cost posture (measured — see costcalc/)

23. **Split node pools**: small on-demand `system-pool` (gateway,
    controllers, ESO — stable footing) + Spot sandbox pool. Spot preemption
    is just an unscheduled suspend; the platform absorbs it by design.
24. **Sandboxes run on an LSSD-swap Spot pool** (`n2d-standard-8` + dedicated
    local-SSD swap): 62 PVC-backed agents/node measured (CPU-request bound;
    memory and the ~128 disk-attach limit both clear), mixed load clean.
    *Caveats: pods must stay Burstable (requests<limits) or kubelet swap is
    off; c4 machines are Hyperdisk-only and cannot attach existing
    pd-balanced user PVCs — that's why n2d.*
25. **Requests are measured, not guessed**: steady-state Hermes RSS is
    248 MiB → requests 100m/256Mi, limits 2 vCPU/2Gi; swap is the safety
    net against burst overlap. Cost history and the trade-offs table above
    cover the numbers; full roadmap: `costcalc/COST-REDUCTION.md`,
    interactive model: `costcalc/index.html`.

