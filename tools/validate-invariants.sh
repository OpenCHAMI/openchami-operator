#!/usr/bin/env bash
# Checks invariant compliance in source code.
# Catches common violations before code review.
#
# Each check is a `grep` against the relevant tree, post-filtered to drop
# comment lines and known-by-design exceptions. The exceptions live near
# their checks so a maintainer reading a failure can decide whether the
# violation is real or whether the carve-out needs to grow.
set -euo pipefail

FAIL=0
ERRORS=()

fail() { echo "  ✗ $1"; FAIL=$((FAIL+1)); ERRORS+=("$1"); }
pass() { echo "  ✓ $1"; }

echo "=== Invariant compliance check ==="

# strip_comments_and_blanks consumes `grep -n` output on stdin and removes
# matches whose source line is a Go single-line comment (`//`) or block
# comment (`/* ... */`). Match lines look like `path:NN: <line>`; we test
# the slice after the second colon. This is the workhorse for "regex hit
# inside a doc comment" false positives.
strip_comments_and_blanks() {
    awk -F: '
    {
        # Reassemble the source line (everything after path:lineno:).
        line = ""
        for (i = 3; i <= NF; i++) {
            line = (line == "" ? $i : line ":" $i)
        }
        # Trim leading whitespace.
        sub(/^[ \t]*/, "", line)
        if (line ~ /^\/\//) next      # // comment
        if (line ~ /^\*/)  next       # ContinUed /* ... */ comment
        if (line ~ /^\/\*/) next      # /* opening
        print
    }
    '
}

# Invariant 8: No direct log.FromContext in sub-reconcilers
echo "Checking: no log.FromContext in sub-reconcilers..."
hits=$(grep -rn "log\.FromContext" internal/reconcilers/ 2>/dev/null \
    | grep -v "_test.go" \
    | strip_comments_and_blanks || true)
if [ -n "$hits" ]; then
    echo "$hits"
    fail "Direct log.FromContext call in reconcilers (use logging.Enrich)"
else
    pass "No direct log.FromContext in reconcilers"
fi

# Invariant 9: No direct recorder.Event (must use helpers.RecordConditionEvent)
# helpers.go is the canonical wrapper; its single call to recorder.Event is
# the implementation of RecordConditionEvent itself, so we exclude that file.
echo "Checking: no direct recorder.Event in reconcilers..."
hits=$(grep -rn "\.Recorder\.Event\b\|recorder\.Event\b" internal/reconcilers/ 2>/dev/null \
    | grep -v "_test.go" \
    | grep -v "internal/reconcilers/helpers.go" \
    | strip_comments_and_blanks || true)
if [ -n "$hits" ]; then
    echo "$hits"
    fail "Direct recorder.Event call (use helpers.RecordConditionEvent)"
else
    pass "No direct recorder.Event in reconcilers"
fi

# Invariant 5: No client.Create followed by client.Update (should use Apply).
# Carve-out: a Get-then-Create pattern that tolerates AlreadyExists is
# idempotent, and Kubernetes Jobs cannot be Apply'd because spec.template is
# immutable after creation. Lines whose Create call is paired with
# `IsAlreadyExists` on the same line satisfy that pattern.
echo "Checking: no Create+Update pattern..."
hits=$(grep -rn "\.Create(ctx" internal/reconcilers/ 2>/dev/null \
    | grep -v "_test.go" \
    | grep -v "fake" \
    | grep -v "IsAlreadyExists" \
    | strip_comments_and_blanks || true)
if [ -n "$hits" ]; then
    echo "$hits"
    fail "Found client.Create in reconcilers (use server-side apply)"
else
    pass "No client.Create in reconcilers"
fi

# Invariant 7: No credential values in types file. Match credential nouns
# only when they appear as JSON tag values, struct field assignments, or
# string literals — not type names like TokensmithSpec or comments.
echo "Checking: no credential literals in CRD types..."
hits=$(grep -rni "password\|credential\|apikey\|api_key\|bearer" \
        api/v1alpha1/openchamicontrolplane_types.go 2>/dev/null \
    | grep -v "SecretRef\|SecretName\|secretName" \
    | strip_comments_and_blanks || true)
if [ -n "$hits" ]; then
    echo "$hits"
    fail "Possible credential value in CRD types (use SecretRef patterns)"
else
    pass "No credential literals in CRD types"
fi

# Invariant 10: No HPC domain types (xname, ipmi, compute node).
echo "Checking: no HPC domain types in operator code..."
hits=$(grep -rni "\bxname\b\|\bipmi\b\|\bcompute.node\b\|\bslurm\b\|\bpbs\b" \
        internal/ api/ cmd/ 2>/dev/null \
    | grep -v "_test.go" \
    | grep -v "vendor" \
    | strip_comments_and_blanks || true)
if [ -n "$hits" ]; then
    echo "$hits"
    fail "HPC domain type found in operator (violates quadlet test)"
else
    pass "No HPC domain types in operator code"
fi

# Vault path isolation: every "openchami/" string in vault.go must come from
# the vault.Paths() function, not a hardcoded literal. Import lines reference
# the openchami repo path and aren't path literals; exclude them.
echo "Checking: Vault paths use Paths() function..."
hits=$(grep -rn "openchami/" internal/reconcilers/vault.go 2>/dev/null \
    | grep -v "Paths(" \
    | grep -v "github.com/openchami" \
    | strip_comments_and_blanks || true)
if [ -n "$hits" ]; then
    echo "$hits"
    fail "Hardcoded Vault path in vault reconciler (use vault.Paths())"
else
    pass "Vault paths use Paths() function"
fi

# Smoke check: the deliberately-excluded helpers.go still calls recorder.Event
# (it has to, by design), and the script must continue to recognise that
# without flagging it. This catches accidental edits to either the helper
# or the exclusion above.
if grep -q "recorder\.Event" internal/reconcilers/helpers.go && [ "$FAIL" -eq 0 ]; then
    pass "helpers.RecordConditionEvent correctly excluded from invariant 9"
fi

echo ""
if [ "$FAIL" -gt 0 ]; then
    echo "Invariant violations: $FAIL"
    for e in "${ERRORS[@]}"; do echo "  - $e"; done
    exit 1
fi
echo "All invariant checks passed."
