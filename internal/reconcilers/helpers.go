// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

// RunbookURL returns the canonical runbook URL for a given Event reason.
// All operator Events must link to a runbook.
func RunbookURL(reason string) string {
	slug := strings.ToLower(strings.ReplaceAll(reason, "_", "-"))
	return "https://openchami.org/docs/ops/" + slug
}

// RecordConditionEvent records a Kubernetes Event with a runbook URL appended.
// All sub-reconcilers must use this function. Never call recorder.Event directly.
func RecordConditionEvent(
	recorder record.EventRecorder,
	obj client.Object,
	eventType, reason, message string,
) {
	full := fmt.Sprintf("%s — Runbook: %s", message, RunbookURL(reason))
	recorder.Event(obj, eventType, reason, full)
}

// EffectiveNodeSelector returns the computed nodeSelector for CoreDHCP
// (probeType="provision") or Magellan (probeType="bmc").
//
// When spec.networkProbe.enabled is true, returns a label selector based
// on the probe-applied node labels, which are set automatically by the
// network probe DaemonSet.
//
// When spec.networkProbe.enabled is false, returns the manually specified
// NodeSelector from the relevant service spec.
func EffectiveNodeSelector(
	cluster *openchamiv1alpha1.OpenCHAMICluster,
	probeType string, // "provision" or "bmc"
) map[string]string {
	if cluster.Spec.NetworkProbe.Enabled {
		key := fmt.Sprintf(probeNetworkReadyLabelFmt,
			cluster.Spec.ClusterName, probeType)
		return map[string]string{key: probeLabelValueTrue}
	}
	switch probeType {
	case probeTypeProvision:
		return cluster.Spec.Services.CoreDHCP.NodeSelector
	case probeTypeBMC:
		return cluster.Spec.Services.Magellan.NodeSelector
	}
	return nil
}

// ClusterNamespace returns the canonical Kubernetes namespace for a cluster.
func ClusterNamespace(cluster *openchamiv1alpha1.OpenCHAMICluster) string {
	return "openchami-" + cluster.Spec.ClusterName
}

// SecretName returns the canonical name for a per-cluster Kubernetes Secret
// (also used as the VSO destination Secret name).
func SecretName(cluster *openchamiv1alpha1.OpenCHAMICluster, suffix string) string {
	return "openchami-" + cluster.Spec.ClusterName + "-" + suffix
}

// SuffixDBCredentials, SuffixS3Credentials, etc. name the standard
// VSO-synced secrets in each cluster namespace.
const (
	SuffixDBCredentials  = "db-credentials"
	SuffixS3Credentials  = "s3-credentials"
	SuffixLogCredentials = "log-credentials"
	SuffixTokensmithOIDC = "tokensmith-oidc"

	// VaultKeySMDPassword and VaultKeyBootServicePassword are the keys
	// inside the db-credentials KV/Secret.
	VaultKeySMDPassword         = "SMD_DB_PASSWORD"
	VaultKeyBootServicePassword = "BOOT_SERVICE_DB_PASSWORD"
)

// Canonical service names. Used as ServiceAccount names, database owners,
// and component-specific secret keys.
const (
	ServiceSMD             = "smd"
	ServiceTokensmith      = "tokensmith"
	ServiceBootService     = "boot-service"
	ServiceMetadataService = "metadata-service"
	ServiceCoreDHCP        = "coredhcp"
	ServiceMagellan        = "magellan"
	ServiceNetworkProbe    = "network-probe"
	ServiceFunicular       = "funicular-collector"
)

// servicePort returns the canonical container port for an HTTP-facing
// OpenCHAMI service. Used by ServiceURL() to assemble the in-cluster URL
// when no externalEndpoint override is set. Keep in sync with the per-
// reconciler port constants (smdPort, tokensmithPort, bootServicePort,
// metadataServicePort).
func servicePort(svc string) int32 {
	switch svc {
	case ServiceSMD:
		return 27779
	case ServiceTokensmith:
		return 8080
	case ServiceBootService:
		return 27778
	case ServiceMetadataService:
		return 27770
	}
	return 0
}

// ServiceURL returns the canonical http(s) URL for one of cluster's
// services. When a site has set spec.services.<svc>.externalEndpoint, that
// URL is returned verbatim (no path or trailing slash added). Otherwise
// the operator-managed in-cluster URL is returned:
//
//	http://<svc>.openchami-<cluster>.svc.cluster.local:<port>
//
// `svc` must be one of the ServiceSMD/Tokensmith/BootService/MetadataService
// constants; passing any other value returns "" so callers fail loudly.
//
// This helper is the single hook every consumer-side reconciler must use to
// wire upstream URLs into env vars. New consumers must call ServiceURL,
// never the in-cluster template directly, so that externalEndpoint is
// honoured uniformly. The unit test
// TestServiceURL_HonoursExternalEndpoint asserts the override path.
func ServiceURL(cluster *openchamiv1alpha1.OpenCHAMICluster, svc string) string {
	if ext := externalEndpointFor(cluster, svc); ext != "" {
		return ext
	}
	port := servicePort(svc)
	if port == 0 {
		return ""
	}
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", svc, ClusterNamespace(cluster), port)
}

