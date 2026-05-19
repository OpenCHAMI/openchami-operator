// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"fmt"
	"time"

	cmv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/logging"
)

const (
	serviceIdentityRequeueAfter = 30 * time.Second

	// kindIssuer is the cert-manager.io/v1 Issuer Kind string used in
	// TypeMeta for server-side apply. Kept as a package-private constant
	// alongside the existing kindCertificate so the service-identity
	// reconciler's SSA Patches don't drift.
	kindIssuer = "Issuer"

	// serviceIdentityCADuration is the lifetime requested for the
	// per-cluster CA Certificate. Long enough that consumer renewal
	// noise dominates; short enough that a compromised cluster ages
	// out in a quarter without operator action.
	serviceIdentityCADuration    = 90 * 24 * time.Hour // 90 days
	serviceIdentityCARenewBefore = 21 * 24 * time.Hour // renew at T-21d

	// serviceIdentityLeafDuration is the lifetime requested for every
	// downstream Certificate (tokensmith server, per-service client
	// certs). cert-manager rotates them transparently; mTLS handshake
	// failures during the renew window are masked by the consumer's
	// retry loop in tokensmith's GetToken flow.
	serviceIdentityLeafDuration    = 30 * 24 * time.Hour // 30 days
	serviceIdentityLeafRenewBefore = 7 * 24 * time.Hour  // renew at T-7d

	// Resource-name suffixes for the two operator-owned Issuers. Both
	// live in the cluster namespace; the bootstrap issuer is consumed
	// only to mint the CA, after which the CA issuer becomes the
	// source for all leaf certificates.
	suffixServiceIdentityBootstrapIssuer = "service-identity-bootstrap"
	suffixServiceIdentityCAIssuer        = "service-identity-issuer"

	// reasonServiceIdentityIssued is the Event reason emitted when the
	// CA + all leaf Secrets have first reached existence after a
	// previously-degraded state. Single-shot per controller process.
	reasonServiceIdentityIssued = "ServiceIdentityIssued"
)

// ServiceIdentityReconciler provisions the per-cluster mTLS CA and the
// downstream server / client certificates that let consumer pods
// authenticate to tokensmith without consuming a single-use bootstrap
// token on every restart.
//
// Resource layout for cluster "alpha" (namespace "openchami-alpha"):
//
//	Issuer                       openchami-alpha-service-identity-bootstrap     (selfSigned)
//	Certificate                  openchami-alpha-service-identity-ca            (isCA, signed by bootstrap)
//	Secret/kubernetes.io/tls     openchami-alpha-service-identity-ca            (CA tls.crt+tls.key+ca.crt)
//	ConfigMap                    openchami-alpha-service-identity-ca            (mirror of ca.crt for BackendTLSPolicy)
//	Issuer                       openchami-alpha-service-identity-issuer        (ca, uses the Secret above)
//	Certificate                  openchami-alpha-tokensmith-server-tls          (server cert for tokensmith)
//	Secret/kubernetes.io/tls     openchami-alpha-tokensmith-server-tls
//	Certificate                  openchami-alpha-boot-service-identity          (client cert, CN=boot-service)
//	Secret/kubernetes.io/tls     openchami-alpha-boot-service-identity
//	Certificate                  openchami-alpha-metadata-service-identity      (client cert, CN=metadata-service)
//	Secret/kubernetes.io/tls     openchami-alpha-metadata-service-identity
//
// Idempotent end-to-end: every object is applied via server-side apply,
// every read is tolerant of NotFound (returns false + requeue rather
// than erroring), and the ConfigMap mirror is rewritten only when the
// CA Secret's ca.crt content has changed. The reconciler reports
// ConditionServiceIdentityReady=True when every Secret listed above
// exists and has been populated by cert-manager. Downstream
// reconcilers (tokensmith, gateway, boot-service, metadata-service)
// consult that condition to gate their mTLS-aware wiring.
type ServiceIdentityReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

