#!/usr/bin/env bash
# UX benchmark wrapper: plumbs gateway access (port-forward on kind, LB on
# GKE) and the admin token into cmd/hermes-bench, and belt-and-braces
# restores the warm pool if the binary died mid-drain.
#
#   ENV=kind|gke  target environment (default kind)
#   NS=...        namespace (default hermes-users)
#   CHECK=1       gate against benchmarks/budgets-$ENV.yaml (exit 1 on violation)
#   TTFT=1        add streamed chat TTFT scenarios (needs provider key; costs credits)
#   DRAIN=1       allow the cold scenario on GKE too (kind allows by default)
#   BENCH_ARGS=   extra flags passed through (e.g. "-scenarios resume,baseline")
set -euo pipefail

ENV="${ENV:-kind}"
NS="${NS:-hermes-users}"
KIND_CTX="${KIND_CTX:-kind-hermes-svc}"
# Derived from GCP_PROJECT/GKE_ZONE/GKE_CLUSTER (kubectl's gke_* naming),
# or set GKE_CTX directly.
GKE_CTX="${GKE_CTX:-gke_${GCP_PROJECT:-}_${GKE_ZONE:-us-central1-a}_${GKE_CLUSTER:-hermes-svc}}"
BIN="${BIN:-bin/hermes-bench}"
POOL="${POOL:-hermes-pool}"

case "$ENV" in
  kind) CTX="$KIND_CTX" ;;
  gke)  CTX="$GKE_CTX" ;;
  *) echo "ENV must be kind or gke" >&2; exit 2 ;;
esac
kubectl config get-contexts -o name | grep -qx "$CTX" || { echo "kubectl context '$CTX' not found" >&2; exit 2; }
k() { kubectl --context "$CTX" -n "$NS" "$@"; }

[ -x "$BIN" ] || { echo "$BIN not built — run 'make bench-build'" >&2; exit 2; }

ADMIN_TOKEN=$(k get secret hermes-gateway-admin -o jsonpath='{.data.admin-token}' | base64 -d)

# Record desired warm-pool replicas so the trap can restore after a hard
# death mid-drain (the binary also restores on its own in normal exits).
SAVED_REPLICAS=$(k get sandboxwarmpool "$POOL" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "")

PF_PID=""
cleanup() {
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
  if [ -n "$SAVED_REPLICAS" ]; then
    CURRENT=$(k get sandboxwarmpool "$POOL" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "")
    if [ -n "$CURRENT" ] && [ "$CURRENT" != "$SAVED_REPLICAS" ]; then
      echo "run.sh: warm pool at replicas=$CURRENT, restoring $SAVED_REPLICAS" >&2
      k patch sandboxwarmpool "$POOL" --type merge -p "{\"spec\":{\"replicas\":$SAVED_REPLICAS}}" || true
    fi
  fi
}
trap cleanup EXIT

if [ "$ENV" = "kind" ]; then
  PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("",0)); print(s.getsockname()[1]); s.close()')
  k port-forward svc/hermes-gateway "$PORT":8080 >/dev/null 2>&1 &
  PF_PID=$!
  GATEWAY="http://localhost:$PORT"
  # Poll readiness instead of a blind sleep — port-forwards come up unevenly.
  for _ in $(seq 1 30); do
    curl -s -m 2 -o /dev/null "$GATEWAY/healthz" && break
    sleep 0.5
  done
else
  LB_IP=$(k get svc hermes-gateway -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
  LB_PORT=$(k get svc hermes-gateway -o jsonpath='{.spec.ports[0].port}')
  [ -n "$LB_IP" ] || { echo "no LoadBalancer IP on hermes-gateway (deploy-gke done?)" >&2; exit 2; }
  GATEWAY="http://$LB_IP:$LB_PORT"
fi

ARGS=(-gateway "$GATEWAY" -env "$ENV" -namespace "$NS" -pool "$POOL" -kube-context "$CTX")
# Cold scenario: kind drains by default (nobody real signs up on kind);
# GKE only with DRAIN=1 — a drained pool degrades real signups.
if [ "$ENV" = "kind" ] || [ "${DRAIN:-0}" = "1" ]; then ARGS+=(-allow-pool-drain); fi
[ "${CHECK:-0}" = "1" ] && ARGS+=(-check)
[ "${TTFT:-0}" = "1" ] && ARGS+=(-ttft)

# shellcheck disable=SC2086
BENCH_ADMIN_TOKEN="$ADMIN_TOKEN" "$BIN" "${ARGS[@]}" ${BENCH_ARGS:-}
