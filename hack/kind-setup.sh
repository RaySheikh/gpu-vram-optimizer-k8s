#!/usr/bin/env bash
# hack/kind-setup.sh: Bootstraps a local multi-node kind cluster that simulates
# a GPU fleet for Phase 2 validation. No real GPUs required.
#
# Prerequisites: kind, kubectl, docker
#
# Usage:
#   ./hack/kind-setup.sh              # build images from source + create cluster
#   SKIP_BUILD=1 ./hack/kind-setup.sh # skip docker build (use cached images)

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-gpu-vram-dev}"
REGISTRY="${REGISTRY:-ghcr.io/ray}"
# For local kind testing we always tag as 'latest' so the deploy manifests
# (which reference :latest) find the image inside the kind nodes without
# requiring a registry push.
VERSION="${VERSION:-latest}"
TELEMETRY_IMG="${REGISTRY}/gpu-vram-telemetry-daemon:${VERSION}"
SCHEDULER_IMG="${REGISTRY}/gpu-packer-scheduler:${VERSION}"

# ── Prerequisite checks ───────────────────────────────────────────────────────

check_prereq() {
  local cmd="$1"
  if ! command -v "$cmd" &>/dev/null; then
    echo "ERROR: '$cmd' not found in PATH. Please install it first." >&2
    exit 1
  fi
}

check_prereq kind
check_prereq kubectl
check_prereq docker

# ── Build images ──────────────────────────────────────────────────────────────

if [[ "${SKIP_BUILD:-0}" != "1" ]]; then
  echo "==> Building Docker images..."
  docker build -f Dockerfile.telemetry -t "${TELEMETRY_IMG}" .
  docker build -f Dockerfile.scheduler -t "${SCHEDULER_IMG}" .
fi

# ── Create kind cluster ───────────────────────────────────────────────────────

echo "==> Creating kind cluster '${CLUSTER_NAME}'..."

# Write a temporary kind config with:
# - 1 control-plane node
# - 2 worker nodes (simulating the H100 and A10G fleet)
KIND_CONFIG=$(mktemp /tmp/kind-config-XXXXXX.yaml)
trap 'rm -f "${KIND_CONFIG}"' EXIT

cat > "${KIND_CONFIG}" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: ${CLUSTER_NAME}
nodes:
  - role: control-plane
  - role: worker
    labels:
      gpu-node: "true"
      sim-gpu-model: "h100"
  - role: worker
    labels:
      gpu-node: "true"
      sim-gpu-model: "a10g"
EOF

if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  echo "    Cluster '${CLUSTER_NAME}' already exists, skipping creation."
else
  kind create cluster --config "${KIND_CONFIG}"
fi

# ── Load images into kind ─────────────────────────────────────────────────────

echo "==> Loading images into kind cluster..."
kind load docker-image "${TELEMETRY_IMG}" --name "${CLUSTER_NAME}"
kind load docker-image "${SCHEDULER_IMG}" --name "${CLUSTER_NAME}"

# ── Apply manifests ───────────────────────────────────────────────────────────

echo "==> Applying Kubernetes manifests..."
kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/telemetry-daemon/
kubectl apply -f deploy/scheduler/rbac.yaml
kubectl apply -f deploy/scheduler/scheduler-config.yaml
kubectl apply -f deploy/scheduler/deployment.yaml
kubectl apply -f deploy/observability/

# ── Wait for readiness ────────────────────────────────────────────────────────

echo "==> Waiting for deployments to become ready (up to 120s)..."
kubectl rollout status deployment/telemetry-daemon  -n gpu-scheduler --timeout=120s
kubectl rollout status deployment/gpu-packer-scheduler -n gpu-scheduler --timeout=120s

# ── Verification steps ────────────────────────────────────────────────────────

echo ""
echo "✅  Cluster is ready. Verify the setup with:"
echo ""
echo "    # Check telemetry daemon is serving metrics:"
echo "    kubectl -n gpu-scheduler port-forward svc/telemetry-daemon-svc 8080:8080 &"
echo "    curl http://localhost:8080/api/v1/nodes"
echo "    curl http://localhost:8080/metrics | grep nvidia_gpu"
echo ""
echo "    # Submit the dummy LLM workload:"
echo "    kubectl apply -f examples/"
echo "    kubectl get pods -w"
echo ""
echo "    # Inspect scheduling decisions:"
echo "    kubectl logs -n gpu-scheduler -l app=gpu-packer-scheduler -f"
echo ""
echo "    # Tear down when done:"
echo "    ./hack/teardown.sh"