func (r *ServiceIdentityReconciler) Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "service-identity")

	// Build + apply every cert-manager object up-front. cert-manager
	// reconciles them asynchronously; we then poll the resulting
	// Secrets below to decide whether to flip ConditionServiceIdentityReady.
	objs := r.applyOrder(cp)
	for _, obj := range objs {
		oLog := logging.EnrichWithResource(log,
			obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName())
		oLog.Info("applying service-identity resource")
		if err := r.Client.Patch(ctx, obj, client.Apply, //nolint:staticcheck // SSA via Patch
			client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
			return ctrl.Result{}, fmt.Errorf("applying %s/%s: %w",
				obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
		}
	}

	// Mirror the CA cert into a ConfigMap so BackendTLSPolicy (which
	// requires Kind=ConfigMap for "Core" support) can reference it.
	// Skipped silently when the CA Secret isn't populated yet — we'll
	// pick it up on the next reconcile after cert-manager catches up.
	caReady, err := r.mirrorCAConfigMap(ctx, cp)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("mirroring CA ConfigMap: %w", err)
	}

	// Check every leaf Secret. Missing ones are an "awaiting cert-manager"
	// state, not an error — cert-manager runs out-of-band and may need
	// a few seconds for the first issuance after the Issuer is first
	// applied. Return RequeueAfter to come back without spamming.
	missing, err := r.missingSecrets(ctx, cp)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking service-identity Secrets: %w", err)
	}

	if !caReady || len(missing) > 0 {
		msg := "waiting for cert-manager to issue service-identity certificates"
		if len(missing) > 0 {
			msg = fmt.Sprintf("%s: %v", msg, missing)
		}
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionServiceIdentityReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonAwaitingServiceIdentity,
			Message:            msg,
			ObservedGeneration: cp.Generation,
		})
		log.Info("service-identity not yet ready", "missingSecrets", missing, "caReady", caReady)
		return ctrl.Result{RequeueAfter: serviceIdentityRequeueAfter}, nil
	}

	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionServiceIdentityReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            "service-identity CA, server cert, and per-service client certs all issued",
		ObservedGeneration: cp.Generation,
	})
	RecordConditionEvent(r.Recorder, cp, corev1.EventTypeNormal,
		reasonServiceIdentityIssued,
		"service-identity CA + tokensmith server cert + per-service client certs all available")
	return ctrl.Result{}, nil
}

// Describe returns the Kubernetes objects this reconciler would apply.
func (r *ServiceIdentityReconciler) Describe(cp *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
	return r.applyOrder(cp), nil
}

// applyOrder returns the cert-manager objects in dependency order:
// bootstrap selfSigned Issuer → CA Certificate → CA Issuer → leaf
// Certificates (server + per-service clients). cert-manager's
// reconciler tolerates an Issuer being applied after the Certificate
// that references it, but ordering them by dependency keeps the
// initial reconcile from logging a flurry of "issuer not found" events.
func (r *ServiceIdentityReconciler) applyOrder(cp *openchamiv1alpha1.OpenCHAMIControlPlane) []client.Object {
	objs := []client.Object{
		r.buildBootstrapIssuer(cp),
		r.buildCACertificate(cp),
		r.buildCAIssuer(cp),
		r.buildTokensmithServerCert(cp),
	}
	if ServiceDeployedInCluster(cp, ServiceBootService) {
		objs = append(objs, r.buildClientCertificate(cp, ServiceBootService))
	}
	if ServiceDeployedInCluster(cp, ServiceMetadataService) {
		objs = append(objs, r.buildClientCertificate(cp, ServiceMetadataService))
	}
	return objs
}

// missingSecrets returns the leaf Secret names that have not yet been
// populated by cert-manager. The CA Secret is checked by mirrorCAConfigMap
// (which is the only caller that needs ca.crt content); here we only
// guard on existence and a non-empty tls.crt to avoid a race where
// cert-manager has created the Secret skeleton but not yet written the
// keys. Order is deterministic so the error message stays stable across
// reconciles.
func (r *ServiceIdentityReconciler) missingSecrets(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) ([]string, error) {
	ns := ControlPlaneNamespace(cp)
	want := []string{
		ServiceIdentityCASecretName(cp),
		serviceIdentityServerCertSecretName(cp),
	}
	if ServiceDeployedInCluster(cp, ServiceBootService) {
		want = append(want, ServiceIdentityClientCertSecretName(cp, ServiceBootService))
	}
	if ServiceDeployedInCluster(cp, ServiceMetadataService) {
		want = append(want, ServiceIdentityClientCertSecretName(cp, ServiceMetadataService))
	}

	var missing []string
	for _, name := range want {
		s := &corev1.Secret{}
		err := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, s)
		switch {
		case apierrors.IsNotFound(err):
			missing = append(missing, name)
		case err != nil:
			return nil, fmt.Errorf("reading Secret %s/%s: %w", ns, name, err)
		default:
			if len(s.Data[corev1.TLSCertKey]) == 0 {
				missing = append(missing, name)
			}
		}
	}
	return missing, nil
}

