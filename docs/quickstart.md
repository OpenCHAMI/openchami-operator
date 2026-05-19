# Quickstart

Local dev cluster, first reconcile, in under fifteen minutes.

## Prerequisites

- Go 1.24.6+
- Docker 17.03+ (28+ recommended; the project tests run on Docker 29.x)
- `kubectl`
- `kind` (for the local dev cluster)
- `helm` (for the prerequisite charts: cert-manager, CNPG, VSO, Envoy Gateway)
- `vault` CLI (for the dev Vault container)
- `awscli-local` or `awscli` configured against LocalStack S3

These overlap heavily with the [OpenCHAMI integration sandbox](https://github.com/OpenCHAMI/integration-sandbox) prerequisites; if you've already run that, you're mostly set (the sandbox doesn't need `kind` or `helm`).

## Bring up the dev environment

```sh
make install-tools     # controller-gen + setup-envtest
make dev-up            # docker-compose Vault+LocalStack, kind cluster, prerequisites, OpenCHAMI CRDs, Vault seed
```

`make dev-up` does the following sequentially:
1. Brings up `hack/local-dev/docker-compose.yaml` — Vault (dev mode, root token `dev-root-token`) and LocalStack S3 on `127.0.0.1:8200` and `127.0.0.1:4566`.
2. Creates the kind cluster `openchami-dev`.
3. Installs cert-manager, CloudNativePG, Vault Secrets Operator, Envoy Gateway via `make dev-install-prereqs`.
4. Installs the `OpenCHAMIControlPlane` CRD via `make install`.
5. Runs `hack/local-dev/seed-vault.sh` to create the per-cluster KV paths and AppRole.

After `make dev-up`, the cluster has the prereq CRDs and the OpenCHAMI CRD,
but the operator itself is **not** running yet. Pick one of the next two
sections.

## Run the operator

Two paths — pick whichever matches your loop.

### Path A — run on your laptop (recommended for iteration)

```sh
make dev-run             # foreground; Ctrl-C to stop
# or:
make dev-run-dry         # logs what would be applied without writing
```

`make dev-run` builds the binary, ensures the CRD is installed (calls
`make install` under the hood), and runs it against `~/.kube/config`. No
container build, no kind image load — fastest cycle.

### Path B — deploy the operator into the kind cluster

```sh
make dev-deploy          # docker-build → kind load → kustomize apply
```

This builds the operator image, loads it into the kind node, and applies
`config/default` (CRDs + RBAC + Deployment + webhook). Use this when you
want to see the operator running as a real `Pod` (e.g. to test webhooks,
RBAC, or leader election).

To remove the in-cluster install:
```sh
make undeploy            # remove Deployment, RBAC, webhook
make uninstall           # remove the CRD itself
```

For a release-tag deploy against an arbitrary cluster:
```sh
make deploy IMG=ghcr.io/openchami/openchami-operator:v1.2.3
```

## Apply a test cluster

After **either** Path A or Path B is running:

```sh
kubectl apply -f test/fixtures/minimal-cluster.yaml
kubectl get openchamicontrolplane testcluster -w
```

You should see the `phase` walk through `Provisioning` → `Ready` (or `Degraded` if a prerequisite is missing — see [troubleshooting](troubleshooting.md)).

## Iterate

```sh
# After editing any reconciler:
make generate manifests fmt vet lint test
```

All four targets must pass before pushing. The pre-push hook `make validate-invariants` enforces the absolute rules from [invariants](invariants.md).

## Tear down

```sh
make dev-down
```

Removes the kind cluster and the local docker-compose stack. Re-running `make dev-up` is the canonical way to reset.

## Beyond the quickstart

- [Architecture](architecture.md) — the operator's mental model.
- [Dev loop](dev-loop.md) — every make target, what it does, when to run it.
- [Testing](testing.md) — envtest vs e2e vs the integration sandbox.
