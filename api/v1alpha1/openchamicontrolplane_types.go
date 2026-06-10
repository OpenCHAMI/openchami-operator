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

// ImageStream selects which set of upstream tags the operator picks
// from when a per-service ImageSpec doesn't override the tag.
//
//   - "release" — the curated set of tags shipped with this operator
//     version (see SERVICES.md). Reproducible across reconciles, bumps
//     only when the operator itself is upgraded. Default.
//   - "bleedingEdge" — `:latest` for every service. Each pod restart
//     re-pulls (PullPolicy=Always is applied automatically). Useful
//     for development, never for production.
//   - "pinned" — operator reads tags from Spec.Images.Pinned. Every
//     enabled service must have an entry; validating webhook rejects
//     a CR with gaps.
//
// In all streams, a per-service `image.tag` override still wins. The
// stream only controls the *default* tag used when no override is set.
//
// +kubebuilder:validation:Enum=release;bleedingEdge;pinned
type ImageStream string

const (
	ImageStreamRelease      ImageStream = "release"
	ImageStreamBleedingEdge ImageStream = "bleedingEdge"
	ImageStreamPinned       ImageStream = "pinned"
)

// ImagesSpec controls how the operator picks container image tags
// across the managed service set. The default — empty struct, with
// Stream defaulting to "release" — is the reproducible safe choice.
//
// Streams resolve **tags only**. The image repository for each
// service still comes from the per-service `image.repository`
// override (when set) or the operator-built-in default
// (`ghcr.io/openchami/<name>` for most services).
type ImagesSpec struct {
	// +kubebuilder:default=release
	// +optional
	Stream ImageStream `json:"stream,omitempty"`

	// Pinned is required when Stream=pinned. Keys are the operator's
	// canonical service names: smd, tokensmith, bootService,
	// metadataService, coredhcp, magellan, funicular, networkProbe.
	// Each value is a tag (e.g. "v1.2.3"), not a full image reference.
	// +optional
	Pinned map[string]string `json:"pinned,omitempty"`
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

	// Image overrides the network-probe container image. The probe runs
	// the operator's own binary (with subcommand `probe`); the default
	// is therefore the operator image itself.
	// +optional
	Image *ImageSpec `json:"image,omitempty"`
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

	// PreserveClientIP exposes metadata-service as a NodePort Service with
	// `externalTrafficPolicy: Local`, so the per-node source IP that
	// cloud-init / metadata-service uses to identify the requesting node
	// is preserved end-to-end. Default false: traffic flows through the
	// Envoy Gateway and the operator-managed Service stays ClusterIP.
	//
	// Set this to true in deployments where compute nodes reach
	// metadata-service directly over L2/L3 without a WireGuard tunnel
	// (the kube-deploy default for cloud-init). Without it, kube-proxy
	// SNATs the request to a node-network IP and metadata-service
	// cannot recognise the caller.
	//
	// +kubebuilder:default=false
	// +optional
	PreserveClientIP bool `json:"preserveClientIP,omitempty"`
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

	// CompactorEnabled gates the logq-compactor CronJob that converts NDJSON
	// to Parquet daily. Defaults to false. When true, requires CompactorImage
	// to be set (no default image published yet).
	// +kubebuilder:default=false
	// +optional
	CompactorEnabled bool `json:"compactorEnabled,omitempty"`

	// CompactorImage overrides the logq-compactor container image.
	// +optional
	CompactorImage *ImageSpec `json:"compactorImage,omitempty"`

	// CompactorSchedule is the cron schedule for the compactor CronJob.
	// Defaults to "0 2 * * *" (daily at 2 AM).
	// +kubebuilder:default="0 2 * * *"
	// +optional
	CompactorSchedule string `json:"compactorSchedule,omitempty"`

	// QueryEnabled gates the logq-query Deployment that provides a query
	// service for Parquet logs. Defaults to false. When true, requires
	// QueryImage to be set (no default image published yet).
	// +kubebuilder:default=false
	// +optional
	QueryEnabled bool `json:"queryEnabled,omitempty"`

	// QueryImage overrides the logq-query container image.
	// +optional
	QueryImage *ImageSpec `json:"queryImage,omitempty"`

	// QueryReplicas is the number of logq-query pods to run.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5
	// +kubebuilder:default=1
	// +optional
	QueryReplicas int32 `json:"queryReplicas,omitempty"`

	// QueryPort is the HTTP port the query service listens on.
	// +kubebuilder:default=8080
	// +optional
	QueryPort int32 `json:"queryPort,omitempty"`
}

