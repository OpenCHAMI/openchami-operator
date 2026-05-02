#!/usr/bin/env bash
#
# rebuild-operator.sh — rebuild the operator image at a specific VERSION,
# load it into the local kind cluster, and roll the controller-manager.
#
# Used by E2E-10 (test/e2e/lifecycle_test.go) to simulate an operator upgrade
# without rebuilding from source between test cases.
#
# Usage:  rebuild-operator.sh <new-version>
#
# Required commands on PATH: docker (for image build), kind, kubectl.
# The kind cluster name and operator namespace are controlled by env vars
# (KIND_CLUSTER, OPERATOR_NAMESPACE, OPERATOR_DEPLOYMENT) so the script
# stays usable from a CI harness that names things differently.

set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $(basename "$0") <new-version>" >&2
  exit 64
fi

NEW_VERSION="$1"

KIND_CLUSTER="${KIND_CLUSTER:-openchami-dev}"
OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE:-openchami-operator-system}"
OPERATOR_DEPLOYMENT="${OPERATOR_DEPLOYMENT:-openchami-operator-controller-manager}"
OPERATOR_CONTAINER="${OPERATOR_CONTAINER:-manager}"
IMAGE_REGISTRY="${IMAGE_REGISTRY:-ghcr.io/openchami}"
IMAGE_NAME="${IMAGE_NAME:-openchami-operator}"

IMG="${IMAGE_REGISTRY}/${IMAGE_NAME}:${NEW_VERSION}"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

echo "[rebuild-operator] building ${IMG}"
make -C "${ROOT_DIR}" docker-build VERSION="${NEW_VERSION}" IMG="${IMG}" >&2

echo "[rebuild-operator] loading ${IMG} into kind cluster ${KIND_CLUSTER}"
kind load docker-image "${IMG}" --name "${KIND_CLUSTER}" >&2

echo "[rebuild-operator] rolling deployment ${OPERATOR_DEPLOYMENT} in ${OPERATOR_NAMESPACE}"
kubectl -n "${OPERATOR_NAMESPACE}" set image \
  "deployment/${OPERATOR_DEPLOYMENT}" \
  "${OPERATOR_CONTAINER}=${IMG}" >&2

kubectl -n "${OPERATOR_NAMESPACE}" rollout status \
  "deployment/${OPERATOR_DEPLOYMENT}" --timeout=180s >&2

echo "[rebuild-operator] ${OPERATOR_DEPLOYMENT} now running ${IMG}"
