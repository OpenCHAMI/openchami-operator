#!/usr/bin/env bash
# Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
#
# SPDX-License-Identifier: MIT
#
# hack/run-e2e.sh
# Completely resets the local Kind + Docker Compose environment from a clean
# slate, provisions all prerequisites and the operator (based on deploy-local.sh
# without polluting cluster state with sample fixtures/labels), and executes
# the end-to-end test suite.
#
# Usage:
#   bash hack/run-e2e.sh                  # Full reset + setup + run full `make e2e`
#   bash hack/run-e2e.sh -run E2E-01      # Full reset + setup + pass flags to `go test`
#   KEEP_CLUSTER=true bash hack/run-e2e.sh # Keep environment running after tests finish

set -Eeuo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

KIND_CLUSTER_NAME="${KIND_CLUSTER:-openchami-dev}"
KUBE_CONTEXT="kind-${KIND_CLUSTER_NAME}"
VAULT_CLI_VERSION="${VAULT_CLI_VERSION:-1.20.2}"
KEEP_CLUSTER="${KEEP_CLUSTER:-true}"
LOCAL_BIN="${REPO_ROOT}/.bin"
mkdir -p "$LOCAL_BIN"
export PATH="${LOCAL_BIN}:${PATH}"

log() {
  printf '\n\033[1;34m==> [%s] %s\033[0m\n' "$(date +%H:%M:%S)" "$*"
}

warn() {
  printf '\033[1;33m[WARN] %s\033[0m\n' "$*" >&2
}

kube() {
  kubectl --context "$KUBE_CONTEXT" "$@"
}

diagnostics() {
  local exit_code=$?
  trap - ERR
  printf '\n\033[1;31m==> E2E run failed (exit %d). Dumping cluster diagnostics...\033[0m\n' "$exit_code" >&2
  docker ps -a || true
  kube get nodes --show-labels || true
  kube get pods -A -o wide || true
  kube get openchamicontrolplane -A -o yaml || true
  kube get events -A --sort-by=.lastTimestamp | tail -n 60 || true
  kube -n openchami-operator-system logs \
    -l control-plane=controller-manager --tail=120 || true
  exit "$exit_code"
}
trap diagnostics ERR

# ------------------------------------------------------------------------------
# 1. Verify (and auto-install if missing) CLI prerequisites
# ------------------------------------------------------------------------------
ensure_prereq_Clis() {
  log "Checking required host CLI tools..."

  for required_cmd in docker go kubectl helm curl unzip; do
    if ! command -v "$required_cmd" >/dev/null 2>&1; then
      echo "ERROR: Required command '$required_cmd' is not installed or not in PATH." >&2
      exit 1
    fi
  done

  if ! docker info >/dev/null 2>&1; then
    echo "ERROR: Docker daemon is not running or not accessible." >&2
    exit 1
  fi

  local os arch
  os="$(go env GOOS)"
  arch="$(go env GOARCH)"

  if ! command -v kind >/dev/null 2>&1; then
    warn "'kind' not found in PATH; installing latest kind to ${LOCAL_BIN}/kind..."
    curl -fsSLo "${LOCAL_BIN}/kind" "https://kind.sigs.k8s.io/dl/latest/kind-${os}-${arch}"
    chmod +x "${LOCAL_BIN}/kind"
  fi

  if ! command -v vault >/dev/null 2>&1; then
    warn "'vault' CLI not found in PATH; installing vault v${VAULT_CLI_VERSION} to ${LOCAL_BIN}/vault..."
    local tmp_zip
    tmp_zip="$(mktemp)"
    curl -fsSLo "$tmp_zip" \
      "https://releases.hashicorp.com/vault/${VAULT_CLI_VERSION}/vault_${VAULT_CLI_VERSION}_${os}_${arch}.zip"
    unzip -qo "$tmp_zip" -d "$LOCAL_BIN"
    chmod +x "${LOCAL_BIN}/vault"
    rm -f "$tmp_zip"
  fi

  echo "  go:      $(go version)"
  echo "  docker:  $(docker --version)"
  echo "  kind:    $(kind version)"
  echo "  kubectl: $(kubectl version --client -o yaml 2>/dev/null | grep gitVersion || true)"
  echo "  helm:    $(helm version --short)"
  echo "  vault:   $(vault version)"
}

