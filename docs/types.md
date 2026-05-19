### Top-level spec and platform types

```go
// VaultAuthMethod selects how the operator authenticates to Vault.
// +kubebuilder:validation:Enum=kubernetes;appRole
type VaultAuthMethod string

const (
    VaultAuthMethodKubernetes VaultAuthMethod = "kubernetes"
    VaultAuthMethodAppRole    VaultAuthMethod = "appRole"
)

type VaultSpec struct {
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:Pattern=`^https?://`
    Address string `json:"address"`

    // +kubebuilder:default=kubernetes
    AuthMethod VaultAuthMethod `json:"authMethod,omitempty"`

    // AppRoleSecretRef names a Secret containing role_id and secret_id.
    // Required when AuthMethod is appRole.
    // +optional
    AppRoleSecretRef *corev1.LocalObjectReference `json:"appRoleSecretRef,omitempty"`

    // CABundleSecretRef names a Secret with key ca.crt for Vault TLS verification.
    // +optional
    CABundleSecretRef *corev1.LocalObjectReference `json:"caBundleSecretRef,omitempty"`
}

type ObjectStorageSpec struct {
    // +kubebuilder:validation:Required
    Endpoint string `json:"endpoint"`

    // Bucket for boot images. Defaults to "{clusterName}-boot-images".
    // +optional
    Bucket string `json:"bucket,omitempty"`

    // TLSInsecure disables TLS certificate verification (dev/test only).
    // +optional
    TLSInsecure bool `json:"tlsInsecure,omitempty"`
}

type PlatformSpec struct {
    Vault         VaultSpec         `json:"vault"`
    ObjectStorage ObjectStorageSpec `json:"objectStorage"`
}

// NetworkProbeTarget describes a network to probe and an optional connectivity check.
type NetworkProbeTarget struct {
    // Subnet is the CIDR to check for a local route.
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:Format=cidr
    Subnet string `json:"subnet"`

    // ValidateHost is an IP/hostname to TCP-dial as a secondary reachability check.
    // +optional
    ValidateHost string `json:"validateHost,omitempty"`

    // ValidatePort is the TCP port for the connectivity check.
    // +optional
    ValidatePort int32 `json:"validatePort,omitempty"`

    // ValidateTimeout is the dial timeout, e.g. "3s".
    // +optional
    ValidateTimeout string `json:"validateTimeout,omitempty"`
}

type NetworkProbeSpec struct {
    // +kubebuilder:default=false
    Enabled bool `json:"enabled,omitempty"`

    // IntervalSeconds between successive probes on each node.
    // +kubebuilder:default=300
    // +optional
    IntervalSeconds int32 `json:"intervalSeconds,omitempty"`

    // ProvisionNetwork describes the provision/PXE network to probe.
    // Required when Enabled=true and CoreDHCP is enabled.
    // +optional
    ProvisionNetwork *NetworkProbeTarget `json:"provisionNetwork,omitempty"`

    // BMCNetwork describes the BMC network to probe.
    // Required when Enabled=true and Magellan is enabled.
    // +optional
    BMCNetwork *NetworkProbeTarget `json:"bmcNetwork,omitempty"`
}

