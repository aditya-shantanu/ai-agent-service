# Hermes container contract (validated 2026-07-16)

We run the **upstream image unmodified**: `nousresearch/hermes-agent:v2026.7.7.2`
(Docker Hub, pinned release tag; mirrored to Artifact Registry for GKE).
No custom Dockerfile is needed — everything the platform requires is driven by
environment variables and the s6-overlay supervision baked into the image.
Every claim below was validated hands-on with `scripts/validate-hermes-image.sh`.

## Why no custom image

The upstream image already provides exactly what the plan's custom image was
going to build:

- **Code/state separation**: install tree at `/opt/hermes` (root-owned,
  immutable, sealed venv), all mutable state under `HERMES_HOME=/opt/data`
  (our PVC mount). Lazy installs are redirected to
  `/opt/data/lazy-packages` and cannot shadow core modules.
- **Supervision**: s6-overlay is PID 1; the dashboard and the messaging
  gateway are supervised services with restart semantics. No custom
  entrypoint/supervisor needed (the exit-75 contract is handled internally).
- **First-boot seeding**: on a blank volume, `config.yaml`, `.env` and
  `SOUL.md` are seeded automatically; subsequent boots never re-seed
  (validated: restart does not clobber state).

## Processes and ports

| Service | Port | Purpose | Auth |
|---|---|---|---|
| dashboard | 9119 | Web UI + session API (FastAPI) | cookie session via basic-auth login |
| gateway API server | 8642 | OpenAI-compatible chat API (`/v1/*`) | `Authorization: Bearer $API_SERVER_KEY` per request |
| gateway (same process as API server) | — | messaging platforms (Telegram, …) + cron | n/a |

## Environment contract (what our SandboxTemplate sets)

```yaml
# Web dashboard
HERMES_DASHBOARD: "1"
HERMES_DASHBOARD_HOST: "0.0.0.0"        # auth gate engages automatically, fail-closed
HERMES_DASHBOARD_BASIC_AUTH_USERNAME: <shared platform secret>
HERMES_DASHBOARD_BASIC_AUTH_PASSWORD: <shared platform secret>
HERMES_DASHBOARD_BASIC_AUTH_SECRET:   <shared 32+ byte secret>   # REQUIRED for us:
  # session cookies are HMAC-signed with this; without it a random per-process
  # secret is used and every suspend/resume logs all users out.

# Messaging gateway + OpenAI-compatible API server
HERMES_GATEWAY_BOOTSTRAP_STATE: "running"  # first-boot-only seed; brings the
  # supervised gateway up on a fresh volume. Later boots follow persisted state.
API_SERVER_ENABLED: "true"
API_SERVER_HOST: "0.0.0.0"
API_SERVER_KEY: <shared platform secret>   # MUST be >= 16 chars or the server
  # refuses to start (validated). Use openssl rand -hex 32.

# LLM provider (platform-shared key, e.g. one of:)
GEMINI_API_KEY / OPENAI_API_KEY / OPENROUTER_API_KEY / ...
```

## Auth flows (validated)

- **Dashboard**: `POST /auth/password-login` with JSON
  `{"provider":"basic","username":...,"password":...}` → sets session cookies,
  returns `{"ok":true,"next":"/"}`. All other API routes 401 without the
  cookie. Basic `Authorization` headers are NOT accepted per-request — it is
  a login-session model. Sessions **survive restarts** when
  `HERMES_DASHBOARD_BASIC_AUTH_SECRET` is set (validated across
  `docker restart`).
- **API server**: plain per-request bearer; 401 on missing/wrong key
  (validated), works immediately after restart.

## Telegram token injection (runtime, per-user)

Validated flow — this is what the control plane does via `pods/exec`:

1. Append to `$HERMES_HOME/.env` (as the hermes user):
   `TELEGRAM_BOT_TOKEN=...` and `TELEGRAM_ALLOWED_USERS=...`
2. Restart the supervised gateway: `/command/s6-svc -r /run/service/gateway-default`
   (note: s6 binaries live in `/command/`, not on `PATH`).
3. The gateway reloads `.env` and connects to Telegram. The token lives on the
   PVC, so it survives suspend/resume without re-injection.

## Non-root (Kubernetes securityContext)

Validated: the container runs fully as UID 10000 (`--user 10000:10000`) when
the data volume is already owned by 10000 — in Kubernetes terms:

```yaml
securityContext:
  runAsUser: 10000
  runAsGroup: 10000
  fsGroup: 10000
```

The root-only bootstrap (UID remap, chown) is skipped safely; seeding and all
services work. Do NOT use arbitrary other UIDs — the image explicitly rejects
them.

## Platform overrides on top of this contract

- The chart pre-seeds `/opt/data/config.yaml` with `model.default`
  (`hermes.defaultModel`, default `google/gemini-flash-latest`) via an init
  container BEFORE Hermes’ own first-boot seed — without it, Gemini-only
  deployments 404 on Hermes’ Anthropic default model.
- The chart’s readiness probe is aggressive (2s/2s): Hermes binds :9119
  within ~2–4s and a lazy probe was inflating every resume.

## Caveats

- **amd64-only image.** On Apple Silicon (kind/colima) it runs under
  emulation — noticeably slower, fine for e2e. GKE default amd64 node pools
  are unaffected.
- `API_SERVER_KEY` < 16 chars → the API server refuses to start (by design).
- The API server warns that the terminal backend is `local` (unsandboxed).
  In our architecture that is intentional: the pod IS the per-user sandbox
  boundary; a user's agent can only affect their own pod/PVC.
- First-boot gateway autostart relies on `HERMES_GATEWAY_BOOTSTRAP_STATE`
  being present at first boot; afterwards `gateway_state.json` on the PVC is
  authoritative (an operator-stopped gateway stays stopped — desired).
