#!/usr/bin/env bash
# Production-candidate swap pool: n2d (supports BOTH pd-balanced PVCs and
# local SSD — c4 is Hyperdisk-only and cannot mount existing user PVCs).
# gcloud-managed until the Terraform provider exposes swapConfig.
# Env: GKE_CLUSTER (default hermes-svc), GKE_ZONE (default us-central1-a);
# project comes from GCP_PROJECT or your active gcloud config.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
CLUSTER="${GKE_CLUSTER:-hermes-svc}"
ZONE="${GKE_ZONE:-us-central1-a}"
PROJECT_FLAG=()
[ -n "${GCP_PROJECT:-}" ] && PROJECT_FLAG=(--project "$GCP_PROJECT")
gcloud container node-pools create hermes-swap-pool "${PROJECT_FLAG[@]}" \
  --cluster "$CLUSTER" --zone "$ZONE" \
  --machine-type n2d-standard-8 --spot \
  --ephemeral-storage-local-ssd count=1 \
  --num-nodes 1 --max-pods-per-node 256 \
  --node-labels hermes-swap=true \
  --node-taints hermes-swap=true:NoSchedule \
  --enable-image-streaming \
  --system-config-from-file "$DIR/kubelet-swap.yaml"
