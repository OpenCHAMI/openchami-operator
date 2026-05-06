// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package status

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/reconcilers"
)

// newTestCluster returns a cluster with all four status-tracked services
// enabled so detectServiceRegressions and the all-services-ready check have
// something to inspect.
func newTestCluster(name string) *openchamiv1alpha1.OpenCHAMICluster {
	c := &openchamiv1alpha1.OpenCHAMICluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: 1},
		Spec: openchamiv1alpha1.OpenCHAMIClusterSpec{
			ClusterName: name,
			Domain:      name + ".test.local",
		},
	}
	c.Spec.Services.SMD.Enabled = true
	c.Spec.Services.Tokensmith.Enabled = true
	c.Spec.Services.BootService.Enabled = true
	c.Spec.Services.MetadataService.Enabled = true
	return c
}

// allServicesReady marks every enabled service as Ready in the cluster
// status, mirroring the post-reconcile state the controller would produce.
func allServicesReady(c *openchamiv1alpha1.OpenCHAMICluster) {
	c.Status.Services = map[string]openchamiv1alpha1.ServiceStatus{
		reconcilers.ServiceSMD:             {Ready: true, Endpoint: "http://smd"},
		reconcilers.ServiceTokensmith:      {Ready: true, Endpoint: "http://tokensmith"},
		reconcilers.ServiceBootService:     {Ready: true, Endpoint: "http://boot"},
		reconcilers.ServiceMetadataService: {Ready: true, Endpoint: "http://meta"},
	}
}

// addCondition is a small helper to set a single condition on a cluster.
func addCondition(c *openchamiv1alpha1.OpenCHAMICluster, t string, status metav1.ConditionStatus, reason string) {
	apimeta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
		Type:               t,
		Status:             status,
		Reason:             reason,
		Message:            reason,
		ObservedGeneration: c.Generation,
	})
}

// addAllConditionsTrue stamps every condition the reporter looks at as True
// so the happy path can flip a single one to False per test.
func addAllConditionsTrue(c *openchamiv1alpha1.OpenCHAMICluster) {
	conds := []string{
		conditions.ConditionReconcileActive,
		conditions.ConditionNamespaceReady,
		conditions.ConditionVaultConfigured,
		conditions.ConditionBucketReady,
		conditions.ConditionDatabaseReady,
		conditions.ConditionNetworkProbeReady,
		conditions.ConditionServicesReady,
		conditions.ConditionDHCPReady,
		conditions.ConditionGatewayReady,
		conditions.ConditionCertificatesValid,
		conditions.ConditionTopologyPublished,
	}
	for _, t := range conds {
		addCondition(c, t, metav1.ConditionTrue, conditions.ReasonReady)
	}
}

func TestReporter_AllConditionsTrueAndServicesReady(t *testing.T) {
	c := newTestCluster("alpha")
	addAllConditionsTrue(c)
	allServicesReady(c)

	r := &Reporter{Recorder: record.NewFakeRecorder(8)}
	r.ComputeAndSetPhase(c)

	if c.Status.Phase != openchamiv1alpha1.PhaseReady {
		t.Fatalf("expected PhaseReady, got %q", c.Status.Phase)
	}
}

func TestReporter_CertificatesValidFalseDegrades(t *testing.T) {
	c := newTestCluster("alpha")
	addAllConditionsTrue(c)
	allServicesReady(c)
	addCondition(c, conditions.ConditionCertificatesValid, metav1.ConditionFalse, conditions.ReasonExpirationImminent)

	r := &Reporter{Recorder: record.NewFakeRecorder(8)}
	r.ComputeAndSetPhase(c)

	if c.Status.Phase != openchamiv1alpha1.PhaseDegraded {
		t.Fatalf("expected PhaseDegraded, got %q", c.Status.Phase)
	}
}

