// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"strings"
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

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
)

const alphaDBInitJobName = "openchami-alpha-db-init"

func newDBScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := newScheme(t)
	if err := cnpgv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding cnpg scheme: %v", err)
	}
	return scheme
}

func cnpgClusterName() string {
	return "openchami-alpha-postgres"
}

func newDBCredsSecret(cluster *openchamiv1alpha1.OpenCHAMICluster) *corev1.Secret {
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
		Name:      cnpgClusterName(),
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
			Name:      cnpgClusterName(),
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
		Name:      alphaDBInitJobName,
	}, job)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected post-init job to be absent, got err=%v", err)
	}
}

// TestDatabaseReconciler_PostInitJobCreatesAndGatesReady covers the full
// post-init job lifecycle introduced 2026-05-04:
//
//  1. First reconcile against a healthy CNPG cluster creates the job and
//     reports Provisioning (NOT Ready), since the job hasn't run yet.
//  2. While the job is running (Succeeded=0, Failed=0), Ready stays False.
//  3. After the job reports Succeeded>0, Ready flips to True.
//  4. If the job fails, Ready=False with Reason=Error and a Warning Event is
//     emitted so an operator notices instead of silently flipping back next
//     reconcile.
//
// This is a regression test for the prior bug where the controller marked
// DatabaseReady=True the moment the job was *created*, regardless of outcome.
func TestDatabaseReconciler_PostInitJobCreatesAndGatesReady(t *testing.T) {
	scheme := newDBScheme(t)
	cluster := newCluster("alpha")
	creds := newDBCredsSecret(cluster)
	cnpg := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cnpgClusterName(),
			Namespace: ClusterNamespace(cluster),
		},
		Status: cnpgv1.ClusterStatus{Phase: cnpgv1.PhaseHealthy},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster, creds, cnpg).
		Build()
	rec := record.NewFakeRecorder(10)
	r := &DatabaseReconciler{Client: c, Recorder: rec}

	// 1: first reconcile creates the job; Ready=False, Reason=Provisioning.
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionDatabaseReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonProvisioning {
		t.Fatalf("after first reconcile expected Ready=False/Provisioning, got %+v", cond)
	}

	job := &batchv1.Job{}
	jobName := types.NamespacedName{Namespace: ClusterNamespace(cluster), Name: alphaDBInitJobName}
	if err := c.Get(context.Background(), jobName, job); err != nil {
		t.Fatalf("expected post-init job to exist: %v", err)
	}
	if len(job.Spec.Template.Spec.Containers) != 1 ||
		job.Spec.Template.Spec.Containers[0].Image != dbInitImage {
		t.Errorf("unexpected init job container: %+v", job.Spec.Template.Spec.Containers)
	}

	// 2: job exists but has not yet reported success — Ready stays False.
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	cond = apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionDatabaseReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonProvisioning {
		t.Fatalf("with in-flight job expected Ready=False/Provisioning, got %+v", cond)
	}

	// 3: simulate the job completing successfully — Ready flips to True.
	job.Status.Succeeded = 1
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatalf("updating job status to Succeeded: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	cond = apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionDatabaseReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("with succeeded job expected Ready=True, got %+v", cond)
	}
}

func TestDatabaseReconciler_PostInitJobFailedSurfacesError(t *testing.T) {
	scheme := newDBScheme(t)
	cluster := newCluster("alpha")
	creds := newDBCredsSecret(cluster)
	cnpg := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cnpgClusterName(),
			Namespace: ClusterNamespace(cluster),
		},
		Status: cnpgv1.ClusterStatus{Phase: cnpgv1.PhaseHealthy},
	}
	failedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ClusterNamespace(cluster),
			Name:      alphaDBInitJobName,
		},
		Status: batchv1.JobStatus{Failed: 1},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster, creds, cnpg, failedJob).
		Build()
	rec := record.NewFakeRecorder(10)
	r := &DatabaseReconciler{Client: c, Recorder: rec}

	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionDatabaseReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonError {
		t.Fatalf("expected Ready=False/Error for a failed job, got %+v", cond)
	}

	select {
	case ev := <-rec.Events:
		// Event format: "<Type> <Reason> <Message>".
		if !strings.Contains(ev, conditions.ReasonError) {
			t.Errorf("expected Warning event with reason=%s, got %q", conditions.ReasonError, ev)
		}
	default:
		t.Errorf("expected a Warning event for the failed post-init job, got none")
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
