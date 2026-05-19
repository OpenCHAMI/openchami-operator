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

## F-1 — RBAC manifests miss controller-file markers

**Problem.** `make manifests` runs `controller-gen` against `paths="./api/..."`
only, so kubebuilder `+kubebuilder:rbac:` markers added on
`internal/controller/openchamicontrolplane_controller.go` (Phases 7, 8, 11, 12)
never make it into `config/rbac/role.yaml`. Production deploys built from
this Makefile would be missing permissions for: gateway-api Gateways/HTTPRoutes,
envoy-gateway SecurityPolicies/BackendTrafficPolicies, cert-manager
Certificates, NetworkPolicies, and monitoring.coreos.com ServiceMonitors.

**Files**
- `Makefile` — the `manifests` target
- (verify) `config/rbac/role.yaml` after the change

**Steps**
1. Find the `manifests:` recipe in `Makefile`. Change `paths="./api/..."` to
   `paths="./api/...;./internal/controller/..."` (semicolon-separated, the
   form controller-gen expects). If the existing form uses a single
   `paths=` argument, splitting into two `paths=` flags also works.
2. Run `make manifests`. Diff `config/rbac/role.yaml` — expect new rules for:
   - `gateway.networking.k8s.io` (gateways, httproutes)
   - `gateway.envoyproxy.io` (securitypolicies, backendtrafficpolicies)
   - `cert-manager.io` (certificates)
   - `networking.k8s.io` (networkpolicies)
   - `monitoring.coreos.com` (servicemonitors)
3. Confirm pre-existing rules are unchanged (no churn from re-ordering).
4. Commit the Makefile + regenerated role.yaml.

**Acceptance.** `make manifests` is idempotent; the regenerated role.yaml
contains all five new resource groups; `make generate manifests fmt vet
lint test` clean.

---

## F-2 — `Describe()` must not contact remote services

**Problem.** `internal/reconcilers/networkpolicies.go::buildPolicies` calls
`VaultEgressPeer` and `VersityGWEgressPeer` from helpers.go, which both
`net.LookupHost` whenever the configured Vault/VersityGW hostname is not
under `.svc.cluster.local`. `Describe()` calls into `buildPolicies` directly,
so `ochami-admin describe` against a CR with an external Vault address
will perform live DNS — violating the SubReconciler contract:
"Describe must not contact any external service" (`internal/reconcilers/reconciler.go`).

**Files**
- `internal/reconcilers/helpers.go` — `VaultEgressPeer`, `VersityGWEgressPeer`
- `internal/reconcilers/networkpolicies.go` — `buildPolicies`, `Describe`
- `internal/reconcilers/networkpolicies_test.go` — extend coverage

**Approach** (pick one):
- **Option A — split the helpers.** Add `VaultEgressPeerSyntax(addr) (Peer, error)`
  and `VersityGWEgressPeerSyntax(endpoint) (Peer, error)` that return a
  conservative peer (e.g. an `ipBlock: 0.0.0.0/0` or just nil meaning
  "skip") when the host needs DNS. `Describe()` calls the syntax-only form;
  `Reconcile()` keeps calling the resolving form.
- **Option B — add a `dryRun` parameter.** `VaultEgressPeer(addr, dryRun bool)`.
  When `dryRun=true`, return a placeholder peer instead of calling LookupHost.
  Less surface area but makes the function uglier.

Recommend **Option A** — clearer separation, no parameter to forget.

**Steps**
1. Add the two `*Syntax` helpers in `helpers.go`. They share the URL parsing
   path with the existing helpers; factor out a `parseEgressHost(addr) (host
   string, err error)` if it cleans things up.
2. In `networkpolicies.go::buildPolicies`, accept a `resolveDNS bool`
   parameter. When false, use the `*Syntax` helpers. Keep the existing
   resolving path for `Reconcile()`.
3. `Reconcile()` calls `buildPolicies(cluster, true)`. `Describe()` calls
   `buildPolicies(cluster, false)`.
4. Update the misleading comment block above `Describe()` (lines ~138-144 in
   the current file) to reflect that DNS no longer happens.
5. New test cases:
   - `TestNetworkPoliciesReconciler_DescribeNoDNS_ExternalVault` — set
     `Spec.Platform.Vault.Address` to a hostname known not to resolve
     (e.g. `https://test.invalid:8200`), call `Describe`, assert no error
     and the resulting allow-vault-egress policy has the syntax-only peer
     form. Wrap the test with a `t.Setenv("...", "...")` if needed to
     ensure the host can't resolve via system resolver.
   - Same for VersityGW.

**Acceptance.** `Describe()` against a CR with externally-resolved Vault and
VersityGW addresses succeeds without making any DNS calls (verify via
`netgo`-built test or by setting an unresolvable hostname). All existing
NetworkPolicies tests still pass. `tools/check-phase.sh 8` still 4/4.

---

## F-3 — `cmd/probe/main.go` is still a stub

