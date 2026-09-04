// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

func newTokensmithCluster() *openchamiv1alpha1.OpenCHAMIControlPlane {
	cp := newControlPlane("alpha")
	cp.Spec.Services.Tokensmith.Enabled = true
	return cp
}

func TestTokensmithReconciler_DisabledSkips(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.Tokensmith.Enabled = false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &TokensmithReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when disabled, got %+v", res)
	}

	dep := &appsv1.Deployment{}
	err = c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      ServiceTokensmith,
	}, dep)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected Deployment to be absent when disabled, got err=%v", err)
	}
}

func TestTokensmithReconciler_AppliesAllResources(t *testing.T) {
	scheme := newScheme(t)
	cp := newTokensmithCluster()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &TokensmithReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ns := ControlPlaneNamespace(cp)

	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: tokensmithPVCName}, pvc); err != nil {
		t.Fatalf("expected PVC to exist: %v", err)
	}
	if pvc.Annotations["helm.sh/resource-policy"] != "keep" {
		t.Errorf("expected helm.sh/resource-policy=keep annotation, got %q", pvc.Annotations["helm.sh/resource-policy"])
	}

	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: ServiceTokensmith}, dep); err != nil {
		t.Fatalf("expected Deployment to exist: %v", err)
	}
	if dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("expected strategy=Recreate, got %q", dep.Spec.Strategy.Type)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
		t.Errorf("expected replicas=1, got %v", dep.Spec.Replicas)
	}

	if len(dep.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(dep.Spec.Template.Spec.Containers))
	}
	cont := dep.Spec.Template.Spec.Containers[0]

	var oidcIssuer, oidcSecretRefName, oidcSecretRefKey string
	for _, e := range cont.Env {
		switch e.Name {
		case tokensmithOIDCIssuerEnvName:
			oidcIssuer = e.Value
		case "OIDC_CLIENT_SECRET":
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				oidcSecretRefName = e.ValueFrom.SecretKeyRef.Name
				oidcSecretRefKey = e.ValueFrom.SecretKeyRef.Key
			}
		}
	}
	if !strings.Contains(oidcIssuer, "/v1/identity/oidc/provider/default") {
		t.Errorf("expected OIDC_ISSUER_URL to contain vault issuer suffix, got %q", oidcIssuer)
	}
	wantSecret := SecretName(cp, SuffixTokensmithOIDC)
	if oidcSecretRefName != wantSecret || oidcSecretRefKey != tokensmithOIDCClientSecretKey {
		t.Errorf("expected OIDC_CLIENT_SECRET to reference Secret %q key %q, got %q/%q",
			wantSecret, tokensmithOIDCClientSecretKey, oidcSecretRefName, oidcSecretRefKey)
	}

	svc := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: ServiceTokensmith}, svc); err != nil {
		t.Fatalf("expected Service to exist: %v", err)
	}

	pdb := &policyv1.PodDisruptionBudget{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: ServiceTokensmith}, pdb); err != nil {
		t.Fatalf("expected PDB to exist: %v", err)
	}
}

func TestTokensmithReconciler_ExternalOIDCRequiresURL(t *testing.T) {
	scheme := newScheme(t)
	cp := newTokensmithCluster()
	cp.Spec.Services.Tokensmith.OIDCProvider = "external"
	cp.Spec.Services.Tokensmith.OIDCIssuerURL = ""
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	rec := record.NewFakeRecorder(10)
	r := &TokensmithReconciler{Client: c, Recorder: rec}
	_, err := r.Reconcile(context.Background(), cp)
	if err == nil {
		t.Fatalf("expected error when oidcProvider=external without oidcIssuerURL")
	}

	dep := &appsv1.Deployment{}
	gerr := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      ServiceTokensmith,
	}, dep)
	if !apierrors.IsNotFound(gerr) {
		t.Errorf("expected Deployment to NOT be created on validation failure, got err=%v", gerr)
	}

	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, reasonOIDCConfigInvalid) {
			t.Errorf("expected event to mention %q, got %q", reasonOIDCConfigInvalid, ev)
		}
	default:
		t.Errorf("expected an event to be recorded for OIDCConfigInvalid")
	}
}

func TestTokensmithReconciler_ExternalOIDCHappy(t *testing.T) {
	scheme := newScheme(t)
	cp := newTokensmithCluster()
	cp.Spec.Services.Tokensmith.OIDCProvider = "external"
	cp.Spec.Services.Tokensmith.OIDCIssuerURL = "https://issuer.example/realm"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &TokensmithReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      ServiceTokensmith,
	}, dep); err != nil {
		t.Fatalf("getting deployment: %v", err)
	}

	var oidcIssuer string
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Name == tokensmithOIDCIssuerEnvName {
			oidcIssuer = e.Value
		}
	}
	if oidcIssuer != "https://issuer.example/realm" {
		t.Errorf("expected OIDC_ISSUER_URL to match external issuer, got %q", oidcIssuer)
	}
}

