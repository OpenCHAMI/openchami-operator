/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package reconcilers

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func metadataEnvByName(envs []corev1.EnvVar, name string) (corev1.EnvVar, bool) {
	for _, e := range envs {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

func TestMetadataServiceReconciler_DisabledSkips(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.Services.MetadataService.Enabled = false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	r := &MetadataServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
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
		Name:      ServiceMetadataService,
	}, dep)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected no Deployment when disabled, got err=%v", err)
	}
}

func TestMetadataServiceReconciler_AppliesAllResources(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.Services.MetadataService.Enabled = true
	cluster.Spec.Services.MetadataService.Replicas = 2
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	r := &MetadataServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ns := ClusterNamespace(cluster)

	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ns, Name: ServiceMetadataService,
	}, dep); err != nil {
		t.Fatalf("getting Deployment: %v", err)
	}
	if dep.Name != "metadata-service" {
		t.Errorf("expected Deployment name metadata-service, got %s", dep.Name)
	}
	if len(dep.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(dep.Spec.Template.Spec.Containers))
	}
	container := dep.Spec.Template.Spec.Containers[0]
	if len(container.Ports) != 1 || container.Ports[0].ContainerPort != 8081 {
		t.Errorf("expected container port 8081, got %+v", container.Ports)
	}

	// Env vars
	clusterNameEnv, ok := metadataEnvByName(container.Env, "METADATA_CLUSTER_NAME")
	if !ok {
		t.Fatalf("METADATA_CLUSTER_NAME env var not found")
	}
	if clusterNameEnv.Value != "alpha" {
		t.Errorf("expected METADATA_CLUSTER_NAME=alpha, got %q", clusterNameEnv.Value)
	}

	smdURLEnv, ok := metadataEnvByName(container.Env, "METADATA_SMD_URL")
	if !ok {
		t.Fatalf("METADATA_SMD_URL env var not found")
	}
	wantSMD := "http://smd.openchami-alpha.svc.cluster.local:27779"
	if smdURLEnv.Value != wantSMD {
		t.Errorf("expected METADATA_SMD_URL=%q, got %q", wantSMD, smdURLEnv.Value)
	}

	jwksURLEnv, ok := metadataEnvByName(container.Env, "METADATA_JWKS_URL")
	if !ok {
		t.Fatalf("METADATA_JWKS_URL env var not found")
	}
	wantJWKS := "http://tokensmith.openchami-alpha.svc.cluster.local:8080/.well-known/jwks.json"
	if jwksURLEnv.Value != wantJWKS {
		t.Errorf("expected METADATA_JWKS_URL=%q, got %q", wantJWKS, jwksURLEnv.Value)
	}

	// Service
	svc := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ns, Name: ServiceMetadataService,
	}, svc); err != nil {
		t.Fatalf("getting Service: %v", err)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 8081 {
		t.Errorf("expected Service port 8081, got %+v", svc.Spec.Ports)
	}

	// PDB
	pdb := &policyv1.PodDisruptionBudget{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ns, Name: ServiceMetadataService,
	}, pdb); err != nil {
		t.Fatalf("getting PDB: %v", err)
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Errorf("expected PDB MinAvailable=1, got %+v", pdb.Spec.MinAvailable)
	}
}

func TestMetadataServiceReconciler_RequeuesUntilAvailable(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.Services.MetadataService.Enabled = true
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	r := &MetadataServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while not available, got %+v", res)
	}

	got, ok := cluster.Status.Services[ServiceMetadataService]
	if !ok {
		t.Fatalf("expected metadata-service in status.services")
	}
	if got.Ready {
		t.Errorf("expected Ready=false while AvailableReplicas=0, got Ready=true")
	}
}

func TestMetadataServiceReconciler_ReadyWhenAvailable(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.Services.MetadataService.Enabled = true

	// Pre-create a Deployment so we can stamp status.AvailableReplicas=1.
	preDep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceMetadataService,
			Namespace: ClusterNamespace(cluster),
		},
		Status: appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&appsv1.Deployment{}).
		WithObjects(preDep).
		Build()

	r := &MetadataServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when ready, got %+v", res)
	}

	got, ok := cluster.Status.Services[ServiceMetadataService]
	if !ok {
		t.Fatalf("expected metadata-service in status.services")
	}
	if !got.Ready {
		t.Errorf("expected Ready=true with AvailableReplicas=1, got Ready=false")
	}
	wantEndpoint := "http://metadata-service.openchami-alpha.svc.cluster.local:8081"
	if got.Endpoint != wantEndpoint {
		t.Errorf("expected endpoint=%q, got %q", wantEndpoint, got.Endpoint)
	}
}

func TestMetadataServiceReconciler_TwoClustersIsolated(t *testing.T) {
	scheme := newScheme(t)
	for _, name := range []string{testClusterRed, testClusterBlue} {
		cluster := newCluster(name)
		cluster.Spec.Services.MetadataService.Enabled = true
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
		r := &MetadataServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
		if _, err := r.Reconcile(context.Background(), cluster); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}

		dep := &appsv1.Deployment{}
		if err := c.Get(context.Background(), types.NamespacedName{
			Namespace: "openchami-" + name,
			Name:      ServiceMetadataService,
		}, dep); err != nil {
			t.Fatalf("getting Deployment for %s: %v", name, err)
		}
		if dep.Namespace != "openchami-"+name {
			t.Errorf("expected namespace openchami-%s, got %s", name, dep.Namespace)
		}

		container := dep.Spec.Template.Spec.Containers[0]
		clusterNameEnv, ok := metadataEnvByName(container.Env, "METADATA_CLUSTER_NAME")
		if !ok {
			t.Fatalf("METADATA_CLUSTER_NAME env var not found for %s", name)
		}
		if clusterNameEnv.Value != name {
			t.Errorf("expected METADATA_CLUSTER_NAME=%s, got %q", name, clusterNameEnv.Value)
		}
	}
}
