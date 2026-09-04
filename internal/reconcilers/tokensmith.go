// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/logging"
)

const (
	tokensmithRequeueAfter = 30 * time.Second

	tokensmithPVCName    = "tokensmith-data"
	tokensmithDataVolume = "data"
	tokensmithDataPath   = "/var/lib/tokensmith"
	tokensmithPort       = int32(8080)
	tokensmithPortName   = "http"
	tokensmithHealthPath = "/health"

	tokensmithOIDCProviderVault    = "vault"
	tokensmithOIDCProviderExternal = "external"

	tokensmithOIDCIssuerSuffix = "/v1/identity/oidc/provider/default"

	tokensmithOIDCClientSecretKey = "client_secret"
	tokensmithOIDCIssuerEnvName   = "OIDC_ISSUER_URL"

	reasonOIDCConfigInvalid = "OIDCConfigInvalid"

	// tokensmithTLSMountPath is where the tokensmith pod sees its
	// server cert + key + CA bundle when the service-identity CA has
	// issued them. Three files: tls.crt, tls.key, ca.crt. The binary
	// reads the latter two via the TOKENSMITH_TLS_CERT_FILE /
	// TOKENSMITH_TLS_KEY_FILE / TOKENSMITH_SERVICE_IDENTITY_CA env
	// vars set below.
	tokensmithTLSMountPath = "/etc/tokensmith/tls"
	tokensmithTLSVolumeNm  = "service-identity-tls"

	tokensmithEnvTLSCertFile = "TOKENSMITH_TLS_CERT_FILE"
	tokensmithEnvTLSKeyFile  = "TOKENSMITH_TLS_KEY_FILE"
	tokensmithEnvSvcIDCA     = "TOKENSMITH_SERVICE_IDENTITY_CA"
)

// TokensmithReconciler ensures the tokensmith Deployment, PVC, Service,
// and PDB exist. Once tokensmith is Ready it also provisions fresh
// RFC 8693 bootstrap tokens for every operator-managed SMD consumer
// (boot-service, metadata-service) by exec'ing into the tokensmith
// pod (see provisionServiceBootstrapTokens) and stores the resulting
// opaque tokens in per-service Secrets that each consumer Deployment
// mounts via SecretKeyRef.
type TokensmithReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder

	// RESTConfig is the kube-client REST config used to build a
	// remotecommand executor for `kubectl exec`-style operations on
	// the tokensmith pod. Nil in unit tests; the bootstrap-token step
	// is skipped when nil so the rest of the reconciler still runs.
	RESTConfig *rest.Config
}

