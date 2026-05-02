#!/usr/bin/env bash
# Checks invariant compliance in source code.
# Catches common violations before code review.
set -euo pipefail

FAIL=0
ERRORS=()

fail() { echo "  ✗ $1"; FAIL=$((FAIL+1)); ERRORS+=("$1"); }
pass() { echo "  ✓ $1"; }

echo "=== Invariant compliance check ==="

# Invariant 8: No direct log.FromContext in sub-reconcilers
echo "Checking: no log.FromContext in sub-reconcilers..."
if grep -rn "log\.FromContext" internal/reconcilers/ 2>/dev/null | grep -v "_test.go"; then
    fail "Direct log.FromContext call in reconcilers (use logging.Enrich)"
else
    pass "No direct log.FromContext in reconcilers"
fi

# Invariant 9: No direct recorder.Event (must use helpers.RecordConditionEvent)
echo "Checking: no direct recorder.Event in reconcilers..."
if grep -rn "\.Recorder\.Event\b\|recorder\.Event\b" internal/reconcilers/ 2>/dev/null | grep -v "_test.go"; then
    fail "Direct recorder.Event call (use helpers.RecordConditionEvent)"
else
    pass "No direct recorder.Event in reconcilers"
fi

# Invariant 5: No client.Create followed by client.Update (should use Apply)
echo "Checking: no Create+Update pattern..."
if grep -rn "\.Create(ctx" internal/reconcilers/ 2>/dev/null | grep -v "_test.go" | grep -v "fake"; then
    fail "Found client.Create in reconcilers (use server-side apply)"
else
    pass "No client.Create in reconcilers"
fi

# Invariant 7: No credential values in types file
echo "Checking: no credential literals in CRD types..."
if grep -rni "password\|secret\|token\|key" api/v1alpha1/openchamicluster_types.go 2>/dev/null | \
   grep -v "SecretRef\|SecretName\|comment\|//"; then
    fail "Possible credential value in CRD types (use SecretRef patterns)"
else
    pass "No credential literals in CRD types"
fi

# Invariant 10: No HPC domain types (xname, ipmi, compute node)
echo "Checking: no HPC domain types in operator code..."
if grep -rni "\bxname\b\|\bipmi\b\|\bcompute.node\b\|\bslurm\b\|\bpbs\b" \
   internal/ api/ cmd/ 2>/dev/null | grep -v "_test.go" | grep -v "vendor"; then
    fail "HPC domain type found in operator (violates quadlet test)"
else
    pass "No HPC domain types in operator code"
fi

# Vault path isolation: all vault writes use Paths() function
echo "Checking: Vault paths use Paths() function..."
if grep -rn "openchami/" internal/reconcilers/vault.go 2>/dev/null | grep -v "Paths("; then
    fail "Hardcoded Vault path in vault reconciler (use vault.Paths())"
else
    pass "Vault paths use Paths() function"
fi

echo ""
if [ "$FAIL" -gt 0 ]; then
    echo "Invariant violations: $FAIL"
    for e in "${ERRORS[@]}"; do echo "  - $e"; done
    exit 1
fi
echo "All invariant checks passed."
