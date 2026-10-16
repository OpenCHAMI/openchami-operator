# Testing

## Start here

From the repository root, run:

```sh
./hack/run-e2e.sh
```

This is the local **setup-and-run E2E entrypoint**. It is intended to remove
the guesswork about which setup commands to run before testing. It currently
gets you to observable test results, not a guarantee that the suite passes.
Known test setup conflicts can cause cascading failures even after the
environment starts successfully. See [Reading the results](#reading-the-results).

**By default, a run deletes the existing `openchami-dev` Kind cluster and dev
Vault/LocalStack volumes.** Do not store persistent data in this stack.
The script leaves infrastructure running afterward; use `make dev-down`
when finished. The suite deploys the operator once in shared setup. Test groups
clean up their own resources and must not remove the shared operator, CRDs, or
namespace.

To rerun against the environment left by a previous run:

```sh
./hack/run-e2e.sh --reuse
```

Reuse skips `make dev-down` and `make dev-up`, including prerequisite
controller installation and Vault seeding. It imports the existing Kind
kubeconfig, checks API reachability and that the Vault/LocalStack containers
are running and unpaused, then builds test helper binaries and runs the E2E
suite. Shared suite setup deploys the operator and waits for admission. It does not validate all
dependency health or repair prerequisite controllers. Missing infrastructure
causes an error, never an automatic reset. Start the existing stack or run
without `--reuse` to reset and provision it again.

Reuse preserves existing infrastructure state, not test isolation: tests can
still change or delete resources, and leftovers can affect later runs. After
prerequisite configuration changes or when investigating state-related
failures, use a fresh run without `--reuse`.

### Prerequisites

Run from a checkout of this repository with these commands on `PATH`:

| Requirement | Why it is needed |
|---|---|
| Bash and Make | Run the script and existing setup/build targets. |
| Go matching [go.mod](../go.mod) | Build the binaries and run tests; race tests also need a working CGO/C compiler toolchain. |
| Docker with Compose v2 | Run Kind, Vault, LocalStack, and container builds. The daemon must be running and accessible without interactive privilege escalation. |
| Kind and kubectl | Create the local Kubernetes cluster and interact with its API. |
| Helm | Install prerequisite controllers. |
| Vault CLI | Seed the development Vault. |
| curl | Download prerequisite manifests and tools used by setup. |
| AWS CLI (`aws`) | Exercise S3 assertions against LocalStack. |

The script checks these commands; it does not install them. `make install-tools`
installs repository Go tooling, not this entire prerequisite list. Allow network
access for Go dependencies, Helm charts, Kubernetes manifests, and container
images. The default Kind configuration also uses host ports 8080 and 8443;
Vault and LocalStack use 8200 and 4566. Those ports must be available.

With `--reuse`, Helm, the Vault CLI, and curl are not checked because
infrastructure provisioning is skipped. Docker, Kind, kubectl, Make, Go,
and the AWS CLI are still required. Rebuilds and tests can still need network
access.

The script inherits exported proxy settings such as `HTTP_PROXY`,
`HTTPS_PROXY`, their lowercase equivalents, and `GOPROXY`. It preserves and
extends `NO_PROXY`/`no_proxy` for loopback, Kubernetes service domains, and
the dev Vault/LocalStack container names. Configure Docker daemon and
container-network proxy access separately where required; exporting host
variables alone does not guarantee container pulls or builds can reach the
network. No organization-specific proxy address is stored in the script.

### What runs

| Stage shown in the terminal | What the script does |
|---|---|
| Prerequisite checks | Verifies commands, Docker daemon access, and Compose availability. |
| Resetting (default only) | Calls `make dev-down`, deleting the dedicated dev stack. |
| Starting (default only) | Calls `make dev-up`: Vault, LocalStack, Kind, cert-manager, CloudNativePG, Vault Secrets Operator, Envoy Gateway, operator CRDs, and Vault seed data. |
| Reusing (`--reuse` only) | Exports the existing Kind kubeconfig and checks the API and dev containers without provisioning infrastructure. |
| Building helper binaries | Calls `make build` for operator/admin binaries used by the tests. |
| E2E shared setup | The suite calls `make dev-deploy` once and waits up to 120 seconds for admission using a server-side dry-run. It does not create the sample control plane. |
| Running the E2E suite | Runs race-enabled Go tests with verbose Ginkgo output and a 30-minute test timeout. Infrastructure setup time is outside that timeout. |

The script uses a temporary kubeconfig, which it removes on exit, rather than
changing your default context. To inspect the retained cluster afterward:

```sh
kind export kubeconfig --name openchami-dev
kubectl --context kind-openchami-dev get pods -A
```

Exporting that kubeconfig explicitly updates your local kubeconfig. The dev
stack uses local-only credentials (`dev-root-token` for Vault and `test` for
AWS access/secret keys); these are not production credentials.

### Reading the results

- If a prerequisite or setup stage fails, tests have not run. Start with that stage's error: missing command, Docker access, download, image pull, or controller readiness.
- If shared setup completes and Ginkgo starts the specs, the operator and admission webhook are available. That does not prove all service dependencies or test fixtures are correct.
- `Will run 20 of 20 specs` is the planned spec count, not a statement that every body executed. Ordered groups skip later specs after an earlier failure.
- Diagnose the **first failure** before later errors. Shared setup owns the operator deployment and CRDs; test groups own only their scenario resources. Later API or resource errors may be consequences of an earlier failed spec or its cleanup, not separate operator bugs.
- A zero exit status means the invoked suite succeeded; inspect skips and optional assertions before treating it as complete coverage. A nonzero status means setup or tests failed, not necessarily that the host environment is broken.

On failure, the script prints available pods, control planes, recent events,
and operator logs. Diagnostics may be incomplete if test cleanup already
deleted those resources. Output goes to the terminal; the script does not
create a reports directory, JUnit files, or persistent snapshots. To retain
the terminal output in Bash while preserving the script's failure status:

```bash
set -o pipefail
./hack/run-e2e.sh 2>&1 | tee /tmp/openchami-e2e.log
```

### Current scope and next steps

Today the script provides reproducible **setup steps and a visible test
invocation**, not fully reproducible test outcomes: downloaded dependencies
are not all pinned, and individual scenarios may still fail. It runs
E2E tests only, not unit/envtest, lint, or the separate integration sandbox.
Its supported options are `--reuse` and `--help`. There is no test-selection
option, automatic teardown mode, or artifact-upload support.

The intended progression is to fix shared test setup/teardown first, then
investigate individual scenario failures. Once the suite is reliable, this
entrypoint can become an environment acceptance check with useful pass/fail
meaning and eventually the basis for CI. Selective reruns, dependency pinning,
and saved diagnostics are possible follow-ups, not current features.

## Choose a test layer

Use the script above for a real local cluster. Use `make test` for faster
code-level checks without Docker or a live Kubernetes cluster. These layers
serve different purposes; the E2E runner is not a replacement for unit tests.

| Layer | Scope | Runtime | Where |
|---|---|---|---|
| Unit tests | One sub-reconciler at a time | < 5 s | `internal/.../*_test.go` |
| Envtest | Full reconcile loop, fake API server | 30 s – 2 min | `internal/.../*_test.go` (the same files; envtest is gated on `TestMain`) |
| End-to-end | Real kind cluster + dev compose stack | Up to 30 min for tests, plus setup | `test/e2e/*_test.go` |
| Integration sandbox | Cross-service flows without operator/kind | 1–2 min | [OpenCHAMI/integration-sandbox][sandbox] |

[sandbox]: https://github.com/OpenCHAMI/integration-sandbox

## Unit tests + envtest

```sh
make test               # unit + envtest
make test-verbose       # with `go test -v`
make test-cover         # with coverage; outputs cover.html
```

Behind the scenes:
- `make test` runs `go test -race -count=1 -timeout 5m ./internal/... ./api/... ./cmd/...` with envtest assets configured.
- `setup-envtest` is invoked the first time to provision Kubernetes 1.35.x binaries (etcd, kube-apiserver) into `testbin/`.
- The test suite uses `controller-runtime`'s envtest to spin up a faked API server per test package.

Envtest exercises the **real** reconcile loop — fake API server, real reconcilers, real client. It does not exercise:
- Webhook conversion flows (webhooks aren't installed by envtest by default).
- Real cert-manager / CNPG / VSO / Envoy Gateway controllers.
- Real Vault or S3.

For those, use e2e.

## End-to-end (e2e)

Use [the setup-and-run workflow above](#start-here) for your first run.
`make e2e` is a lower-level test invocation against an already provisioned
cluster; it does not perform the runner's reset, prerequisite checks, or
admission wait. Both currently use a 30-minute Go test timeout. The `e2e`
build tag excludes these tests from `make test`.

### Current coverage limits

Successful provisioning is not a claim that all scenarios pass. In particular:

- E2E-08 pauses the dedicated `openchami-vault-dev` container. The runner sets both the Vault endpoint and container name so this test is not skipped by the custom-endpoint safety guard. Never aim it at shared Vault infrastructure.
- E2E-13 checks collector readiness by default; Parquet presence is only asserted with `E2E_PARQUET_PRESENCE=1`. The default collector image is documented as unpublished in the minimal fixture, which can also block logging-enabled Ready tests.
- E2E-14 checks local backup output and a CNPG Backup resource. S3 object presence is opt-in with `E2E_BACKUP_PRESENCE=1`; the CLI currently prints manual upload instructions rather than uploading those artifacts itself.
- E2E-10 skips when its rebuild helper is unavailable. Consult the terminal summary for actual skips, failures, and attempted specs.
- Full Ready tests also depend on fixture selectors, available service images, and service configuration. Keep these failures visible and diagnose them from condition status rather than weakening Ready assertions.

### Probe egress policy verification (issue #42)

The default kindnet setup does not enforce NetworkPolicy. Passing the usual
kind tests therefore does not demonstrate that probe egress policies work.
Use a disposable cluster with an enforcing CNI for connectivity verification:

1. Configure reachable provision and BMC TCP validation endpoints on ports
   other than 443 (e.g. 22 and 623), with subnets attached to eligible hosts.
2. Verify `networkprobe-policy` contains those ports and a separate rule
   restricted to the discovered API endpoint IPs and listening ports (e.g.
   6443), while DNS remains allowed by `allow-dns-egress`.
3. Confirm probe logs have no dial or node-patch failures and eligible nodes
   receive readiness=true plus the current probe revision labels.
4. Change a validation port, then remove TCP validation. Verify reconciliation
   removes obsolete port allowances while preserving API access.

The probe uses `hostNetwork: true`, whose NetworkPolicy enforcement varies by
CNI. Establish whether the CNI applies these policies to host-network pods.
Where it does not, verify the generated rules with an ordinary pod carrying
the network-probe label in the control-plane namespace: configured validation
ports and API access should succeed, while an unrelated listening TCP port
should be blocked. Test the real host-network probe separately and account
for any host firewall or CNI host policies. A successful host-network probe
alone does not prove ordinary NetworkPolicy enforcement.

### Suite layout

| File | Tests |
|---|---|
| `test/e2e/suite_test.go` | Builds and loads a test operator image, checks for cert-manager and installs it if absent. Shared deployment ownership is not yet consolidated. |
| `test/e2e/e2e_test.go` | Operator pod running, metrics endpoint, webhook CA injection. |
| `test/e2e/lifecycle_test.go` | E2E-01 single-cluster Ready; E2E-02 dual-cluster; E2E-07 DHCP webhook rejection; E2E-08 Vault unreachable recovery; E2E-09/10 version pinning. |
| `test/e2e/network_test.go` | E2E-03 probe DaemonSet labels nodes; E2E-04 CoreDHCP waits for label; E2E-05 Magellan waits for label; E2E-06 bad-subnet warning. |
| `test/e2e/observability_test.go` | E2E-11 cert-expiry event; E2E-12 log-bucket lifecycle; E2E-13 Funicular Parquet write; E2E-14 ochami-admin backup. |

Phase 15 (`docs/phases/phase-15-e2e.md`) carries the named test IDs and the fault-injection follow-ons (F-01 through F-06).

### Fixtures

| File | Cluster spec |
|---|---|
| `test/fixtures/minimal-controlplane.yaml` | `testcluster`. Probes disabled. All services enabled. 1 DB replica. AppRole auth. Container-hostname Vault + LocalStack. |
| `test/fixtures/full-controlplane.yaml` | `venado-prod`. Network probing enabled. 3 DB replicas. Backup enabled. ServiceMonitor enabled. |
| `test/fixtures/dual-controlplane.yaml` | Two clusters (`venado` and `frontier`) for cross-cluster isolation tests. |
| `test/fixtures/production-controlplane.yaml.example` | Heavily-annotated production-shaped example. Not picked up by tests; intended to be copied and edited. |

## Integration sandbox

The [integration sandbox][sandbox] is a separate repository. It tests
the OpenCHAMI services themselves (SMD, tokensmith, boot-service, …) via
docker compose, without involving Kubernetes or the operator at all.

Use it when:
- You're chasing a service-level bug and don't want kind in the loop.
- You're testing a service PR build with `SBX_<NAME>_IMAGE=ghcr.io/openchami/<svc>:pr-<N>`.
- You're validating a cross-service flow (UC1/UC2/UC3) without the operator.

See the sandbox README and the canonical boundary doc at
[`integration-sandbox/docs/relationship-to-operator.md`][sandbox-rel] —
linked from [relationship-to-integration-sandbox](relationship-to-integration-sandbox.md).

[sandbox-rel]: https://github.com/OpenCHAMI/integration-sandbox/blob/main/docs/relationship-to-operator.md

## What to write where

| Change | Test it in |
|---|---|
| New reconciler logic | unit + envtest |
| New CRD field | unit (validation) + envtest (defaulting + reconcile flow) |
| New webhook check | unit (`api/v1alpha1/*_webhook_test.go`) + e2e (kubectl apply round-trip) |
| New condition / reason | unit + e2e (assert the condition appears in `kubectl get`) |
| New default service image | unit (constant) + e2e (deployment uses it) + service-side smoke in integration sandbox |
| Cross-service interaction | integration sandbox (the operator e2e is the wrong layer) |
| Disaster recovery | e2e fault-injection (Phase 15 F-* tests) |

## CI

`.github/workflows/` (your distribution's wiring) should run:

1. `go mod tidy`, `make generate manifests fmt`, and `git diff --exit-code` on every PR.
2. `make vet validate-invariants build test` on every PR.
3. A Docker build and image smoke test on every PR. Same-repository PRs push `ghcr.io/openchami/openchami-operator:pr-<number>` and `:pr-<number>-<head-sha>` without SBOM or provenance attestations; fork PRs do not push.
4. E2E testing on PRs with the `e2e` label, pushes to `main`, and manual workflow dispatch. See the current wiring caveat below.
5. The [integration sandbox][sandbox] `make ci` separately (in its own repo's CI), optionally with `SBX_*_IMAGE` overrides for the service under test.

Phase 0 (`docs/phases/phase-00-bootstrap.md`) describes the canonical CI shape.

The checked-in [E2E workflow](../.github/workflows/test-e2e.yml) currently uses
`make dev-up`, `make e2e`, and an unconditional `make dev-down` cleanup step.
It does not call the local runner. Reusing the runner in CI, and providing
saved reports there, are follow-up work rather than current script behavior.

## Local iteration loop

```sh
# Inner loop: unit + envtest
make test

# Snapshot before pushing: full pipeline
make generate manifests fmt vet lint test validate-invariants build
make docker-build IMG=ghcr.io/openchami/openchami-operator:local

# Big change: e2e (slow, but catches webhook + Gateway API surprises)
./hack/run-e2e.sh
make dev-down
```

## Common test failures

- **`controller-gen` regenerates types and tests fail with stale field references.** Run `make generate manifests` after editing CRD types; commit the regenerated files.
- **Envtest binaries not found.** Run `make install-tools` once.
- **kind cluster from a previous run.** `kind delete cluster --name openchami-dev` (which `make dev-down` does) and rerun.
- **`make e2e` times out on `Vault unreachable`.** The dev compose stack didn't come up; check `docker compose -f hack/local-dev/docker-compose.yaml ps`.
- **Webhook CA injection drift in `make e2e`.** `make manifests` was likely skipped; the kustomize patches that wire cert-manager into the webhook are stale.