func TestTokensmithReconciler_ReadyWhenAvailable(t *testing.T) {
	scheme := newScheme(t)
	cp := newTokensmithCluster()

	// Pre-create the Deployment with AvailableReplicas=1 so reconcile reads it back as ready.
	existing := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceTokensmith,
			Namespace: ControlPlaneNamespace(cp),
		},
		Status: appsv1.DeploymentStatus{
			AvailableReplicas: 1,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cp).
		WithStatusSubresource(&appsv1.Deployment{}).
		WithObjects(existing).
		Build()

	r := &TokensmithReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue once ready, got %+v", res)
	}

	st, ok := cp.Status.Services[ServiceTokensmith]
	if !ok {
		t.Fatalf("expected status.services[tokensmith] to be set")
	}
	if !st.Ready {
		t.Errorf("expected status.services[tokensmith].Ready=true, got %+v", st)
	}
	if st.Endpoint == "" {
		t.Errorf("expected non-empty Endpoint, got %+v", st)
	}
}

func TestTokensmithReconciler_ReplicasAlwaysOne(t *testing.T) {
	cp := newTokensmithCluster()
	cp.Spec.Services.Tokensmith.Replicas = 5

	r := &TokensmithReconciler{}
	dep := r.buildDeployment(cp)
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
		t.Errorf("expected replicas=1 even when spec.replicas=5, got %v", dep.Spec.Replicas)
	}
}

func TestTokensmithReconciler_TLSDisabledByDefault(t *testing.T) {
	cp := newTokensmithCluster()
	// Don't set .tls.enabled — should default to false
	r := &TokensmithReconciler{}
	dep := r.buildDeployment(cp)

	cont := dep.Spec.Template.Spec.Containers[0]

	// Probes should use HTTP when TLS is disabled
	if cont.StartupProbe.HTTPGet.Scheme != corev1.URISchemeHTTP {
		t.Errorf("expected startupProbe scheme=HTTP when TLS disabled, got %q", cont.StartupProbe.HTTPGet.Scheme)
	}
	if cont.LivenessProbe.HTTPGet.Scheme != corev1.URISchemeHTTP {
		t.Errorf("expected livenessProbe scheme=HTTP when TLS disabled, got %q", cont.LivenessProbe.HTTPGet.Scheme)
	}
	if cont.ReadinessProbe.HTTPGet.Scheme != corev1.URISchemeHTTP {
		t.Errorf("expected readinessProbe scheme=HTTP when TLS disabled, got %q", cont.ReadinessProbe.HTTPGet.Scheme)
	}

	// TLS env vars should not be set when TLS is disabled
	for _, e := range cont.Env {
		if e.Name == tokensmithEnvTLSCertFile || e.Name == tokensmithEnvTLSKeyFile || e.Name == tokensmithEnvSvcIDCA {
			t.Errorf("expected TLS env vars to be absent when TLS disabled, found %q", e.Name)
		}
	}

	// TLS volume should not be mounted when TLS is disabled
	for _, m := range cont.VolumeMounts {
		if m.Name == tokensmithTLSVolumeNm {
			t.Errorf("expected TLS volume mount to be absent when TLS disabled, found %q", m.Name)
		}
	}
}