// ObservabilitySpec configures observability integrations.
type ObservabilitySpec struct {
	// PrometheusOperator enables ServiceMonitor creation.
	// Requires prometheus-operator CRDs to be installed.
	// +optional
	PrometheusOperator bool `json:"prometheusOperator,omitempty"`
}

// OpenCHAMIControlPlaneSpec defines the desired state of an OpenCHAMI control plane.
type OpenCHAMIControlPlaneSpec struct {
	// ClusterName is the logical name of the HPC cluster this control plane
	// serves. Used as the suffix in per-control-plane namespaces, Vault paths,
	// and labels. Immutable after creation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]*$`
	ClusterName string `json:"clusterName"`

	// Notes is free-form CommonMark markdown describing this control plane:
	// site context, contact, deployment history, known quirks. Surfaced
	// verbatim by `ochami-admin describe` and untouched by any reconciler.
	// Purely human-facing — no field on it is parsed or validated beyond
	// the length cap.
	// +kubebuilder:validation:MaxLength=8192
	// +optional
	Notes string `json:"notes,omitempty"`

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
	Images        ImagesSpec        `json:"images,omitempty"`

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

// GatewayStatus reports the operator-managed Envoy Gateway's externally
// reachable URL and the per-service paths the operator mounts on it.
//
// Populated once the Gateway resource reports Programmed=True; nil
// while the gateway is still being provisioned (or when the cluster's
// `spec.networking.gatewayClass` is unset). Clients construct a
// concrete request URL as `URL + Routes[<key>]` — e.g.
//
//	curl "$(.status.gateway.url)$(.status.gateway.routes.smd)/hsm/v2/State/Components"
//
// Services that are deployed under `externalEndpoint` are omitted from
// Routes: the operator's gateway does not proxy them, and clients
// should hit the external URL directly via `.status.services[<svc>].endpoint`.
type GatewayStatus struct {
	// URL is the canonical https:// origin clients use to reach the
	// operator-managed control plane. Hostname matches `spec.domain`;
	// scheme is always https (the operator's HTTP listener
	// 301-redirects to HTTPS unconditionally).
	URL string `json:"url"`

	// Routes maps stable canonical keys to the HTTPRoute path prefix
	// (or exact path, for the few exact-match routes tokensmith
	// mounts at the origin root). The keys are stable across operator
	// releases so consumers can rely on lookups by name.
	//
	// Currently exposed keys:
	//
	//   - smd                    → SMD HSM API base prefix
	//   - boot-service           → boot-service iPXE/bootscript base prefix
	//   - metadata-service       → cloud-init metadata (public) prefix
	//   - metadata-service-admin → cloud-init admin prefix (JWT-gated)
	//   - tokensmith-jwks        → OIDC JWKS discovery path (exact)
	//   - tokensmith-token       → RFC 8693 token-exchange path (exact)
	//
	// +optional
	Routes map[string]string `json:"routes,omitempty"`
}

// ClusterPhase is the high-level lifecycle phase of an OpenCHAMIControlPlane.
type ClusterPhase string

const (
	PhaseProvisioning ClusterPhase = "Provisioning"
	PhaseReady        ClusterPhase = "Ready"
	PhaseDegraded     ClusterPhase = "Degraded"
	PhaseDeleting     ClusterPhase = "Deleting"
	PhaseFailed       ClusterPhase = "Failed"
)

// OpenCHAMIControlPlaneStatus defines the observed state of OpenCHAMIControlPlane.
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

	// Gateway reports the canonical https:// ingress URL and the
	// per-service path map clients should use to reach operator-managed
	// services. Populated once the Envoy Gateway resource reports
	// Programmed=True; nil while still provisioning.
	// +optional
	Gateway *GatewayStatus `json:"gateway,omitempty"`

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
// +kubebuilder:resource:shortName=ocp;ocps
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Domain",type=string,JSONPath=`.spec.domain`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.gateway.url`
// +kubebuilder:printcolumn:name="Operator",type=string,JSONPath=`.status.managedByVersion`
// +kubebuilder:printcolumn:name="CertExpiry",type=string,JSONPath=`.status.certExpiryTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`

// OpenCHAMIControlPlane is the Schema for the openchamicontrolplanes API.
type OpenCHAMIControlPlane struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec OpenCHAMIControlPlaneSpec `json:"spec"`

	// +optional
	Status OpenCHAMIControlPlaneStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OpenCHAMIControlPlaneList contains a list of OpenCHAMIControlPlane.
type OpenCHAMIControlPlaneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OpenCHAMIControlPlane `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OpenCHAMIControlPlane{}, &OpenCHAMIControlPlaneList{})
}
