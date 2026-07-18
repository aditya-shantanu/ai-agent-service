#!/usr/bin/env bash
# Full-loop e2e against a helm-deployed hermes-service (idle.timeout=1m default).
# Exercises: provision (warm), proxy auth, both surfaces, idle suspend,
# wake-on-connect with state persistence, telegram inject/remove, idempotent
# replay, cascade delete, negative auth.
set -euo pipefail

NS="${NS:-hermes-users}"
USER_ID="${USER_ID:-e2e}"
PORT="${PORT:-18090}"
PASS_COUNT=0
pass() { echo "PASS: $1"; PASS_COUNT=$((PASS_COUNT + 1)); }
fail() { echo "FAIL: $1" >&2; exit 1; }

ADMIN_TOKEN=$(kubectl -n "$NS" get secret hermes-gateway-admin -o jsonpath='{.data.admin-token}' | base64 -d)
A="Authorization: Bearer $ADMIN_TOKEN"
GW="localhost:$PORT"

cleanup() {
  curl -s -H "$A" -X DELETE "http://$GW/api/v1/users/$USER_ID" >/dev/null 2>&1 || true
  kill "$PF_PID" 2>/dev/null || true
}
kubectl -n "$NS" port-forward svc/hermes-gateway "$PORT":8080 >/dev/null 2>&1 &
PF_PID=$!
trap cleanup EXIT
sleep 3
curl -s -H "$A" -X DELETE "http://$GW/api/v1/users/$USER_ID" >/dev/null 2>&1 || true
sleep 3

# 1. Provision: warm adoption, Ready fast
T0=$(date +%s)
CREATE=$(curl -s -H "$A" -X POST "http://$GW/api/v1/users" -d "{\"userId\":\"$USER_ID\"}")
ELAPSED=$(( $(date +%s) - T0 ))
STATE=$(echo "$CREATE" | jq -r .state)
SB=$(echo "$CREATE" | jq -r .sandboxName)
TOKEN=$(echo "$CREATE" | jq -r .token)
[ "$STATE" = "Ready" ] || fail "create state=$STATE (want Ready): $CREATE"
[[ "$SB" == hermes-pool-* ]] || fail "not warm-adopted: $SB"
[ -n "$TOKEN" ] && [ "$TOKEN" != "null" ] || fail "no token returned"
pass "provision: warm adoption ($SB) Ready in ${ELAPSED}s, token minted"

# 2. Proxy auth negative
CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://$GW/u/$USER_ID/api/health")
[ "$CODE" = "401" ] || fail "proxy without token: $CODE"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer wrong" "http://$GW/u/$USER_ID/api/health")
[ "$CODE" = "401" ] || fail "proxy with wrong token: $CODE"
pass "proxy rejects missing/wrong user tokens"

# 3. Dashboard surface through proxy (Hermes auth gate + login flow)
BODY=$(curl -s -H "Authorization: Bearer $TOKEN" "http://$GW/u/$USER_ID/api/health")
echo "$BODY" | grep -q 'unauthenticated' || fail "dashboard gate not seen: $BODY"
DASH_USER=$(kubectl -n "$NS" get secret hermes-platform-secrets -o jsonpath='{.data.dashboard-username}' | base64 -d)
DASH_PASS=$(kubectl -n "$NS" get secret hermes-platform-secrets -o jsonpath='{.data.dashboard-password}' | base64 -d)
LOGIN=$(curl -s -c /tmp/e2e-cookies -X POST "http://$GW/u/$USER_ID/auth/password-login?token=$TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"provider\":\"basic\",\"username\":\"$DASH_USER\",\"password\":\"$DASH_PASS\"}")
echo "$LOGIN" | grep -q '"ok":true' || fail "dashboard login through proxy failed: $LOGIN"
pass "dashboard through proxy: auth gate + basic login work"

