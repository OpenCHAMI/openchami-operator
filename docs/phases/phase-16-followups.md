# Phase 16 — Follow-ups from Phases 0-15

Punch list for things deferred during the initial 16-phase build. Each item
has a sized scope and a clear acceptance gate so it can be picked up
independently. Order them by risk: F-1 first (correctness), then F-2 (test
contract), then F-3 (real-feature gap), then F-4/F-5 (scaffolded e2e),
then F-6 (cosmetic).

Read `CLAUDE.md` and the relevant existing reconciler/test before starting
each item. Run `make generate manifests fmt vet lint test` after every
landed change.

---

## F-1 — RBAC manifests miss controller-file markers *(fixed 2026-05-04)*

**Problem.** `make manifests` ran `controller-gen` against `paths="./api/..."`
only, so kubebuilder `+kubebuilder:rbac:` markers added on
`internal/controller/openchamicontrolplane_controller.go` (Phases 7, 8, 11, 12)
never made it into `config/rbac/role.yaml`. Production deploys built from
this Makefile were missing permissions for: gateway-api Gateways/HTTPRoutes,
envoy-gateway SecurityPolicies/BackendTrafficPolicies, cert-manager
Certificates, NetworkPolicies, and monitoring.coreos.com ServiceMonitors.
This was the same gap surfaced under "In-cluster operator missing RBAC for
resources it applies" in [`bugs.md`](../../bugs.md).

