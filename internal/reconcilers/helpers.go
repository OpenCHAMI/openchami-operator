// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"fmt"
	"net"
	"net/url"
	"os"
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
	cp *openchamiv1alpha1.OpenCHAMIControlPlane,
	probeType string, // "provision" or "bmc"
) map[string]string {
	if cp.Spec.NetworkProbe.Enabled {
		key := fmt.Sprintf(probeNetworkReadyLabelFmt,
			cp.Spec.ClusterName, probeType)
		return map[string]string{key: probeLabelValueTrue}
	}
	switch probeType {
	case probeTypeProvision:
		return cp.Spec.Services.CoreDHCP.NodeSelector
	case probeTypeBMC:
		return cp.Spec.Services.Magellan.NodeSelector
	}
	return nil
}

// ControlPlaneNamespace returns the canonical Kubernetes namespace for a cluster.
func ControlPlaneNamespace(cp *openchamiv1alpha1.OpenCHAMIControlPlane) string {
	return "openchami-" + cp.Spec.ClusterName
}

// SecretName returns the canonical name for a per-cluster Kubernetes Secret
// (also used as the VSO destination Secret name).
func SecretName(cp *openchamiv1alpha1.OpenCHAMIControlPlane, suffix string) string {
	return "openchami-" + cp.Spec.ClusterName + "-" + suffix
}

var boolTrue = true

