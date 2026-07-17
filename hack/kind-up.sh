#!/usr/bin/env bash
# Create (or reuse) the dev kind cluster and install agent-sandbox
# (core + extensions CRDs/controller) at the pinned version.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-hermes-svc}"
AGENT_SANDBOX_VERSION="${AGENT_SANDBOX_VERSION:-v0.5.2}"
MANIFEST_URL="https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/sandbox-with-extensions.yaml"

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  echo "Creating kind cluster '$CLUSTER_NAME'..."
  kind create cluster --name "$CLUSTER_NAME" --wait 120s
else
  echo "kind cluster '$CLUSTER_NAME' already exists"
fi

kubectl config use-context "kind-$CLUSTER_NAME" >/dev/null

echo "Installing agent-sandbox $AGENT_SANDBOX_VERSION..."
kubectl apply -f "$MANIFEST_URL"

echo "Waiting for agent-sandbox controller..."
kubectl -n agent-sandbox-system rollout status deployment --timeout=180s

echo "Verifying CRDs..."
for crd in sandboxes.agents.x-k8s.io \
           sandboxclaims.extensions.agents.x-k8s.io \
           sandboxtemplates.extensions.agents.x-k8s.io \
           sandboxwarmpools.extensions.agents.x-k8s.io; do
  kubectl get crd "$crd" >/dev/null
done
echo "agent-sandbox ready."
