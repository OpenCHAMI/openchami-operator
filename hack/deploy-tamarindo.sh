#!/bin/bash
# Exit immediately if a command exits with a non-zero status
set -Eeuo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"
READY_TIMEOUT="${READY_TIMEOUT:-600s}"
if [[ -f "$HOME/.docker/config.json" ]]; then
  export HTTP_PROXY="${HTTP_PROXY:-${http_proxy:-$(jq -r '.proxies.default.httpProxy // empty' "$HOME/.docker/config.json")}}"
  export HTTPS_PROXY="${HTTPS_PROXY:-${https_proxy:-$(jq -r '.proxies.default.httpsProxy // empty' "$HOME/.docker/config.json")}}"
  export NO_PROXY="${NO_PROXY:-${no_proxy:-$(jq -r '.proxies.default.noProxy // empty' "$HOME/.docker/config.json")}}"
fi
export http_proxy="${HTTP_PROXY:-${http_proxy:-}}"
export https_proxy="${HTTPS_PROXY:-${https_proxy:-}}"
export NO_PROXY="${NO_PROXY:-${no_proxy:-}},localhost,127.0.0.1,::1,openchami-dev-control-plane,openchami-vault-dev,openchami-localstack-dev,.svc,.cluster.local,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00:f853:ccd:e793::/64"
export no_proxy="$NO_PROXY"

diagnostics() {
  local exit_code=$?
  trap - ERR
  echo "Deployment failed (exit $exit_code). Current cluster diagnostics:" >&2
  if ! kubectl config get-contexts kind-openchami-dev -o name 2>/dev/null | grep -qx kind-openchami-dev; then
    echo "The kind cluster did not finish creating; no Kubernetes diagnostics are available." >&2
    exit "$exit_code"
  fi
  kubectl --context kind-openchami-dev get pods -A -o wide || true
  kubectl --context kind-openchami-dev -n default describe openchamicontrolplane frodo || true
  kubectl --context kind-openchami-dev -n openchami-frodo get events --sort-by=.lastTimestamp || true
  kubectl --context kind-openchami-dev -n openchami-operator-system logs \
    -l control-plane=controller-manager --tail=80 || true
  kubectl --context kind-openchami-dev -n openchami-frodo logs \
    -l app.kubernetes.io/name=metadata-service --tail=80 || true
  kubectl --context kind-openchami-dev -n openchami-frodo logs \
    -l app.kubernetes.io/name=metadata-service --previous --tail=80 || true
  exit "$exit_code"
}
trap diagnostics ERR

echo "Tearing down any existing environment to ensure a clean slate..."
make dev-down

echo "Bringing up the kind cluster, Vault, LocalStack, and CRDs..."
make dev-up

echo "Building and deploying the operator into the kind cluster (Path B)..."
make dev-deploy
kubectl --context kind-openchami-dev -n openchami-operator-system scale \
  deployment/openchami-operator-controller-manager --replicas=1

echo "Waiting up to 120 seconds for the Operator webhook Pod to be Ready..."
kubectl --context kind-openchami-dev wait --namespace openchami-operator-system \
  --for=condition=Available deployment/openchami-operator-controller-manager \
  --timeout=120s

echo "Applying the test cluster configuration..."
kubectl --context kind-openchami-dev label nodes openchami-dev-control-plane \
  openchami.org/frodo-provision-network-ready=true \
  openchami.org/frodo-bmc-network-ready=true --overwrite
echo "Waiting up to 120 seconds for admission webhooks to accept the configuration..."
admission_deadline=$((SECONDS + 120))
until kubectl --context kind-openchami-dev apply --dry-run=server \
  --request-timeout=15s -f test/fixtures/minimal-controlplane.yaml; do
  if (( SECONDS >= admission_deadline )); then
    echo "Admission webhooks did not become available within 120 seconds." >&2
    false
  fi
done
kubectl --context kind-openchami-dev apply -f test/fixtures/minimal-controlplane.yaml

echo "Waiting up to $READY_TIMEOUT for the OpenCHAMI control plane to reach Ready state..."
kubectl --context kind-openchami-dev -n default wait --for=jsonpath='{.status.phase}'=Ready \
  openchamicontrolplane/frodo --timeout="$READY_TIMEOUT"

echo "Control plane is Ready. Fetching Envoy Gateway service details for Magellan routing:"
kubectl --context kind-openchami-dev get svc -n envoy-gateway-system