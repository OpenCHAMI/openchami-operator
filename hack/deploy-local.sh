#!/usr/bin/env bash
set -Eeuo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
READY_TIMEOUT="${READY_TIMEOUT:-600s}"

case "${1:-}" in
  --reset) reset=true ;;
  "") reset=false ;;
  *) echo "Usage: bash hack/deploy-local.sh [--reset]" >&2; exit 2 ;;
esac
if (( $# > 1 )); then
  echo "Usage: bash hack/deploy-local.sh [--reset]" >&2
  exit 2
fi

kube() {
  kubectl --context kind-openchami-dev "$@"
}

diagnostics() {
  local exit_code=$?
  trap - ERR
  echo "Deployment failed (exit $exit_code). Cluster diagnostics:" >&2
  kube get pods -A -o wide || true
  kube -n default describe openchamicontrolplane testcluster || true
  kube -n openchami-testcluster get events --sort-by=.lastTimestamp || true
  kube -n openchami-operator-system logs \
    -l control-plane=controller-manager --tail=80 || true
  exit "$exit_code"
}
trap diagnostics ERR

if "$reset"; then
  echo "Removing the existing local cluster and Vault/LocalStack volumes..."
  make dev-down
fi

echo "Starting the local development environment and deploying the operator..."
make dev-up
make dev-deploy
kube -n openchami-operator-system scale \
  deployment/openchami-operator-controller-manager --replicas=1
kube -n openchami-operator-system wait --for=condition=Available \
  deployment/openchami-operator-controller-manager --timeout=120s

echo "Preparing the sample control plane..."
kube label nodes -l node-role.kubernetes.io/control-plane \
  openchami.org/testcluster-provision-network-ready=true \
  openchami.org/testcluster-bmc-network-ready=true --overwrite
admission_deadline=$((SECONDS + 120))
until kube apply --dry-run=server --request-timeout=15s \
  -f test/fixtures/minimal-controlplane.yaml; do
  if (( SECONDS >= admission_deadline )); then
    echo "Admission webhooks did not become available within 120 seconds." >&2
    false
  fi
done
kube apply -f test/fixtures/minimal-controlplane.yaml

echo "Waiting up to $READY_TIMEOUT for the sample control plane..."
kube -n default wait --for=jsonpath='{.status.phase}'=Ready \
  openchamicontrolplane/testcluster --timeout="$READY_TIMEOUT"
kube get svc -n envoy-gateway-system