# hermes-gateway API

Two surfaces on one port (default `:8080`):

- **Management** `/api/v1/*` — admin bearer token (`hermes-gateway-admin` Secret).
- **User proxy** `/u/{user}/**` — per-user bearer token (returned once at
  create/rotate), via `Authorization: Bearer …` or `?token=` (for browsers).
  A valid `?token=` is promoted to a `/u/{user}`-scoped HttpOnly session
  cookie (`hermes_gw_token`) so browser navigations, redirects, SPA assets
  and fetches — which all drop the query param — stay authenticated. The
  cookie holds the same token, is verified per request, is never forwarded
  upstream, and dies with token rotation.

## Management

| Method & path | Description |
|---|---|
| `POST /api/v1/users` `{"userId":"alice"}` | Provision. Waits for warm adoption + Ready. `201` with one-time `token`; `200` without token on idempotent replay. |
| `GET /api/v1/users` | List users with derived state. |
| `GET /api/v1/users/{id}` | `state`: `Provisioning · Ready · Suspending · Suspended · Waking`, `sandboxName`, `serviceFQDN`, `suspendExempt`, `nextCronWake` (set while suspended with a pending Hermes cron job), `lastWakeReason` (`connect·api·cron`). |
| `POST /api/v1/users/{id}/suspend` | Explicit suspend (works even for exempt users). |
| `POST /api/v1/users/{id}/resume` | Resume; holds until Ready (`200`) or returns `202` if not Ready within `wakeTimeout`. |
| `POST /api/v1/users/{id}/token` | Rotate the user token (returned once). |
| `PUT /api/v1/users/{id}/suspend-exempt` `{"exempt":true}` | Toggle idle-suspension exemption ("always alive"). Explicit suspend still works on exempt users; `DELETE …/telegram-token` force-clears the exemption. |
| `PUT /api/v1/users/{id}/telegram-token` `{"token":"...","allowedUsers":"id1,id2"}` | Install bot token (runtime injection; marks user suspend-exempt). |
| `DELETE /api/v1/users/{id}/telegram-token` | Remove bot token; re-enables idle suspension. |
| `DELETE /api/v1/users/{id}` | Delete user. Cascades: claim → sandbox → PVC → owned Secrets. Irreversible. |

User IDs: DNS-1123 label, max 40 chars (`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`).

## User proxy

| Path | Upstream | Auth handling |
|---|---|---|
| `/u/{user}/v1/**` | sandbox `:8642` (OpenAI-compatible) | user token verified, then platform `API_SERVER_KEY` injected upstream |
| `/u/{user}/**` (everything else) | sandbox `:9119` (dashboard) | user token verified then stripped; Hermes' own cookie login flows through |

Behavior:

- **Browser dashboard flow**: opening `/u/{user}/?token=…` 302s to the
  Hermes login page (`/u/{user}/login`) — the proxy re-anchors upstream
  redirects under the user's subtree, sends `X-Forwarded-Prefix` so Hermes
  self-prefixes URLs/cookies/assets, and steers the basic-provider
  auto-SSO redirect away from Hermes' OAuth-only `/auth/login` route
  (which 500s for password auth in v2026.7.7.2).
- **Wake-on-connect**: requests against a suspended agent are held (≤60s)
  while it resumes; timeout → `503` + `Retry-After: 10`.
- WebSockets and SSE stream through; open sockets count as activity and
  block idle suspension.
- Any proxied request resets the user's idle clock. Suspension is
  adaptive: 15s tail after an isolated request, the configured active
  window (2m kind / 10m GKE) while a conversation is in progress.

### Examples

```sh
ADMIN=... GW=localhost:8080
# create
curl -H "Authorization: Bearer $ADMIN" -X POST http://$GW/api/v1/users -d '{"userId":"alice"}'
# chat (OpenAI-compatible)
curl -H "Authorization: Bearer $USER_TOKEN" http://$GW/u/alice/v1/chat/completions \
  -d '{"model":"hermes-agent","messages":[{"role":"user","content":"hi"}]}'
# dashboard in a browser
open "http://$GW/u/alice/?token=$USER_TOKEN"   # then log in (platform dashboard creds)
```
