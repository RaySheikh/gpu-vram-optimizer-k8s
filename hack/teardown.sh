#!/usr/bin/env bash
# hack/teardown.sh: Tears down the local kind cluster created by kind-setup.sh.

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-gpu-vram-dev}"

if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  echo "==> Deleting kind cluster '${CLUSTER_NAME}'..."
  kind delete cluster --name "${CLUSTER_NAME}"
  echo "✅  Cluster deleted."
else
  echo "No cluster named '${CLUSTER_NAME}' found."
fi