// OpenCHAMIControlPlaneSpec defines the desired state of an OpenCHAMI service cluster.
type OpenCHAMIControlPlaneSpec struct {
    // ClusterName is the logical name used in namespaces, Vault paths, and labels.
    // Immutable after creation.
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=32
    // +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]*$`
    ClusterName string `json:"clusterName"`

    // Domain is the external FQDN for the cluster gateway.
    // +kubebuilder:validation:Required
    Domain string `json:"domain"`

    Platform     PlatformSpec     `json:"platform"`
    NetworkProbe NetworkProbeSpec `json:"networkProbe,omitempty"`
    Services     ServicesSpec     `json:"services,omitempty"`
    Networking   NetworkingSpec   `json:"networking,omitempty"`
    Database     DatabaseSpec     `json:"database,omitempty"`
    Logging      LoggingSpec      `json:"logging,omitempty"`
    Observability ObservabilitySpec `json:"observability,omitempty"`

    // OperatorChannel controls reconcile behaviour.
    // "stable"  — always reconcile.
    // "pinned"  — only reconcile if PinnedVersion matches the running operator version.
    // +kubebuilder:validation:Enum=stable;pinned
    // +kubebuilder:default=stable
    OperatorChannel string `json:"operatorChannel,omitempty"`

    // PinnedVersion is the exact operator semver required when OperatorChannel=pinned.
    // +optional
    PinnedVersion string `json:"pinnedVersion,omitempty"`
}
```

---

### ServicesSpec

```go
type ServicesSpec struct {
    SMD             SMDSpec             `json:"smd,omitempty"`
    Tokensmith      TokensmithSpec      `json:"tokensmith,omitempty"`
    BootService     BootServiceSpec     `json:"bootService,omitempty"`
    MetadataService MetadataServiceSpec `json:"metadataService,omitempty"`
    CoreDHCP        CoreDHCPSpec        `json:"coreDHCP,omitempty"`
    Magellan        MagellanSpec        `json:"magellan,omitempty"`
}