func (r *TokensmithReconciler) Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "tokensmith")

	if !ServiceDeployedInCluster(cp, ServiceTokensmith) {
		log.Info("tokensmith not operator-managed (disabled or external), skipping deployment")
		return ctrl.Result{}, nil
	}

	ts := cp.Spec.Services.Tokensmith
	if ts.OIDCProvider == tokensmithOIDCProviderExternal && ts.OIDCIssuerURL == "" {
		RecordConditionEvent(r.Recorder, cp, corev1.EventTypeWarning,
			reasonOIDCConfigInvalid,
			"tokensmith oidcProvider=external requires oidcIssuerURL")
		return ctrl.Result{}, fmt.Errorf("tokensmith oidcProvider=external requires oidcIssuerURL")
	}

	pvc := r.buildPVC(cp)
	logPVC := logging.EnrichWithResource(log, "PersistentVolumeClaim", pvc.Name)
	logPVC.Info("applying tokensmith PVC")
	if err := r.Client.Patch(ctx, pvc, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying tokensmith PVC: %w", err)
	}

	dep := r.buildDeployment(cp)
	logDep := logging.EnrichWithResource(log, "Deployment", dep.Name)
	logDep.Info("applying tokensmith Deployment")
	if err := r.Client.Patch(ctx, dep, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying tokensmith Deployment: %w", err)
	}

	svc := r.buildService(cp)
	logSvc := logging.EnrichWithResource(log, "Service", svc.Name)
	logSvc.Info("applying tokensmith Service")
	if err := r.Client.Patch(ctx, svc, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying tokensmith Service: %w", err)
	}

	pdb := r.buildPDB(cp)
	logPDB := logging.EnrichWithResource(log, "PodDisruptionBudget", pdb.Name)
	logPDB.Info("applying tokensmith PodDisruptionBudget")
	if err := r.Client.Patch(ctx, pdb, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying tokensmith PodDisruptionBudget: %w", err)
	}

	ns := ControlPlaneNamespace(cp)
	current := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: ServiceTokensmith}, current); err != nil {
		return ctrl.Result{}, fmt.Errorf("reading tokensmith Deployment status: %w", err)
	}

	ready := current.Status.AvailableReplicas >= 1
	scheme := "http"
	if cp.Spec.Services.Tokensmith.TLS.Enabled {
		scheme = "https"
	}
	endpoint := fmt.Sprintf("%s://%s.%s.svc.cluster.local:%d", scheme, ServiceTokensmith, ns, tokensmithPort)
	message := "tokensmith Deployment available"
	if !ready {
		message = fmt.Sprintf("waiting for tokensmith Deployment (availableReplicas=%d)", current.Status.AvailableReplicas)
	}

	if cp.Status.Services == nil {
		cp.Status.Services = map[string]openchamiv1alpha1.ServiceStatus{}
	}
	cp.Status.Services[ServiceTokensmith] = openchamiv1alpha1.ServiceStatus{
		Ready:    ready,
		Endpoint: endpoint,
		Message:  message,
	}

	if !ready {
		return ctrl.Result{RequeueAfter: tokensmithRequeueAfter}, nil
	}

	// Tokensmith is up. Provision/refresh bootstrap tokens for every
	// operator-managed SMD consumer (boot-service, metadata-service).
	// Failure here is non-fatal to tokensmith readiness — the service
	// itself is fine; the consumers will surface the missing token via
	// their own readiness — but we DO return the error so reconcile
	// retries soon. Skipped silently when RESTConfig is nil (unit tests).
	if err := r.provisionServiceBootstrapTokens(ctx, cp); err != nil {
		return ctrl.Result{RequeueAfter: tokensmithRequeueAfter},
			fmt.Errorf("provisioning service bootstrap tokens: %w", err)
	}
	// No periodic requeue: bootstrap-token provisioning is fire-and-forget
	// at this layer. Consumer-side recovery from single-use token replay
	// is intentionally NOT an operator concern (see
	// docs/portable-service-identity-prompt.md in the consumer service
	// repos for the mTLS service-identity path that replaces this whole
	// machinery).
	return ctrl.Result{}, nil
}

func (r *TokensmithReconciler) Describe(cp *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
	return []client.Object{
		r.buildPVC(cp),
		r.buildDeployment(cp),
		r.buildService(cp),
		r.buildPDB(cp),
	}, nil
}

// podLabels returns the canonical label set for tokensmith pods.
func tokensmithPodLabels(cp *openchamiv1alpha1.OpenCHAMIControlPlane) map[string]string {
	return map[string]string{
		labelAppName:   ServiceTokensmith,
		labelAppInst:   "openchami-" + cp.Spec.ClusterName,
		labelManagedBy: managedByValue,
	}
}

func (r *TokensmithReconciler) buildPVC(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      tokensmithPVCName,
			Namespace: ControlPlaneNamespace(cp),
			Annotations: map[string]string{
				"helm.sh/resource-policy": "keep",
			},
			Labels: tokensmithPodLabels(cp),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}
}

// tokensmithMTLSEnabled reports whether the service-identity flow is
// active for this cluster. When true, tokensmith is configured to:
//   - listen on HTTPS instead of HTTP
//   - use HTTPS in startup/liveness/readiness probes
//   - enable the mTLS service-identity handler by passing the CA bundle
//
// Consumers MUST present a client cert to use the service-identity endpoint.
// The legacy bootstrap-token / OAuth2 endpoints remain available on the same
// listener so existing deployments keep working — but everything goes through
// TLS now.
//
// This function reads the spec.services.tokensmith.tls.enabled field directly,
// giving users explicit control over when TLS is activated.
func tokensmithMTLSEnabled(cp *openchamiv1alpha1.OpenCHAMIControlPlane) bool {
	return cp.Spec.Services.Tokensmith.TLS.Enabled
}