// externalEndpointFor returns spec.services.<svc>.externalEndpoint when set,
// or "" otherwise. Centralised so the per-service spec walking lives in one
// place; callers must not reach into spec.services themselves.
func externalEndpointFor(cluster *openchamiv1alpha1.OpenCHAMICluster, svc string) string {
	s := &cluster.Spec.Services
	switch svc {
	case ServiceSMD:
		if s.SMD.ExternalEndpoint != nil {
			return *s.SMD.ExternalEndpoint
		}
	case ServiceTokensmith:
		if s.Tokensmith.ExternalEndpoint != nil {
			return *s.Tokensmith.ExternalEndpoint
		}
	case ServiceBootService:
		if s.BootService.ExternalEndpoint != nil {
			return *s.BootService.ExternalEndpoint
		}
	case ServiceMetadataService:
		if s.MetadataService.ExternalEndpoint != nil {
			return *s.MetadataService.ExternalEndpoint
		}
	}
	return ""
}

// ServiceDeployedInCluster reports whether the given service is being
// deployed and managed by the operator for this cluster. False when the
// service is disabled OR when it is provided externally — in either case
// the sub-reconciler should not create Deployment / Service / etc.
// objects for it.
func ServiceDeployedInCluster(cluster *openchamiv1alpha1.OpenCHAMICluster, svc string) bool {
	s := &cluster.Spec.Services
	switch svc {
	case ServiceSMD:
		return s.SMD.Enabled && s.SMD.ExternalEndpoint == nil
	case ServiceTokensmith:
		return s.Tokensmith.Enabled && s.Tokensmith.ExternalEndpoint == nil
	case ServiceBootService:
		return s.BootService.Enabled && s.BootService.ExternalEndpoint == nil
	case ServiceMetadataService:
		return s.MetadataService.Enabled && s.MetadataService.ExternalEndpoint == nil
	}
	return false
}

// BootBucketName returns the S3 bucket name for boot images.
func BootBucketName(cluster *openchamiv1alpha1.OpenCHAMICluster) string {
	if cluster.Spec.Platform.ObjectStorage.Bucket != "" {
		return cluster.Spec.Platform.ObjectStorage.Bucket
	}
	return cluster.Spec.ClusterName + "-boot-images"
}

// LogBucketName returns the S3 bucket name for log collection.
func LogBucketName(cluster *openchamiv1alpha1.OpenCHAMICluster) string {
	if cluster.Spec.Logging.LogBucket != "" {
		return cluster.Spec.Logging.LogBucket
	}
	return cluster.Spec.ClusterName + "-logs"
}

// SyntaxOnlyExternalCIDR is the placeholder ipBlock used by the *Syntax
// helpers when the configured address is an external hostname that would
// otherwise require DNS resolution. Describe() must not contact remote
// services; consumers that need the resolved peer call the non-Syntax form
// inside Reconcile().
const SyntaxOnlyExternalCIDR = "0.0.0.0/0"

// parseEgressHost extracts the hostname portion of an egress URL. Returns the
// in-cluster .svc.cluster.local namespace peer when the host is a service-DNS
// name; otherwise returns the raw host so callers can decide whether to
// resolve it (Reconcile) or substitute a syntax-only placeholder (Describe).
//
// The bool inCluster is true exactly when peer is non-zero (the namespace
// selector form). When false, host holds the external hostname/IP that
// requires further handling.
func parseEgressHost(addr, kind string) (host string, peer networkingv1.NetworkPolicyPeer, inCluster bool, err error) {
	u, parseErr := url.Parse(addr)
	if parseErr != nil {
		return "", networkingv1.NetworkPolicyPeer{}, false, fmt.Errorf("parsing %s address: %w", kind, parseErr)
	}
	host = u.Hostname()

	if strings.HasSuffix(host, ".svc.cluster.local") {
		parts := strings.Split(host, ".")
		// parts: [service, namespace, svc, cluster, local]
		if len(parts) < 5 {
			return "", networkingv1.NetworkPolicyPeer{}, false, fmt.Errorf("unexpected %s svc hostname: %s", kind, host)
		}
		ns := parts[1]
		return host, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					kubernetesMetadataNameLabel: ns,
				},
			},
		}, true, nil
	}
	return host, networkingv1.NetworkPolicyPeer{}, false, nil
}

// resolveExternalPeer turns an external hostname/IP into a /32 ipBlock peer.
// Performs DNS via net.LookupHost — only call from Reconcile paths, never from
// Describe (see VaultEgressPeerSyntax / VersityGWEgressPeerSyntax).
func resolveExternalPeer(host, kind string) (networkingv1.NetworkPolicyPeer, error) {
	addrs, err := net.LookupHost(host)
	if err != nil {
		return networkingv1.NetworkPolicyPeer{}, fmt.Errorf("resolving %s host %q: %w", kind, host, err)
	}
	if len(addrs) == 0 {
		return networkingv1.NetworkPolicyPeer{}, fmt.Errorf("%s host %q resolved to no addresses", kind, host)
	}
	return networkingv1.NetworkPolicyPeer{
		IPBlock: &networkingv1.IPBlock{CIDR: addrs[0] + "/32"},
	}, nil
}