**Problem.** The probe binary at `cmd/probe/main.go` reads env vars and
exits 0. The actual netlink+TCP probing and node-label patching are missing.
Phase 6 spec (`docs/phases/phase-06-network.md`) and the TODO comment at
the top of probe/main.go define the contract:

1. Read `PROBE_*` env vars (CLUSTER_NAME, NODE_NAME, PROBE_TYPE, SUBNET, VALIDATE_HOST, VALIDATE_PORT, INTERVAL_SECONDS).
2. Loop:
   a. `netlink.RouteGet(target IP)` — confirm a route exists to the configured subnet.
   b. If `VALIDATE_HOST` is set: `net.DialTimeout("tcp", host:port, timeout)`.
   c. Patch this node's labels:
      - `openchami.org/{cluster}/provision-network-ready=true|false`, and/or
      - `openchami.org/{cluster}/bmc-network-ready=true|false`
        (depending on PROBE_TYPE).
   d. Sleep `INTERVAL_SECONDS`, repeat.

**Files**
- `cmd/probe/main.go` — implement the loop
- `cmd/probe/probe_test.go` (new) — unit tests for the probe-decision logic
- `internal/reconcilers/networkprobe.go` — verify the env var names this binary expects match what the reconciler injects (read both, fix whichever drifted)
- `test/e2e/network_test.go` — un-Skip the E2E-03 label-presence assertion and the E2E-06 misconfigured-subnet assertion once the binary is real

**Steps**
1. Add netlink dep: `go get github.com/vishvananda/netlink@latest && go mod tidy`. Pin the version in go.mod.
2. Add k8s in-cluster client setup:
   - `rest.InClusterConfig()`
   - `kubernetes.NewForConfig(cfg)`
   - Use `corev1client.Nodes().Patch(ctx, NODE_NAME, types.StrategicMergePatchType, ...)` with the JSON patch shape that sets the two labels.
