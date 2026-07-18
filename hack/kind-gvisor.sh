#!/usr/bin/env bash
# Install gVisor (runsc) into every node of the dev kind cluster and register
# the `gvisor` RuntimeClass, so sandbox pods can run with
# `runtimeClassName: gvisor` locally (parity with the GKE Sandbox pool).
#
# Idempotent: safe to re-run; skips nodes that already have runsc configured.
# Validated on kind + containerd 2.3 (v2 config), arm64 and x86_64, with the
# systrap platform and systemd cgroups (kind's default cgroup driver).
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-hermes-svc}"
GVISOR_RELEASE="${GVISOR_RELEASE:-release/latest}"

nodes=$(kind get nodes --name "$CLUSTER_NAME")
[ -n "$nodes" ] || { echo "no kind nodes found for cluster $CLUSTER_NAME" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
downloaded_arch=""

for node in $nodes; do
  if docker exec "$node" test -x /usr/local/bin/runsc 2>/dev/null \
     && docker exec "$node" grep -q 'runtimes.runsc' /etc/containerd/config.toml; then
    echo "$node: runsc already installed"
    continue
  fi

  arch=$(docker exec "$node" uname -m)   # aarch64 | x86_64
  if [ "$downloaded_arch" != "$arch" ]; then
    base="https://storage.googleapis.com/gvisor/releases/${GVISOR_RELEASE}/${arch}"
    echo "Downloading runsc (${GVISOR_RELEASE}, $arch)..."
    curl -fsSL -o "$tmp/runsc" "$base/runsc"
    curl -fsSL -o "$tmp/containerd-shim-runsc-v1" "$base/containerd-shim-runsc-v1"
    chmod +x "$tmp/runsc" "$tmp/containerd-shim-runsc-v1"
    downloaded_arch="$arch"
  fi

  echo "$node: installing runsc + containerd runtime handler..."
  docker cp "$tmp/runsc" "$node:/usr/local/bin/runsc"
  docker cp "$tmp/containerd-shim-runsc-v1" "$node:/usr/local/bin/containerd-shim-runsc-v1"

  docker exec "$node" sh -c 'grep -q "runtimes.runsc" /etc/containerd/config.toml || cat >> /etc/containerd/config.toml <<EOF

# gVisor (runsc) runtime -- added by hack/kind-gvisor.sh
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc.options]
    TypeUrl = "io.containerd.runsc.v1.options"
    ConfigPath = "/etc/containerd/runsc.toml"
EOF'
  # kind uses the systemd cgroup driver; runsc must match it.
  docker exec "$node" sh -c 'cat > /etc/containerd/runsc.toml <<EOF
[runsc_config]
  systemd-cgroup = "true"
EOF'
  docker exec "$node" systemctl restart containerd
done

kubectl --context "kind-$CLUSTER_NAME" apply -f - <<EOF
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
EOF

echo "gVisor ready: RuntimeClass 'gvisor' registered."
