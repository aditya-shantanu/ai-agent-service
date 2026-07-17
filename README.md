# ai-agent-service

Multi-user **AI agent as a service** on Kubernetes: every user gets a personal
[Hermes Agent](https://github.com/NousResearch/hermes-agent) running in its own
[agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) `Sandbox`,
provisioned in seconds from a warm pool and **suspended when idle** to save
cost — with state (conversations, memory, skills) surviving on a PVC and a
transparent wake-on-connect when the user returns.

```
user ──▶ hermes-gateway (REST control plane + authenticating proxy)
              │  /api/v1/users…       provision / suspend / resume / delete
              │  /u/{user}/**         proxy → user's sandbox (dashboard :9119, OpenAI API :8642)
              ▼
      SandboxClaim ──▶ SandboxWarmPool ──▶ Sandbox (pod + PVC + headless Service)
                                             ▲ spec.operatingMode: Running|Suspended
```

## Status

| Milestone | State |
|---|---|
| M1 Hermes image contract validated | ✅ done — see `docs/hermes-image.md`, `make validate-hermes-image` |
| M2 K8s dress rehearsal (kind) | ✅ done — `hack/m2-dress-rehearsal.sh` (7 checks) |
| M3 Control plane REST API | ⬜ |
| M4 Proxy + wake + idle suspend | ⬜ |
| M5 Telegram token injection | ⬜ |
| M6 Helm chart + e2e | ⬜ |
| M7 GKE (`gke-ai-eco-dev`) | ⬜ |

## Design decisions & caveats

Every load-bearing decision lives here. If you change one, update this list.

### Agent runtime

1. **Upstream Hermes image, unmodified, pinned** (`nousresearch/hermes-agent:v2026.7.7.2`).
   The upstream image already separates immutable code (`/opt/hermes`) from
   state (`HERMES_HOME=/opt/data` → our PVC), supervises the dashboard +
   messaging gateway via s6-overlay, and seeds config on first boot. A custom
   image bought us nothing. Full validated env contract: `docs/hermes-image.md`.
   *Caveat: image is amd64-only — Apple Silicon dev machines run it emulated;
   GKE amd64 default node pools are fine.*
2. **Two user surfaces per sandbox**: web dashboard (`:9119`, cookie-session
   auth via basic-auth login) and OpenAI-compatible API (`:8642`, per-request
   bearer). Both env-configured, so they work with warm pools.
3. **`HERMES_DASHBOARD_BASIC_AUTH_SECRET` must be set** (shared): dashboard
   session cookies are HMAC-signed with it. Without it, every suspend/resume
   would log all users out. With it (validated), sessions survive restarts.
4. **Shared platform credentials inside sandboxes** (dashboard basic auth,
   `API_SERVER_KEY`, LLM provider key are the same for every sandbox).
   Required for warm pools (pod env is baked before the user is known).
   *Caveat: the NetworkPolicy admitting only the gateway is therefore the
   real per-user isolation boundary — it is non-optional. Validated enforced
   on kind (kube-network-policies ships with recent kind) and expected on GKE
   Dataplane V2; verify enforcement on any other CNI before onboarding users.*
5. **The pod is the sandbox.** Hermes' API server warns that its terminal
   backend is "local/unsandboxed"; in this architecture that is intentional —
   an agent can only affect its own pod and PVC.

### Provisioning (agent-sandbox v0.5.2, pinned)

6. **SandboxClaim + SandboxWarmPool for fast first start.** Per-user claims
   must keep `spec.env` and `spec.volumeClaimTemplates` **empty** — either one
   would bypass the warm pool and cold-start. Our template sets both injection
   policies to `Disallowed`, so such claims are **rejected outright**
   (validated: `EnvVarsInjectionRejected`) instead of silently cold-starting.
   The only warm-compatible per-claim customization is `additionalPodMetadata`
   (pod labels must use the `sandbox.users.io` domain under default controller
   flags). Measured on kind: warm adoption ≤1s, resume from suspend ~11s.
7. **Never set claim `lifecycle`.** Every claim-expiry path deletes the
   Sandbox, which garbage-collects the user's PVC (their entire memory).
   User deletion = delete the claim (deliberate cascade). Cost saving comes
   exclusively from `Sandbox.spec.operatingMode` — which no controller fights.
8. **Sandbox name ≠ claim name.** Warm-adopted sandboxes keep their
   pool-generated name. Always resolve
   `claim.status.sandbox.name → Sandbox → status.serviceFQDN`.
9. **Suspend deletes only the pod**; PVC and the per-sandbox headless Service
   survive; resume reattaches the same PVCs. Pod-spec changes only take
   effect on the next pod recreation (i.e., a suspend/resume cycle).

### Control plane (M3+)

10. **No database.** The SandboxClaim *is* the user record; the per-user
    bearer token is stored as a SHA-256 annotation on the claim; the Telegram
    token lives in a claim-owned Secret. `kubectl` is the admin UI of last resort.
11. **One Go binary, one replica (v1).** Control plane + proxy + idle tracker
    share in-memory state. Scale-out (sticky routing / leader-elected idle
    loop) is a documented follow-up, not built.
12. **Custom gateway instead of agent-sandbox's sandbox-router**: the router
    has no per-user authorization, no wake-on-request, and no idle tracking —
    everything we need is custom, so a second hop buys nothing.
13. **Wake-on-connect holds the request** (up to 60s) rather than failing
    fast; resume typically takes seconds. Timeout → 503 + `Retry-After`.
14. **Users with a Telegram bot token are exempt from idle suspend** by
    default: the bot long-polls from inside the sandbox and dies while
    suspended. Webhook-mode Telegram via the gateway is the v2 fix.
15. **Telegram tokens are injected at runtime** (`pods/exec` → append to
    `$HERMES_HOME/.env` → `s6-svc -r /run/service/gateway-default`), because
    claim env would kill warm starts and template env is shared. Validated.

### Packaging

16. **agent-sandbox is a documented prerequisite**, installed from its pinned
    release manifest (`sandbox-with-extensions.yaml`, v0.5.2) — not vendored
    as a subchart (upstream chart is unpublished and drift-prone).
17. Our Helm chart (`charts/hermes-service`, M6) carries the gateway,
    SandboxTemplate/WarmPool, secrets and RBAC. `storageClassName` defaults to
    the cluster default (works on kind and GKE).
18. Images for GKE live in Artifact Registry in project `gke-ai-eco-dev`.

## Development

```sh
make validate-hermes-image   # M1: prove the pinned Hermes image contract locally (needs Docker)
```

More targets land with each milestone (`make help`).
