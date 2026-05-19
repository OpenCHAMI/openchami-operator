# Sub-reconcilers

One section per sub-reconciler. Format: what it owns, condition(s) it sets, dependencies, source file.

## Order

```
Namespace → RBAC → Vault → Bucket → Database
  → SMD → Tokensmith → BootService → MetadataService
  → NetworkProbe → CoreDHCP → Magellan
  → Certificates → Gateway → NetworkPolicies
  → Funicular → LogBucket → Topology → ServiceMonitor
```

The order is fixed in `internal/controller/openchamicontrolplane_controller.go::reconcileAll`. Reordering is a breaking change.

## Namespace
- **File:** `internal/reconcilers/namespace.go`
- **Owns:** the `openchami-{clusterName}` namespace itself.
- **Condition:** `NamespaceReady` (per invariant 2 — every other resource lives inside this namespace).
- **Depends on:** nothing. Always first.

## RBAC
- **File:** `internal/reconcilers/rbac.go`
- **Owns:** ServiceAccount, Role, RoleBinding, ClusterRole, ClusterRoleBinding for service-to-Kubernetes API access (used mostly by network-probe).
- **Condition:** `RBACReady`.
- **Depends on:** Namespace.

## Vault
- **File:** `internal/reconcilers/vault.go`
- **Owns:** AppRole role, KV paths under `openchami/{clusterName}/`, optional VaultStaticSecret resources for VSO.
- **Condition:** `VaultReady`.
- **Reason on failure:** `VaultUnreachable` requeues with backoff (invariant 1 — Vault is external; never crash).
- **Depends on:** Namespace, RBAC.

## Bucket
- **File:** `internal/reconcilers/bucket.go`
- **Owns:** the boot-image bucket on VersityGW (or whatever S3 endpoint the user configured).
- **Condition:** `BucketReady`.
- **Reason on failure:** `S3Unreachable` requeues with backoff (invariant 1 — VersityGW is external).
- **Depends on:** Namespace.

## Database
- **File:** `internal/reconcilers/database.go`
- **Owns:** a CloudNativePG `Cluster` per `OpenCHAMIControlPlane`. Replicas, storage size, backup configuration come from `spec.database`.
- **Condition:** `DatabaseReady`.
- **Depends on:** Vault (for the bootstrap superuser secret), Bucket (for backup target).
- **Tokensmith specifics:** Tokensmith is the one Deployment that owns its own PVC instead of going through CNPG. See `tokensmith.go`.

## SMD
- **File:** `internal/reconcilers/smd.go`
- **Owns:** SMD Deployment + Service.
- **Default image constant:** `defaultSMDImage` (currently `ghcr.io/openchami/smd:latest`).
- **Condition:** participates in `ServicesReady` (aggregated after the four core services have all completed).
- **Depends on:** Database.

## Tokensmith
- **File:** `internal/reconcilers/tokensmith.go`
- **Owns:** Tokensmith Deployment + PVC + Service.
- **Strategy:** `Recreate`. Replicas always 1 (single-writer; the PVC pins it).
- **Condition:** participates in `ServicesReady`.
- **Depends on:** SMD (some token-issuance flows reference SMD).

## BootService
- **File:** `internal/reconcilers/boot_service.go`
- **Owns:** boot-service Deployment + Service.
- **Condition:** participates in `ServicesReady`.
- **Depends on:** SMD, Tokensmith, Bucket (for boot-image fetch).

## MetadataService
- **File:** `internal/reconcilers/metadata_service.go`
- **Owns:** metadata-service Deployment + Service.
- **Condition:** participates in `ServicesReady`.
- **Depends on:** SMD.

## NetworkProbe
- **File:** `internal/reconcilers/networkprobe.go`
- **Owns:** the network-probe DaemonSet plus a small controller that labels Nodes when probes pass.
- **Labels applied:** `openchami.org/<clusterName>-provision-network-ready=true`, `openchami.org/<clusterName>-bmc-network-ready=true`. (One pair per cluster — Kubernetes label keys allow at most one `/`, so the cluster name is joined with a hyphen rather than a second slash.)
- **Condition:** `NetworkProbeReady`.
- **Status:** populates `cluster.Status.NetworkProbe.NodesWithProvisionAccess[]` and `NodesWithBMCAccess[]`.
- **Skipped when:** `spec.networkProbe.enabled=false`. In that case CoreDHCP/Magellan node selection comes from `spec.services.coreDHCP.nodeSelector` / `spec.services.magellan.nodeSelector` directly. The validation webhook enforces no-overlap across clusters (invariant 4) when probes are disabled.

