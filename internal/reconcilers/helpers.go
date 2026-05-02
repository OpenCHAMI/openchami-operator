/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

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

	openahamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
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
	cluster *openahamiv1alpha1.OpenCHAMICluster,
	probeType string, // "provision" or "bmc"
) map[string]string {
	if cluster.Spec.NetworkProbe.Enabled {
		key := fmt.Sprintf("openchami.org/%s/%s-network-ready",
			cluster.Spec.ClusterName, probeType)
		return map[string]string{key: "true"}
	}
	switch probeType {
	case "provision":
		return cluster.Spec.Services.CoreDHCP.NodeSelector
	case "bmc":
		return cluster.Spec.Services.Magellan.NodeSelector
	}
	return nil
}

// ClusterNamespace returns the canonical Kubernetes namespace for a cluster.
func ClusterNamespace(cluster *openahamiv1alpha1.OpenCHAMICluster) string {
	return "openchami-" + cluster.Spec.ClusterName
}

// SecretName returns the canonical name for a per-cluster Kubernetes Secret
// (also used as the VSO destination Secret name).
func SecretName(cluster *openahamiv1alpha1.OpenCHAMICluster, suffix string) string {
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

// BootBucketName returns the S3 bucket name for boot images.
func BootBucketName(cluster *openahamiv1alpha1.OpenCHAMICluster) string {
	if cluster.Spec.Platform.ObjectStorage.Bucket != "" {
		return cluster.Spec.Platform.ObjectStorage.Bucket
	}
	return cluster.Spec.ClusterName + "-boot-images"
}

// LogBucketName returns the S3 bucket name for log collection.
func LogBucketName(cluster *openahamiv1alpha1.OpenCHAMICluster) string {
	if cluster.Spec.Logging.LogBucket != "" {
		return cluster.Spec.Logging.LogBucket
	}
	return cluster.Spec.ClusterName + "-logs"
}

// VaultEgressPeer returns the appropriate NetworkPolicyPeer for Vault egress.
//
// If the Vault address resolves to a .svc.cluster.local hostname, a
// namespaceSelector peer is returned (same-cluster deployment).
// Otherwise the hostname is resolved to an IP and an ipBlock /32 peer
// is returned (cross-cluster deployment).
func VaultEgressPeer(vaultAddress string) (networkingv1.NetworkPolicyPeer, error) {
	u, err := url.Parse(vaultAddress)
	if err != nil {
		return networkingv1.NetworkPolicyPeer{}, fmt.Errorf("parsing vault address: %w", err)
	}
	host := u.Hostname()

	// Same-cluster: hostname ends with .svc.cluster.local
	if strings.HasSuffix(host, ".svc.cluster.local") {
		parts := strings.Split(host, ".")
		// parts: [service, namespace, svc, cluster, local]
		if len(parts) < 5 {
			return networkingv1.NetworkPolicyPeer{}, fmt.Errorf("unexpected vault svc hostname: %s", host)
		}
		ns := parts[1]
		nsSelector := &networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"kubernetes.io/metadata.name": ns,
				},
			},
		}
		return *nsSelector, nil
	}

	// Cross-cluster: resolve to IP and use ipBlock
	addrs, err := net.LookupHost(host)
	if err != nil {
		return networkingv1.NetworkPolicyPeer{}, fmt.Errorf("resolving vault host %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return networkingv1.NetworkPolicyPeer{}, fmt.Errorf("vault host %q resolved to no addresses", host)
	}
	cidr := addrs[0] + "/32"
	return networkingv1.NetworkPolicyPeer{
		IPBlock: &networkingv1.IPBlock{CIDR: cidr},
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