// ServiceDefaults is embedded in every per-service spec.
type ServiceDefaults struct {
    // +kubebuilder:default=true
    Enabled bool `json:"enabled,omitempty"`

    // Replicas is ignored for DaemonSet and CronJob services.
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=10
    // +kubebuilder:default=2
    Replicas int32 `json:"replicas,omitempty"`

    // +optional
    Image *ImageSpec `json:"image,omitempty"`

    // +optional
    Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

type ImageSpec struct {
    // +optional
    Repository string `json:"repository,omitempty"`
    // +optional
    Tag string `json:"tag,omitempty"`
    // +kubebuilder:validation:Enum=Always;Never;IfNotPresent
    // +optional
    PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
}

type SMDSpec struct {
    ServiceDefaults `json:",inline"`
}

type TokensmithSpec struct {
    ServiceDefaults `json:",inline"`

    // OIDCProvider selects the upstream OIDC token source.
    // "vault"    — use Vault's identity/oidc engine as the issuer.
    //              No external OIDC provider required.
    // "external" — use an existing OIDC provider. OIDCIssuerURL required.
    // +kubebuilder:validation:Enum=vault;external
    // +kubebuilder:default=vault
    OIDCProvider string `json:"oidcProvider,omitempty"`

    // OIDCIssuerURL is required when OIDCProvider is "external".
    // +optional
    OIDCIssuerURL string `json:"oidcIssuerURL,omitempty"`
}

type BootServiceSpec struct {
    ServiceDefaults `json:",inline"`
}

type MetadataServiceSpec struct {
    ServiceDefaults `json:",inline"`
}

type CoreDHCPSpec struct {
    // +kubebuilder:default=true
    Enabled bool `json:"enabled,omitempty"`

    // NodeSelector is used when spec.networkProbe.enabled is false.
    // When network probing is enabled this field is ignored; the operator
    // generates the selector from probe-applied node labels.
    // When probing is disabled this field is required.
    // When set, it must contain at least one cluster-discriminating key
    // (validated by the admission webhook).
    // +optional
    NodeSelector map[string]string `json:"nodeSelector,omitempty"`

    // LeaseRanges defines DHCP subnet ranges to serve.
    // +kubebuilder:validation:MinItems=1
    LeaseRanges []DHCPLeaseRange `json:"leaseRanges"`

    // +kubebuilder:default="5m"
    UnknownLeaseDuration string `json:"unknownLeaseDuration,omitempty"`

    // +kubebuilder:default="1h"
    KnownLeaseDuration string `json:"knownLeaseDuration,omitempty"`

    // +optional
    Image *ImageSpec `json:"image,omitempty"`

    // +optional
    Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

    // +optional
    Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

type DHCPLeaseRange struct {
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:Format=cidr
    Subnet string `json:"subnet"`
    // +kubebuilder:validation:Required
    Start string `json:"start"`
    // +kubebuilder:validation:Required
    End string `json:"end"`
}

type MagellanSpec struct {
    // +kubebuilder:default=true
    Enabled bool `json:"enabled,omitempty"`

    // NodeSelector is used when spec.networkProbe.enabled is false.
    // When probing is enabled this field is ignored.
    // +optional
    NodeSelector map[string]string `json:"nodeSelector,omitempty"`

    // +kubebuilder:default="*/30 * * * *"
    Schedule string `json:"schedule,omitempty"`

    // BMCSubnet is the CIDR range to scan for BMC endpoints.
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:Format=cidr
    BMCSubnet string `json:"bmcSubnet"`

    // +kubebuilder:validation:Enum=Allow;Forbid;Replace
    // +kubebuilder:default=Forbid
    ConcurrencyPolicy batchv1.ConcurrencyPolicy `json:"concurrencyPolicy,omitempty"`

    // +optional
    Image *ImageSpec `json:"image,omitempty"`

    // +optional
    Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}
```

### NetworkingSpec / DatabaseSpec / LoggingSpec / ObservabilitySpec

```go
type NetworkingSpec struct {
    // +kubebuilder:default=envoy
    GatewayClass string `json:"gatewayClass,omitempty"`
    // +optional
    TLS TLSSpec `json:"tls,omitempty"`
}

type TLSSpec struct {
    // +kubebuilder:default=vault-pki-issuer
    Issuer string `json:"issuer,omitempty"`
    // Defaults to "{clusterName}-gateway-tls".
    // +optional
    SecretName string `json:"secretName,omitempty"`
}

type DatabaseSpec struct {
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=5
    // +kubebuilder:default=3
    Instances int32 `json:"instances,omitempty"`

    // +kubebuilder:default="20Gi"
    StorageSize resource.Quantity `json:"storageSize,omitempty"`

    // +optional
    StorageClass string `json:"storageClass,omitempty"`

    // BackupEnabled enables CNPG WAL archiving to the object storage bucket.
    // +optional
    BackupEnabled bool `json:"backupEnabled,omitempty"`
}

// LoggingSpec configures legendary-funicular log collection infrastructure.
// The operator provisions the S3 bucket and deploys the collector DaemonSet.
// Log schema and query patterns are defined by legendary-funicular and the
// services — not by the operator.
type LoggingSpec struct {
    // +kubebuilder:default=true
    Enabled bool `json:"enabled,omitempty"`

    // LogBucket is the S3 bucket for collected logs.
    // Defaults to "{clusterName}-logs".
    // +optional
    LogBucket string `json:"logBucket,omitempty"`

    // RetentionDays is how long Parquet objects are kept. A lifecycle rule
    // is applied to the bucket.
    // +kubebuilder:default=90
    // +optional
    RetentionDays int32 `json:"retentionDays,omitempty"`

    // FlushIntervalSeconds controls how often the collector writes Parquet.
    // +kubebuilder:default=60
    // +optional
    FlushIntervalSeconds int32 `json:"flushIntervalSeconds,omitempty"`

    // IncludeServices limits collection to named services.
    // Empty means collect from all services in the namespace.
    // +optional
    IncludeServices []string `json:"includeServices,omitempty"`
}

type ObservabilitySpec struct {
    // PrometheusOperator enables ServiceMonitor creation.
    // Requires prometheus-operator CRDs.
    // +optional
    PrometheusOperator bool `json:"prometheusOperator,omitempty"`
}
```

### OpenCHAMIControlPlaneStatus

```go
type OpenCHAMIControlPlaneStatus struct {
    // +optional
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`

    // +optional
    Phase ClusterPhase `json:"phase,omitempty"`

    // +listType=map
    // +listMapKey=type
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`

    // Services reports per-service readiness and internal endpoint.
    // +optional
    Services map[string]ServiceStatus `json:"services,omitempty"`

    // Namespace is the Kubernetes namespace created for this cluster.
    // +optional
    Namespace string `json:"namespace,omitempty"`

    // VaultPathPrefix is the Vault KV path prefix in use.
    // +optional
    VaultPathPrefix string `json:"vaultPathPrefix,omitempty"`

    // NetworkProbe reports which Kubernetes management nodes currently
    // pass each network reachability probe.
    // +optional
    NetworkProbe *NetworkProbeStatus `json:"networkProbe,omitempty"`

    // TopologyVersion is the SHA-256 of the current topology ConfigMap.
    // +optional
    TopologyVersion string `json:"topologyVersion,omitempty"`

    // ManagedByVersion is the operator semver that last reconciled this cluster.
    // +optional
    ManagedByVersion string `json:"managedByVersion,omitempty"`

    // CertExpiryTime is the RFC3339 timestamp of the soonest-expiring
    // TLS certificate managed for this cluster.
    // +optional
    CertExpiryTime string `json:"certExpiryTime,omitempty"`

    // LogBucket is the resolved S3 bucket name for log collection.
    // +optional
    LogBucket string `json:"logBucket,omitempty"`
}

type NetworkProbeStatus struct {
    // NodesWithProvisionAccess lists Kubernetes management node names where
    // the provision/PXE network probe is currently passing.
    // +optional
    NodesWithProvisionAccess []string `json:"nodesWithProvisionAccess,omitempty"`

    // NodesWithBMCAccess lists Kubernetes management node names where
    // the BMC network probe is currently passing.
    // +optional
    NodesWithBMCAccess []string `json:"nodesWithBMCAccess,omitempty"`

    // ProbeReady is true when the probe DaemonSet is running and at least
    // one node passes each configured probe type.
    ProbeReady bool `json:"probeReady"`
}

type ServiceStatus struct {
    Ready    bool   `json:"ready"`
    Endpoint string `json:"endpoint,omitempty"`
    Message  string `json:"message,omitempty"`
}

type ClusterPhase string

const (
    PhaseProvisioning ClusterPhase = "Provisioning"
    PhaseReady        ClusterPhase = "Ready"
    PhaseDegraded     ClusterPhase = "Degraded"
    PhaseDeleting     ClusterPhase = "Deleting"
    PhaseFailed       ClusterPhase = "Failed"
)
```

---

### Condition constant additions (conditions.go)

Add to `internal/conditions/conditions.go` alongside the existing constants:

```go
// ConditionMagellanReady is true when the Magellan CronJob exists.
ConditionMagellanReady = "MagellanReady"
```

---

### VSO import information

- Module: `github.com/hashicorp/vault-secrets-operator`
- Import path for types: `github.com/hashicorp/vault-secrets-operator/api/v1beta1`
- Latest stable release: v1.3.0 (use instead of v0.10 from bootstrap stub)
- `go get` command: `go get github.com/hashicorp/vault-secrets-operator@v1.3.0`

Key VSO types used by the vault sub-reconciler:
```go
// VaultConnection — one per cluster
vsov1beta1.VaultConnection{Spec: vsov1beta1.VaultConnectionSpec{
    Address:          cluster.Spec.Platform.Vault.Address,
    CACertSecretRef:  "...",   // from caBundleSecretRef if set
    SkipTLSVerify:    cluster.Spec.Platform.ObjectStorage.TLSInsecure,
}}

// VaultAuth — one per cluster
vsov1beta1.VaultAuth{Spec: vsov1beta1.VaultAuthSpec{
    VaultConnectionRef: "openchami-{name}-vault-connection",
    Method:             "kubernetes" | "appRole",
    Mount:              "kubernetes" | "approle",
    Kubernetes: &vsov1beta1.VaultAuthConfigKubernetes{
        Role:           paths.K8sRoleServices,
        ServiceAccount: "operator-config-reader",
    },
    AppRole: &vsov1beta1.VaultAuthConfigAppRole{
        RoleID:    roleID,
        SecretRef: cluster.Spec.Platform.Vault.AppRoleSecretRef.Name,
    },
}}

// VaultStaticSecret — four per cluster (db, s3, logs, oidc)
vsov1beta1.VaultStaticSecret{Spec: vsov1beta1.VaultStaticSecretSpec{
    VaultAuthRef:  "openchami-{name}-vault-auth",
    Mount:         paths.KVMount,
    Type:          "kv-v2",
    Path:          paths.DBCredentials,  // etc.
    RefreshAfter:  "1h",
    Destination: vsov1beta1.Destination{
        Name:   "openchami-{name}-db-creds",
        Create: true,
    },
}}
```