3. Implement a `probeRunner` struct with the env vars as fields, a `kubernetes.Interface` for testability, and a `now func() time.Time` clock for tests.
4. `runOnce()` does steps 2a/2b/2c above. `run()` is the loop. `main()` wires env → runner → run().
5. Tests cover:
   - `runOnce` with VALIDATE_HOST set and reachable → label=true.
   - `runOnce` with VALIDATE_HOST unreachable → label=false (no error).
   - `runOnce` with no route → label=false.
   - PROBE_TYPE=provision sets only the provision label; PROBE_TYPE=bmc sets only the bmc label.
   - Patch is StrategicMergePatch with only the relevant key (doesn't blow away other labels).
6. Build with `make build` — confirm `bin/probe` works.
7. Update `test/e2e/network_test.go`:
   - Remove the Skip on E2E-03's label-presence assertion. Keep a wider polling timeout (~3× intervalSeconds + buffer).
   - Remove the Skip on E2E-06. The misconfigured-subnet path now produces a real label=false reply, so the reconciler's `NoEligibleNodes` condition path activates.
8. Verify `tools/check-phase.sh 6` and `tools/check-phase.sh 15` still pass.

**Acceptance.** `make test` clean (probe unit tests pass). `make build`
produces `bin/probe`. `tools/check-phase.sh 6` and `15` still pass. E2E-03
and E2E-06 are no longer skipped (note: they need `make test-e2e` against
a kind cluster to actually exercise — out of scope to fix the kind setup
here, just remove the Skip).

---

## F-4 — E2E-10: operator-binary rebuild during e2e

**Problem.** `test/e2e/lifecycle_test.go::E2E-10` is `Skip`ped with
"requires operator-binary rebuild inside e2e harness; manual step". The
test simulates an operator upgrade by changing the `version.Version`
ldflag and observing that pinned clusters are untouched.

**Files**
- `test/e2e/lifecycle_test.go` — the Skipped E2E-10 spec
- `Makefile` — may need a `test-e2e-upgrade` target
- `hack/local-dev/` — may need a helper script to rebuild + redeploy the operator inside a kind cluster

**Approach**
1. Build a helper `hack/local-dev/rebuild-operator.sh <new-version>` that:
   a. Re-runs `make build` with `VERSION=<new-version>`.
   b. Builds an operator image (`make docker-build IMG=...`) tagged with the new version.
   c. `kind load docker-image ...` into the dev kind cluster.
   d. `kubectl set image deployment/openchami-operator-controller-manager manager=...` and waits for rollout.
2. Un-Skip E2E-10. Test body:
   a. Apply a `pinned` cluster at version `0.1.0` (PinnedVersion=0.1.0). Wait for it to settle (ReconcileActive=False/VersionPinned, current operator is "dev").
   b. Apply an unpinned cluster `frontier`. Wait for it to reach Ready.
   c. Shell out to `hack/local-dev/rebuild-operator.sh 0.2.0`.
   d. Wait for the new operator to be up.
   e. Assert: pinned cluster's `Status.ManagedByVersion` is unchanged
      (still "dev" or whatever it was, NOT "0.2.0"); ReconcileActive=False.
   f. Assert: unpinned `frontier`'s `Status.ManagedByVersion == "0.2.0"`.
3. The whole test only runs against `make test-e2e` — it stays gated under
   the `e2e` build tag.

**Acceptance.** `tools/check-phase.sh 15` still passes. `make test-e2e`
exercises E2E-10 end-to-end against a kind cluster (manual verification —
CI would need a kind cluster to do this, which is a separate scope).

---

## F-5 — E2E-11: deterministic short-cert harness

**Problem.** `test/e2e/observability_test.go::E2E-11` is `Skip`ped because
exercising the Phase 7 cert-expiry Warning Event path (cert <48h remaining)
requires a deterministic short-lived TLS Secret in the e2e environment.
The unit-test path in `internal/reconcilers/certificates_test.go` already
covers the parse/condition logic; this is just about the e2e flow.

**Files**
- `test/e2e/observability_test.go` — the Skipped E2E-11 spec
- `test/fixtures/short-tls-secret.yaml` (new) — a hand-crafted, deterministic Secret
- `hack/local-dev/gen-short-cert.sh` (new, optional) — script to regenerate when it expires

**Approach**
1. Generate a self-signed cert/key pair with `notAfter = now + 1h`. Hard-code
   it as a Kubernetes Secret manifest in a fixture file. Include a comment
   with the regeneration command (`openssl req -x509 -newkey ec ...`) so
   the next maintainer can regenerate after the cert expires for real.
   - **Caveat**: if checked-in cert needs to look "1h from when the test
     runs," that's not possible with a static fixture. Solution: have the
     test script generate the cert at test start using `openssl` shell-out
     OR `crypto/x509` in Go — same way `internal/reconcilers/certificates_test.go`
     does it. The latter is cleaner; copy that helper into the e2e file.
2. Un-Skip E2E-11. Test body:
   a. Generate a self-signed cert with `notAfter = now + 1h` (well under 48h).
   b. `kubectl apply -f -` a Secret named `<cluster>-gateway-tls` in `openchami-<cluster>` containing the cert as `tls.crt`/`tls.key`.
   c. Apply an OpenCHAMIControlPlane CR referencing that secretName via `networking.tls.secretName`.
   d. The Phase 7 reconciler will see the existing secret, parse expiry, and (because <48h) emit a Warning Event with reason `ExpirationImminent`.
   e. Poll: `kubectl get events -n openchami-<cluster> --field-selector reason=ExpirationImminent` returns at least one Event of type Warning. Also assert `cluster.Status.CertExpiryTime` is set to a time within the next 1h ± skew.
3. Cleanup: delete the cluster, secret, fixtures.

**Acceptance.** `tools/check-phase.sh 15` still passes. `make test-e2e`
exercises E2E-11 against a kind cluster.

---

## F-6 — `tools/validate-invariants.sh` false positives

**Problem.** The script reports 5 violations in this repo, all of which are
false positives where the regex matches comment text:
- `internal/reconcilers/reconciler.go` mentions `recorder.Event` and `client.Create` in doc comments listing things sub-reconcilers must NOT do
- `internal/reconcilers/helpers.go::RecordConditionEvent` is the helper that
  does call `recorder.Event` — by design — and the script flags it
- Similar comment mentions in `database.go`, `vault.go`, `openchamicontrolplane_types.go`

**Files**
- `tools/validate-invariants.sh` — tighten the regex

**Approach**
1. Read the script. Each grep should:
   - Skip lines starting with `//` (Go single-line comments)
   - Skip lines inside `/* ... */` blocks (rarer; can grep for the start/end and use awk if needed)
   - For `recorder.Event`: also skip the helper file `internal/reconcilers/helpers.go` (it's the canonical wrapper, by design)
2. Re-run; expect 0 violations or only true positives (none should exist
   today; the previous "5 violations" count came entirely from comment matches).
3. Add a test fixture or a quick smoke check at the bottom of the script:
   `if grep -q 'recorder.Event' internal/reconcilers/helpers.go && [ <violation count> -eq 0 ]; then echo "OK: helpers.go correctly excluded"; fi`.

**Acceptance.** `tools/validate-invariants.sh` reports 0 violations on the
current main. CI (when added) can run this script as a gate without
suppression.

---

## Phase 16 acceptance

When all six items are landed:
- `make generate manifests fmt vet lint test` clean
- `tools/check-phase.sh 0` through `15` all pass
- `tools/validate-invariants.sh` reports 0 violations
- `bin/probe` is a real binary that reports node reachability
- `test/e2e/lifecycle_test.go`, `network_test.go`, `observability_test.go`
  contain no `Skip()` calls related to F-3/F-4/F-5

Commit each F-N as its own commit so the history reflects independent
changes.
