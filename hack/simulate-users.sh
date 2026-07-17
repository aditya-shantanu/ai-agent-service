#!/usr/bin/env bash
# Emulate multiple users using the platform concurrently.
#
# Creates N users in parallel (exercising warm-pool adoption and, once the
# pool is drained, the cold-start path), drives concurrent traffic through
# the proxy as each user, then keeps ONE user active while the rest go idle —
# so you can watch the idle sweeper suspend only the inactive ones and a
# single request transparently wake a suspended agent.
#
# Usage:
#   hack/simulate-users.sh                # 3 users against ns hermes-users
#   USERS=5 NS=hermes-users PORT=18095 hack/simulate-users.sh
#   KEEP=1 hack/simulate-users.sh         # leave the users running afterwards
#
# Works against any deployment (kind or GKE) reachable via kubectl.
# NOTE: the idle-suspension phase is only fast enough to watch when the
# deployment uses a short idle timeout (default: 1m).
set -euo pipefail

NS="${NS:-hermes-users}"
USERS="${USERS:-3}"
# Default to a fresh free port per run: orphaned port-forwards from killed
# runs otherwise squat on a fixed port and silently blackhole the new run.
PORT="${PORT:-$(python3 -c 'import socket; s=socket.socket(); s.bind(("",0)); print(s.getsockname()[1]); s.close()')}"
KEEP="${KEEP:-}"
PREFIX="${PREFIX:-sim}"
GW="localhost:$PORT"
WORKDIR=$(mktemp -d)
CURL="curl -s -m 150"   # every call bounded: nothing in this script may hang forever

ADMIN_TOKEN=$(kubectl -n "$NS" get secret hermes-gateway-admin -o jsonpath='{.data.admin-token}' | base64 -d)
A="Authorization: Bearer $ADMIN_TOKEN"

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
row()  { printf '  %-10s %-12s %-22s %s\n' "$@"; }

cleanup() {
  kill "$PF_PID" 2>/dev/null || true
  if [ -z "$KEEP" ]; then
    for i in $(seq 1 "$USERS"); do
      curl -s -H "$A" -X DELETE "http://$GW/api/v1/users/$PREFIX$i" >/dev/null 2>&1 || true
    done
  fi
  rm -rf "$WORKDIR"
}
kubectl -n "$NS" port-forward svc/hermes-gateway "$PORT":8080 >/dev/null 2>&1 &
PF_PID=$!
trap cleanup EXIT
sleep 3

pool_size() { kubectl -n "$NS" get sandboxes -o json | jq '[.items[] | select(.metadata.ownerReferences[0].kind == "SandboxWarmPool")] | length'; }

# Pre-clean leftovers from prior runs and WAIT until fully gone: creating a
# user whose old claim is still terminating replays idempotently WITHOUT a
# token, and every subsequent request 401s.
for i in $(seq 1 "$USERS"); do
  $CURL -H "$A" -X DELETE "http://$GW/api/v1/users/$PREFIX$i" >/dev/null 2>&1 || true
done
for i in $(seq 1 "$USERS"); do
  for _ in $(seq 1 30); do
    CODE=$($CURL -o /dev/null -w '%{http_code}' -H "$A" "http://$GW/api/v1/users/$PREFIX$i")
    [ "$CODE" = "404" ] && break
    sleep 2
  done
done

say "Warm pool before: $(pool_size) spare(s). Creating $USERS users in parallel..."
T0=$(date +%s)
# NB: collect explicit PIDs — a bare `wait` would also wait on the
# port-forward background job and hang forever.
PIDS=()
for i in $(seq 1 "$USERS"); do
  (
    START=$(date +%s)
    RESP=$($CURL -H "$A" -X POST "http://$GW/api/v1/users" -d "{\"userId\":\"$PREFIX$i\"}")
    took=$(( $(date +%s) - START ))
    echo "$RESP" | jq -r .token > "$WORKDIR/$PREFIX$i.token"
    echo "$RESP" | jq -r ".sandboxName + \" ${took}s\"" > "$WORKDIR/$PREFIX$i.info"
  ) &
  PIDS+=($!)
