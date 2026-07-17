#!/usr/bin/env bash
# EXPERIMENT: Spot c4-standard-8-lssd pool with dedicated-LSSD swap
# (mirrors agent-sandbox examples/gke-swap). Not Terraform-managed on
# purpose — provider lacks swapConfig; terraform-ify if results win.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
gcloud container node-pools create swap-test-pool \
  --cluster hermes-svc --zone us-central1-a \
  --machine-type c4-standard-8-lssd --spot \
  --num-nodes 1 --max-pods-per-node 256 \
  --node-labels hermes-swap=true \
  --node-taints hermes-swap=true:NoSchedule \
  --system-config-from-file "$DIR/kubelet-swap.yaml"
