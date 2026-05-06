# openchami-operator

A Kubernetes operator that deploys and manages the [OpenCHAMI](https://openchami.org) HPC provisioning stack as a single, namespaced custom resource.

## What it does

Each `OpenCHAMICluster` custom resource produces a self-contained, namespace-isolated OpenCHAMI deployment:

- **Stateful services** — SMD, tokensmith, boot-service, metadata-service, fru-tracker, power-control — backed by a CloudNativePG-managed Postgres cluster.
- **Network services** — CoreDHCP for PXE, Magellan for BMC discovery — pinned to nodes that pass a configurable network probe.
- **Object storage** — boot images and Parquet logs land in an external VersityGW (or any S3-compatible endpoint).
- **Auth + secrets** — service-to-service tokens and credentials flow through an external Vault, surfaced into the namespace via the Vault Secrets Operator.
- **Gateway** — TLS-terminated, OIDC-protected access via Envoy Gateway HTTPRoute.
- **Observability** — Prometheus ServiceMonitors, structured logs, runbook-linked Events on every condition.

The operator never deploys Vault or VersityGW; both are external prerequisites. See [docs/external-dependencies.md](docs/external-dependencies.md).

## Documentation

The full documentation tree lives under [`docs/`](docs/README.md). Start with:

- **[Quickstart](docs/quickstart.md)** — local dev cluster + first reconcile in under 15 minutes.
- **[Architecture](docs/architecture.md)** — controller, sub-reconcilers, reconcile order.
- **[CRD reference](docs/crd-reference.md)** — every `OpenCHAMICluster` field.
- **[Invariants](docs/invariants.md)** — the 10 absolute rules.
- **[Troubleshooting](docs/troubleshooting.md)** — common failure modes with fixes.
- **[Contributing](docs/contributing.md)** — adding a sub-reconciler / CRD field / webhook.

## Quick start

```sh
# Prereqs (one-time)
make install-tools
make dev-up                       # docker-compose Vault+LocalStack, kind cluster, prereq CRDs

# Apply a test cluster
kubectl apply -f test/fixtures/minimal-cluster.yaml
kubectl get openchamicluster testcluster -w

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

All four must pass before pushing. `make validate-invariants` runs in CI to catch the most common invariant violations.

For end-to-end testing against a real kind cluster:
```sh
make dev-up
make e2e
make dev-down
```

For service-level testing without involving Kubernetes (PR builds, cross-service flows), use the [OpenCHAMI integration sandbox](https://github.com/OpenCHAMI/integration-sandbox). See [`docs/relationship-to-integration-sandbox.md`](docs/relationship-to-integration-sandbox.md) for the short version of the boundary.

## Project metadata

| Item | Value |
|---|---|
| Go module | `github.com/openchami/openchami-operator` |
| CRD group/version | `openchami.openchami.org/v1alpha1` |
| Primary resource | `OpenCHAMICluster` |
| Operator image | `ghcr.io/openchami/openchami-operator` |
| Admin CLI | `ochami-admin` |
| Kubebuilder | v4 |
| Go | 1.24.6+ |
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
│   └── fixtures/                   minimal-cluster.yaml, dual-cluster.yaml, full-cluster.yaml
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
