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

	openahamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

func newBootServiceCluster(name string) *openahamiv1alpha1.OpenCHAMICluster {
	cluster := newCluster(name)
	cluster.Spec.Services.BootService.Enabled = true
	cluster.Spec.Services.BootService.Replicas = 2
	return cluster
}

func bootServiceEnvByName(envs []corev1.EnvVar, name string) (corev1.EnvVar, bool) {
	for _, e := range envs {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

func TestBootServiceReconciler_DisabledSkips(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.Services.BootService.Enabled = false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	r := &BootServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
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
		Name:      ServiceBootService,
	}, dep)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected boot-service Deployment to be absent, got err=%v", err)
	}
}

func TestBootServiceReconciler_AppliesAllResources(t *testing.T) {
	scheme := newScheme(t)
	cluster := newBootServiceCluster("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	r := &BootServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ns := ClusterNamespace(cluster)

	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: ServiceBootService}, dep); err != nil {
		t.Fatalf("getting boot-service Deployment: %v", err)
	}
	if len(dep.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(dep.Spec.Template.Spec.Containers))
	}
	cont := dep.Spec.Template.Spec.Containers[0]
	if cont.Name != ServiceBootService {
		t.Errorf("expected container name %q, got %q", ServiceBootService, cont.Name)
	}
	if len(cont.Ports) != 1 || cont.Ports[0].ContainerPort != 27778 {
		t.Errorf("expected single port 27778, got %+v", cont.Ports)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
		t.Errorf("expected replicas=2, got %v", dep.Spec.Replicas)
	}
	if dep.Spec.Template.Spec.ServiceAccountName != ServiceBootService {
		t.Errorf("expected ServiceAccountName=%q, got %q", ServiceBootService, dep.Spec.Template.Spec.ServiceAccountName)
	}

	dbpass, ok := bootServiceEnvByName(cont.Env, "BOOT_SERVICE_DBPASS")
	if !ok {
		t.Fatalf("BOOT_SERVICE_DBPASS env var missing")
	}
	if dbpass.ValueFrom == nil || dbpass.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("BOOT_SERVICE_DBPASS missing SecretKeyRef")
	}
	if dbpass.ValueFrom.SecretKeyRef.Name != SecretName(cluster, SuffixDBCredentials) {
		t.Errorf("BOOT_SERVICE_DBPASS expected secret %q, got %q",
			SecretName(cluster, SuffixDBCredentials), dbpass.ValueFrom.SecretKeyRef.Name)
	}
	if dbpass.ValueFrom.SecretKeyRef.Key != VaultKeyBootServicePassword {
		t.Errorf("BOOT_SERVICE_DBPASS expected key %q, got %q",
			VaultKeyBootServicePassword, dbpass.ValueFrom.SecretKeyRef.Key)
	}

	dbname, _ := bootServiceEnvByName(cont.Env, "BOOT_SERVICE_DBNAME")
	if dbname.Value != "boot_service" {
		t.Errorf("BOOT_SERVICE_DBNAME expected %q, got %q", "boot_service", dbname.Value)
	}

	s3ak, ok := bootServiceEnvByName(cont.Env, "BOOT_SERVICE_S3_ACCESS_KEY")
	if !ok || s3ak.ValueFrom == nil || s3ak.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("BOOT_SERVICE_S3_ACCESS_KEY missing or has no SecretKeyRef: %+v", s3ak)
	}
	if s3ak.ValueFrom.SecretKeyRef.Name != SecretName(cluster, SuffixS3Credentials) {
		t.Errorf("BOOT_SERVICE_S3_ACCESS_KEY expected secret %q, got %q",
			SecretName(cluster, SuffixS3Credentials), s3ak.ValueFrom.SecretKeyRef.Name)
	}
	if s3ak.ValueFrom.SecretKeyRef.Key != s3AccessKeyKey {
		t.Errorf("BOOT_SERVICE_S3_ACCESS_KEY expected key %q, got %q", s3AccessKeyKey, s3ak.ValueFrom.SecretKeyRef.Key)
	}

	s3bucket, _ := bootServiceEnvByName(cont.Env, "BOOT_SERVICE_S3_BUCKET")
	if s3bucket.Value != BootBucketName(cluster) {
		t.Errorf("BOOT_SERVICE_S3_BUCKET expected %q, got %q", BootBucketName(cluster), s3bucket.Value)
	}

	svc := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: ServiceBootService}, svc); err != nil {
		t.Fatalf("getting boot-service Service: %v", err)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 27778 {
		t.Errorf("expected service port 27778, got %+v", svc.Spec.Ports)
	}

	pdb := &policyv1.PodDisruptionBudget{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: ServiceBootService}, pdb); err != nil {
		t.Fatalf("getting boot-service PDB: %v", err)
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Errorf("expected PDB MinAvailable=1, got %+v", pdb.Spec.MinAvailable)
	}
}