## CoreDHCP
- **File:** `internal/reconcilers/coredhcp.go`
- **Owns:** CoreDHCP DaemonSet.
- **Default image constant:** `defaultCoreDHCPImage` (`ghcr.io/openchami/coredhcp:latest`).
- **Condition:** `DHCPReady`.
- **Skipped when:** `spec.services.coreDHCP.enabled=false`.
- **Depends on:** NetworkProbe (waits for `NetworkProbeReady` if probes enabled), SMD (auth token for SMD lookups; minted by tokensmith — TODO referenced in source as `phase06b`).

## Magellan
- **File:** `internal/reconcilers/magellan.go`
- **Owns:** a Magellan CronJob (BMC discovery scan).
- **Condition:** `MagellanReady`.
- **Skipped when:** `spec.services.magellan.enabled=false`.
- **Depends on:** NetworkProbe (BMC-network reachability), SMD (writes inventory back).

## Certificates
- **File:** `internal/reconcilers/certificates.go`
- **Owns:** cert-manager `Certificate` resources for the gateway TLS termination.
- **Condition:** `CertificatesReady`.
- **Status:** populates `cluster.Status.CertExpiryTime` (RFC3339 timestamp of the soonest-expiring cert).
- **Depends on:** cert-manager CRDs being installed (external prerequisite; see [external-dependencies](external-dependencies.md)).

## Gateway
- **File:** `internal/reconcilers/gateway.go`
- **Owns:** Envoy Gateway `HTTPRoute` plus optional `SecurityPolicy` for OIDC.
- **Condition:** `GatewayReady`.
- **Depends on:** Certificates, services (so the route targets exist).

## NetworkPolicies
- **File:** `internal/reconcilers/networkpolicies.go`
- **Owns:** ≥19 `NetworkPolicy` resources implementing zero-trust intra-namespace egress/ingress.
- **Condition:** `NetworkPoliciesReady`.
- **Order within the file** (no functional dependency, only readability):
  1. defaultDenyAll
  2. allowDNSEgress
  3. allowVaultEgress (uses `vaultEgressPeer()`)
  4. allowVersityGWEgress
  5. allowLogsEgress
  6. (… service-specific allows)
- **Depends on:** Namespace.

## Funicular
- **File:** `internal/reconcilers/funicular.go`
- **Owns:** the funicular log-collector DaemonSet.
- **Condition:** `LoggingReady`.
- **Skipped when:** `spec.logging.enabled=false`.
- **Depends on:** LogBucket (target for Parquet writes), services (sources of NDJSON).

## LogBucket
- **File:** `internal/reconcilers/logbucket.go`
- **Owns:** the per-cluster log bucket on VersityGW + lifecycle rules from `spec.logging.retentionDays`.
- **Condition:** part of `LoggingReady`.
- **Depends on:** Bucket reconciler infrastructure (S3 client).

## Topology
- **File:** `internal/reconcilers/topology.go` (+ `topology_schema.go`)
- **Owns:** a single ConfigMap describing the cluster's effective topology — service endpoints, DB DSN shape, Vault paths.
- **Condition:** none directly; updates `cluster.Status.TopologyVersion` (SHA-256 of content).
- **Depends on:** all services have been applied.

## ServiceMonitor
- **File:** `internal/reconcilers/servicemonitor.go`
- **Owns:** Prometheus Operator `ServiceMonitor` resources for the operator and each managed service.
- **Condition:** `ObservabilityReady`.
- **Skipped when:** `spec.observability.prometheusOperator=false`.
- **Depends on:** prometheus-operator CRDs being installed (optional external prerequisite).

## Helpers shared across all sub-reconcilers

`internal/reconcilers/helpers.go`:
- `RunbookURL(reason string) string` — builds `https://openchami.org/docs/ops/{reason-in-kebab-case}`.
- `RecordConditionEvent(recorder, cluster, eventType, reason, message)` — emits an Event with the runbook URL appended (invariant 9). Use this; never call `recorder.Event` directly.
- `EffectiveNodeSelector(cluster, kind)` — resolves `spec.services.<kind>.nodeSelector` or the network-probe-applied label, depending on whether probes are enabled.

`internal/logging/logger.go`:
- `Enrich(ctx, cluster, reconciler) logr.Logger` — returns a logger pre-bound with `cluster`, `reconciler`, and (later) `resource` keys (invariant 8).
- `EnrichWithResource(ctx, cluster, reconciler, resource) logr.Logger` — same plus `resource`.

Use these. Calling `log.FromContext` directly is an invariant violation.