func (r *TokensmithReconciler) buildDeployment(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *appsv1.Deployment {
	labels := tokensmithPodLabels(cp)
	// Replicas always 1 — tokensmith holds key material, never scaled.
	replicas := int32(1)

	image, pullPolicy := ResolveImage(cp, ServiceTokensmith)

	tmpVol, tmpMount := TmpVolume()

	// Use the spec field to determine TLS enablement rather than the
	// service-identity condition. This gives users direct control over
	// when TLS is activated.
	tlsEnabled := cp.Spec.Services.Tokensmith.TLS.Enabled

	env := []corev1.EnvVar{
		{
			Name: "OIDC_CLIENT_SECRET",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: SecretName(cp, SuffixTokensmithOIDC),
					},
					Key: tokensmithOIDCClientSecretKey,
				},
			},
		},
		// Redirect tokensmith's RFC 8693 stores and key dir to the PVC
		// mounted at tokensmithDataPath. The image declares VOLUMEs at
		// /tokensmith/{data,keys,config} but Kubernetes ignores Dockerfile
		// VOLUMEs and the readOnlyRootFilesystem=true policy makes those
		// paths unwritable. The image's entrypoint is fixed to pass
		// --config="$TOKENSMITH_CONFIG", --key-dir="$TOKENSMITH_KEY_DIR",
		// --rfc8693-bootstrap-store and --rfc8693-refresh-store on every
		// invocation, so overriding the env vars is the only way to
		// redirect them.
		//
		// TOKENSMITH_CONFIG is set to the empty string deliberately:
		// LoadFileConfig("") in tokensmith/pkg/tokenservice/config.go
		// returns DefaultFileConfig() (group scopes only) without
		// touching the filesystem. Setting any non-empty path here would
		// require us to also write a config.json into the PVC, which the
		// operator has no source of truth for.
		{Name: "TOKENSMITH_CONFIG", Value: ""},
		{Name: "TOKENSMITH_KEY_DIR", Value: tokensmithDataPath + "/keys"},
		{Name: "TOKENSMITH_RFC8693_BOOTSTRAP_STORE", Value: tokensmithDataPath + "/bootstrap-tokens"},
		{Name: "TOKENSMITH_RFC8693_REFRESH_STORE", Value: tokensmithDataPath + "/refresh-tokens"},
	}

	switch cp.Spec.Services.Tokensmith.OIDCProvider {
	case tokensmithOIDCProviderVault:
		base := strings.TrimSuffix(cp.Spec.Platform.Vault.Address, "/")
		env = append(env, corev1.EnvVar{
			Name:  tokensmithOIDCIssuerEnvName,
			Value: base + tokensmithOIDCIssuerSuffix,
		})
	case tokensmithOIDCProviderExternal:
		env = append(env, corev1.EnvVar{
			Name:  tokensmithOIDCIssuerEnvName,
			Value: cp.Spec.Services.Tokensmith.OIDCIssuerURL,
		})
	}

	if tlsEnabled {
		// PR-24's single-listener model: setting both --tls-cert-file
		// and --tls-key-file switches the same port to HTTPS, and
		// --service-identity-ca enables the
		// POST /service-identity/session handler. Without all three,
		// tokensmith stays on HTTP and consumers fall back to the
		// bootstrap-token flow. Per the prompt, mounting these files
		// via the operator-provisioned per-cluster CA is the cluster
		// equivalent of the quadlet's LoadCredential= directives.
		env = append(env,
			corev1.EnvVar{Name: tokensmithEnvTLSCertFile, Value: tokensmithTLSMountPath + "/" + corev1.TLSCertKey},
			corev1.EnvVar{Name: tokensmithEnvTLSKeyFile, Value: tokensmithTLSMountPath + "/" + corev1.TLSPrivateKeyKey},
			corev1.EnvVar{Name: tokensmithEnvSvcIDCA, Value: tokensmithTLSMountPath + "/" + ServiceIdentityCAKey},
		)
	}

	// Health probes switch to HTTPS when the service.tls.enabled field is true.
	// When TLS is enabled, kubelet validates the certificate against its trusted
	// CA bundle — the operator must ensure the service-identity CA is trusted
	// by the cluster, or the probes will fail.
	probe := func(period, failures int32) *corev1.Probe {
		scheme := corev1.URISchemeHTTP
		if cp.Spec.Services.Tokensmith.TLS.Enabled {
			scheme = corev1.URISchemeHTTPS
		}
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   tokensmithHealthPath,
					Port:   intstr.FromString(tokensmithPortName),
					Scheme: scheme,
				},
			},
			PeriodSeconds:    period,
			FailureThreshold: failures,
		}
	}
	startup := probe(5, 30)
	liveness := probe(20, 3)
	readiness := probe(10, 3)

	volumes := []corev1.Volume{
		tmpVol,
		{
			Name: tokensmithDataVolume,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: tokensmithPVCName,
				},
			},
		},
	}
	mounts := []corev1.VolumeMount{
		tmpMount,
		{
			Name:      tokensmithDataVolume,
			MountPath: tokensmithDataPath,
		},
	}
	if tlsEnabled {
		// Projected volume so tls.crt / tls.key come from the server-cert
		// Secret and ca.crt comes from the CA Secret — without forcing
		// cert-manager to write ca.crt into every leaf Secret
		// (which it does anyway when the issuer is a CA Issuer, but
		// projecting decouples us from that implementation detail).
		volumes = append(volumes, corev1.Volume{
			Name: tokensmithTLSVolumeNm,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{
							Secret: &corev1.SecretProjection{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: serviceIdentityServerCertSecretName(cp),
								},
								Items: []corev1.KeyToPath{
									{Key: corev1.TLSCertKey, Path: corev1.TLSCertKey},
									{Key: corev1.TLSPrivateKeyKey, Path: corev1.TLSPrivateKeyKey},
								},
							},
						},
						{
							Secret: &corev1.SecretProjection{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: ServiceIdentityCASecretName(cp),
								},
								Items: []corev1.KeyToPath{
									{Key: ServiceIdentityCAKey, Path: ServiceIdentityCAKey},
								},
							},
						},
					},
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      tokensmithTLSVolumeNm,
			MountPath: tokensmithTLSMountPath,
			ReadOnly:  true,
		})
	}

	container := corev1.Container{
		Name:            ServiceTokensmith,
		Image:           image,
		ImagePullPolicy: pullPolicy,
		SecurityContext: CommonSecurityContext(),
		Ports: []corev1.ContainerPort{{
			Name:          tokensmithPortName,
			ContainerPort: tokensmithPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		Env:            env,
		VolumeMounts:   mounts,
		StartupProbe:   startup,
		LivenessProbe:  liveness,
		ReadinessProbe: readiness,
	}
	if res := cp.Spec.Services.Tokensmith.Resources; res != nil {
		container.Resources = *res
	}

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: appsAPIVersion, Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceTokensmith,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: ServiceTokensmith,
					EnableServiceLinks: DisableServiceLinks(),
					SecurityContext:    CommonPodSecurityContext(),
					Containers:         []corev1.Container{container},
					Volumes:            volumes,
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
						MaxSkew:           1,
						TopologyKey:       topologyHostKey,
						WhenUnsatisfiable: corev1.ScheduleAnyway,
						LabelSelector:     &metav1.LabelSelector{MatchLabels: labels},
					}},
				},
			},
		},
	}
}

func (r *TokensmithReconciler) buildService(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *corev1.Service {
	labels := tokensmithPodLabels(cp)
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: coreAPIVersion, Kind: kindService},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceTokensmith,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       tokensmithPortName,
				Port:       tokensmithPort,
				TargetPort: intstr.FromString(tokensmithPortName),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

func (r *TokensmithReconciler) buildPDB(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *policyv1.PodDisruptionBudget {
	labels := tokensmithPodLabels(cp)
	minAvail := intstr.FromInt32(1)
	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{APIVersion: policyAPIVersion, Kind: "PodDisruptionBudget"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceTokensmith,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    labels,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvail,
			Selector:     &metav1.LabelSelector{MatchLabels: labels},
		},
	}
}
