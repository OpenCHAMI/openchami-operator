// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"crypto/x509"
	"encoding/pem"
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
	certificatesRequeueAfter = 30 * time.Second

	// certDuration is the lifetime requested for the gateway TLS certificate.
	certDuration = 72 * time.Hour
	// certRenewBefore is the lead time before NotAfter at which cert-manager
	// renews the certificate.
	certRenewBefore = 12 * time.Hour

	// certWarningGap is the threshold below which a Warning Event is recorded.
	certWarningGap = 48 * time.Hour
	// certImminentGap is the threshold below which the cert is considered
	// imminently expiring (CertificatesValid=False/ExpirationImminent).
	certImminentGap = 24 * time.Hour

	// certManagerGroup / certManagerAPIVersion are the cert-manager.io
	// group + apiVersion strings shared across cert-manager.io IssuerRefs
	// and TypeMeta values produced by certificates.go and service_identity.go.
	certManagerGroup      = "cert-manager.io"
	certManagerAPIVersion = certManagerGroup + "/v1"
	// kindCertificate is the Kind string for cert-manager Certificate.
	kindCertificate = "Certificate"

	// defaultTLSIssuer is the ClusterIssuer name used when none is set in spec.
	defaultTLSIssuer = "vault-pki-issuer"

	// gatewayTLSSuffix is the default suffix for the cert-manager Certificate
	// and the destination TLS Secret.
	gatewayTLSSuffix = "gateway-tls"
)

// CertificatesReconciler ensures a cert-manager Certificate exists for the
// cluster gateway and tracks the expiry of the resulting TLS secret.
type CertificatesReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

// Reconcile applies the Certificate resource and reports CertificatesValid
// based on the NotAfter timestamp parsed from the resulting TLS secret.
func (r *CertificatesReconciler) Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "certificates")

	cert := r.buildCertificate(cp)
	cLog := logging.EnrichWithResource(log, kindCertificate, cert.Name)
	cLog.Info("applying gateway Certificate")
	if err := r.Client.Patch(ctx, cert, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying gateway Certificate: %w", err)
	}

	secretName := GatewayTLSSecretName(cp)
	secret := &corev1.Secret{}
	getErr := r.Client.Get(ctx, types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      secretName,
	}, secret)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return ctrl.Result{}, fmt.Errorf("reading gateway TLS Secret: %w", getErr)
	}
	if apierrors.IsNotFound(getErr) {
		log.Info("waiting for cert-manager to populate TLS Secret", "secret", secretName)
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionCertificatesValid,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonAwaitingCertificate,
			Message:            fmt.Sprintf("waiting for TLS Secret %q to be issued", secretName),
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{RequeueAfter: certificatesRequeueAfter}, nil
	}

	notAfter, err := parseNotAfter(secret.Data[corev1.TLSCertKey])
	if err != nil {
		log.Error(err, "parsing TLS certificate", "secret", secretName)
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionCertificatesValid,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonError,
			Message:            fmt.Sprintf("parsing TLS Secret %q: %v", secretName, err),
			ObservedGeneration: cp.Generation,
		})
		RecordConditionEvent(r.Recorder, cp, corev1.EventTypeWarning,
			conditions.ReasonError,
			fmt.Sprintf("could not parse TLS certificate from Secret %q", secretName))
		return ctrl.Result{RequeueAfter: certificatesRequeueAfter}, nil
	}

	cp.Status.CertExpiryTime = notAfter.UTC().Format(time.RFC3339)

	gap := time.Until(notAfter)
	switch {
	case gap <= 0:
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionCertificatesValid,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonExpired,
			Message:            fmt.Sprintf("certificate expired at %s", notAfter.UTC().Format(time.RFC3339)),
			ObservedGeneration: cp.Generation,
		})
		RecordConditionEvent(r.Recorder, cp, corev1.EventTypeWarning,
			conditions.ReasonExpired,
			fmt.Sprintf("gateway TLS certificate expired at %s", notAfter.UTC().Format(time.RFC3339)))
	case gap <= certImminentGap:
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionCertificatesValid,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonExpirationImminent,
			Message:            fmt.Sprintf("certificate expires in %s", gap.Round(time.Minute)),
			ObservedGeneration: cp.Generation,
		})
		RecordConditionEvent(r.Recorder, cp, corev1.EventTypeWarning,
			conditions.ReasonExpirationImminent,
			fmt.Sprintf("gateway TLS certificate expires in %s", gap.Round(time.Minute)))
	default:
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionCertificatesValid,
			Status:             metav1.ConditionTrue,
			Reason:             conditions.ReasonReady,
			Message:            fmt.Sprintf("certificate valid until %s", notAfter.UTC().Format(time.RFC3339)),
			ObservedGeneration: cp.Generation,
		})
		if gap <= certWarningGap {
			RecordConditionEvent(r.Recorder, cp, corev1.EventTypeWarning,
				conditions.ReasonExpirationImminent,
				fmt.Sprintf("gateway TLS certificate expires in %s", gap.Round(time.Minute)))
		}
	}

	return ctrl.Result{}, nil
}

