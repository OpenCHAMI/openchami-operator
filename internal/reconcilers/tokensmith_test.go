/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

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

	openahamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

func newTokensmithCluster() *openahamiv1alpha1.OpenCHAMICluster {
	cluster := newCluster("alpha")
	cluster.Spec.Services.Tokensmith.Enabled = true
	return cluster
}

func TestTokensmithReconciler_DisabledSkips(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.Services.Tokensmith.Enabled = false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	r := &TokensmithReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when disabled, got %+v", res)
	}

	dep := &appsv1.Deployment{}
	err = c.Get(context.Background(), types.NamespacedName{
		Namespace: ClusterNamespace(cluster),
		Name:      ServiceTokensmith,
	}, dep)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected Deployment to be absent when disabled, got err=%v", err)
	}
}

func TestTokensmithReconciler_AppliesAllResources(t *testing.T) {
	scheme := newScheme(t)
	cluster := newTokensmithCluster()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	r := &TokensmithReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ns := ClusterNamespace(cluster)

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
	wantSecret := SecretName(cluster, SuffixTokensmithOIDC)
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
	cluster := newTokensmithCluster()
	cluster.Spec.Services.Tokensmith.OIDCProvider = "external"
	cluster.Spec.Services.Tokensmith.OIDCIssuerURL = ""
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	rec := record.NewFakeRecorder(10)
	r := &TokensmithReconciler{Client: c, Recorder: rec}
	_, err := r.Reconcile(context.Background(), cluster)
	if err == nil {
		t.Fatalf("expected error when oidcProvider=external without oidcIssuerURL")
	}

	dep := &appsv1.Deployment{}
	gerr := c.Get(context.Background(), types.NamespacedName{
		Namespace: ClusterNamespace(cluster),
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
	cluster := newTokensmithCluster()
	cluster.Spec.Services.Tokensmith.OIDCProvider = "external"
	cluster.Spec.Services.Tokensmith.OIDCIssuerURL = "https://issuer.example/realm"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	r := &TokensmithReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ClusterNamespace(cluster),
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
	cluster := newTokensmithCluster()

	// Pre-create the Deployment with AvailableReplicas=1 so reconcile reads it back as ready.
	existing := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceTokensmith,
			Namespace: ClusterNamespace(cluster),
		},
		Status: appsv1.DeploymentStatus{
			AvailableReplicas: 1,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&appsv1.Deployment{}).
		WithObjects(existing).
		Build()

	r := &TokensmithReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue once ready, got %+v", res)
	}

	st, ok := cluster.Status.Services[ServiceTokensmith]
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
	cluster := newTokensmithCluster()
	cluster.Spec.Services.Tokensmith.Replicas = 5

	r := &TokensmithReconciler{}
	dep := r.buildDeployment(cluster)
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
		t.Errorf("expected replicas=1 even when spec.replicas=5, got %v", dep.Spec.Replicas)
	}
}
