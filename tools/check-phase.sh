#!/usr/bin/env bash
# Validates completion criteria for a given phase.
# Usage: tools/check-phase.sh <phase-number>
# Exit 0 = phase complete. Exit 1 = criteria not met.
set -euo pipefail

PHASE=${1:-""}
PASS=0
FAIL=0
ERRORS=()

pass() { echo "  ✓ $1"; PASS=$((PASS+1)); }
fail() { echo "  ✗ $1"; FAIL=$((FAIL+1)); ERRORS+=("$1"); }

file_exists() { [ -f "$1" ] && pass "File exists: $1" || fail "Missing file: $1"; }
dir_exists()  { [ -d "$1" ] && pass "Dir exists: $1"  || fail "Missing dir: $1"; }
cmd_succeeds() {
    local desc="$1"; shift
    if "$@" &>/dev/null; then pass "$desc"; else fail "$desc"; fi
}
go_compiles() { cmd_succeeds "go build ./..." go build ./...; }
tests_pass()  { cmd_succeeds "make test" make test; }
lint_passes() { cmd_succeeds "make lint" make lint; }
binary_exists() { [ -x "bin/$1" ] && pass "Binary: bin/$1" || fail "Missing binary: bin/$1"; }

echo "=== Phase $PHASE validation ==="

case "$PHASE" in
0)
    echo "Checking Phase 0: Bootstrap"
    file_exists "go.mod"
    file_exists "Makefile"
    file_exists ".golangci.yml"
    file_exists "hack/local-dev/docker-compose.yaml"
    file_exists "hack/local-dev/kind-config.yaml"
    file_exists "hack/local-dev/seed-vault.sh"
    file_exists "api/v1alpha1/openchamicontrolplane_types.go"
    file_exists "internal/controller/openchamicontrolplane_controller.go"
    file_exists "cmd/operator/main.go"
    file_exists "cmd/ochami-admin/main.go"
    go_compiles
    binary_exists "operator"
    binary_exists "ochami-admin"
    tests_pass
    lint_passes
    ;;
1)
    echo "Checking Phase 1: CRD Types"
    file_exists "api/v1alpha1/openchamicontrolplane_types.go"
    dir_exists "config/crd/bases"
    cmd_succeeds "CRD generates cleanly" make generate manifests
    cmd_succeeds "CRD YAML is valid" bash -c 'for f in config/crd/bases/*.yaml; do grep -q "kind: CustomResourceDefinition" "$f" || exit 1; done'
    go_compiles
    tests_pass
    ;;
2)
    echo "Checking Phase 2: Controller Skeleton"
    file_exists "internal/logging/logger.go"
    file_exists "internal/reconcilers/reconciler.go"
    file_exists "internal/reconcilers/helpers.go"
    file_exists "internal/reconcilers/namespace.go"
    file_exists "internal/reconcilers/rbac.go"
    file_exists "internal/controller/openchamicontrolplane_controller.go"
    go_compiles
    tests_pass
    lint_passes
    ;;
3)
    echo "Checking Phase 3: Vault Integration"
    file_exists "internal/vault/client.go"
    file_exists "internal/vault/client_vault.go"
    file_exists "internal/vault/fake/client.go"
    file_exists "internal/vault/paths.go"
    file_exists "internal/vault/policies.go"
    file_exists "internal/s3/client.go"
    file_exists "internal/s3/fake/client.go"
    file_exists "internal/reconcilers/vault.go"
    file_exists "internal/reconcilers/vault_test.go"
    file_exists "internal/reconcilers/bucket.go"
    go_compiles
    tests_pass
    ;;
4)
    echo "Checking Phase 4: Database"
    file_exists "internal/reconcilers/database.go"
    file_exists "internal/reconcilers/database_test.go"
    go_compiles
    tests_pass
    ;;
5)
    echo "Checking Phase 5: Core Services"
    for svc in smd tokensmith boot_service metadata_service; do
        file_exists "internal/reconcilers/${svc}.go"
        file_exists "internal/reconcilers/${svc}_test.go"
    done
    go_compiles
    tests_pass
    ;;
6)
    echo "Checking Phase 6: Network Services"
    file_exists "internal/reconcilers/networkprobe.go"
    file_exists "internal/reconcilers/networkprobe_test.go"
    file_exists "internal/reconcilers/coredhcp.go"
    file_exists "internal/reconcilers/coredhcp_test.go"
    file_exists "internal/reconcilers/magellan.go"
    file_exists "internal/reconcilers/magellan_test.go"
    go_compiles
    tests_pass
    ;;
7)
    echo "Checking Phase 7: Gateway + Certs"
    file_exists "internal/reconcilers/gateway.go"
    file_exists "internal/reconcilers/certificates.go"
    go_compiles
    tests_pass
    ;;
8)
    echo "Checking Phase 8: Network Policies"
    file_exists "internal/reconcilers/networkpolicies.go"
    file_exists "internal/reconcilers/networkpolicies_test.go"
    go_compiles
    tests_pass
    ;;
9)
    echo "Checking Phase 9: Topology"
    file_exists "internal/reconcilers/topology.go"
    file_exists "internal/reconcilers/topology_schema.go"
    file_exists "internal/reconcilers/topology_test.go"
    go_compiles
    tests_pass
    ;;
10)
    echo "Checking Phase 10: Webhooks"
    file_exists "api/v1alpha1/openchamicontrolplane_webhook.go"
    go_compiles
    tests_pass
    ;;
11)
    echo "Checking Phase 11: Observability"
    file_exists "internal/status/reporter.go"
    go_compiles
    tests_pass
    ;;
12)
    echo "Checking Phase 12: Funicular"
    file_exists "internal/reconcilers/logbucket.go"
    file_exists "internal/reconcilers/funicular.go"
    go_compiles
    tests_pass
    ;;
13)
    echo "Checking Phase 13: Versioning"
    file_exists "hack/migrate-storage-version.sh"
    file_exists "UPGRADE.md"
    go_compiles
    tests_pass
    ;;
14)
    echo "Checking Phase 14: CLI"
    for cmd in init describe backup restore logs; do
        file_exists "internal/admin/${cmd}.go"
    done
    binary_exists "ochami-admin"
    cmd_succeeds "ochami-admin --help" bin/ochami-admin --help
    go_compiles
    tests_pass
    ;;
15)
    echo "Checking Phase 15: E2E"
    file_exists "test/e2e/suite_test.go"
    file_exists "test/e2e/lifecycle_test.go"
    file_exists "test/e2e/network_test.go"
    file_exists "test/e2e/observability_test.go"
    cmd_succeeds "go test -list e2e" go test -tags e2e -list '.*' ./test/e2e/
    tests_pass
    ;;
*)
    echo "Usage: tools/check-phase.sh <0-15>"
    exit 1
    ;;
esac

echo ""
echo "Results: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
    echo "Failed criteria:"
    for e in "${ERRORS[@]}"; do echo "  - $e"; done
    exit 1
fi
echo "Phase $PHASE complete. Proceed to next phase."