**Fix.** `Makefile` `manifests:` recipe now passes
`paths="./api/...;./internal/controller/..."` to controller-gen. Re-running
`make manifests` regenerates `config/rbac/role.yaml` with rules for all
five previously-missing groups (`gateway.networking.k8s.io`,
`gateway.envoyproxy.io`, `cert-manager.io`, `networking.k8s.io`,
`monitoring.coreos.com`). Verified: every kubebuilder rbac marker in
`internal/controller/openchamicontrolplane_controller.go:51-74` is reflected
as a rule in `config/rbac/role.yaml`. Production-RBAC guidance for admins
lives in [`docs/install-production.md`](../install-production.md#5-deploy-the-operator).

**Audit command** (idempotency check; re-run after any change to controller
RBAC markers):
```sh
make manifests
git diff --stat config/rbac/role.yaml   # expect empty
```

**Severity in retrospect.** High — blocked every in-cluster reconcile.
**Found:** 2026-05-04 during in-cluster shakedown. **Closed:** 2026-05-22.

---

## F-2 — `Describe()` must not contact remote services *(fixed 2026-05-04)*

**Problem.** `internal/reconcilers/networkpolicies.go::buildPolicies` called
`VaultEgressPeer` / `VersityGWEgressPeer` from helpers.go, which both
`net.LookupHost` whenever the configured Vault/VersityGW hostname was not
under `.svc.cluster.local`. `Describe()` calls into `buildPolicies`
directly, so `ochami-admin describe` against a CR with an external Vault
address performed live DNS — violating the SubReconciler contract.

**Fix.** Took Option A from the original plan: split the helpers.
- `internal/reconcilers/helpers.go:554` — `VaultEgressPeer(addr)` (DNS-resolving,
  Reconcile-only).
- `internal/reconcilers/helpers.go:573` — `VaultEgressPeerSyntax(addr)`
  (returns a sentinel `0.0.0.0/0` ipBlock for external hostnames, no DNS).
- Mirrored split for VersityGW at `helpers.go:599` / `helpers.go:613`.
- Shared `parseEgressHost(addr, label)` factored out so both variants share
  the URL-parsing path.
- `networkpolicies.go:179` — `buildPolicies(cp, resolveDNS bool)`. When
  `resolveDNS=false`, swaps the helper function pointers to the `*Syntax`
  variants.
- `Reconcile()` calls `buildPolicies(cp, true)`. `Describe()` calls
  `buildPolicies(cp, false)`.

**Regression coverage** in `networkpolicies_test.go`:
- `TestNetworkPoliciesReconciler_DescribeNoDNS_ExternalVault` —
  `Spec.Platform.Vault.Address = "https://test.invalid:8200"` (RFC 6761
  reserved domain; system resolver returns NXDOMAIN). `Describe()` returns
  no error.
- `TestNetworkPoliciesReconciler_DescribeNoDNS_ExternalVersityGW` —
  mirror for the object-storage endpoint.

A live DNS call against `test.invalid` would surface as a "no such host"
failure on most systems, so test-success here means the syntax-only path
is exercised.

**Severity in retrospect.** Medium — would have broken `ochami-admin
describe` in air-gapped environments or any context where DNS for the
external endpoints isn't available. Found before any user hit it.
**Found:** 2026-05-04. **Closed:** 2026-05-22.

---

## F-3 — `cmd/probe/main.go` is still a stub *(fixed 2026-05-04)*

**Problem.** The probe binary read env vars and exited 0; netlink+TCP
probing and node-label patching were missing. E2E-03 and E2E-06 were
Skipped because the binary couldn't produce real labels.

**Fix.** Full implementation landed across:
- `cmd/probe/main.go` (344 lines) — `probeRunner` struct with the env-var
  config and a `kubernetes.Interface` for testability, `runOnce()` loop
  (netlink route check → DialTimeout reachability → node-label patch),
  signal-aware `run()` loop, env-driven `runnerFromEnv()`.
- `cmd/probe/route_linux.go` / `cmd/probe/route_other.go` — platform-split
  netlink wrapper. Linux uses `vishvananda/netlink`; other GOOS uses a
  stub that always returns "no route".
- `cmd/probe/probe_test.go` — unit tests with a fake clientset.
- Label keys match `internal/reconcilers/networkprobe.go`'s
  `openchami.org/<cluster>-<probe>-network-ready` format (single `/`,
  Kubernetes-label-syntax-compliant after the bug fix recorded in
  `bugs.md`).

**Build artefact.** `make build` produces `bin/probe` alongside `bin/operator`
and `bin/ochami-admin`.

**E2E.** `test/e2e/network_test.go` no longer Skips the label-presence
assertions. (The actual exercise needs `make test-e2e` against a kind
cluster — out of scope for the binary fix.)

**Severity in retrospect.** High — without this, the operator's network-
probe DaemonSet was effectively a no-op and the cluster's node labels
were unmaintained.
**Found:** 2026-05-04. **Closed:** 2026-05-22.

---

## F-4 — E2E-10: operator-binary rebuild during e2e *(fixed 2026-05-04)*

**Problem.** `test/e2e/lifecycle_test.go::E2E-10` was Skipped with
"requires operator-binary rebuild inside e2e harness; manual step". The
test needed to simulate an operator upgrade and observe that pinned
clusters are untouched.

**Fix.**
- `hack/local-dev/rebuild-operator.sh` — rebuilds the operator with a new
  `VERSION=`, builds a docker image, `kind load docker-image`s it, runs
  `kubectl set image ...` against the in-cluster Deployment, waits for
  rollout.
- `test/e2e/lifecycle_test.go::E2E-10` — implemented the full assertion
  sequence: apply pinned cluster, apply unpinned cluster, invoke
  rebuild script, Eventually-assert that unpinned `ManagedByVersion`
  advanced and the pinned cluster's `ManagedByVersion` did NOT.
  `lifecycleSkipIfNoRebuildScript` keeps the test honest in environments
  without a kind cluster — it skips only if the helper isn't executable,
  not because the implementation is missing.

**Severity in retrospect.** Medium — the version-pinning logic was
implemented in Phase 13 and unit-tested, but E2E-10 was the only
end-to-end exercise of "operator rolls forward, pinned cluster doesn't
follow".
**Found:** 2026-05-04. **Closed:** 2026-05-22.

---

## F-5 — E2E-11: deterministic short-cert harness *(fixed 2026-05-04)*

**Problem.** `test/e2e/observability_test.go::E2E-11` was Skipped because
exercising the Phase 7 cert-expiry Warning Event path required a
deterministic short-lived TLS Secret. The unit-test path in
`internal/reconcilers/certificates_test.go` covered the parse/condition
logic; only the e2e flow was missing.

**Fix.** Cert generation happens at test runtime via Go's `crypto/x509`
(same pattern as `internal/reconcilers/certificates_test.go`):
- `observabilityShortTLSSecretYAML(notAfter time.Time)` returns the
  `kubernetes.io/tls` Secret YAML with both PEM blocks, scoped so the
  notAfter is exactly `now + 1h`.
- E2E-11 test body pre-creates the per-cluster namespace, applies the
  Secret, applies an `OpenCHAMIControlPlane` referencing it via
  `networking.tls.secretName`, then Eventually-asserts that the
  `CertificatesValid` condition reaches `reason=ExpirationImminent`.

No static fixture file — the cert would expire from sitting in Git, so
in-test generation is the right shape.

**Severity in retrospect.** Low — the cert-expiry warning logic was
unit-tested; this was the missing e2e cap-stone.
**Found:** 2026-05-04. **Closed:** 2026-05-22.

---

## F-6 — `tools/validate-invariants.sh` false positives *(fixed 2026-05-04)*

**Problem.** The script reported 5 violations in the repo, all of which
were false positives where the regex matched comment text:
- `internal/reconcilers/reconciler.go` mentions `recorder.Event` and `client.Create` in doc comments listing things sub-reconcilers must NOT do
- `internal/reconcilers/helpers.go::RecordConditionEvent` is the helper that does call `recorder.Event` — by design — and the script flagged it
- Similar comment mentions in `database.go`, `vault.go`, `openchamicontrolplane_types.go`

**Fix.** Added a `strip_comments_and_blanks` awk helper that skips `//` and
`/* */` lines, plus per-check carve-outs:
- `recorder.Event` check now excludes `internal/reconcilers/helpers.go`
  (canonical wrapper, by design).
- `Create` check tolerates `IsAlreadyExists` get-then-create idempotency
  pattern (Kubernetes Jobs cannot be SSA-applied because `spec.template`
  is immutable post-creation).
- Vault-path-isolation check tolerates the `github.com/openchami` Go
  import-path string.

Smoke check at the bottom of the script verifies the helpers.go carve-out
is still active — catches accidental edits to either the helper or the
exclusion.

**Verified post-fix.** `tools/validate-invariants.sh` reports 0 violations
and exits 0 on current main.

**Severity in retrospect.** Low — the script was advisory and ran clean
once the noise was filtered out; never blocked anything.
**Found:** 2026-05-04. **Closed:** 2026-05-22.

---

## Phase 16 acceptance — closed 2026-05-22

All six items landed in 2026-05-04's in-cluster shakedown commit series.
The followup doc was left as a punch list and not updated; the
2026-05-22 docs-audit pass closed each item in place. Verified at close:

- ✅ `go build ./...` and `go vet ./...` clean.
- ✅ `make manifests` idempotent; `config/rbac/role.yaml` regenerates byte-identical.
- ✅ `tools/validate-invariants.sh` reports 0 violations and exits 0.
- ✅ `bin/probe` builds; `cmd/probe/probe_test.go` unit-tests pass.
- ✅ Only Skip in `test/e2e/` is the conditional `lifecycleSkipIfNoRebuildScript`
  guard in `lifecycle_test.go:483` — a safety check for environments
  without the rebuild helper, not a "not implemented" gate.

Future follow-ups (none open at close):

Any item discovered after this date is logged in `bugs.md` rather than
re-opening Phase 16.
