// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"slices"
	"testing"

	cmv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openchami/openchami-operator/internal/conditions"
)

// TestServiceIdentityReconciler_AppliesEverything covers the steady-state
// initial-bootstrap case: ServiceIdentityReady=False on the first pass
// (cert-manager has not populated the Secrets yet), with every
// expected Issuer / Certificate object having been applied. The
// follow-on test feeds simulated cert-manager output back into the
// fake client to verify the Ready=True transition.
func TestServiceIdentityReconciler_AppliesEverything(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane(testClusterAlpha)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &ServiceIdentityReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while awaiting cert-manager, got %+v", res)
	}

	ns := ControlPlaneNamespace(cp)

	// Bootstrap selfSigned issuer.
	bs := &cmv1.Issuer{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ns, Name: serviceIdentityBootstrapIssuerName(cp),
	}, bs); err != nil {
		t.Fatalf("expected bootstrap Issuer: %v", err)
	}
	if bs.Spec.SelfSigned == nil {
		t.Errorf("bootstrap issuer must be selfSigned, got %+v", bs.Spec.IssuerConfig)
	}

	// CA Certificate signed by the bootstrap issuer.
	caCert := &cmv1.Certificate{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ns, Name: serviceIdentityCACertName(cp),
	}, caCert); err != nil {
		t.Fatalf("expected CA Certificate: %v", err)
	}
	if !caCert.Spec.IsCA {
		t.Errorf("CA Certificate.Spec.IsCA must be true")
	}
	if caCert.Spec.IssuerRef.Name != serviceIdentityBootstrapIssuerName(cp) {
		t.Errorf("CA Certificate must be signed by bootstrap issuer, got %q", caCert.Spec.IssuerRef.Name)
	}
	if caCert.Spec.IssuerRef.Kind != kindIssuer {
		t.Errorf("CA Certificate.IssuerRef.Kind=%q want %q", caCert.Spec.IssuerRef.Kind, kindIssuer)
	}

	// CA Issuer.
	caIss := &cmv1.Issuer{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ns, Name: serviceIdentityCAIssuerName(cp),
	}, caIss); err != nil {
		t.Fatalf("expected CA Issuer: %v", err)
	}
	if caIss.Spec.CA == nil || caIss.Spec.CA.SecretName != ServiceIdentityCASecretName(cp) {
		t.Errorf("CA Issuer must source from CA Secret %q, got %+v",
			ServiceIdentityCASecretName(cp), caIss.Spec.IssuerConfig)
	}

	// Tokensmith server cert with proper SANs.
	srv := &cmv1.Certificate{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ns, Name: serviceIdentityServerCertSecretName(cp),
	}, srv); err != nil {
		t.Fatalf("expected tokensmith server cert: %v", err)
	}
	hasSvcFQDN := false
	for _, dn := range srv.Spec.DNSNames {
		if dn == ServiceTokensmith+"."+ns+".svc.cluster.local" {
			hasSvcFQDN = true
		}
	}
	if !hasSvcFQDN {
		t.Errorf("server cert must include svc.cluster.local FQDN, got %v", srv.Spec.DNSNames)
	}
	if !containsKeyUsage(srv.Spec.Usages, cmv1.UsageServerAuth) {
		t.Errorf("server cert must request ServerAuth usage, got %v", srv.Spec.Usages)
	}

	// Boot-service client cert with CN matching the service-identity subject.
	bootClient := &cmv1.Certificate{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ns, Name: ServiceIdentityClientCertSecretName(cp, ServiceBootService),
	}, bootClient); err != nil {
		t.Fatalf("expected boot-service client cert: %v", err)
	}
	if bootClient.Spec.CommonName != ServiceBootService {
		t.Errorf("boot-service client cert CN=%q want %q",
			bootClient.Spec.CommonName, ServiceBootService)
	}
	if !containsKeyUsage(bootClient.Spec.Usages, cmv1.UsageClientAuth) {
		t.Errorf("boot-service client cert must request ClientAuth usage, got %v",
			bootClient.Spec.Usages)
	}

	// Metadata-service client cert.
	mdClient := &cmv1.Certificate{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ns, Name: ServiceIdentityClientCertSecretName(cp, ServiceMetadataService),
	}, mdClient); err != nil {
		t.Fatalf("expected metadata-service client cert: %v", err)
	}
	if mdClient.Spec.CommonName != ServiceMetadataService {
		t.Errorf("metadata-service client cert CN=%q want %q",
			mdClient.Spec.CommonName, ServiceMetadataService)
	}

	// ServiceIdentityReady condition should be False/AwaitingServiceIdentity
	// because the CA Secret hasn't been populated by the fake cert-manager
	// (no controller running). This is the contract that downstream
	// reconcilers gate on.
	cond := apimeta.FindStatusCondition(cp.Status.Conditions,
		conditions.ConditionServiceIdentityReady)
	if cond == nil {
		t.Fatalf("expected ConditionServiceIdentityReady to be set")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("ServiceIdentityReady status=%v want False (no cert-manager in fake)", cond.Status)
	}
	if cond.Reason != conditions.ReasonAwaitingServiceIdentity {
		t.Errorf("ServiceIdentityReady reason=%q want %q",
			cond.Reason, conditions.ReasonAwaitingServiceIdentity)
	}
}