done
wait "${PIDS[@]}"
say "All $USERS users provisioned in $(( $(date +%s) - T0 ))s total:"
row USER STATE SANDBOX "PROVISION TIME"
for i in $(seq 1 "$USERS"); do
  INFO=$(cat "$WORKDIR/$PREFIX$i.info")
   STATE=$($CURL -H "$A" "http://$GW/api/v1/users/$PREFIX$i" | jq -r .state)
  row "$PREFIX$i" "$STATE" ${INFO}
done
echo "  Warm pool after: $(pool_size) spare(s) (replenishing in background)"

say "Concurrent traffic: every user hits their own agent through the proxy"
PIDS=()
for i in $(seq 1 "$USERS"); do
  (
    TOKEN=$(cat "$WORKDIR/$PREFIX$i.token")
    CODE=$($CURL -o /dev/null -w '%{http_code}' \
      -H "Authorization: Bearer $TOKEN" "http://$GW/u/$PREFIX$i/v1/models")
    echo "  $PREFIX$i -> /v1/models: HTTP $CODE"
  ) &
  PIDS+=($!)
done
wait "${PIDS[@]}"

say "Cross-user isolation: ${PREFIX}1's token must NOT work for ${PREFIX}2"
TOKEN1=$(cat "$WORKDIR/${PREFIX}1.token")
CODE=$($CURL -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN1" "http://$GW/u/${PREFIX}2/v1/models")
echo "  ${PREFIX}1 token on /u/${PREFIX}2: HTTP $CODE (expect 401)"

IDLE_TIMEOUT=$(kubectl -n "$NS" get deploy hermes-gateway -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="IDLE_TIMEOUT")].value}')
# Normalize Go durations like "90s" / "1m" / "2m30s" to seconds.
IDLE_SECS=$(python3 - "$IDLE_TIMEOUT" <<'PYEOF'
import re, sys
total = 0
for num, unit in re.findall(r'(\d+)([hms])', sys.argv[1]):
    total += int(num) * {'h': 3600, 'm': 60, 's': 1}[unit]
print(total or 99999)
PYEOF
)
say "Idle phase: ${PREFIX}1 stays active (heartbeat); the rest go idle (idle timeout: $IDLE_TIMEOUT)"
if [ "$IDLE_SECS" -gt 120 ]; then
  echo "  Idle timeout is $IDLE_TIMEOUT — too long to demo interactively; skipping the suspend/wake phase."
  echo "  (Deploy with the default idle.timeout=1m to watch it.)"
else
  WINDOW=$(( IDLE_SECS + 90 ))
  END=$(( $(date +%s) + WINDOW ))
  while [ "$(date +%s)" -lt "$END" ]; do
    $CURL -o /dev/null -H "Authorization: Bearer $TOKEN1" "http://$GW/u/${PREFIX}1/v1/models" || true
    sleep 15
  done
  say "States after the idle window (only ${PREFIX}1 should still be Ready):"
  row USER STATE SANDBOX ""
  for i in $(seq 1 "$USERS"); do
    U=$($CURL -H "$A" "http://$GW/api/v1/users/$PREFIX$i")
    row "$PREFIX$i" "$(echo "$U" | jq -r .state)" "$(echo "$U" | jq -r .sandboxName)" ""
  done

  say "Wake-on-connect: ${PREFIX}2 comes back — one request, transparently served"
  TOKEN2=$(cat "$WORKDIR/${PREFIX}2.token")
  T0=$(date +%s)
  CODE=$($CURL -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer $TOKEN2" "http://$GW/u/${PREFIX}2/v1/models")
  echo "  ${PREFIX}2 -> HTTP $CODE after $(( $(date +%s) - T0 ))s (held while resuming)"
  echo "  ${PREFIX}2 state now: $($CURL -H "$A" "http://$GW/api/v1/users/${PREFIX}2" | jq -r .state)"
fi

if [ -n "$KEEP" ]; then
  say "KEEP=1 — leaving users in place. Tokens are in this run's output above; clean up with:"
  for i in $(seq 1 "$USERS"); do
    echo "  curl -H \"Authorization: Bearer \$ADMIN_TOKEN\" -X DELETE http://\$GW/api/v1/users/$PREFIX$i"
  done
else
  say "Cleaning up $USERS users (claims, sandboxes, PVCs cascade)"
fi
echo
echo "Simulation complete."