// SuffixSMDDB, SuffixBootServiceDB, SuffixS3Credentials, etc. name the
// standard VSO-synced secrets in each cluster namespace.
//
// Database credentials are split per-service so CNPG's managed.roles
// declarative role management (introduced 2026-05-08, modeled on
// kube-deploy) can reference each service role's password Secret
// directly. Each per-service Secret carries `username` and `password`
// keys — the kubernetes.io/basic-auth shape CNPG and the consumer
// Deployments both expect.
const (
	SuffixSMDDB                  = "smd-db"
	SuffixBootServiceDB          = "boot-service-db"
	SuffixS3Credentials          = "s3-credentials"
	SuffixLogCredentials         = "log-credentials"
	SuffixTokensmithOIDC         = "tokensmith-oidc"
	SuffixBootServiceBootstr     = "boot-service-bootstrap-token"
	SuffixMetadataServiceBootstr = "metadata-service-bootstrap-token"

	// SuffixServiceIdentityCA is the secret holding the per-cluster
	// service-identity CA (kubernetes.io/tls keys plus a ca.crt copy).
	// Mirrored into a ConfigMap of the same name so BackendTLSPolicy's
	// caCertificateRefs (Core support: ConfigMap with key ca.crt) can
	// reference it without granting envoy gateway Secret read access.
	SuffixServiceIdentityCA = "service-identity-ca"

	// SuffixTokensmithServerTLS is the secret holding tokensmith's
	// HTTPS server cert+key (signed by the service-identity CA).
	// Mounted into the tokensmith pod at TOKENSMITH_TLS_CERT_FILE /
	// TOKENSMITH_TLS_KEY_FILE.
	SuffixTokensmithServerTLS = "tokensmith-server-tls"

	// SuffixBootServiceIdentity and SuffixMetadataServiceIdentity are
	// the secrets holding per-consumer mTLS client certs whose
	// Subject.CommonName matches the tokensmith bootstrap-token-policy
	// subject. The consumer's tokensmith client library presents
	// these to tokensmith's /service-identity/session endpoint to
	// obtain a fresh access+refresh pair on every startup — replacing
	// the single-use RFC 8693 bootstrap-token flow.
	SuffixBootServiceIdentity     = "boot-service-identity"
	SuffixMetadataServiceIdentity = "metadata-service-identity"

	// ServiceIdentityCAKey is the data key the operator stores the
	// CA certificate under in both the CA Secret and the mirrored
	// ConfigMap. Matches the BackendTLSPolicy "Core" support level
	// requirement for ConfigMap CA references.
	ServiceIdentityCAKey = "ca.crt"

	// VaultKeyDBUsername / VaultKeyDBPassword are the keys inside each
	// per-service db Secret. They match what CNPG's RoleConfiguration.PasswordSecret
	// reads (`password`) and what Deployment env-vars map to via
	// SecretKeyRef (`username` for *_DBUSER, `password` for *_DBPASS).
	VaultKeyDBUsername = "username"
	VaultKeyDBPassword = "password"

	// BootstrapTokenKey is the key inside the boot-service bootstrap
	// Secret. boot-service reads its `tokensmith-bootstrap-token` flag
	// from this value via SecretKeyRef.
	BootstrapTokenKey = "token"

	// BootstrapTokenMintedAtAnnotation records when the operator last
	// minted a fresh bootstrap token via tokensmith. The reconciler
	// re-mints when the timestamp is missing or older than
	// bootstrapTokenRefreshAge — see internal/reconcilers/tokensmith.go.
	BootstrapTokenMintedAtAnnotation = "openchami.org/bootstrap-token-minted-at"
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
	ServiceLogqCompactor   = "logq-compactor"
	ServiceLogqQuery       = "logq-query"
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
func ServiceURL(cp *openchamiv1alpha1.OpenCHAMIControlPlane, svc string) string {
	if ext := externalEndpointFor(cp, svc); ext != "" {
		return ext
	}
	port := servicePort(svc)
	if port == 0 {
		return ""
	}
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", svc, ControlPlaneNamespace(cp), port)
}

// TokensmithBaseURL is the canonical https:// URL the operator advertises
// for tokensmith. Distinct from ServiceURL(cp, ServiceTokensmith) only in
// the scheme — service-identity has already pinned tokensmith's single
// listener to HTTPS, so every consumer and the gateway's JWT provider
// must dial https, never http. When the site has supplied an
// externalEndpoint we honour it verbatim; otherwise we synthesise the
// in-cluster .svc URL with https://.
func TokensmithBaseURL(cp *openchamiv1alpha1.OpenCHAMIControlPlane) string {
	if ext := externalEndpointFor(cp, ServiceTokensmith); ext != "" {
		return ext
	}
	port := servicePort(ServiceTokensmith)
	if port == 0 {
		return ""
	}
	return fmt.Sprintf("https://%s.%s.svc.cluster.local:%d",
		ServiceTokensmith, ControlPlaneNamespace(cp), port)
}

// externalEndpointFor returns spec.services.<svc>.externalEndpoint when set,
// or "" otherwise. Centralised so the per-service spec walking lives in one
// place; callers must not reach into spec.services themselves.
func externalEndpointFor(cp *openchamiv1alpha1.OpenCHAMIControlPlane, svc string) string {
	s := &cp.Spec.Services
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
func ServiceDeployedInCluster(cp *openchamiv1alpha1.OpenCHAMIControlPlane, svc string) bool {
	s := &cp.Spec.Services
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

// ServiceIdentityCASecretName returns the canonical Secret name for the
// per-cluster service-identity CA (kubernetes.io/tls keys + ca.crt).
func ServiceIdentityCASecretName(cp *openchamiv1alpha1.OpenCHAMIControlPlane) string {
	return SecretName(cp, SuffixServiceIdentityCA)
}

// ServiceIdentityCAConfigMapName returns the canonical ConfigMap name for
// the mirrored CA cert that BackendTLSPolicy references.
func ServiceIdentityCAConfigMapName(cp *openchamiv1alpha1.OpenCHAMIControlPlane) string {
	return SecretName(cp, SuffixServiceIdentityCA)
}

// ServiceIdentityClientCertSecretName returns the canonical Secret name
// for a consumer service's mTLS client cert+key. Maps the service name
// to the canonical suffix; returns "" for non-mTLS services so callers
// can short-circuit at a single point.
func ServiceIdentityClientCertSecretName(cp *openchamiv1alpha1.OpenCHAMIControlPlane, svc string) string {
	switch svc {
	case ServiceBootService:
		return SecretName(cp, SuffixBootServiceIdentity)
	case ServiceMetadataService:
		return SecretName(cp, SuffixMetadataServiceIdentity)
	}
	return ""
}

// serviceIdentityClientCertMountPath is the in-container directory where
// consumer pods see their mTLS client cert + the trusted CA. tls.crt and
// tls.key come from the per-service Secret; ca.crt comes from the
// service-identity CA Secret. See serviceIdentityVolumesAndMounts for
// the wiring.
const serviceIdentityClientCertMountPath = "/etc/openchami/service-identity"

// serviceIdentityCAMountSubPath is the subPath name inside the
// projected volume that holds the CA cert. Consumer code reads it from
// serviceIdentityClientCertMountPath + "/" + this value.
const serviceIdentityCAMountSubPath = "ca.crt"

// serviceIdentityCertMountSubPath / serviceIdentityKeyMountSubPath are
// the subPath names for the per-service client cert and key inside the
// shared projected volume.
const (
	serviceIdentityCertMountSubPath = "tls.crt"
	serviceIdentityKeyMountSubPath  = "tls.key"
)

// serviceIdentityCertFilePath / serviceIdentityKeyFilePath return the
// in-container absolute paths consumers pass to the tokensmith client
// library via the cert / key flags. The CA cert is consumed through
// the subPath system-trust mount (systemCATrustFilePath), not via an
// env var, so its in-container path is internal to the volume wiring
// and not surfaced here.
func serviceIdentityCertFilePath() string {
	return serviceIdentityClientCertMountPath + "/" + serviceIdentityCertMountSubPath
}
func serviceIdentityKeyFilePath() string {
	return serviceIdentityClientCertMountPath + "/" + serviceIdentityKeyMountSubPath
}

// systemCATrustFilePath is where consumer pods receive a copy of the
// service-identity CA inside the default Go TLS trust scan directory.
// Mounting via subPath drops a single file alongside the image's
// existing /etc/ssl/certs bundle without making the dir writable, so
// Go's loadSystemRoots() picks up our CA on top of the system roots.
// The file basename has to live under /etc/ssl/certs/ — Go's default
// linux cert dir — because we don't set SSL_CERT_DIR (which would
// REPLACE the default list and break trust for any other HTTPS
// endpoint the service talks to, e.g. an external S3).
const systemCATrustFilePath = "/etc/ssl/certs/openchami-service-identity-ca.crt"

// serviceIdentityVolumesAndMounts returns the projected volume + mounts
// every consumer pod needs to use mTLS service-identity. Two mounts:
//
//   - <serviceIdentityClientCertMountPath>/{tls.crt,tls.key,ca.crt}
//     — what the tokensmith client library reads when WithServiceIdentityCertKey
//     is set on the consumer.
//   - /etc/ssl/certs/openchami-service-identity-ca.crt — subPath mount
//     that drops the CA into Go's default trust dir so HTTPS dials to
//     tokensmith (now serving its single port over TLS) verify
//     successfully against the in-namespace CA. Side-effect-free for
//     other HTTPS endpoints since the image's own ca-certificates.crt
//     bundle stays in place.
//
// Returns (vols, mounts, true) when the service has a registered mTLS
// client cert; (nil, nil, false) otherwise. Callers that pass an
// unsupported service name get an empty result and should fall through
// to the legacy bootstrap-token path.
func serviceIdentityVolumesAndMounts(cp *openchamiv1alpha1.OpenCHAMIControlPlane, svc string) ([]corev1.Volume, []corev1.VolumeMount, bool) {
	certSecret := ServiceIdentityClientCertSecretName(cp, svc)
	if certSecret == "" {
		return nil, nil, false
	}
	caSecret := ServiceIdentityCASecretName(cp)

	// Single projected volume so all three files share one in-container
	// directory; a separate single-file subPath mount drops the CA into
	// /etc/ssl/certs for system-trust pickup. Two volumes are needed
	// because subPath mounts cannot live inside a directory mount on
	// some k8s versions (volumeMounts conflict), so we re-source the
	// CA Secret as its own volume for the trust-store mount.
	clientVol := corev1.Volume{
		Name: "service-identity",
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{
						Secret: &corev1.SecretProjection{
							LocalObjectReference: corev1.LocalObjectReference{Name: certSecret},
							Items: []corev1.KeyToPath{
								{Key: corev1.TLSCertKey, Path: serviceIdentityCertMountSubPath},
								{Key: corev1.TLSPrivateKeyKey, Path: serviceIdentityKeyMountSubPath},
							},
							Optional: &boolTrue,
						},
					},
					{
						Secret: &corev1.SecretProjection{
							LocalObjectReference: corev1.LocalObjectReference{Name: caSecret},
							Items: []corev1.KeyToPath{
								{Key: ServiceIdentityCAKey, Path: serviceIdentityCAMountSubPath},
							},
							Optional: &boolTrue,
						},
					},
				},
			},
		},
	}
	caVol := corev1.Volume{
		Name: "service-identity-ca",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: caSecret,
				Items: []corev1.KeyToPath{
					{Key: ServiceIdentityCAKey, Path: serviceIdentityCAMountSubPath},
				},
				Optional: &boolTrue,
			},
		},
	}

	readOnly := true
	clientMount := corev1.VolumeMount{
		Name:      clientVol.Name,
		MountPath: serviceIdentityClientCertMountPath,
		ReadOnly:  readOnly,
	}
	trustMount := corev1.VolumeMount{
		Name:      caVol.Name,
		MountPath: systemCATrustFilePath,
		SubPath:   serviceIdentityCAMountSubPath,
		ReadOnly:  readOnly,
	}
	return []corev1.Volume{clientVol, caVol}, []corev1.VolumeMount{clientMount, trustMount}, true
}

