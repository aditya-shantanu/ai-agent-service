#!/usr/bin/env bash
# M2 dress rehearsal: proves the agent-sandbox layer behaves as the platform
# requires, using only kubectl + the deploy/dev manifests. Assumes
# hack/kind-up.sh has run and deploy/dev/{00..03} are applied and the pool is Ready.
set -euo pipefail

NS=hermes-users
USER_ID="${USER_ID:-rehearsal}"
CLAIM="hermes-$USER_ID"
API_KEY=$(kubectl -n $NS get secret hermes-platform-secrets -o jsonpath='{.data.api-server-key}' | base64 -d)
PASS_COUNT=0
pass() { echo "PASS: $1"; PASS_COUNT=$((PASS_COUNT + 1)); }
fail() { echo "FAIL: $1" >&2; exit 1; }
cleanup() { kubectl -n $NS delete sandboxclaim "$CLAIM" --ignore-not-found >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

# 1. Warm adoption: claim binds to a pool spare fast, with a pool-generated name
kubectl -n $NS wait --for=condition=Ready sandbox --all --timeout=300s >/dev/null
T0=$(date +%s)
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxClaim
metadata:
  name: $CLAIM
  namespace: $NS
  labels:
    app.kubernetes.io/managed-by: hermes-gateway
    hermes.ai-agent-service.dev/user: $USER_ID
spec:
  warmPoolRef:
    name: hermes-pool
  additionalPodMetadata:
    labels:
      sandbox.users.io/hermes-user: $USER_ID
EOF
kubectl -n $NS wait --for=condition=Ready "sandboxclaim/$CLAIM" --timeout=60s >/dev/null
ELAPSED=$(( $(date +%s) - T0 ))
SB=$(kubectl -n $NS get sandboxclaim "$CLAIM" -o jsonpath='{.status.sandbox.name}')
[[ "$SB" == hermes-pool-* ]] || fail "expected warm adoption (pool name), got '$SB'"
[ "$ELAPSED" -le 15 ] || fail "warm adoption took ${ELAPSED}s (>15s)"
FQDN=$(kubectl -n $NS get sandbox "$SB" -o jsonpath='{.status.serviceFQDN}')
[ -n "$FQDN" ] || fail "no serviceFQDN on adopted sandbox"
pass "warm adoption in ${ELAPSED}s → $SB ($FQDN)"

# 2. Claims carrying env are rejected by template policy (no silent cold start)
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxClaim
metadata: {name: ${CLAIM}-envtest, namespace: $NS}
spec:
  warmPoolRef: {name: hermes-pool}
  env: [{name: FOO, value: bar}]
EOF
sleep 5
REASON=$(kubectl -n $NS get sandboxclaim "${CLAIM}-envtest" -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}')
kubectl -n $NS delete sandboxclaim "${CLAIM}-envtest" --ignore-not-found >/dev/null
[ "$REASON" = "EnvVarsInjectionRejected" ] || fail "env claim not rejected (reason=$REASON)"
pass "claim with env rejected by template policy ($REASON)"

# 3. NetworkPolicy: unlabeled pod blocked; gateway-labeled pod admitted through auth gates
run_curl() { # labels, url_suffix, extra_args...
  local labels="$1" path="$2"; shift 2
  kubectl -n $NS run "curl-$RANDOM" --image=curlimages/curl --rm -i --restart=Never \
    ${labels:+--labels=$labels} --command -- \
    curl -s -m 8 -o /dev/null -w 'HTTPCODE:%{http_code}:' "$@" "http://$FQDN$path" 2>/dev/null \
    | grep -o 'HTTPCODE:[0-9]*:' | head -1 | cut -d: -f2
}
CODE=$(run_curl "" ":9119/api/health" || true)
[ "$CODE" != "401" ] || fail "unlabeled pod reached sandbox (expected block, got $CODE)"
pass "NetworkPolicy blocks unlabeled pods (got '${CODE:-timeout}')"
CODE=$(run_curl "app.kubernetes.io/name=hermes-gateway" ":9119/api/health")
[ "$CODE" = "401" ] || fail "gateway label expected 401 auth gate, got '$CODE'"
CODE=$(run_curl "app.kubernetes.io/name=hermes-gateway" ":8642/v1/models" -H "Authorization: Bearer $API_KEY")
[ "$CODE" = "200" ] || fail "API server with key expected 200, got '$CODE'"
pass "gateway-labeled pod reaches dashboard (401 gate) and API server (200 with key)"

# 4. Suspend: pod deleted; PVC + Service retained
kubectl -n $NS exec "$SB" -- sh -c 'echo marker > /opt/data/m2-marker' >/dev/null
kubectl -n $NS patch sandbox "$SB" --type=merge -p '{"spec":{"operatingMode":"Suspended"}}' >/dev/null
kubectl -n $NS wait --for=condition=Suspended=True "sandbox/$SB" --timeout=120s >/dev/null
kubectl -n $NS wait --for=delete "pod/$SB" --timeout=120s >/dev/null 2>&1 || true
kubectl -n $NS get pod "$SB" >/dev/null 2>&1 && fail "pod still exists while suspended"
[ "$(kubectl -n $NS get pvc "data-$SB" -o jsonpath='{.status.phase}')" = "Bound" ] || fail "PVC not retained"
kubectl -n $NS get svc "$SB" >/dev/null 2>&1 || fail "Service not retained"
pass "suspend: pod gone, PVC Bound, Service retained"

# 5. Resume: pod recreated, same PVC, data intact; time-boxed
T0=$(date +%s)
kubectl -n $NS patch sandbox "$SB" --type=merge -p '{"spec":{"operatingMode":"Running"}}' >/dev/null
kubectl -n $NS wait --for=condition=Ready "sandbox/$SB" --timeout=300s >/dev/null
ELAPSED=$(( $(date +%s) - T0 ))
MARKER=$(kubectl -n $NS exec "$SB" -- cat /opt/data/m2-marker)
[ "$MARKER" = "marker" ] || fail "PVC data lost across suspend/resume"
pass "resume in ${ELAPSED}s with PVC data intact"

# 6. Telegram runtime injection: .env append + s6 gateway restart is picked up
kubectl -n $NS exec "$SB" -- sh -c 'printf "\nTELEGRAM_BOT_TOKEN=1234567890:DUMMY-m2\nTELEGRAM_ALLOWED_USERS=1\n" >> /opt/data/.env'
kubectl -n $NS exec "$SB" -- /command/s6-svc -r /run/service/gateway-default
SEEN=""
for _ in $(seq 1 30); do
  if kubectl -n $NS logs "$SB" --since=2m 2>/dev/null | grep -qi 'plugins.telegram'; then SEEN=1; break; fi
  sleep 3
done
[ -n "$SEEN" ] || fail "gateway did not pick up telegram token"
pass "telegram injection via exec + s6-svc restart picked up"

echo
echo "All $PASS_COUNT dress-rehearsal checks passed (user=$USER_ID, sandbox=$SB)"