# 4. OpenAI-compatible surface (platform key injected upstream)
MODELS=$(curl -s -H "Authorization: Bearer $TOKEN" "http://$GW/u/$USER_ID/v1/models")
echo "$MODELS" | jq -e '.object == "list"' >/dev/null || fail "v1/models failed: $MODELS"
pass "OpenAI-compatible API through proxy (key injection)"

# 5. Idle suspend. The preceding request burst counts as a conversation, so
# the deployment's ACTIVE window applies + 30s sweep granularity. Read the
# window from the deployment so this works at any configured timeout.
ACTIVE=$(kubectl -n "$NS" get deploy hermes-gateway -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="IDLE_ACTIVE_TIMEOUT")].value}')
ACTIVE_SECS=$(python3 -c "
import re,sys
print(sum(int(n)*{'h':3600,'m':60,'s':1}[u] for n,u in re.findall(r'(\d+)([hms])','${ACTIVE:-2m}')) or 120)")
TRIES=$(( (ACTIVE_SECS + 120) / 5 ))
echo "  waiting up to $((TRIES*5))s for idle suspension (active window $ACTIVE)..."
SUSPENDED=""
for _ in $(seq 1 $TRIES); do
  STATE=$(curl -s -H "$A" "http://$GW/api/v1/users/$USER_ID" | jq -r .state)
  [ "$STATE" = "Suspended" ] && SUSPENDED=1 && break
  sleep 5
done
[ -n "$SUSPENDED" ] || fail "not suspended after idle window (state=$STATE)"
# Pod deletion is async (s6 graceful shutdown); wait for it to finish.
kubectl -n "$NS" wait --for=delete "pod/$SB" --timeout=90s >/dev/null 2>&1 || true
kubectl -n "$NS" get pod "$SB" >/dev/null 2>&1 && fail "pod still exists while suspended"
PVC_PHASE=$(kubectl -n "$NS" get pvc "data-$SB" -o jsonpath='{.status.phase}')
[ "$PVC_PHASE" = "Bound" ] || fail "PVC not retained (phase=$PVC_PHASE)"
pass "idle suspend: pod deleted, PVC retained"

# 6. Wake-on-connect: one request wakes and serves
T0=$(date +%s)
MODELS=$(curl -s -m 90 -H "Authorization: Bearer $TOKEN" "http://$GW/u/$USER_ID/v1/models")
ELAPSED=$(( $(date +%s) - T0 ))
echo "$MODELS" | jq -e '.object == "list"' >/dev/null || fail "wake request failed: $MODELS"
pass "wake-on-connect: served after ${ELAPSED}s hold"

# 7. Dashboard session survived suspend/resume (stable session secret)
CODE=$(curl -s -b /tmp/e2e-cookies -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "http://$GW/u/$USER_ID/api/health")
[ "$CODE" != "401" ] || fail "dashboard session lost across suspend/resume"
pass "dashboard session survived suspend/resume (code $CODE)"

# 8. Telegram inject + remove (dummy token; format-valid)
TG=$(curl -s -H "$A" -X PUT "http://$GW/api/v1/users/$USER_ID/telegram-token" \
  -d '{"token":"1234567890:AAE2eDummyTokenForE2E_0123456789abcd","allowedUsers":"1"}')
echo "$TG" | jq -e '.suspendExempt == true' >/dev/null || fail "telegram inject failed: $TG"
kubectl -n "$NS" get secret "hermes-user-$USER_ID-telegram" >/dev/null || fail "telegram secret missing"
TG=$(curl -s -H "$A" -X DELETE "http://$GW/api/v1/users/$USER_ID/telegram-token")
echo "$TG" | jq -e '.suspendExempt == false' >/dev/null || fail "telegram remove failed: $TG"
pass "telegram token inject + remove (secret, exemption)"

# 9. Cron-aware wake: a scheduled job resumes a suspended sandbox with ZERO
# user traffic (docs/cron-wake-design.md). Uses a recurring --no-agent script
# job so no LLM key is needed.
SB=$(curl -s -H "$A" "http://$GW/api/v1/users/$USER_ID" | jq -r .sandboxName)
POD=$(kubectl -n "$NS" get sandbox "$SB" -o jsonpath='{.metadata.annotations.agents\.x-k8s\.io/pod-name}')
POD=${POD:-$SB}
kubectl -n "$NS" exec "$POD" -- sh -c \
  'mkdir -p /opt/data/scripts && printf "#!/bin/bash\ndate -u >> /opt/data/cron-marker\necho ran\n" > /opt/data/scripts/marker.sh'
kubectl -n "$NS" exec "$POD" -- hermes cron create 'every 2m' --name e2e-marker --script marker.sh --no-agent >/dev/null
SUSPENDED=""
for _ in $(seq 1 $TRIES); do
  STATE=$(curl -s -H "$A" "http://$GW/api/v1/users/$USER_ID" | jq -r .state)
  [ "$STATE" = "Suspended" ] && SUSPENDED=1 && break
  sleep 5
done
[ -n "$SUSPENDED" ] || fail "cron: user did not idle-suspend (state=$STATE)"
NEXT=$(curl -s -H "$A" "http://$GW/api/v1/users/$USER_ID" | jq -r '.nextCronWake // empty')
[ -n "$NEXT" ] || fail "cron: suspend did not capture nextCronWake"
CRON_WOKE=""
for _ in $(seq 1 50); do  # job due within 2m; waker sweeps every 30s
  RESP=$(curl -s -H "$A" "http://$GW/api/v1/users/$USER_ID")
  if [ "$(echo "$RESP" | jq -r .state)" = "Ready" ] && \
     [ "$(echo "$RESP" | jq -r .lastWakeReason)" = "cron" ]; then CRON_WOKE=1; break; fi
  sleep 5
done
[ -n "$CRON_WOKE" ] || fail "cron: waker did not resume the sandbox (last: $RESP)"
# The waker may resume up to one sweep-interval EARLY (before the job is
# due), in which case the immediate `cron tick` is a no-op and the in-pod
# 60s ticker fires the job at/after due time — poll rather than sleep.
MARKED=""
for _ in $(seq 1 30); do
  if kubectl -n "$NS" exec "$SB" -- test -s /opt/data/cron-marker >/dev/null 2>&1; then MARKED=1; break; fi
  sleep 5
done
[ -n "$MARKED" ] || fail "cron: marker not written by scheduled job"
kubectl -n "$NS" exec "$SB" -- hermes cron remove e2e-marker >/dev/null 2>&1 || \
  kubectl -n "$NS" exec "$SB" -- sh -c 'rm -f /opt/data/cron/jobs.json' >/dev/null 2>&1 || true
pass "cron wake: captured at suspend ($NEXT), waker resumed (reason=cron), job ran with no user traffic"

# 10. Idempotent replay: no token leak
REPLAY=$(curl -s -H "$A" -X POST "http://$GW/api/v1/users" -d "{\"userId\":\"$USER_ID\"}")
TOK2=$(echo "$REPLAY" | jq -r '.token // empty')
[ -z "$TOK2" ] || fail "replay leaked a token"
pass "idempotent replay does not re-issue tokens"

# 11. Cascade delete
SB=$(curl -s -H "$A" "http://$GW/api/v1/users/$USER_ID" | jq -r .sandboxName)
curl -s -H "$A" -X DELETE "http://$GW/api/v1/users/$USER_ID" >/dev/null
DELETED=""
for _ in $(seq 1 30); do
  if ! kubectl -n "$NS" get sandbox "$SB" >/dev/null 2>&1 && \
     ! kubectl -n "$NS" get pvc "data-$SB" >/dev/null 2>&1; then DELETED=1; break; fi
  sleep 2
done
[ -n "$DELETED" ] || fail "sandbox/PVC not garbage-collected after delete"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -H "$A" "http://$GW/api/v1/users/$USER_ID")
[ "$CODE" = "404" ] || fail "user still resolvable after delete: $CODE"
pass "cascade delete: claim, sandbox, PVC all gone"

echo
echo "All $PASS_COUNT e2e checks passed."
