# openchami-operator

A Kubernetes operator that deploys and manages the [OpenCHAMI](https://openchami.org) HPC provisioning stack as a single, namespaced custom resource.

## Start here: check your local test environment

To provision a local development environment and watch the operator's
end-to-end tests run, start from the repository root:

```sh
./hack/run-e2e.sh
```

**This resets the `openchami-dev` Kind cluster and dev Vault/LocalStack
volumes. Any data in that dev stack is deleted.** Use a disposable local
environment, not a cluster or Vault containing data you need to keep.

For subsequent runs, keep the existing infrastructure and data:

```sh
./hack/run-e2e.sh --reuse
```

This skips teardown, infrastructure provisioning, prerequisite installation,
and Vault seeding. It still rebuilds and deploys the operator, waits for
admission readiness, and runs the tests. The existing Kind cluster, Vault,
and LocalStack must be running. Run without `--reuse` for a fresh reset.
Tests can still modify or delete retained resources.

Before running, install Bash, Make, Docker with Compose v2, Go matching
[go.mod](go.mod), kubectl, Helm, Kind, the Vault CLI, curl, and the AWS CLI.
Docker must be running and accessible to your user. The script checks tools
but does not install them. Downloads and container pulls require network
access; configure your host and Docker proxies if needed.

The script starts Kind, Vault, LocalStack, and prerequisite controllers;
builds and deploys the operator; waits for admission readiness; then streams
the E2E test output. It leaves the environment running afterward. Clean up with:

```sh
make dev-down
```

**Current status:** this is a bootstrap and debugging entrypoint, not yet a
reliable green/red environment acceptance test. Some scenarios may still fail
for independent fixture or behavior issues. A failed run does not by itself
mean your machine is configured incorrectly. Start with the first failure,
not the number of later failures or skips.

See [Testing](docs/testing.md) for prerequisites, the stages to expect,
how to interpret results, and the intended next steps. For fast unit/envtest
checks without Docker or a live cluster, run `make test` separately; the
script runs E2E tests only.

## What it does

Each `OpenCHAMIControlPlane` custom resource produces a self-contained, namespace-isolated OpenCHAMI deployment:

- **Stateful services** — SMD, tokensmith, boot-service, metadata-service, fru-tracker, power-control — backed by a CloudNativePG-managed Postgres cluster.
- **Network services** — CoreDHCP for PXE runs on nodes that pass a configurable network probe. Magellan discovery is unavailable pending end-to-end wiring; when enabled it reports `MagellanReady=False/NotConfigured`, and legacy schedules are suspended (issue #44).
- **Object storage** — boot images and Parquet logs land in an external VersityGW (or any S3-compatible endpoint).
- **Auth + secrets** — service-to-service tokens and credentials flow through an external Vault, surfaced into the namespace via the Vault Secrets Operator.
- **Gateway** — TLS-terminated, OIDC-protected access via Envoy Gateway HTTPRoute.
- **Observability** — Prometheus ServiceMonitors, structured logs, runbook-linked Events on every condition.

The operator never deploys Vault or VersityGW; both are external prerequisites. See [docs/external-dependencies.md](docs/external-dependencies.md).

## Documentation

The full documentation tree lives under [`docs/`](docs/README.md). Start with:

- **[Quickstart](docs/quickstart.md)** — local dev cluster + first reconcile in under 15 minutes.
- **[Install (production)](docs/install-production.md)** — end-to-end walkthrough for a fresh non-dev cluster.
- **[Architecture](docs/architecture.md)** — controller, sub-reconcilers, reconcile order.
- **[CRD reference](docs/crd-reference.md)** — every `OpenCHAMIControlPlane` field.
- **[SERVICES.md](SERVICES.md)** — default container versions and how to control them at build time and runtime.
- **[ochami-admin CLI](docs/cli.md)** — companion CLI: `init` / `describe` / `backup` / `restore` / `logs`.
- **[Invariants](docs/invariants.md)** — the 10 absolute rules.
- **[Troubleshooting](docs/troubleshooting.md)** — common failure modes with fixes.
- **[Contributing](docs/contributing.md)** — adding a sub-reconciler / CRD field / webhook.

## Manual development setup

For a guided environment-and-test run, use the script above. These commands
are the separate, manual path for working with a sample control plane:

```sh
# Prereqs (one-time)
make install-tools
make dev-up                       # docker-compose Vault+LocalStack, kind cluster, prereq CRDs

# Apply a test cluster
kubectl apply -f test/fixtures/minimal-controlplane.yaml
kubectl get openchamicontrolplane testcluster -w

# Run the operator locally against the dev cluster
make dev-run

# Tear down
make dev-down
```

See [docs/quickstart.md](docs/quickstart.md) for the full walkthrough and [docs/dev-loop.md](docs/dev-loop.md) for every make target.

## Development

```sh
make generate manifests fmt vet lint test
```

`make validate-invariants` and `make build` are also part of the PR gate.
Run the full no-cluster validation path before pushing:

```sh
make install-tools
make generate manifests fmt vet lint test validate-invariants build
make docker-build IMG=ghcr.io/openchami/openchami-operator:local
```

`make test` uses controller-runtime envtest and does not require a live
Kubernetes cluster. Tagged releases publish
`ghcr.io/openchami/openchami-operator:<tag>`. Same-repository PRs publish
temporary images as `ghcr.io/openchami/openchami-operator:pr-<number>` and
`ghcr.io/openchami/openchami-operator:pr-<number>-<head-sha>` without SBOM or
provenance attestations; fork PRs build and smoke-test locally without pushing.

For end-to-end testing with automatic local environment setup:
```sh
./hack/run-e2e.sh
make dev-down
```

For service-level testing without involving Kubernetes (PR builds, cross-service flows), use the [OpenCHAMI integration sandbox](https://github.com/OpenCHAMI/integration-sandbox). See [`docs/relationship-to-integration-sandbox.md`](docs/relationship-to-integration-sandbox.md) for the short version of the boundary.

## Project metadata

| Item | Value |
|---|---|
| Go module | `github.com/openchami/openchami-operator` |
| CRD group/version | `openchami.openchami.org/v1alpha1` |
| Primary resource | `OpenCHAMIControlPlane` |
| Operator image | `ghcr.io/openchami/openchami-operator` |
| Admin CLI | `ochami-admin` |
| Kubebuilder | v4 |
| Go | 1.26.3+ |
| Kubernetes target | 1.29+ |

## Project layout

```
openchami-operator/
├── api/v1alpha1/                   CRD types, webhooks, conversions
├── cmd/
│   ├── operator/                   manager binary
│   ├── ochami-admin/               CLI: init / describe / backup / restore / logs
│   └── probe/                      network-probe DaemonSet binary
├── config/                         controller-gen output: CRD, RBAC, webhook manifests
├── docs/                           full documentation tree (start here)
├── hack/local-dev/                 docker-compose, kind config, Vault seed
├── internal/
│   ├── controller/                 top-level controller + reconcile loop
│   ├── reconcilers/                one file per concern (Namespace, Vault, SMD, ...)
│   ├── conditions/                 condition Type/Reason constants
│   ├── logging/                    log enrichment helpers (invariant 8)
│   ├── status/                     post-reconcile aggregator + Prometheus metrics
│   ├── vault/                      Vault client interface + AppRole auth
│   ├── s3/                         S3 client interface
│   └── version/                    operator semver and image tag config
├── test/
│   ├── e2e/                        Ginkgo end-to-end tests
│   └── fixtures/                   minimal-controlplane.yaml, dual-controlplane.yaml, full-controlplane.yaml, production-controlplane.yaml.example
├── SERVICES.md                     pinned default service images for the current release
├── UPGRADE.md                      release-to-release upgrade notes
├── CLAUDE.md                       agent-specific guidance
├── AGENTS.md                       sub-agent collaboration rules
└── bugs.md                         known issues, severity-tagged
```

## Project rules

If you're contributing code or AI-assisted edits, [docs/invariants.md](docs/invariants.md) is the contract. The shortest version:

1. Vault and VersityGW are external. Configure them; never deploy them.
2. Per-cluster namespace isolation. Everything for cluster `foo` lives in `openchami-foo`.
3. Vault path isolation. All KV paths under `openchami/{clusterName}/`.
4. DHCP node exclusivity. No two clusters target the same node when probes are off.
5. Idempotent reconciliation. Server-side apply only.
6. Status is always written last.
7. No secrets in the CRD spec.
8. Structured logging on every line.
9. Every Event includes a runbook URL.
10. The operator has no HPC domain knowledge. (The "quadlet test" — read [docs/invariants.md](docs/invariants.md#10-the-operator-has-no-hpc-domain-knowledge).)

## License

MIT — see [`LICENSE`](LICENSE).

The repository is [REUSE](https://reuse.software/) compliant. License
metadata for every file is recorded via SPDX headers (or via
[`REUSE.toml`](REUSE.toml) for files that cannot carry a header), and the
`REUSE Compliance Check` GitHub Action verifies this on every PR. Run
`reuse lint` locally before sending changes.
