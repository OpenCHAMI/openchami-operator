// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

// v1alpha1 — current storage version. No stability guarantees.
//
//	Review UPGRADE.md on every operator upgrade.
//
// v1beta1  — promoted when stable 2+ releases + production use.
// v1       — promoted after v1beta1 stable 6+ months.
package v1alpha1

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VaultAuthMethod selects how the operator authenticates to Vault.
// +kubebuilder:validation:Enum=kubernetes;appRole
type VaultAuthMethod string

const (
	VaultAuthMethodKubernetes VaultAuthMethod = "kubernetes"
	VaultAuthMethodAppRole    VaultAuthMethod = "appRole"
)

// ImageSpec overrides the default container image for a service.
type ImageSpec struct {
	// +optional
	Repository string `json:"repository,omitempty"`
	// +optional
	Tag string `json:"tag,omitempty"`
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +optional
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
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

	// ExternalEndpoint, when set, declares that the service is provided
	// externally (e.g. by a shared platform deployment) rather than by the
	// operator. Consumer services within this cluster will be wired to the
	// supplied URL instead of the in-cluster Service DNS.
	//
	// Setting ExternalEndpoint requires Enabled=false; the validating
	// webhook enforces this. The URL must be an http:// or https:// URL.
	//
	// Not all services support an external endpoint — DHCP is a layer-2/3
	// concern and Magellan is a CronJob; both reject this field.
	//
	// +optional
	ExternalEndpoint *string `json:"externalEndpoint,omitempty"`
}

// DHCPLeaseRange defines a DHCP subnet range to serve.
type DHCPLeaseRange struct {
	// +kubebuilder:validation:Required
	Subnet string `json:"subnet"`
	// +kubebuilder:validation:Required
	Start string `json:"start"`
	// +kubebuilder:validation:Required
	End string `json:"end"`
}

