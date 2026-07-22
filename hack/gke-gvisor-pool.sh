#!/usr/bin/env bash
# gVisor (GKE Sandbox) production pool: identical economics to the swap pool
# (n2d Spot + dedicated local-SSD swap + image streaming) with kernel-syscall
# isolation via gVisor on top. Carries the same hermes-swap label/taint so
# values-gke selectors are unchanged; GKE adds sandbox.gke.io/runtime=gvisor
# label+taint automatically, and the gvisor RuntimeClass adds the matching
# nodeSelector/toleration to every sandbox pod.
# gcloud-managed until the Terraform provider exposes swapConfig.
# Env: GKE_CLUSTER (default hermes-svc), GKE_ZONE (default us-central1-a);
# project comes from GCP_PROJECT or your active gcloud config.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
CLUSTER="${GKE_CLUSTER:-hermes-svc}"
ZONE="${GKE_ZONE:-us-central1-a}"
PROJECT_FLAG=()
[ -n "${GCP_PROJECT:-}" ] && PROJECT_FLAG=(--project "$GCP_PROJECT")
gcloud container node-pools create hermes-gvisor-pool "${PROJECT_FLAG[@]}" \
  --cluster "$CLUSTER" --zone "$ZONE" \
  --machine-type n2d-standard-8 --spot \
  --image-type cos_containerd \
  --sandbox type=gvisor \
  --ephemeral-storage-local-ssd count=1 \
  --num-nodes 1 --max-pods-per-node 256 \
  --node-labels hermes-swap=true \
  --node-taints hermes-swap=true:NoSchedule \
  --enable-image-streaming \
  --system-config-from-file "$DIR/kubelet-swap.yaml"