func TestReporter_ServiceFlipsReadyToNotReady(t *testing.T) {
	c := newTestCluster("alpha")
	addAllConditionsTrue(c)
	allServicesReady(c)
	// Previous reconcile had reached Ready; this is what unlocks the
	// regression detection in detectServiceRegressions.
	c.Status.Phase = openchamiv1alpha1.PhaseReady

	// SMD has just dropped its Ready flag.
	c.Status.Services[reconcilers.ServiceSMD] = openchamiv1alpha1.ServiceStatus{
		Ready:   false,
		Message: "rolling restart",
	}

	rec := record.NewFakeRecorder(8)
	r := &Reporter{Recorder: rec}
	r.ComputeAndSetPhase(c)

	if c.Status.Phase != openchamiv1alpha1.PhaseDegraded {
		t.Fatalf("expected PhaseDegraded after Ready→NotReady, got %q", c.Status.Phase)
	}

	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "Ready→NotReady") {
			t.Errorf("expected event message to contain 'Ready→NotReady', got %q", ev)
		}
		if !strings.Contains(ev, "Warning") {
			t.Errorf("expected Warning event, got %q", ev)
		}
	default:
		t.Fatal("expected a regression Warning event, got none")
	}
}

func TestReporter_NetworkProbeNoEligibleNodes(t *testing.T) {
	c := newTestCluster("alpha")
	addAllConditionsTrue(c)
	allServicesReady(c)
	addCondition(c, conditions.ConditionNetworkProbeReady, metav1.ConditionFalse, conditions.ReasonNoEligibleNodes)

	rec := record.NewFakeRecorder(8)
	r := &Reporter{Recorder: rec}
	r.ComputeAndSetPhase(c)

	if c.Status.Phase != openchamiv1alpha1.PhaseDegraded {
		t.Fatalf("expected PhaseDegraded for NoEligibleNodes, got %q", c.Status.Phase)
	}
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, conditions.ReasonNoEligibleNodes) {
			t.Errorf("expected event to mention NoEligibleNodes, got %q", ev)
		}
	default:
		t.Fatal("expected a NoEligibleNodes Warning event, got none")
	}
}

func TestReporter_ProvisioningReason(t *testing.T) {
	c := newTestCluster("alpha")
	// All services ready, but a non-cert/non-NoEligibleNodes condition is
	// False with a Provisioning reason. Should pick Provisioning, not Ready.
	addAllConditionsTrue(c)
	allServicesReady(c)
	addCondition(c, conditions.ConditionDatabaseReady, metav1.ConditionFalse, conditions.ReasonProvisioning)

	r := &Reporter{Recorder: record.NewFakeRecorder(8)}
	r.ComputeAndSetPhase(c)

	if c.Status.Phase != openchamiv1alpha1.PhaseProvisioning {
		t.Fatalf("expected PhaseProvisioning, got %q", c.Status.Phase)
	}
}

func TestReporter_ErrorReasonFails(t *testing.T) {
	c := newTestCluster("alpha")
	addAllConditionsTrue(c)
	allServicesReady(c)
	addCondition(c, conditions.ConditionVaultConfigured, metav1.ConditionFalse, conditions.ReasonError)

	r := &Reporter{Recorder: record.NewFakeRecorder(8)}
	r.ComputeAndSetPhase(c)

	if c.Status.Phase != openchamiv1alpha1.PhaseFailed {
		t.Fatalf("expected PhaseFailed, got %q", c.Status.Phase)
	}
}