// NetworkProbeTarget describes a network to probe and an optional connectivity check.
type NetworkProbeTarget struct {
	// Subnet is the CIDR to check for a local route.
	// +kubebuilder:validation:Required
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

// NetworkProbeSpec configures the network reachability probe DaemonSet.
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

// VaultSpec configures access to the external Vault instance.
// Vault is never deployed by this operator.
type VaultSpec struct {
	// +kubebuilder:validation:Required
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

// ObjectStorageSpec configures access to the VersityGW S3-compatible object store.
// VersityGW is never deployed by this operator.
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

// PlatformSpec references the external infrastructure this cluster depends on.
type PlatformSpec struct {
	Vault         VaultSpec         `json:"vault"`
	ObjectStorage ObjectStorageSpec `json:"objectStorage"`
}

// SMDSpec configures the SMD (State Management Database) service.
type SMDSpec struct {
	ServiceDefaults `json:",inline"`
}

// TokensmithSpec configures the tokensmith OIDC token service.
type TokensmithSpec struct {
	ServiceDefaults `json:",inline"`

	// OIDCProvider selects the upstream OIDC token source.
	// "vault"    — use Vault's identity/oidc engine as the issuer.
	// "external" — use an existing OIDC provider; OIDCIssuerURL required.
	// +kubebuilder:validation:Enum=vault;external
	// +kubebuilder:default=vault
	OIDCProvider string `json:"oidcProvider,omitempty"`

	// OIDCIssuerURL is required when OIDCProvider is "external".
	// +optional
	OIDCIssuerURL string `json:"oidcIssuerURL,omitempty"`
}

// BootServiceSpec configures the boot-service.
type BootServiceSpec struct {
	ServiceDefaults `json:",inline"`
}

// MetadataServiceSpec configures the metadata (cloud-init) service.
type MetadataServiceSpec struct {
	ServiceDefaults `json:",inline"`
}

// CoreDHCPSpec configures the CoreDHCP DaemonSet.
type CoreDHCPSpec struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`

	// NodeSelector is used when spec.networkProbe.enabled is false.
	// When network probing is enabled this field is ignored; the operator
	// generates the selector from probe-applied node labels.
	// When probing is disabled this field is required.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// LeaseRanges defines DHCP subnet ranges to serve.
	// At least one range is required when Enabled=true (validated by admission webhook).
	// +optional
	LeaseRanges []DHCPLeaseRange `json:"leaseRanges,omitempty"`

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

// MagellanSpec configures the Magellan BMC discovery CronJob.
type MagellanSpec struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`

	// NodeSelector is used when spec.networkProbe.enabled is false.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// +kubebuilder:default="*/30 * * * *"
	Schedule string `json:"schedule,omitempty"`

	// BMCSubnet is the CIDR range to scan for BMC endpoints.
	// Required when Enabled=true (validated by admission webhook).
	// +optional
	BMCSubnet string `json:"bmcSubnet,omitempty"`

	// +kubebuilder:validation:Enum=Allow;Forbid;Replace
	// +kubebuilder:default=Forbid
	ConcurrencyPolicy batchv1.ConcurrencyPolicy `json:"concurrencyPolicy,omitempty"`

	// +optional
	Image *ImageSpec `json:"image,omitempty"`

	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ServicesSpec configures all operator-managed services.
type ServicesSpec struct {
	SMD             SMDSpec             `json:"smd,omitempty"`
	Tokensmith      TokensmithSpec      `json:"tokensmith,omitempty"`
	BootService     BootServiceSpec     `json:"bootService,omitempty"`
	MetadataService MetadataServiceSpec `json:"metadataService,omitempty"`
	CoreDHCP        CoreDHCPSpec        `json:"coreDHCP,omitempty"`
	Magellan        MagellanSpec        `json:"magellan,omitempty"`
}

// TLSSpec configures TLS certificate management.
type TLSSpec struct {
	// +kubebuilder:default=vault-pki-issuer
	Issuer string `json:"issuer,omitempty"`

	// SecretName defaults to "{clusterName}-gateway-tls".
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// NetworkingSpec configures the external gateway.
type NetworkingSpec struct {
	// +kubebuilder:default=envoy
	GatewayClass string `json:"gatewayClass,omitempty"`
	// +optional
	TLS TLSSpec `json:"tls,omitempty"`
}

// DatabaseSpec configures the CloudNativePG PostgreSQL cluster.
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
	// Enabled gates the funicular-collector DaemonSet. Defaults to false
	// because the upstream `ghcr.io/openchami/funicular-collector` image
	// is not yet published; deployments that flip this on must also set
	// .image to point at a working collector. When false, LogCollectorReady
	// reaches True with reason=Ready/message="logging disabled".
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// LogBucket is the S3 bucket for collected logs.
	// Defaults to "{clusterName}-logs".
	// +optional
	LogBucket string `json:"logBucket,omitempty"`

	// RetentionDays is how long Parquet objects are kept.
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

	// Image overrides the funicular-collector container image. Required in
	// practice while no public default image exists; the reconciler will
	// emit LogCollectorReady=False/Reason=ImageNotConfigured when Enabled
	// is true and Image is unset.
	// +optional
	Image *ImageSpec `json:"image,omitempty"`
}

// ObservabilitySpec configures observability integrations.
type ObservabilitySpec struct {
	// PrometheusOperator enables ServiceMonitor creation.
	// Requires prometheus-operator CRDs to be installed.
	// +optional
	PrometheusOperator bool `json:"prometheusOperator,omitempty"`
}

// OpenCHAMIClusterSpec defines the desired state of an OpenCHAMI service cluster.
type OpenCHAMIClusterSpec struct {
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

	Platform      PlatformSpec      `json:"platform"`
	NetworkProbe  NetworkProbeSpec  `json:"networkProbe,omitempty"`
	Services      ServicesSpec      `json:"services,omitempty"`
	Networking    NetworkingSpec    `json:"networking,omitempty"`
	Database      DatabaseSpec      `json:"database,omitempty"`
	Logging       LoggingSpec       `json:"logging,omitempty"`
	Observability ObservabilitySpec `json:"observability,omitempty"`

	// OperatorChannel controls reconcile behaviour.
	// "stable" — always reconcile.
	// "pinned" — only reconcile if PinnedVersion matches the running operator version.
	// +kubebuilder:validation:Enum=stable;pinned
	// +kubebuilder:default=stable
	OperatorChannel string `json:"operatorChannel,omitempty"`

	// PinnedVersion is the exact operator semver required when OperatorChannel=pinned.
	// +optional
	PinnedVersion string `json:"pinnedVersion,omitempty"`
}

// ServiceStatus reports runtime state for a single operator-managed service.
type ServiceStatus struct {
	Ready    bool   `json:"ready"`
	Endpoint string `json:"endpoint,omitempty"`
	Message  string `json:"message,omitempty"`
}

// NetworkProbeStatus reports which nodes currently pass each network probe.
type NetworkProbeStatus struct {
	// NodesWithProvisionAccess lists node names where the provision/PXE probe passes.
	// +optional
	NodesWithProvisionAccess []string `json:"nodesWithProvisionAccess,omitempty"`

	// NodesWithBMCAccess lists node names where the BMC probe passes.
	// +optional
	NodesWithBMCAccess []string `json:"nodesWithBMCAccess,omitempty"`

	// ProbeReady is true when the DaemonSet is running and at least one node
	// passes each configured probe type.
	ProbeReady bool `json:"probeReady"`
}

// ClusterPhase is the high-level lifecycle phase of an OpenCHAMICluster.
type ClusterPhase string

const (
	PhaseProvisioning ClusterPhase = "Provisioning"
	PhaseReady        ClusterPhase = "Ready"
	PhaseDegraded     ClusterPhase = "Degraded"
	PhaseDeleting     ClusterPhase = "Deleting"
	PhaseFailed       ClusterPhase = "Failed"
)

// OpenCHAMIClusterStatus defines the observed state of OpenCHAMICluster.
type OpenCHAMIClusterStatus struct {
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

	// NetworkProbe reports probe state for each node.
	// +optional
	NetworkProbe *NetworkProbeStatus `json:"networkProbe,omitempty"`

	// TopologyVersion is the SHA-256 of the current topology ConfigMap content.
	// +optional
	TopologyVersion string `json:"topologyVersion,omitempty"`

	// ManagedByVersion is the operator semver that last reconciled this cluster.
	// +optional
	ManagedByVersion string `json:"managedByVersion,omitempty"`

	// CertExpiryTime is the RFC3339 timestamp of the soonest-expiring TLS certificate.
	// +optional
	CertExpiryTime string `json:"certExpiryTime,omitempty"`

	// LogBucket is the resolved S3 bucket name for log collection.
	// +optional
	LogBucket string `json:"logBucket,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Domain",type=string,JSONPath=`.spec.domain`
// +kubebuilder:printcolumn:name="Operator",type=string,JSONPath=`.status.managedByVersion`
// +kubebuilder:printcolumn:name="CertExpiry",type=string,JSONPath=`.status.certExpiryTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`

// OpenCHAMICluster is the Schema for the openchamiclusters API.
type OpenCHAMICluster struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec OpenCHAMIClusterSpec `json:"spec"`

	// +optional
	Status OpenCHAMIClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OpenCHAMIClusterList contains a list of OpenCHAMICluster.
type OpenCHAMIClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OpenCHAMICluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OpenCHAMICluster{}, &OpenCHAMIClusterList{})
}