// mirrorCAConfigMap copies the CA Secret's ca.crt into a same-named
// ConfigMap so envoy gateway's BackendTLSPolicy (which can only
// reference ConfigMaps at the Core support level) can validate
// tokensmith's HTTPS listener.
//
// Returns (caReady=true, err=nil) once the ConfigMap matches the
// Secret. Returns (false, nil) when the Secret is missing or its
// ca.crt is empty — the next reconcile picks it up. SSA-patches the
// ConfigMap each time so a manually-modified value snaps back to the
// CA content.
func (r *ServiceIdentityReconciler) mirrorCAConfigMap(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (bool, error) {
	ns := ControlPlaneNamespace(cp)
	caName := ServiceIdentityCASecretName(cp)

	secret := &corev1.Secret{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: caName}, secret)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading CA Secret %s/%s: %w", ns, caName, err)
	}
	caBytes := secret.Data[ServiceIdentityCAKey]
	if len(caBytes) == 0 {
		// cert-manager hasn't written ca.crt yet — back off.
		return false, nil
	}

	cm := r.buildCAConfigMap(cp, string(caBytes))
	if err := r.Client.Patch(ctx, cm, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return false, fmt.Errorf("applying CA ConfigMap: %w", err)
	}
	return true, nil
}

func serviceIdentityBootstrapIssuerName(cp *openchamiv1alpha1.OpenCHAMIControlPlane) string {
	return SecretName(cp, suffixServiceIdentityBootstrapIssuer)
}

func serviceIdentityCAIssuerName(cp *openchamiv1alpha1.OpenCHAMIControlPlane) string {
	return SecretName(cp, suffixServiceIdentityCAIssuer)
}

func serviceIdentityCACertName(cp *openchamiv1alpha1.OpenCHAMIControlPlane) string {
	return SecretName(cp, SuffixServiceIdentityCA)
}

func serviceIdentityServerCertSecretName(cp *openchamiv1alpha1.OpenCHAMIControlPlane) string {
	return SecretName(cp, SuffixTokensmithServerTLS)
}

func serviceIdentityLabels(cp *openchamiv1alpha1.OpenCHAMIControlPlane) map[string]string {
	return map[string]string{
		labelAppName:   "service-identity",
		labelAppInst:   "openchami-" + cp.Spec.ClusterName,
		labelManagedBy: managedByValue,
	}
}

func (r *ServiceIdentityReconciler) buildBootstrapIssuer(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *cmv1.Issuer {
	return &cmv1.Issuer{
		TypeMeta: metav1.TypeMeta{APIVersion: certManagerAPIVersion, Kind: kindIssuer},
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceIdentityBootstrapIssuerName(cp),
			Namespace: ControlPlaneNamespace(cp),
			Labels:    serviceIdentityLabels(cp),
		},
		Spec: cmv1.IssuerSpec{
			IssuerConfig: cmv1.IssuerConfig{
				SelfSigned: &cmv1.SelfSignedIssuer{},
			},
		},
	}
}

func (r *ServiceIdentityReconciler) buildCACertificate(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *cmv1.Certificate {
	duration := metav1.Duration{Duration: serviceIdentityCADuration}
	renewBefore := metav1.Duration{Duration: serviceIdentityCARenewBefore}
	return &cmv1.Certificate{
		TypeMeta: metav1.TypeMeta{APIVersion: certManagerAPIVersion, Kind: kindCertificate},
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceIdentityCACertName(cp),
			Namespace: ControlPlaneNamespace(cp),
			Labels:    serviceIdentityLabels(cp),
		},
		Spec: cmv1.CertificateSpec{
			SecretName: ServiceIdentityCASecretName(cp),
			IsCA:       true,
			CommonName: "openchami-" + cp.Spec.ClusterName + "-service-identity-ca",
			// Issuer is namespaced (Issuer, not ClusterIssuer) so the
			// CA never leaks across cluster boundaries — Invariant #2.
			IssuerRef: cmmeta.IssuerReference{
				Name:  serviceIdentityBootstrapIssuerName(cp),
				Kind:  kindIssuer,
				Group: certManagerGroup,
			},
			Duration:    &duration,
			RenewBefore: &renewBefore,
			// PrivateKey defaults are fine; cert-manager uses RSA 2048
			// unless overridden — sufficient for this CA's lifetime
			// and avoids requiring a fresh CRD field for algorithm
			// selection.
			Usages: []cmv1.KeyUsage{
				cmv1.UsageCertSign,
				cmv1.UsageCRLSign,
				cmv1.UsageDigitalSignature,
			},
		},
	}
}

