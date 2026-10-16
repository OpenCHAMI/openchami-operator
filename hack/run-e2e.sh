#!/usr/bin/env bash
# Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
#
# SPDX-License-Identifier: MIT

set -Eeuo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

usage() {
  echo 'Usage: ./hack/run-e2e.sh [--reuse | --help]'
  echo 'Default: reset openchami-dev and dev Vault/LocalStack volumes, then run E2E tests.'
  echo '--reuse: keep existing infrastructure; rebuild/deploy the operator and run tests.'
  echo 'Leaves the environment running afterward. Clean up with: make dev-down'
}

reuse=false
if (( $# > 1 )); then
  usage >&2
  exit 2
fi
case "${1:-}" in
  --help) usage; exit 0 ;;
  --reuse) reuse=true ;;
  "") ;;
  *) usage >&2; exit 2 ;;
esac

tools=(make docker go kubectl kind aws)
if ! "$reuse"; then
  tools+=(helm vault curl)
fi
for tool in "${tools[@]}"; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "ERROR: Required command '$tool' is missing from PATH." >&2
    exit 1
  fi
done
docker info >/dev/null
docker compose version >/dev/null

export KIND_CLUSTER=openchami-dev
export KUBECTL_KUBERC=false
export NO_PROXY="${NO_PROXY:-},${no_proxy:-},localhost,127.0.0.1,.svc,.cluster.local,openchami-vault-dev,openchami-localstack-dev"
export no_proxy="$NO_PROXY"
export E2E_VAULT_ADDR=http://openchami-vault-dev:8200
export E2E_VAULT_CONTAINER=openchami-vault-dev
export VAULT_ADDR=http://127.0.0.1:8200
export VAULT_TOKEN=dev-root-token
export LOCALSTACK_ENDPOINT=http://127.0.0.1:4566
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_REGION=us-east-1
export AWS_PAGER=''
export KUBECONFIG
KUBECONFIG="$(mktemp)"

kube() {
  kubectl --context kind-openchami-dev --request-timeout=15s "$@"
}

finish() {
  local status=$?
  trap - EXIT
  if (( status != 0 )); then
    echo "E2E run failed (exit $status). Cluster diagnostics:" >&2
    kube get pods -A -o wide || true
    kube get openchamicontrolplane -A || true
    kube get events -A --sort-by=.lastTimestamp | tail -n 40 || true
    kube -n openchami-operator-system logs \
      -l control-plane=controller-manager --tail=100 || true
  fi
  rm -f "$KUBECONFIG"
  echo 'Environment left running for inspection. Clean up with: make dev-down'
  exit "$status"
}
trap finish EXIT

if "$reuse"; then
  echo 'Reusing openchami-dev and existing dev Vault/LocalStack data...'
  if ! kind export kubeconfig --name "$KIND_CLUSTER"; then
    echo 'ERROR: Cannot reuse openchami-dev. Run ./hack/run-e2e.sh for a fresh setup.' >&2
    exit 1
  fi
  if ! kube get --raw=/livez >/dev/null; then
    echo 'ERROR: The existing Kubernetes API is unavailable. Start the dev stack or run without --reuse.' >&2
    exit 1
  fi
  for container in openchami-vault-dev openchami-localstack-dev; do
    if [[ "$(docker inspect --format '{{.State.Running}} {{.State.Paused}}' "$container")" != 'true false' ]]; then
      echo "ERROR: $container must be running and unpaused. Start it or run without --reuse." >&2
      exit 1
    fi
  done
else
  echo 'Resetting openchami-dev and dev Vault/LocalStack volumes...'
  make dev-down
  echo 'Starting Kind, Vault, LocalStack, and prerequisite controllers...'
  make dev-up
fi
echo 'Building binaries needed by the E2E tests...'
make build

echo 'Running the E2E suite (shared suite setup deploys the operator and waits for admission)...'
go test -race -v -count=1 -timeout 30m -tags e2e ./test/e2e/... -args -ginkgo.v