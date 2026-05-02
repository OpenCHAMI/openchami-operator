/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package reconcilers

import (
	"context"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openahamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
)

func newDBScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := newScheme(t)
	if err := cnpgv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding cnpg scheme: %v", err)
	}
	return scheme
}

func cnpgClusterName(clusterName string) string {
	return "openchami-" + clusterName + "-postgres"
}

func newDBCredsSecret(cluster *openahamiv1alpha1.OpenCHAMICluster) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretName(cluster, SuffixDBCredentials),
			Namespace: ClusterNamespace(cluster),
		},
		Data: map[string][]byte{
			VaultKeySMDPassword:         []byte("smd-pw"),
			VaultKeyBootServicePassword: []byte("boot-pw"),
		},
	}
}

func TestDatabaseReconciler_WaitsForCredentials(t *testing.T) {
	scheme := newDBScheme(t)
	cluster := newCluster("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	r := &DatabaseReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while waiting for db creds, got %+v", res)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionDatabaseReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonProvisioning {
		t.Fatalf("expected DatabaseReady=False/Provisioning, got %+v", cond)
	}
}

func TestDatabaseReconciler_CreatesCNPGCluster(t *testing.T) {
	scheme := newDBScheme(t)
	cluster := newCluster("alpha")
	creds := newDBCredsSecret(cluster)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, creds).Build()

	r := &DatabaseReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &cnpgv1.Cluster{}
	key := types.NamespacedName{
		Namespace: ClusterNamespace(cluster),
		Name:      cnpgClusterName("alpha"),
	}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatalf("getting cnpg cluster: %v", err)
	}
	if got.Spec.Instances != 3 {
		t.Errorf("expected 3 instances by default, got %d", got.Spec.Instances)
	}
	if got.Spec.Bootstrap == nil || got.Spec.Bootstrap.InitDB == nil {
		t.Fatalf("expected bootstrap.initdb to be set")
	}
	if got.Spec.Bootstrap.InitDB.Database != ServiceSMD || got.Spec.Bootstrap.InitDB.Owner != ServiceSMD {
		t.Errorf("unexpected initdb config: %+v", got.Spec.Bootstrap.InitDB)
	}
	if got.Spec.Bootstrap.InitDB.Secret == nil ||
		got.Spec.Bootstrap.InitDB.Secret.Name != creds.Name {
		t.Errorf("expected initdb secret to reference %s", creds.Name)
	}
}

func TestDatabaseReconciler_DegradedWhileNotHealthy(t *testing.T) {
	scheme := newDBScheme(t)
	cluster := newCluster("alpha")
	creds := newDBCredsSecret(cluster)
	cnpg := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cnpgClusterName("alpha"),
			Namespace: ClusterNamespace(cluster),
		},
		Status: cnpgv1.ClusterStatus{Phase: "Setting up primary"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster, creds).
		WithStatusSubresource(&cnpgv1.Cluster{}).
		WithObjects(cnpg).
		Build()

	r := &DatabaseReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while not healthy")
	}
	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionDatabaseReady)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected DatabaseReady=False, got %+v", cond)
	}

	// Post-init job should NOT exist yet.
	job := &batchv1.Job{}
	err = c.Get(context.Background(), types.NamespacedName{
		Namespace: ClusterNamespace(cluster),
		Name:      "openchami-alpha-db-init",
	}, job)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected post-init job to be absent, got err=%v", err)
	}
}

func TestDatabaseReconciler_PostInitJobOnceHealthy(t *testing.T) {
	scheme := newDBScheme(t)
	cluster := newCluster("alpha")
	creds := newDBCredsSecret(cluster)
	cnpg := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cnpgClusterName("alpha"),
			Namespace: ClusterNamespace(cluster),
		},
		Status: cnpgv1.ClusterStatus{Phase: cnpgv1.PhaseHealthy},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster, creds, cnpg).
		Build()

	r := &DatabaseReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionDatabaseReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected DatabaseReady=True, got %+v", cond)
	}

	job := &batchv1.Job{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ClusterNamespace(cluster),
		Name:      "openchami-alpha-db-init",
	}, job); err != nil {
		t.Fatalf("expected post-init job to exist: %v", err)
	}
	if len(job.Spec.Template.Spec.Containers) != 1 ||
		job.Spec.Template.Spec.Containers[0].Image != dbInitImage {
		t.Errorf("unexpected init job container: %+v", job.Spec.Template.Spec.Containers)
	}
}

func TestDatabaseReconciler_TwoClustersIsolated(t *testing.T) {
	scheme := newDBScheme(t)
	for _, name := range []string{testClusterRed, testClusterBlue} {
		cluster := newCluster(name)
		creds := newDBCredsSecret(cluster)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, creds).Build()
		r := &DatabaseReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
		if _, err := r.Reconcile(context.Background(), cluster); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}

		got := &cnpgv1.Cluster{}
		if err := c.Get(context.Background(), types.NamespacedName{
			Namespace: "openchami-" + name,
			Name:      "openchami-" + name + "-postgres",
		}, got); err != nil {
			t.Fatalf("getting cnpg cluster for %s: %v", name, err)
		}
		if got.Namespace != "openchami-"+name {
			t.Errorf("expected namespace openchami-%s, got %s", name, got.Namespace)
		}
	}
}
