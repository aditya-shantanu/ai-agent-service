#!/usr/bin/env bash
# gVisor (GKE Sandbox) production pool: identical economics to the swap pool
# (n2d Spot + dedicated local-SSD swap + image streaming) with kernel-syscall
# isolation via gVisor on top. Carries the same hermes-swap label/taint so
# values-gke selectors are unchanged; GKE adds sandbox.gke.io/runtime=gvisor
# label+taint automatically, and the gvisor RuntimeClass adds the matching
# nodeSelector/toleration to every sandbox pod.
# gcloud-managed until the Terraform provider exposes swapConfig.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
gcloud container node-pools create hermes-gvisor-pool \
  --cluster hermes-svc --zone us-central1-a \
  --machine-type n2d-standard-8 --spot \
  --image-type cos_containerd \
  --sandbox type=gvisor \
  --ephemeral-storage-local-ssd count=1 \
  --num-nodes 1 --max-pods-per-node 256 \
  --node-labels hermes-swap=true \
  --node-taints hermes-swap=true:NoSchedule \
  --enable-image-streaming \
  --system-config-from-file "$DIR/swap-experiment/kubelet-swap.yaml"
