#!/usr/bin/env bash
# Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
#
# SPDX-License-Identifier: MIT

set -Eeuo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

if [[ "${1:-}" == --help ]]; then
  echo 'Usage: ./hack/run-e2e.sh'
  echo 'Resets openchami-dev and dev Vault/LocalStack volumes, then runs the E2E tests.'
  echo 'Leaves the environment running afterward. Clean up with: make dev-down'
  exit 0
fi
if (( $# != 0 )); then
  echo 'Usage: ./hack/run-e2e.sh' >&2
  exit 2
fi

for tool in make docker go kubectl helm kind vault curl aws; do
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

echo 'Resetting openchami-dev and dev Vault/LocalStack volumes...'
make dev-down
echo 'Starting Kind, Vault, LocalStack, and prerequisite controllers...'
make dev-up
echo 'Building binaries and deploying the operator...'
make build
make dev-deploy

echo 'Waiting for admission webhooks...'
deadline=$((SECONDS + 120))
until kube apply --dry-run=server --request-timeout=15s \
  -f test/fixtures/minimal-controlplane.yaml >/dev/null 2>&1; do
  if (( SECONDS >= deadline )); then
    echo 'ERROR: Admission webhooks did not become ready within 120 seconds.' >&2
    exit 1
  fi
  sleep 2
done

echo 'Running the E2E suite (test failures are expected to be investigated separately)...'
go test -race -v -count=1 -timeout 30m -tags e2e ./test/e2e/... -args -ginkgo.v