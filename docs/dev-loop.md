# Dev loop

Every make target, what it does, and when to run it.

## Building

| Target | What it does |
|---|---|
| `make build` | Builds three binaries into `bin/`: `manager` (the operator), `ochami-admin` (CLI), `probe` (network-probe DaemonSet entrypoint). |
| `make docker-build` | Builds the operator container image. **Note:** [bugs.md](../bugs.md) flags this as broken on `main` — Dockerfile points at `cmd/main.go` instead of `cmd/operator/main.go`. |
| `make generate` | Runs `controller-gen` to (re)generate `zz_generated.deepcopy.go`. |
| `make manifests` | Runs `controller-gen` to (re)generate the CRD, RBAC, and webhook manifests under `config/`. |

After editing any CRD type or any `+kubebuilder` marker, run `make generate manifests` and commit the regenerated files. CI rejects PRs with stale generated code.

## Code quality

| Target | What it does |
|---|---|
| `make fmt` | `goimports -w .` |
| `make vet` | `go vet ./...` |
| `make lint` | `golangci-lint run` |
| `make validate-invariants` | Greps the source for the most common invariant violations: direct `recorder.Event`, direct `log.FromContext`, `client.Create` followed by `client.Update`, runbook reasons missing slugs, etc. |

`make validate-invariants` is intentionally simple — it's a fast pre-merge check, not a static analyzer. Read it as the "obvious bugs" filter; use code review for the subtler ones.

## Tests

See [testing.md](testing.md) for the full layout. The dev-loop summary:

| Target | When to run |
|---|---|
| `make test` | Inner loop. Every save. Unit + envtest. |
| `make test-verbose` | When a test fails and you need to see what was logged. |
| `make test-cover` | Before a release; outputs `cover.html`. |
| `make e2e` | Before merging anything that touches the reconcile graph or webhooks. Requires `make dev-up`. |

## Tools

| Target | What it does |
|---|---|
| `make install-tools` | Installs `controller-gen` and `setup-envtest` into `$GOPATH/bin`. Idempotent. |
| `make controller-gen` | Sub-target that ensures controller-gen exists; called by `generate` and `manifests`. |
| `make envtest` | Sub-target that ensures setup-envtest exists; called by `test`. |

## Local development cluster

The `dev-*` family stands up a kind cluster with all the prerequisite CRDs and the OpenCHAMI CRD preinstalled. The operator itself is **not** started by `dev-up` — pick `dev-run` (off-cluster) or `dev-deploy` (in-cluster).

| Target | What it does |
|---|---|
| `make dev-up` | One-shot: docker compose (Vault + LocalStack) + kind cluster `openchami-dev` + prereq charts + `make install` (OpenCHAMI CRDs) + Vault seed. |
| `make dev-down` | Tears down kind + docker compose. |
| `make dev-install-prereqs` | Install cert-manager, CNPG, VSO, Envoy Gateway. Called by `dev-up`. |
| `make install` | Apply just the OpenCHAMI CRDs (`config/crd`) into the current `kubectl` context. Called by `dev-up` and `dev-run`. Idempotent. |
| `make uninstall` | Remove the OpenCHAMI CRDs. |
| `make deploy` | Build the full kustomize tree (`config/default`: CRDs + RBAC + manager Deployment + webhook) and apply it to the current context. Override the operator image with `IMG=ghcr.io/openchami/openchami-operator:v1.2.3`. |
| `make undeploy` | Remove what `deploy` installed. |
| `make dev-deploy` | `docker-build` → `kind load docker-image` → `make deploy IMG=controller:latest`. Use this to see the operator running as a real `Pod` against the dev kind cluster. |
| `make dev-run` | Build the operator and run it locally against the dev kind cluster (`KUBECONFIG=~/.kube/config`). Calls `make install` first so the CRDs are present. Fastest iteration loop. |
| `make dev-run-dry` | Same as `dev-run` but with `OPENCHAMI_DRY_RUN=true` — the operator logs what it would apply without touching the API server. Useful for testing fixture changes without cluster mutation. |

The dev cluster brings up Vault on `127.0.0.1:8200` (root token `dev-root-token`) and LocalStack on `127.0.0.1:4566` via docker compose, then `make dev-up` attaches both compose containers to the `kind` docker network so pods inside the cluster can resolve them by container hostname (`openchami-vault-dev:8200`, `openchami-localstack-dev:4566`). The fixtures under `test/fixtures/` use the container-hostname form. From the host you can still reach Vault and LocalStack on `127.0.0.1` for `vault read`, `awslocal`, etc.

### Apply a test cluster

```sh
kubectl apply -f test/fixtures/minimal-cluster.yaml
kubectl get openchamicontrolplane testcluster -w
```

### Reset

`make dev-down && make dev-up` is the canonical reset. The local-dev compose stack and the kind cluster are both ephemeral; nothing under `hack/local-dev/` should accumulate state across runs.

## Storage migration helpers

| Target | When to run |
|---|---|
| `make migrate-storage-version` | After installing a new operator that ships a new CRD storage version. No-ops every `OpenCHAMIControlPlane` so the API server re-encodes them. Safe to re-run. See [upgrade-and-versioning](upgrade-and-versioning.md). |

## Phase tracking

| Target | What it does |
|---|---|
| `make check-phase PHASE=N` | Validates that the implementation matches the assertions in `docs/phases/phase-NN-*.md`. Exits non-zero on the first miss. |

`check-phase` is mostly a historical artifact at this point — the operator is past the structured-phase implementation stage. It's still useful when introducing a major new sub-system (e.g. adding a new sub-reconciler tier) and you want a sanity check.

## Help

```sh
make help
```

Auto-generated from the `## ` comments on each target. The list above is the same set in tabular form.

## Common one-shots

```sh
# Watching reconciliation for a specific cluster
kubectl get openchamicontrolplane -A -w

# Inspecting a cluster's status without all the spec noise
kubectl get openchamicontrolplane <name> -o jsonpath='{.status}' | jq

# Triggering a re-reconcile (no-op patch)
kubectl patch openchamicontrolplane <name> --type=merge -p '{}'

# Dumping every operator-managed resource for a cluster
kubectl get all,networkpolicy,httproute,certificate,vaultstaticsecret \
  -n openchami-<name> -o yaml > /tmp/dump.yaml

# Quick OpenAPI sanity for a CRD update
kubectl explain openchamicontrolplane.spec.services.coreDHCP

# Local manager log for one cluster
make dev-run 2>&1 | grep cluster=foo
```

## Don't

- Don't add make targets that mutate `~/.kube/config` for clusters other than `openchami-dev`. The dev loop targets must be a no-op against unrelated clusters.
- Don't add make targets that require sudo. The build tooling has to work in unprivileged CI runners.
- Don't skip `make manifests` after editing CRD types. The generated YAML is part of the contract; staleness here is a real bug.