// serviceIdentityCATrustOnly returns the single Volume + VolumeMount
// pair needed by services that have to *trust* tokensmith's HTTPS
// server cert but don't themselves present a client cert. SMD is the
// canonical case — it pulls JWKS from tokensmith over HTTPS to
// validate inbound JWTs, but it isn't an mTLS service-identity
// subject.
//
// Same subPath shape as serviceIdentityVolumesAndMounts uses for its
// CA mount — a single file dropped into /etc/ssl/certs/ so Go's
// loadSystemRoots() picks it up on top of the image's existing
// ca-certificates bundle. No SSL_CERT_DIR override needed.
func serviceIdentityCATrustOnly(cp *openchamiv1alpha1.OpenCHAMIControlPlane) (corev1.Volume, corev1.VolumeMount) {
	vol := corev1.Volume{
		Name: "service-identity-ca",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: ServiceIdentityCASecretName(cp),
				Items: []corev1.KeyToPath{
					{Key: ServiceIdentityCAKey, Path: serviceIdentityCAMountSubPath},
				},
				Optional: &boolTrue,
			},
		},
	}
	mount := corev1.VolumeMount{
		Name:      vol.Name,
		MountPath: systemCATrustFilePath,
		SubPath:   serviceIdentityCAMountSubPath,
		ReadOnly:  true,
	}
	return vol, mount
}

