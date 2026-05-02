/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package reconcilers

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	cmv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openchami/openchami-operator/internal/conditions"
)

// makeTestCert returns a self-signed PEM certificate that expires at notAfter.
func makeTestCert(t *testing.T, notAfter time.Time) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "openchami-test"},
		NotBefore:    notAfter.Add(-72 * time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("creating test cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestCertificatesReconciler_AppliesCertificate(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	r := &CertificatesReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while awaiting cert-manager-issued Secret, got %+v", res)
	}

	cert := &cmv1.Certificate{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ClusterNamespace(cluster),
		Name:      gatewayCertificateName(cluster),
	}, cert); err != nil {
		t.Fatalf("expected Certificate to be applied: %v", err)
	}

	if got := cert.Spec.SecretName; got != GatewayTLSSecretName(cluster) {
		t.Errorf("Certificate.SecretName=%q want=%q", got, GatewayTLSSecretName(cluster))
	}
	if len(cert.Spec.DNSNames) != 1 || cert.Spec.DNSNames[0] != cluster.Spec.Domain {
		t.Errorf("Certificate.DNSNames=%v want=[%s]", cert.Spec.DNSNames, cluster.Spec.Domain)
	}
	if cert.Spec.Duration == nil || cert.Spec.Duration.Duration != 72*time.Hour {
		t.Errorf("Certificate.Duration=%v want 72h", cert.Spec.Duration)
	}
	if cert.Spec.RenewBefore == nil || cert.Spec.RenewBefore.Duration != 12*time.Hour {
		t.Errorf("Certificate.RenewBefore=%v want 12h", cert.Spec.RenewBefore)
	}
	if cert.Spec.IssuerRef.Kind != "ClusterIssuer" {
		t.Errorf("Certificate.IssuerRef.Kind=%q want ClusterIssuer", cert.Spec.IssuerRef.Kind)
	}
	if cert.Spec.IssuerRef.Name != defaultTLSIssuer {
		t.Errorf("Certificate.IssuerRef.Name=%q want %q", cert.Spec.IssuerRef.Name, defaultTLSIssuer)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionCertificatesValid)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonAwaitingCertificate {
		t.Fatalf("expected CertificatesValid=False/AwaitingCertificate, got %+v", cond)
	}
}

func TestCertificatesReconciler_HappyPath(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")

	notAfter := time.Now().Add(48 * time.Hour)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      GatewayTLSSecretName(cluster),
			Namespace: ClusterNamespace(cluster),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       makeTestCert(t, notAfter),
			corev1.TLSPrivateKeyKey: []byte("ignored-by-reconciler"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, secret).Build()

	rec := record.NewFakeRecorder(10)
	r := &CertificatesReconciler{Client: c, Recorder: rec}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionCertificatesValid)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != conditions.ReasonReady {
		t.Fatalf("expected CertificatesValid=True/Ready, got %+v", cond)
	}
	if cluster.Status.CertExpiryTime == "" {
		t.Errorf("expected CertExpiryTime to be set")
	}
	if _, err := time.Parse(time.RFC3339, cluster.Status.CertExpiryTime); err != nil {
		t.Errorf("CertExpiryTime %q not RFC3339: %v", cluster.Status.CertExpiryTime, err)
	}

	// gap < 48h — expect Warning Event
	select {
	case ev := <-rec.Events:
		if !contains(ev, conditions.ReasonExpirationImminent) {
			t.Errorf("expected ExpirationImminent Warning event, got %q", ev)
		}
	default:
		t.Errorf("expected a Warning event for <48h cert, got none")
	}
}

func TestCertificatesReconciler_ExpiryImminent(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")

	notAfter := time.Now().Add(12 * time.Hour)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      GatewayTLSSecretName(cluster),
			Namespace: ClusterNamespace(cluster),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey: makeTestCert(t, notAfter),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, secret).Build()

	rec := record.NewFakeRecorder(10)
	r := &CertificatesReconciler{Client: c, Recorder: rec}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionCertificatesValid)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonExpirationImminent {
		t.Fatalf("expected CertificatesValid=False/ExpirationImminent, got %+v", cond)
	}

	select {
	case ev := <-rec.Events:
		if !contains(ev, conditions.ReasonExpirationImminent) {
			t.Errorf("expected ExpirationImminent Warning event, got %q", ev)
		}
	default:
		t.Errorf("expected a Warning event for imminent-expiry cert, got none")
	}
}

func TestCertificatesReconciler_Expired(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")

	notAfter := time.Now().Add(-1 * time.Hour)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      GatewayTLSSecretName(cluster),
			Namespace: ClusterNamespace(cluster),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey: makeTestCert(t, notAfter),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, secret).Build()

	r := &CertificatesReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionCertificatesValid)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonExpired {
		t.Fatalf("expected CertificatesValid=False/Expired, got %+v", cond)
	}
}

func TestCertificatesReconciler_TwoClustersIsolated(t *testing.T) {
	scheme := newScheme(t)
	rec := record.NewFakeRecorder(20)

	for _, name := range []string{testClusterRed, testClusterBlue} {
		cluster := newCluster(name)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
		r := &CertificatesReconciler{Client: c, Recorder: rec}
		if _, err := r.Reconcile(context.Background(), cluster); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}

		cert := &cmv1.Certificate{}
		if err := c.Get(context.Background(), types.NamespacedName{
			Namespace: ClusterNamespace(cluster),
			Name:      gatewayCertificateName(cluster),
		}, cert); err != nil {
			t.Fatalf("getting cluster %q Certificate: %v", name, err)
		}
		if cert.Namespace != "openchami-"+name {
			t.Errorf("cluster %q Certificate namespace=%q want openchami-%s",
				name, cert.Namespace, name)
		}
		if cert.Spec.SecretName != name+"-"+gatewayTLSSuffix {
			t.Errorf("cluster %q SecretName=%q want %s-%s",
				name, cert.Spec.SecretName, name, gatewayTLSSuffix)
		}
	}
}

func TestCertificatesReconciler_RespectsCustomSecretName(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.Networking.TLS.SecretName = "custom-tls-secret"
	cluster.Spec.Networking.TLS.Issuer = "custom-issuer"

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	r := &CertificatesReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cert := &cmv1.Certificate{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ClusterNamespace(cluster),
		Name:      gatewayCertificateName(cluster),
	}, cert); err != nil {
		t.Fatalf("getting Certificate: %v", err)
	}
	if cert.Spec.SecretName != "custom-tls-secret" {
		t.Errorf("SecretName=%q want custom-tls-secret", cert.Spec.SecretName)
	}
	if cert.Spec.IssuerRef.Name != "custom-issuer" {
		t.Errorf("IssuerRef.Name=%q want custom-issuer", cert.Spec.IssuerRef.Name)
	}
}

func TestParseNotAfter_Errors(t *testing.T) {
	if _, err := parseNotAfter(nil); err == nil {
		t.Errorf("expected error on nil input")
	}
	if _, err := parseNotAfter([]byte("not pem")); err == nil {
		t.Errorf("expected error on non-PEM input")
	}
	other := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("xxx")})
	if _, err := parseNotAfter(other); err == nil {
		t.Errorf("expected error on wrong PEM block type")
	}
}

// contains is a tiny helper to check substring presence in a string. The
// fake EventRecorder emits "<type> <reason> <message>" so we match on reason.
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