func TestReporter_PrecedenceLadder(t *testing.T) {
	// One condition triggers Failed, another triggers Degraded (cert), and
	// a third triggers Provisioning. Failed must win.
	c := newTestCluster("alpha")
	addAllConditionsTrue(c)
	allServicesReady(c)
	addCondition(c, conditions.ConditionVaultConfigured, metav1.ConditionFalse, conditions.ReasonError)
	addCondition(c, conditions.ConditionCertificatesValid, metav1.ConditionFalse, conditions.ReasonExpirationImminent)
	addCondition(c, conditions.ConditionDatabaseReady, metav1.ConditionFalse, conditions.ReasonProvisioning)

	r := &Reporter{Recorder: record.NewFakeRecorder(8)}
	r.ComputeAndSetPhase(c)
	if c.Status.Phase != openchamiv1alpha1.PhaseFailed {
		t.Fatalf("expected Failed to outrank Degraded/Provisioning, got %q", c.Status.Phase)
	}

	// Drop the Error reason; Degraded must outrank Provisioning.
	addCondition(c, conditions.ConditionVaultConfigured, metav1.ConditionTrue, conditions.ReasonReady)
	c.Status.Phase = ""
	r.ComputeAndSetPhase(c)
	if c.Status.Phase != openchamiv1alpha1.PhaseDegraded {
		t.Fatalf("expected Degraded to outrank Provisioning, got %q", c.Status.Phase)
	}

	// Drop the cert too; Provisioning must outrank Ready.
	addCondition(c, conditions.ConditionCertificatesValid, metav1.ConditionTrue, conditions.ReasonReady)
	c.Status.Phase = ""
	r.ComputeAndSetPhase(c)
	if c.Status.Phase != openchamiv1alpha1.PhaseProvisioning {
		t.Fatalf("expected Provisioning over Ready, got %q", c.Status.Phase)
	}
}

func TestReporter_UpdateCertExpiry(t *testing.T) {
	notAfter := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	pemBytes := makeStatusTestCert(t, notAfter)

	c := newTestCluster("alpha")
	r := &Reporter{Recorder: record.NewFakeRecorder(2)}
	secret := &corev1.Secret{
		Data: map[string][]byte{corev1.TLSCertKey: pemBytes},
	}
	if err := r.UpdateCertExpiry(c, secret); err != nil {
		t.Fatalf("UpdateCertExpiry: %v", err)
	}
	parsed, err := time.Parse(time.RFC3339, c.Status.CertExpiryTime)
	if err != nil {
		t.Fatalf("parsing CertExpiryTime %q: %v", c.Status.CertExpiryTime, err)
	}
	if delta := parsed.Sub(notAfter); delta > time.Second || delta < -time.Second {
		t.Errorf("expected CertExpiryTime within 1s of %v, got %v (delta %v)", notAfter, parsed, delta)
	}
}

func TestReporter_UpdateServiceStatusInitMap(t *testing.T) {
	c := newTestCluster("alpha")
	if c.Status.Services != nil {
		t.Fatalf("preconditions: expected nil Services map, got %v", c.Status.Services)
	}

	r := &Reporter{Recorder: record.NewFakeRecorder(2)}
	r.UpdateServiceStatus(c, reconcilers.ServiceSMD, true, "http://smd", "ok")

	got, ok := c.Status.Services[reconcilers.ServiceSMD]
	if !ok {
		t.Fatalf("expected Services map to contain smd entry, got %v", c.Status.Services)
	}
	if !got.Ready || got.Endpoint != "http://smd" || got.Message != "ok" {
		t.Errorf("unexpected ServiceStatus: %+v", got)
	}
}

func TestReporter_SetConditionStampsObservedGeneration(t *testing.T) {
	c := newTestCluster("alpha")
	c.Generation = 7

	r := &Reporter{Recorder: record.NewFakeRecorder(2)}
	r.SetCondition(c, conditions.ConditionDatabaseReady, metav1.ConditionTrue, conditions.ReasonReady, "db ready")

	cond := apimeta.FindStatusCondition(c.Status.Conditions, conditions.ConditionDatabaseReady)
	if cond == nil {
		t.Fatalf("expected DatabaseReady condition, got nil")
	}
	if cond.ObservedGeneration != 7 {
		t.Errorf("expected ObservedGeneration=7, got %d", cond.ObservedGeneration)
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != conditions.ReasonReady {
		t.Errorf("unexpected condition: %+v", cond)
	}
}

// makeStatusTestCert returns a self-signed PEM certificate that expires at
// the supplied time. Mirrors the helper in
// internal/reconcilers/certificates_test.go but lives in this package so
// reporter_test.go has no cross-package test dependency.
func makeStatusTestCert(t *testing.T, notAfter time.Time) []byte {
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
