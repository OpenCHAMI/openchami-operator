# Testing

Three layers, each at a different speed/scope tradeoff. All three should pass before merging.

| Layer | Scope | Runtime | Where |
|---|---|---|---|
| Unit tests | One sub-reconciler at a time | < 5 s | `internal/.../*_test.go` |
| Envtest | Full reconcile loop, fake API server | 30 s – 2 min | `internal/.../*_test.go` (the same files; envtest is gated on `TestMain`) |
| End-to-end | Real kind cluster + dev compose stack | 5–15 min | `test/e2e/*_test.go` |
| Integration sandbox | Cross-service flows without operator/kind | 1–2 min | [OpenCHAMI/integration-sandbox][sandbox] |

[sandbox]: https://github.com/OpenCHAMI/integration-sandbox

## Unit tests + envtest

```sh
make test               # unit + envtest
make test-verbose       # with `go test -v`
make test-cover         # with coverage; outputs cover.html
```

Behind the scenes:
- `make test` runs `go test -count=1 ./internal/... ./api/...`.
- `setup-envtest` is invoked the first time to provision Kubernetes binaries (etcd, kube-apiserver) into `testbin/`.
- The test suite uses `controller-runtime`'s envtest to spin up a faked API server per test package.

Envtest exercises the **real** reconcile loop — fake API server, real reconcilers, real client. It does not exercise:
- Webhook conversion flows (webhooks aren't installed by envtest by default).
- Real cert-manager / CNPG / VSO / Envoy Gateway controllers.
- Real Vault or S3.

For those, use e2e.

## End-to-end (e2e)

Requires the full dev stack from `make dev-up` to be running.

```sh
make dev-up            # one-shot: docker compose + kind + prereqs + Vault seed
make e2e               # 30-minute timeout
make dev-down          # clean up afterward
```

`make e2e` expands to:
```sh
go test -race -v -count=1 -timeout 30m -tags e2e ./test/e2e/...
```

The build tag `e2e` keeps these tests out of `make test`. They take real time and need real infrastructure.

### Suite layout

| File | Tests |
|---|---|
| `test/e2e/suite_test.go` | Builds the operator image, loads it into kind, conditionally installs cert-manager. |
| `test/e2e/e2e_test.go` | Operator pod running, metrics endpoint, webhook CA injection. |
| `test/e2e/lifecycle_test.go` | E2E-01 single-cluster Ready; E2E-02 dual-cluster; E2E-07 DHCP webhook rejection; E2E-08 Vault unreachable recovery; E2E-09/10 version pinning. |
| `test/e2e/network_test.go` | E2E-03 probe DaemonSet labels nodes; E2E-04 CoreDHCP waits for label; E2E-05 Magellan waits for label; E2E-06 bad-subnet warning. |
| `test/e2e/observability_test.go` | E2E-11 cert-expiry event; E2E-12 log-bucket lifecycle; E2E-13 Funicular Parquet write; E2E-14 ochami-admin backup. |

Phase 15 (`docs/phases/phase-15-e2e.md`) carries the named test IDs and the fault-injection follow-ons (F-01 through F-06).

### Fixtures

| File | Cluster spec |
|---|---|
| `test/fixtures/minimal-cluster.yaml` | `testcluster`. Probes disabled. All services enabled. 1 DB replica. AppRole auth. localhost:8200 / localstack:4566. |
| `test/fixtures/full-cluster.yaml` | Same name, 3 DB replicas, backup enabled. |
| `test/fixtures/dual-cluster.yaml` | Two clusters: `venado` and `frontier`. Used for cross-cluster isolation tests. |

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

1. `make generate manifests fmt vet lint test` on every PR.
2. `make e2e` on every PR with the `e2e` label, plus on every push to `main`.
3. The [integration sandbox][sandbox] `make ci` separately (in its own repo's CI), optionally with `SBX_*_IMAGE` overrides for the service under test.

Phase 0 (`docs/phases/phase-00-bootstrap.md`) describes the canonical CI shape.

## Local iteration loop

```sh
# Inner loop: unit + envtest
make test

# Snapshot before pushing: full pipeline
make generate manifests fmt vet lint test validate-invariants

# Big change: e2e (slow, but catches webhook + Gateway API surprises)
make dev-up
make e2e
make dev-down
```

## Common test failures

- **`controller-gen` regenerates types and tests fail with stale field references.** Run `make generate manifests` after editing CRD types; commit the regenerated files.
- **Envtest binaries not found.** Run `make install-tools` once.
- **kind cluster from a previous run.** `kind delete cluster --name openchami-dev` (which `make dev-down` does) and rerun.
- **`make e2e` times out on `Vault unreachable`.** The dev compose stack didn't come up; check `docker compose -f hack/local-dev/docker-compose.yaml ps`.
- **Webhook CA injection drift in `make e2e`.** `make manifests` was likely skipped; the kustomize patches that wire cert-manager into the webhook are stale.