// TestServiceIdentityReconciler_FlipsReadyOnceSecretsPresent simulates
// cert-manager having issued every Secret by pre-populating them in
// the fake client. ConditionServiceIdentityReady must flip to True and
// the CA ConfigMap mirror must be created with the CA Secret's ca.crt
// content.
func TestServiceIdentityReconciler_FlipsReadyOnceSecretsPresent(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane(testClusterAlpha)
	ns := ControlPlaneNamespace(cp)
	caPEM := []byte("---fake-ca-pem---")

	// Pre-populate the Secrets that the reconciler checks. The
	// kubernetes.io/tls Secret carries tls.crt / tls.key; the CA
	// Secret additionally carries ca.crt (which is what cert-manager
	// writes when the issuer is a CAIssuer).
	caSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: ServiceIdentityCASecretName(cp),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte("---ca-cert---"),
			corev1.TLSPrivateKeyKey: []byte("---ca-key---"),
			ServiceIdentityCAKey:    caPEM,
		},
	}
	serverSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: serviceIdentityServerCertSecretName(cp),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte("---srv-cert---"),
			corev1.TLSPrivateKeyKey: []byte("---srv-key---"),
		},
	}
	bootSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: ServiceIdentityClientCertSecretName(cp, ServiceBootService),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte("---boot-cert---"),
			corev1.TLSPrivateKeyKey: []byte("---boot-key---"),
		},
	}
	mdSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: ServiceIdentityClientCertSecretName(cp, ServiceMetadataService),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte("---md-cert---"),
			corev1.TLSPrivateKeyKey: []byte("---md-key---"),
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cp, caSecret, serverSecret, bootSecret, mdSecret).Build()

	r := &ServiceIdentityReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue once Ready, got %+v", res)
	}

	// CA ConfigMap mirror should now exist with the CA PEM payload.
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ns, Name: ServiceIdentityCAConfigMapName(cp),
	}, cm); err != nil {
		t.Fatalf("expected CA ConfigMap mirror: %v", err)
	}
	if got := cm.Data[ServiceIdentityCAKey]; got != string(caPEM) {
		t.Errorf("CA ConfigMap[%q]=%q want %q", ServiceIdentityCAKey, got, caPEM)
	}

	cond := apimeta.FindStatusCondition(cp.Status.Conditions,
		conditions.ConditionServiceIdentityReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("expected ConditionServiceIdentityReady=True, got %+v", cond)
	}
}

// TestTokensmithMTLSEnabled_GatesOnSpecField guards the helper used by
// every downstream reconciler. The helper now reads the spec field directly
// rather than gating on the ServiceIdentityReady condition, giving users
// explicit control over when TLS is activated.
func TestTokensmithMTLSEnabled_GatesOnCondition(t *testing.T) {
	cp := newControlPlane(testClusterAlpha)
	// Default: TLS disabled
	if tokensmithMTLSEnabled(cp) {
		t.Errorf("expected mTLS disabled by default (spec.services.tokensmith.tls.enabled=false)")
	}
	// Enable TLS via spec field
	cp.Spec.Services.Tokensmith.TLS.Enabled = true
	if !tokensmithMTLSEnabled(cp) {
		t.Errorf("expected mTLS enabled once spec.services.tokensmith.tls.enabled=true")
	}
}

// containsKeyUsage returns true if usages contains the target — small
// helper to keep the table-style asserts above readable.
func containsKeyUsage(usages []cmv1.KeyUsage, want cmv1.KeyUsage) bool {
	return slices.Contains(usages, want)
}