# ------------------------------------------------------------------------------
# 2. Total Clean Slate (Nuke existing clusters, containers, and volumes)
# ------------------------------------------------------------------------------
clean_slate() {
  log "Wiping existing local clusters, containers, and volumes for a clean slate..."

  # Standard Makefile teardown
  make dev-down || true

  # Delete target kind cluster and any accidental default 'kind' cluster
  for cluster in "$KIND_CLUSTER_NAME" "kind"; do
    if kind get clusters 2>/dev/null | grep -qx "$cluster"; then
      echo "Deleting leftover kind cluster: $cluster"
      kind delete cluster --name "$cluster" || true
    fi
  done

  # Force-remove any lingering dev containers
  docker rm -f openchami-vault-dev openchami-localstack-dev 2>/dev/null || true

  # Clean up any stale docker compose project resources
  docker compose -f hack/local-dev/docker-compose.yaml down -v --remove-orphans 2>/dev/null || true
}

# ------------------------------------------------------------------------------
# 3. Provision Local Dev Stack & Operator (without sample fixtures)
# ------------------------------------------------------------------------------
setup_environment() {
  log "Starting local development stack (Vault, LocalStack, Kind, CRDs, Prereqs)..."
  make dev-up

  log "Setting active kubectl context to ${KUBE_CONTEXT}..."
  kubectl config use-context "$KUBE_CONTEXT"

  log "Building, loading, and deploying the operator (make dev-deploy)..."
  make dev-deploy

  log "Ensuring controller-manager is scaled to 1 replica and Available..."
  kube -n openchami-operator-system scale \
    deployment/openchami-operator-controller-manager --replicas=1
  kube -n openchami-operator-system wait --for=condition=Available \
    deployment/openchami-operator-controller-manager --timeout=120s

  log "Waiting for operator admission webhooks to finish TLS warmup (dry-run only)..."
  local admission_deadline=$((SECONDS + 120))
  until kube apply --dry-run=server --request-timeout=15s \
    -f test/fixtures/minimal-controlplane.yaml >/dev/null 2>&1; do
    if (( SECONDS >= admission_deadline )); then
      echo "ERROR: Operator admission webhooks did not become ready within 120s." >&2
      false
    fi
    sleep 2
  done
  echo "Admission webhooks are active and responding."

  # Ensure no leftover OpenCHAMIControlPlane resources or test labels exist before starting e2e
  kube delete openchamicontrolplane --all -A --ignore-not-found=true
  kube label nodes --all \
    openchami.org/testcluster-provision-network-ready- \
    openchami.org/testcluster-bmc-network-ready- 2>/dev/null || true
}

# ------------------------------------------------------------------------------
# 4. Run the E2E Test Suite
# ------------------------------------------------------------------------------
run_e2e_suite() {
  # Export environment variables expected by test/utils and test/e2e
  export KIND_CLUSTER="$KIND_CLUSTER_NAME"
  export E2E_VAULT_ADDR="${E2E_VAULT_ADDR:-http://openchami-vault-dev:8200}"
  export E2E_SKIP_CLEANUP="true"
  export VAULT_ADDR="http://127.0.0.1:8200"
  export VAULT_TOKEN="dev-root-token"

  log "Environment ready:"
  echo "  KUBE_CONTEXT   = ${KUBE_CONTEXT}"
  echo "  KIND_CLUSTER   = ${KIND_CLUSTER}"
  echo "  E2E_VAULT_ADDR = ${E2E_VAULT_ADDR}"

  if (( $# == 0 )); then
    log "Running full E2E suite via 'make e2e'..."
    make e2e
  else
    log "Running E2E suite with custom go test arguments: $*"
    go test -race -v -count=1 -timeout 30m -tags e2e ./test/e2e/... "$@"
  fi
}

# ------------------------------------------------------------------------------
# Main execution flow
# ------------------------------------------------------------------------------
ensure_prereq_Clis
clean_slate
setup_environment
run_e2e_suite "$@"

if [[ "$KEEP_CLUSTER" != "true" ]]; then
  log "Tearing down environment (KEEP_CLUSTER=${KEEP_CLUSTER})..."
  make dev-down
else
  log "E2E run succeeded! Cluster '${KIND_CLUSTER_NAME}' left running for inspection (run 'make dev-down' to clean up)."
fi
