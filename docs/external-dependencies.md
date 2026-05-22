# External dependencies

Things the operator expects to find in the cluster (or beyond) but never installs itself. For the full install procedure see [install-production.md](install-production.md); this page is the dependency catalogue.

## Always required

### Vault
- **What:** A reachable Vault instance (any version 1.13+).
- **Why:** Stores per-cluster credentials and acts as the auth backbone.
- **Configured in:** `spec.platform.vault`.
- **Auth methods:** `kubernetes` (default) or `appRole`.
- **Never deployed by the operator** (invariant 1).
- **What you must provision yourself before the first reconcile:**
  - KV-v2 mount (conventionally at path `openchami/`).
  - An AppRole role per cluster (e.g. `openchami-<clusterName>-services`).
  - A Vault policy granting read on `openchami/<clusterName>/data/*` and create+update on any PKI roles you reference.
  - A Kubernetes Secret in `openchami-<clusterName>` namespace holding the AppRole secret_id (key: `id`).
  - Real credentials (not operator-generated placeholders) at the S3 and OIDC KV paths; the DB paths may be left empty for the operator to populate randomly.
  - See [install-production.md §6 and §8](install-production.md#6-provision-vault) for the canonical procedure.
- **In dev:** `make dev-up` brings up a docker-compose Vault on `127.0.0.1:8200` with root token `dev-root-token` and seeds the cluster via `hack/local-dev/seed-vault.sh`.

### VersityGW (or any S3-compatible object store)
- **What:** S3 endpoint for boot images and (when logging is enabled) Parquet log archives.
- **Configured in:** `spec.platform.objectStorage`.
- **Required keys:** `endpoint`, optional `bucket`, optional `tlsInsecure`.
- **Never deployed by the operator** (invariant 1).
- **What you must provision yourself:**
  - One bucket per cluster for boot images (default name `<clusterName>-boot-images`).
  - One bucket for logs (`<clusterName>-logs`) when logging is enabled.
  - A bucket-policy granting read/write to the access-key/secret-key pair stored at `openchami/<clusterName>/s3/versitygw` (and `s3/logs`). See the JSON snippet in [install-production.md §7](install-production.md#7-provision-s3-bucket-access).
- **In dev:** `make dev-up` brings up LocalStack S3 on `127.0.0.1:4566`. Treat it as a stand-in for VersityGW.

## Required prerequisite CRDs

These must be installed before the operator's first reconcile. `make dev-install-prereqs` installs them for the dev cluster; for production see [install-production.md §2](install-production.md#2-install-prerequisite-controllers-and-crds) for the same shell commands tuned for a real cluster.

### gateway-api (standard channel, ≥ 1.4)
- **What:** The upstream Gateway API CRDs. The operator's gateway reconciler emits `BackendTLSPolicy` against the `gateway.networking.k8s.io/v1` API, which was promoted from `v1alpha3` in gateway-api 1.4.
- **CRDs used (v1):** `GatewayClass`, `Gateway`, `HTTPRoute`, `BackendTLSPolicy`, `ReferenceGrant`.
- **Used by:** `internal/reconcilers/gateway.go`.
- **Install:** `kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml`
- **Important:** the Envoy Gateway helm chart does **not** install the v1 standard CRDs on its own. If you skip this step the operator gracefully degrades — it reports `GatewayReady=False/MissingCRD` and defers JWT-gated routes — but the JWT layer never comes online until you install the v1 CRDs and re-reconcile. See `backendTLSPolicyAvailable` in `internal/reconcilers/gateway.go` for the runtime check pattern.

### cert-manager
- **What:** Issues TLS certificates for the gateway.
- **CRDs used:** `Certificate`, `Issuer`, `ClusterIssuer`.
- **Used by:** `internal/reconcilers/certificates.go`.
- **Install:** `kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml`.
- **Note:** the operator never creates `Issuer` or `ClusterIssuer` resources — it only references them by name via `spec.networking.tls.issuer`. Stage the issuer yourself before applying the CR. See [install-production.md §3](install-production.md#3-configure-cert-manager-issuers) for production issuer choices.

### CloudNativePG
- **What:** Postgres operator. The OpenCHAMI operator emits `cnpg.io/Cluster` resources, not raw StatefulSets.
- **CRDs used:** `Cluster`, `Backup`, `ScheduledBackup`.
- **Used by:** `internal/reconcilers/database.go`.
- **Install:** the dev-up flow pins `CNPG_VERSION=1.29.0` and pulls from the release-1.29 manifest. Production:
  ```sh
  kubectl apply --server-side -f https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.29/releases/cnpg-1.29.0.yaml
  ```
  (The previous `main/releases/cnpg-latest.yaml` URL is dead — CNPG no longer publishes a `latest`-aliased manifest there.)

### Vault Secrets Operator (VSO)
- **What:** Syncs Vault KV to Kubernetes Secrets, so service Pods can mount them as env or files.
- **CRDs used:** `VaultStaticSecret`, `VaultConnection`, `VaultAuth`.
- **Used by:** `internal/reconcilers/vault.go` (the operator emits `VaultStaticSecret` per cluster).
- **Install:** `helm upgrade --install vault-secrets-operator hashicorp/vault-secrets-operator --namespace vault-secrets-operator-system --create-namespace --set defaultVaultConnection.enabled=false`.

### Envoy Gateway
- **What:** The L7 gateway implementation. The operator emits `gateway.networking.k8s.io/HTTPRoute` and `gateway.envoyproxy.io/{SecurityPolicy,BackendTrafficPolicy}`.
- **CRDs used:** `SecurityPolicy`, `BackendTrafficPolicy` (the gateway-api primitives come from §1 above).
- **Used by:** `internal/reconcilers/gateway.go`.
- **Install (OCI helm chart):**
  ```sh
  helm upgrade --install envoy-gateway oci://docker.io/envoyproxy/gateway-helm \
    --version v1.5.1 \
    --namespace envoy-gateway-system --create-namespace
  ```
  (Older docs reference a classic `helm repo add envoy-gateway https://charts.envoyproxy.io` form; that URL doesn't exist — the chart is published only as an OCI image.)
- **Also required:** a matching `GatewayClass`. The dev cluster ships `hack/local-dev/envoy-gatewayclass.yaml`; production sites apply an equivalent (see [install-production.md §2.5](install-production.md#25-envoy-gateway--gatewayclass)).

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

| Component | Tested versions | Notes |
|---|---|---|
| Kubernetes | 1.29 – 1.32 | Anything that ships Gateway API v1 stable. |
| gateway-api standard CRDs | 1.4 – 1.5.x | 1.4 first promoted `BackendTLSPolicy` to v1. Dev cluster runs 1.5.1. |
| cert-manager | 1.13+ | Tested with cert-manager `latest` from the upstream release URL. |
| CloudNativePG | 1.29.x | Dev cluster pins `CNPG_VERSION=1.29.0`. 1.22+ should work; not regression-tested below 1.29. |
| Vault Secrets Operator | 0.4+ | |
| Envoy Gateway | v1.5.x | Dev cluster pins `ENVOY_GATEWAY_VERSION=v1.5.1`. |
| Vault | 1.13+ | 1.21 in dev. |
| VersityGW | 1.0+ | LocalStack 3 in dev (S3-compatible). |

The operator does not pin these — it consumes them through stable CRD versions. Bumping to a new minor version of any prerequisite is a documentation-only change unless the CRD shape moves (e.g. `BackendTLSPolicy v1alpha3 → v1`).

## Things the operator does NOT need

- A service mesh (Istio, Linkerd). NetworkPolicies + Envoy Gateway cover the L7 needs.
- An external scheduler. The Kubernetes scheduler is sufficient.
- Any HPC-specific software on the host nodes (Slurm, NHC, etc.). Those are workload concerns, not deployment concerns. (See invariant 10.)
- Real BMCs. Magellan and CoreDHCP can be tested with the integration-sandbox simulators; see [relationship-to-integration-sandbox](relationship-to-integration-sandbox.md).
