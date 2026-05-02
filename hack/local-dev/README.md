# Local Development Environment

## Prerequisites

- Docker + docker compose
- kind: `go install sigs.k8s.io/kind@latest`
- kubectl
- helm
- vault CLI (optional, for manual Vault interaction)
- awslocal: `pip install awscli-local`

## Quick start

```bash
# 1. Start Vault dev + localstack
make dev-up

# 2. Apply a test cluster
kubectl apply -f test/fixtures/minimal-cluster.yaml

# 3. Watch it come up
kubectl get openchamicluster testcluster -w

# 4. Run the operator locally
make dev-run
```

## Services

| Service | URL | Credentials |
|---|---|---|
| Vault | http://localhost:8200 | Token: `dev-root-token` |
| LocalStack S3 | http://localhost:4566 | Key: `test` / Secret: `test` |
| kind API | From kubeconfig | N/A |

## Re-seeding Vault

```bash
hack/local-dev/seed-vault.sh testcluster
```

## Tear down

```bash
make dev-down
```
