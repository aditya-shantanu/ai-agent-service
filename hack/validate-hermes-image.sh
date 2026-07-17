#!/usr/bin/env bash
# M1 validation: proves the pinned upstream Hermes image satisfies the
# platform's env-driven contract (docs/hermes-image.md). Run against a local
# Docker daemon. Idempotent; cleans up after itself.
set -euo pipefail

HERMES_IMAGE="${HERMES_IMAGE:-nousresearch/hermes-agent:v2026.7.7.2}"
NAME=hermes-image-validate
VOL=${NAME}-data
DASH_PORT="${DASH_PORT:-19119}"
API_PORT="${API_PORT:-18642}"
PASS_COUNT=0

pass() { echo "PASS: $1"; PASS_COUNT=$((PASS_COUNT + 1)); }
fail() { echo "FAIL: $1" >&2; exit 1; }

cleanup() {
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  docker volume rm "$VOL" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup

API_KEY=$(openssl rand -hex 32)
SESSION_SECRET=$(openssl rand -hex 32)

docker volume create "$VOL" >/dev/null
docker run -d --name "$NAME" \
  -v "$VOL":/opt/data \
  -e HERMES_DASHBOARD=1 \
  -e HERMES_DASHBOARD_HOST=0.0.0.0 \
  -e HERMES_DASHBOARD_BASIC_AUTH_USERNAME=platform \
  -e HERMES_DASHBOARD_BASIC_AUTH_PASSWORD=validate-secret \
  -e HERMES_DASHBOARD_BASIC_AUTH_SECRET="$SESSION_SECRET" \
  -e HERMES_GATEWAY_BOOTSTRAP_STATE=running \
  -e API_SERVER_ENABLED=true \
  -e API_SERVER_KEY="$API_KEY" \
  -e API_SERVER_HOST=0.0.0.0 \
  -p "$DASH_PORT":9119 -p "$API_PORT":8642 \
  "$HERMES_IMAGE" sleep infinity >/dev/null

wait_http() { # url, want, tries
  local i
  for i in $(seq 1 "$3"); do
    [ "$(curl -s -o /dev/null -w '%{http_code}' "$1")" = "$2" ] && return 0
    sleep 2
  done
  return 1
}

# 1. Dashboard up, auth gate fail-closed
wait_http "http://localhost:$DASH_PORT/api/health" 401 30 \
  || fail "dashboard did not come up with 401 auth gate"
pass "dashboard up; unauthenticated API request gets 401"

# 2. Password login mints a working session cookie
COOKIES=$(mktemp)
LOGIN=$(curl -s -c "$COOKIES" -X POST "http://localhost:$DASH_PORT/auth/password-login" \
  -H 'Content-Type: application/json' \
  -d '{"provider":"basic","username":"platform","password":"validate-secret"}')
echo "$LOGIN" | grep -q '"ok":true' || fail "password-login rejected: $LOGIN"
CODE=$(curl -s -b "$COOKIES" -o /dev/null -w '%{http_code}' "http://localhost:$DASH_PORT/api/health")
[ "$CODE" != "401" ] || fail "session cookie not accepted"
pass "basic-auth login flow works; session cookie authenticates"

# 3. API server: bearer required, key works
wait_http "http://localhost:$API_PORT/v1/models" 401 30 \
  || fail "API server did not come up with 401 for missing bearer"
curl -s -H "Authorization: Bearer $API_KEY" "http://localhost:$API_PORT/v1/models" \
  | grep -q '"object": *"list"' || fail "API server rejected valid key"
pass "OpenAI-compatible API server enforces and accepts bearer key"

# 4. Telegram token injection: .env write + s6 gateway restart picks it up
docker exec -u hermes "$NAME" sh -c \
  'printf "\nTELEGRAM_BOT_TOKEN=1234567890:DUMMY-validate\nTELEGRAM_ALLOWED_USERS=1\n" >> /opt/data/.env'
docker exec "$NAME" /command/s6-svc -r /run/service/gateway-default
TELEGRAM_SEEN=""
for _ in $(seq 1 30); do
  if docker logs "$NAME" 2>&1 | grep -qi 'plugins.telegram'; then
    TELEGRAM_SEEN=1
    break
  fi
  sleep 3
done
[ -n "$TELEGRAM_SEEN" ] \
  || fail "gateway did not attempt telegram after .env injection + restart"
pass "telegram token injection via .env + s6-svc restart is picked up"

# 5. Restart persistence: no re-seed, session survives, state intact
docker restart "$NAME" >/dev/null
wait_http "http://localhost:$API_PORT/v1/models" 401 45 \
  || fail "API server did not come back after restart"
CODE=$(curl -s -b "$COOKIES" -o /dev/null -w '%{http_code}' "http://localhost:$DASH_PORT/api/health")
[ "$CODE" != "401" ] || fail "session cookie invalidated by restart (secret not honored?)"
docker exec "$NAME" grep -q TELEGRAM_BOT_TOKEN /opt/data/.env \
  || fail ".env content lost across restart"
pass "restart: state persists, no re-seed, dashboard session survives"

rm -f "$COOKIES"
echo
echo "All $PASS_COUNT checks passed for $HERMES_IMAGE"