func TestTokensmithReconciler_TLSEnabledHTTPSProbes(t *testing.T) {
	cp := newTokensmithCluster()
	cp.Spec.Services.Tokensmith.TLS.Enabled = true

	r := &TokensmithReconciler{}
	dep := r.buildDeployment(cp)

	cont := dep.Spec.Template.Spec.Containers[0]

	// Probes should use HTTPS when TLS is enabled
	if cont.StartupProbe.HTTPGet.Scheme != corev1.URISchemeHTTPS {
		t.Errorf("expected startupProbe scheme=HTTPS when TLS enabled, got %q", cont.StartupProbe.HTTPGet.Scheme)
	}
	if cont.LivenessProbe.HTTPGet.Scheme != corev1.URISchemeHTTPS {
		t.Errorf("expected livenessProbe scheme=HTTPS when TLS enabled, got %q", cont.LivenessProbe.HTTPGet.Scheme)
	}
	if cont.ReadinessProbe.HTTPGet.Scheme != corev1.URISchemeHTTPS {
		t.Errorf("expected readinessProbe scheme=HTTPS when TLS enabled, got %q", cont.ReadinessProbe.HTTPGet.Scheme)
	}

	// TLS env vars should be set when TLS is enabled
	var foundCertFile, foundKeyFile, foundCA bool
	for _, e := range cont.Env {
		switch e.Name {
		case tokensmithEnvTLSCertFile:
			foundCertFile = true
			expectedPath := tokensmithTLSMountPath + "/" + corev1.TLSCertKey
			if e.Value != expectedPath {
				t.Errorf("expected TOKENSMITH_TLS_CERT_FILE=%q, got %q", expectedPath, e.Value)
			}
		case tokensmithEnvTLSKeyFile:
			foundKeyFile = true
			expectedPath := tokensmithTLSMountPath + "/" + corev1.TLSPrivateKeyKey
			if e.Value != expectedPath {
				t.Errorf("expected TOKENSMITH_TLS_KEY_FILE=%q, got %q", expectedPath, e.Value)
			}
		case tokensmithEnvSvcIDCA:
			foundCA = true
			expectedPath := tokensmithTLSMountPath + "/" + ServiceIdentityCAKey
			if e.Value != expectedPath {
				t.Errorf("expected TOKENSMITH_SERVICE_IDENTITY_CA=%q, got %q", expectedPath, e.Value)
			}
		}
	}
	if !foundCertFile {
		t.Errorf("expected TOKENSMITH_TLS_CERT_FILE env var when TLS enabled")
	}
	if !foundKeyFile {
		t.Errorf("expected TOKENSMITH_TLS_KEY_FILE env var when TLS enabled")
	}
	if !foundCA {
		t.Errorf("expected TOKENSMITH_SERVICE_IDENTITY_CA env var when TLS enabled")
	}

	// TLS volume should be mounted when TLS is enabled
	var foundMount bool
	for _, m := range cont.VolumeMounts {
		if m.Name == tokensmithTLSVolumeNm {
			foundMount = true
			if m.MountPath != tokensmithTLSMountPath {
				t.Errorf("expected TLS mount path=%q, got %q", tokensmithTLSMountPath, m.MountPath)
			}
			if !m.ReadOnly {
				t.Errorf("expected TLS volume mount to be read-only")
			}
		}
	}
	if !foundMount {
		t.Errorf("expected TLS volume mount when TLS enabled")
	}

	// TLS volume should be present
	var foundVolume bool
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == tokensmithTLSVolumeNm {
			foundVolume = true
			if v.Projected == nil {
				t.Errorf("expected TLS volume to use projected source")
			}
		}
	}
	if !foundVolume {
		t.Errorf("expected TLS volume when TLS enabled")
	}
}

func TestTokensmithReconciler_EndpointSchemeReflectsTLS(t *testing.T) {
	scheme := newScheme(t)

	// Test without TLS
	cpHTTP := newTokensmithCluster()
	cpHTTP.Spec.Services.Tokensmith.TLS.Enabled = false

	existingHTTP := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceTokensmith,
			Namespace: ControlPlaneNamespace(cpHTTP),
		},
		Status: appsv1.DeploymentStatus{AvailableReplicas: 1},
	}

	cHTTP := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cpHTTP).
		WithStatusSubresource(&appsv1.Deployment{}).
		WithObjects(existingHTTP).
		Build()

	rHTTP := &TokensmithReconciler{Client: cHTTP, Recorder: record.NewFakeRecorder(10)}
	if _, err := rHTTP.Reconcile(context.Background(), cpHTTP); err != nil {
		t.Fatalf("reconcile HTTP: %v", err)
	}

	if st, ok := cpHTTP.Status.Services[ServiceTokensmith]; ok {
		if !strings.HasPrefix(st.Endpoint, "http://") {
			t.Errorf("expected http:// endpoint when TLS disabled, got %q", st.Endpoint)
		}
	} else {
		t.Errorf("expected status.services[tokensmith] to be set")
	}

	// Test with TLS
	cpHTTPS := newTokensmithCluster()
	cpHTTPS.Spec.Services.Tokensmith.TLS.Enabled = true

	existingHTTPS := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceTokensmith,
			Namespace: ControlPlaneNamespace(cpHTTPS),
		},
		Status: appsv1.DeploymentStatus{AvailableReplicas: 1},
	}

	cHTTPS := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cpHTTPS).
		WithStatusSubresource(&appsv1.Deployment{}).
		WithObjects(existingHTTPS).
		Build()

	rHTTPS := &TokensmithReconciler{Client: cHTTPS, Recorder: record.NewFakeRecorder(10)}
	if _, err := rHTTPS.Reconcile(context.Background(), cpHTTPS); err != nil {
		t.Fatalf("reconcile HTTPS: %v", err)
	}

	if st, ok := cpHTTPS.Status.Services[ServiceTokensmith]; ok {
		if !strings.HasPrefix(st.Endpoint, "https://") {
			t.Errorf("expected https:// endpoint when TLS enabled, got %q", st.Endpoint)
		}
	} else {
		t.Errorf("expected status.services[tokensmith] to be set")
	}
}
