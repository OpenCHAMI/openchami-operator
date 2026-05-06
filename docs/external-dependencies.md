# External dependencies

Things the operator expects to find in the cluster (or beyond) but never installs itself.

## Always required

### Vault
- **What:** A reachable Vault instance (any version 1.13+).
- **Why:** Stores per-cluster credentials and acts as the auth backbone.
- **Configured in:** `spec.platform.vault`.
- **Auth methods:** `kubernetes` (default) or `appRole`.
- **Never deployed by the operator** (invariant 1).
- **In dev:** `make dev-up` brings up a docker-compose Vault on `127.0.0.1:8200` with root token `dev-root-token`.

### VersityGW (or any S3-compatible object store)
- **What:** S3 endpoint for boot images and (when logging is enabled) Parquet log archives.
- **Configured in:** `spec.platform.objectStorage`.
- **Required keys:** `endpoint`, optional `bucket`, optional `tlsInsecure`.
- **Never deployed by the operator** (invariant 1).
- **In dev:** `make dev-up` brings up LocalStack S3 on `127.0.0.1:4566`. Treat it as a stand-in for VersityGW.

## Required prerequisite CRDs

These must be installed before the operator's first reconcile. `make dev-install-prereqs` does it for the dev cluster.

### cert-manager
- **What:** Issues TLS certificates for the gateway.
- **CRDs used:** `Certificate`, `Issuer`, `ClusterIssuer`.
- **Used by:** `internal/reconcilers/certificates.go`.
- **Install:** `kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml`.

### CloudNativePG
- **What:** Postgres operator. The OpenCHAMI operator emits `cnpg.io/Cluster` resources, not raw StatefulSets.
- **CRDs used:** `Cluster`, `Backup`, `ScheduledBackup`.
- **Used by:** `internal/reconcilers/database.go`.
- **Install:** `kubectl apply -f https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/main/releases/cnpg-latest.yaml`.

### Vault Secrets Operator (VSO)
- **What:** Syncs Vault KV to Kubernetes Secrets, so service Pods can mount them as env or files.
- **CRDs used:** `VaultStaticSecret`, `VaultConnection`, `VaultAuth`.
- **Used by:** `internal/reconcilers/vault.go` (the operator emits `VaultStaticSecret` per cluster).
- **Install:** `helm upgrade --install vault-secrets-operator hashicorp/vault-secrets-operator --namespace vault-secrets-operator-system --create-namespace --set defaultVaultConnection.enabled=false`.

### Envoy Gateway
- **What:** The L7 gateway implementation. The operator emits `gateway.networking.k8s.io/HTTPRoute` and `gateway.envoyproxy.io/SecurityPolicy`.
- **CRDs used:** `Gateway`, `HTTPRoute`, `SecurityPolicy`, `BackendTrafficPolicy`.
- **Used by:** `internal/reconcilers/gateway.go`.
- **Install:** `helm upgrade --install envoy-gateway envoy-gateway/gateway-helm --namespace envoy-gateway-system --create-namespace`.

## Optional prerequisite CRDs

### Prometheus Operator
- **When:** `spec.observability.prometheusOperator=true`.
- **CRDs used:** `ServiceMonitor`.
- **Used by:** `internal/reconcilers/servicemonitor.go`.
- **If absent:** the operator's `ServiceMonitor` writes are skipped silently.
- **Install:** the kube-prometheus-stack chart, or any distribution that ships `monitoring.coreos.com/v1`.

### Cert-Manager DNS01 / HTTP01 issuers
Used at the user's discretion to back the gateway certificate. The operator does not create issuers itself; it only references them via `spec.networking.tls.issuer`.

## Compatibility matrix

| Component | Tested versions |
|---|---|
| Kubernetes | 1.29 – 1.32 (anything that ships Gateway API v1 stable) |
| cert-manager | 1.13+ |
| CloudNativePG | 1.22+ |
| Vault Secrets Operator | 0.4+ |
| Envoy Gateway | 1.0+ |
| Vault | 1.13+ (1.21 in dev) |
| VersityGW | 1.0+ (LocalStack 3 in dev) |

The operator does not pin these — it consumes them through stable CRD versions. Bumping to a new minor version of any prerequisite is a documentation-only change unless the CRD shape moves.

## Things the operator does NOT need

- A service mesh (Istio, Linkerd). NetworkPolicies + Envoy Gateway cover the L7 needs.
- An external scheduler. The Kubernetes scheduler is sufficient.
- Any HPC-specific software on the host nodes (Slurm, NHC, etc.). Those are workload concerns, not deployment concerns. (See invariant 10.)
- Real BMCs. Magellan and CoreDHCP can be tested with the integration-sandbox simulators; see [relationship-to-integration-sandbox](relationship-to-integration-sandbox.md).