func TestBootServiceReconciler_RequeuesUntilAvailable(t *testing.T) {
	scheme := newScheme(t)
	cluster := newBootServiceCluster("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	r := &BootServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while not available, got %+v", res)
	}
	st, ok := cluster.Status.Services[ServiceBootService]
	if !ok {
		t.Fatalf("expected status.Services[%q] to be set", ServiceBootService)
	}
	if st.Ready {
		t.Errorf("expected Ready=false, got true")
	}
}

func TestBootServiceReconciler_ReadyWhenAvailable(t *testing.T) {
	scheme := newScheme(t)
	cluster := newBootServiceCluster("alpha")

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceBootService,
			Namespace: ClusterNamespace(cluster),
		},
		Status: appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&appsv1.Deployment{}).
		WithObjects(dep).
		Build()

	r := &BootServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when ready, got %+v", res)
	}
	st, ok := cluster.Status.Services[ServiceBootService]
	if !ok {
		t.Fatalf("expected status.Services[%q] to be set", ServiceBootService)
	}
	if !st.Ready {
		t.Errorf("expected Ready=true, got false (msg=%q)", st.Message)
	}
	if st.Endpoint == "" {
		t.Errorf("expected non-empty endpoint")
	}
}

func TestBootServiceReconciler_TwoClustersIsolated(t *testing.T) {
	scheme := newScheme(t)
	for _, name := range []string{testClusterRed, testClusterBlue} {
		cluster := newBootServiceCluster(name)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
		r := &BootServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
		if _, err := r.Reconcile(context.Background(), cluster); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}

		ns := "openchami-" + name
		dep := &appsv1.Deployment{}
		if err := c.Get(context.Background(), types.NamespacedName{
			Namespace: ns,
			Name:      ServiceBootService,
		}, dep); err != nil {
			t.Fatalf("getting boot-service Deployment for %s: %v", name, err)
		}
		if dep.Namespace != ns {
			t.Errorf("expected namespace %q, got %q", ns, dep.Namespace)
		}

		cont := dep.Spec.Template.Spec.Containers[0]
		dbpass, _ := bootServiceEnvByName(cont.Env, "BOOT_SERVICE_DBPASS")
		expectedDBSecret := "openchami-" + name + "-" + SuffixDBCredentials
		if dbpass.ValueFrom.SecretKeyRef.Name != expectedDBSecret {
			t.Errorf("[%s] expected DB secret %q, got %q", name, expectedDBSecret,
				dbpass.ValueFrom.SecretKeyRef.Name)
		}

		s3ak, _ := bootServiceEnvByName(cont.Env, "BOOT_SERVICE_S3_ACCESS_KEY")
		expectedS3Secret := "openchami-" + name + "-" + SuffixS3Credentials
		if s3ak.ValueFrom.SecretKeyRef.Name != expectedS3Secret {
			t.Errorf("[%s] expected S3 secret %q, got %q", name, expectedS3Secret,
				s3ak.ValueFrom.SecretKeyRef.Name)
		}

		dbhost, _ := bootServiceEnvByName(cont.Env, "BOOT_SERVICE_DBHOST")
		expectedHost := "openchami-" + name + "-postgres-rw.openchami-" + name + ".svc.cluster.local"
		if dbhost.Value != expectedHost {
			t.Errorf("[%s] expected DBHOST %q, got %q", name, expectedHost, dbhost.Value)
		}
	}
}