// BootBucketName returns the S3 bucket name for boot images.
func BootBucketName(cp *openchamiv1alpha1.OpenCHAMIControlPlane) string {
	if cp.Spec.Platform.ObjectStorage.Bucket != "" {
		return cp.Spec.Platform.ObjectStorage.Bucket
	}
	return cp.Spec.ClusterName + "-boot-images"
}

// LogBucketName returns the S3 bucket name for log collection.
func LogBucketName(cp *openchamiv1alpha1.OpenCHAMIControlPlane) string {
	if cp.Spec.Logging.LogBucket != "" {
		return cp.Spec.Logging.LogBucket
	}
	return cp.Spec.ClusterName + "-logs"
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
//
// When OPENCHAMI_BEST_EFFORT_DNS=true is set in the environment, an
// unresolvable host degrades to a syntax-only sentinel peer (0.0.0.0/0)
// rather than aborting the entire policy build. This is the off-cluster
// `make dev-run` mode where the operator's local resolver cannot see
// kind-network or compose-network internal hostnames; in-cluster
// deployments (the prod path) leave it unset and stay strict.
func resolveExternalPeer(host, kind string) (networkingv1.NetworkPolicyPeer, error) {
	addrs, err := net.LookupHost(host)
	if err != nil {
		if os.Getenv("OPENCHAMI_BEST_EFFORT_DNS") == "true" { //nolint:goconst // env value, not a label
			return networkingv1.NetworkPolicyPeer{
				IPBlock: &networkingv1.IPBlock{CIDR: SyntaxOnlyExternalCIDR},
			}, nil
		}
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

// DataVolumeAt returns a memory-backed emptyDir volume named "data" mounted at
// the supplied path. Services whose binaries write to a relative ./data path
// at startup (boot-service, metadata-service) need this because
// readOnlyRootFilesystem=true makes the image's WORKDIR (typically /app)
// read-only. The mount path therefore has to match the binary's WORKDIR/data
// resolution rather than the literal /data, since the upstream services
// have a viper alias bug — `mapstructure:"data_dir"` on the struct field
// vs. `--data-dir` (with a dash) on the cobra flag, no
// `viper.RegisterAlias("data_dir", "data-dir")` — that makes
// --data-dir / BOOT_SERVICE_DATA_DIR / METADATA_DATA_DIR all silently
// ignored. The only way to redirect storage is to make ./data resolve to
// a writable location, which means matching the WORKDIR.
//
// The volume is per-pod ephemeral; nothing the services write under ./data
// (cache, in-flight session state) should be expected to survive a restart.
func DataVolumeAt(mountPath string) (corev1.Volume, corev1.VolumeMount) {
	medium := corev1.StorageMediumMemory
	vol := corev1.Volume{
		Name: "data",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: medium},
		},
	}
	mount := corev1.VolumeMount{Name: "data", MountPath: mountPath}
	return vol, mount
}

// DisableServiceLinks returns the constant *bool=false that every operator-
// managed PodSpec sets for EnableServiceLinks. With it true (the kubelet
// default) Kubernetes injects <SERVICE_NAME>_PORT=tcp://...:port environment
// variables for every Service in the namespace into every Pod, which collides
// with viper/cobra-style auto-binding of --port and --host flags in the
// OpenCHAMI services. Disabling these injections is a no-op for code that
// doesn't read them (the cluster DNS still resolves Services by name) and a
// fix for code that does. This is also a CIS benchmark hardening requirement.
func DisableServiceLinks() *bool {
	v := false
	return &v
}