// Describe returns the Kubernetes objects this reconciler would apply.
func (r *CertificatesReconciler) Describe(cp *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
	return []client.Object{r.buildCertificate(cp)}, nil
}

// GatewayTLSSecretName returns the resolved name of the TLS Secret that
// cert-manager populates for the gateway. Exported because the controller's
// Secret-to-cluster watcher uses it to gate enqueues.
func GatewayTLSSecretName(cp *openchamiv1alpha1.OpenCHAMIControlPlane) string {
	if name := cp.Spec.Networking.TLS.SecretName; name != "" {
		return name
	}
	return cp.Spec.ClusterName + "-" + gatewayTLSSuffix
}

// gatewayTLSIssuer returns the resolved cert-manager ClusterIssuer name.
func gatewayTLSIssuer(cp *openchamiv1alpha1.OpenCHAMIControlPlane) string {
	if iss := cp.Spec.Networking.TLS.Issuer; iss != "" {
		return iss
	}
	return defaultTLSIssuer
}

// gatewayCertificateName returns the cert-manager Certificate object name.
func gatewayCertificateName(cp *openchamiv1alpha1.OpenCHAMIControlPlane) string {
	return cp.Spec.ClusterName + "-" + gatewayTLSSuffix
}

func (r *CertificatesReconciler) buildCertificate(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *cmv1.Certificate {
	labels := map[string]string{
		labelAppName:   "gateway",
		labelAppInst:   "openchami-" + cp.Spec.ClusterName,
		labelManagedBy: managedByValue,
	}
	duration := metav1.Duration{Duration: certDuration}
	renewBefore := metav1.Duration{Duration: certRenewBefore}
	return &cmv1.Certificate{
		TypeMeta: metav1.TypeMeta{APIVersion: certManagerAPIVersion, Kind: kindCertificate},
		ObjectMeta: metav1.ObjectMeta{
			Name:      gatewayCertificateName(cp),
			Namespace: ControlPlaneNamespace(cp),
			Labels:    labels,
		},
		Spec: cmv1.CertificateSpec{
			SecretName:  GatewayTLSSecretName(cp),
			DNSNames:    []string{cp.Spec.Domain},
			Duration:    &duration,
			RenewBefore: &renewBefore,
			IssuerRef: cmmeta.IssuerReference{
				Name:  gatewayTLSIssuer(cp),
				Kind:  "ClusterIssuer",
				Group: certManagerGroup,
			},
		},
	}
}

// parseNotAfter decodes the first PEM block in tls.crt and returns the
// certificate's NotAfter timestamp. Returns an error when the PEM is missing,
// malformed, or the decoded block is not an X.509 certificate.
func parseNotAfter(crtPEM []byte) (time.Time, error) {
	if len(crtPEM) == 0 {
		return time.Time{}, fmt.Errorf("tls.crt is empty")
	}
	block, _ := pem.Decode(crtPEM)
	if block == nil {
		return time.Time{}, fmt.Errorf("tls.crt does not contain a PEM block")
	}
	if block.Type != "CERTIFICATE" {
		return time.Time{}, fmt.Errorf("unexpected PEM block type %q", block.Type)
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing X.509 certificate: %w", err)
	}
	return parsed.NotAfter, nil
}