// VaultEgressPeer returns the appropriate NetworkPolicyPeer for Vault egress.
//
// If the Vault address resolves to a .svc.cluster.local hostname, a
// namespaceSelector peer is returned (same-cluster deployment).
// Otherwise the hostname is resolved to an IP and an ipBlock /32 peer
// is returned (cross-cluster deployment).
//
// This helper performs live DNS for external hostnames and so must only be
// called from a Reconcile path. Describe paths must use VaultEgressPeerSyntax.
func VaultEgressPeer(vaultAddress string) (networkingv1.NetworkPolicyPeer, error) {
	host, peer, inCluster, err := parseEgressHost(vaultAddress, "vault")
	if err != nil {
		return networkingv1.NetworkPolicyPeer{}, err
	}
	if inCluster {
		return peer, nil
	}
	return resolveExternalPeer(host, "vault")
}

// VaultEgressPeerSyntax is the syntax-only counterpart to VaultEgressPeer for
// use from Describe paths. For in-cluster .svc.cluster.local addresses it
// returns the same namespaceSelector peer; for external hostnames it returns
// a sentinel ipBlock 0.0.0.0/0 instead of performing DNS, so the SubReconciler
// "Describe must not contact any external service" contract is preserved.
//
// The sentinel is acceptable because Describe output is only ever rendered to
// a human via `ochami-admin describe`; it is never used to apply policies.
func VaultEgressPeerSyntax(vaultAddress string) (networkingv1.NetworkPolicyPeer, error) {
	_, peer, inCluster, err := parseEgressHost(vaultAddress, "vault")
	if err != nil {
		return networkingv1.NetworkPolicyPeer{}, err
	}
	if inCluster {
		return peer, nil
	}
	return networkingv1.NetworkPolicyPeer{
		IPBlock: &networkingv1.IPBlock{CIDR: SyntaxOnlyExternalCIDR},
	}, nil
}

// VersityGWEgressPeer returns the appropriate NetworkPolicyPeer for VersityGW
// (S3-compatible object store) egress.
//
// If the endpoint resolves to a .svc.cluster.local hostname, a
// namespaceSelector peer is returned (same-cluster deployment).
// Otherwise the hostname is resolved to an IP and an ipBlock /32 peer
// is returned (cross-cluster or external deployment).
//
// VersityGW is never deployed by this operator (invariant #1); this helper
// exists solely so the funicular and boot-service NetworkPolicies can permit
// egress to whichever location the cluster admin has provisioned.
//
// Describe paths must use VersityGWEgressPeerSyntax instead.
func VersityGWEgressPeer(endpoint string) (networkingv1.NetworkPolicyPeer, error) {
	host, peer, inCluster, err := parseEgressHost(endpoint, "versitygw")
	if err != nil {
		return networkingv1.NetworkPolicyPeer{}, err
	}
	if inCluster {
		return peer, nil
	}
	return resolveExternalPeer(host, "versitygw")
}

// VersityGWEgressPeerSyntax is the syntax-only counterpart to
// VersityGWEgressPeer for use from Describe paths. See VaultEgressPeerSyntax
// for the rationale.
func VersityGWEgressPeerSyntax(endpoint string) (networkingv1.NetworkPolicyPeer, error) {
	_, peer, inCluster, err := parseEgressHost(endpoint, "versitygw")
	if err != nil {
		return networkingv1.NetworkPolicyPeer{}, err
	}
	if inCluster {
		return peer, nil
	}
	return networkingv1.NetworkPolicyPeer{
		IPBlock: &networkingv1.IPBlock{CIDR: SyntaxOnlyExternalCIDR},
	}, nil
}

// CommonSecurityContext returns the standard restricted security context
// applied to all operator-managed containers.
func CommonSecurityContext() *corev1.SecurityContext {
	allowPrivilegeEscalation := false
	readOnly := true
	nonRoot := true
	uid := int64(65534)
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &allowPrivilegeEscalation,
		ReadOnlyRootFilesystem:   &readOnly,
		RunAsNonRoot:             &nonRoot,
		RunAsUser:                &uid,
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

// CommonPodSecurityContext returns the standard pod-level security context.
func CommonPodSecurityContext() *corev1.PodSecurityContext {
	nonRoot := true
	uid := int64(65534)
	gid := int64(65534)
	return &corev1.PodSecurityContext{
		RunAsNonRoot: &nonRoot,
		RunAsUser:    &uid,
		RunAsGroup:   &gid,
		FSGroup:      &gid,
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

// TmpVolume returns a memory-backed emptyDir volume named "tmp" and its mount.
// All containers must mount /tmp from this volume (readOnlyRootFilesystem=true).
func TmpVolume() (corev1.Volume, corev1.VolumeMount) {
	medium := corev1.StorageMediumMemory
	vol := corev1.Volume{
		Name: "tmp",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: medium},
		},
	}
	mount := corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"}
	return vol, mount
}