func (r *ServiceIdentityReconciler) buildCAIssuer(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *cmv1.Issuer {
	return &cmv1.Issuer{
		TypeMeta: metav1.TypeMeta{APIVersion: certManagerAPIVersion, Kind: kindIssuer},
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceIdentityCAIssuerName(cp),
			Namespace: ControlPlaneNamespace(cp),
			Labels:    serviceIdentityLabels(cp),
		},
		Spec: cmv1.IssuerSpec{
			IssuerConfig: cmv1.IssuerConfig{
				CA: &cmv1.CAIssuer{
					SecretName: ServiceIdentityCASecretName(cp),
				},
			},
		},
	}
}

func (r *ServiceIdentityReconciler) buildTokensmithServerCert(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *cmv1.Certificate {
	ns := ControlPlaneNamespace(cp)
	// SANs cover both the in-cluster DNS forms a client might use.
	// envoy gateway dials tokensmith via the Service DNS, so the
	// .svc and .svc.cluster.local entries are both required —
	// envoy's TLS validator matches the SNI it sets to one of these.
	dnsNames := []string{
		fmt.Sprintf("%s.%s.svc.cluster.local", ServiceTokensmith, ns),
		fmt.Sprintf("%s.%s.svc", ServiceTokensmith, ns),
		fmt.Sprintf("%s.%s", ServiceTokensmith, ns),
		ServiceTokensmith,
	}
	duration := metav1.Duration{Duration: serviceIdentityLeafDuration}
	renewBefore := metav1.Duration{Duration: serviceIdentityLeafRenewBefore}
	return &cmv1.Certificate{
		TypeMeta: metav1.TypeMeta{APIVersion: certManagerAPIVersion, Kind: kindCertificate},
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceIdentityServerCertSecretName(cp),
			Namespace: ns,
			Labels:    serviceIdentityLabels(cp),
		},
		Spec: cmv1.CertificateSpec{
			SecretName: serviceIdentityServerCertSecretName(cp),
			CommonName: ServiceTokensmith,
			DNSNames:   dnsNames,
			IssuerRef: cmmeta.IssuerReference{
				Name:  serviceIdentityCAIssuerName(cp),
				Kind:  kindIssuer,
				Group: certManagerGroup,
			},
			Duration:    &duration,
			RenewBefore: &renewBefore,
			Usages: []cmv1.KeyUsage{
				cmv1.UsageDigitalSignature,
				cmv1.UsageKeyEncipherment,
				cmv1.UsageServerAuth,
			},
		},
	}
}

// buildClientCertificate builds a per-consumer mTLS client cert whose
// Subject.CommonName matches the tokensmith bootstrap-policy subject
// for that service. tokensmith's service-identity handler extracts
// `cert.Subject.CommonName` and looks up the per-subject policy to
// derive the issued access-token's scopes — so the CN here MUST
// match the canonical service name string the bootstrap-token
// provisioner uses (see tokensmith_bootstrap.go's subject field).
func (r *ServiceIdentityReconciler) buildClientCertificate(cp *openchamiv1alpha1.OpenCHAMIControlPlane, svc string) *cmv1.Certificate {
	name := ServiceIdentityClientCertSecretName(cp, svc)
	duration := metav1.Duration{Duration: serviceIdentityLeafDuration}
	renewBefore := metav1.Duration{Duration: serviceIdentityLeafRenewBefore}
	return &cmv1.Certificate{
		TypeMeta: metav1.TypeMeta{APIVersion: certManagerAPIVersion, Kind: kindCertificate},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    serviceIdentityLabels(cp),
		},
		Spec: cmv1.CertificateSpec{
			SecretName: name,
			CommonName: svc, // subject the policy store keys on
			IssuerRef: cmmeta.IssuerReference{
				Name:  serviceIdentityCAIssuerName(cp),
				Kind:  kindIssuer,
				Group: certManagerGroup,
			},
			Duration:    &duration,
			RenewBefore: &renewBefore,
			Usages: []cmv1.KeyUsage{
				cmv1.UsageDigitalSignature,
				cmv1.UsageKeyEncipherment,
				cmv1.UsageClientAuth,
			},
		},
	}
}

func (r *ServiceIdentityReconciler) buildCAConfigMap(cp *openchamiv1alpha1.OpenCHAMIControlPlane, caPEM string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: coreAPIVersion, Kind: kindConfigMap},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceIdentityCAConfigMapName(cp),
			Namespace: ControlPlaneNamespace(cp),
			Labels:    serviceIdentityLabels(cp),
		},
		Data: map[string]string{
			ServiceIdentityCAKey: caPEM,
		},
	}
}
