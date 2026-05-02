/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package reconcilers

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openchami/openchami-operator/internal/conditions"
	s3fake "github.com/openchami/openchami-operator/internal/s3/fake"
)

// logCredsSecret returns a Secret matching what VSO would sync from the
// cluster's log-credentials Vault path.
func logCredsSecret(clusterName string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openchami-" + clusterName + "-" + SuffixLogCredentials,
			Namespace: "openchami-" + clusterName,
		},
		Data: map[string][]byte{
			s3AccessKeyKey: []byte("AKIA-test"),
			s3SecretKeyKey: []byte("secret-test"),
		},
	}
}

func TestLogBucketReconciler_DisabledTriviallyReady(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.Logging.Enabled = false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	s3c := s3fake.NewClient()
	r := &LogBucketReconciler{Client: c, Recorder: record.NewFakeRecorder(10), S3Client: s3c}

	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if s3c.CallCount("EnsureBucket") != 0 {
		t.Errorf("expected no EnsureBucket calls when logging disabled, got %d",
			s3c.CallCount("EnsureBucket"))
	}
	if s3c.CallCount("EnsureLifecycleRule") != 0 {
		t.Errorf("expected no EnsureLifecycleRule calls when logging disabled, got %d",
			s3c.CallCount("EnsureLifecycleRule"))
	}
	if cluster.Status.LogBucket != "" {
		t.Errorf("expected status.logBucket empty when disabled, got %q",
			cluster.Status.LogBucket)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionLogBucketReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != conditions.ReasonReady {
		t.Fatalf("expected LogBucketReady=True/Ready when disabled, got %+v", cond)
	}
}

func TestLogBucketReconciler_WaitsForCredsSecret(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.Logging.Enabled = true
	cluster.Spec.Logging.RetentionDays = 90
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	s3c := s3fake.NewClient()
	r := &LogBucketReconciler{Client: c, Recorder: record.NewFakeRecorder(10), S3Client: s3c}

	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue when log creds Secret missing, got %+v", res)
	}

	if s3c.CallCount("EnsureBucket") != 0 {
		t.Errorf("expected no S3 calls before creds present, got EnsureBucket=%d",
			s3c.CallCount("EnsureBucket"))
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionLogBucketReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonProvisioning {
		t.Fatalf("expected LogBucketReady=False/Provisioning, got %+v", cond)
	}
}

func TestLogBucketReconciler_HappyPath(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.Logging.Enabled = true
	cluster.Spec.Logging.RetentionDays = 45

	creds := logCredsSecret(cluster.Spec.ClusterName)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, creds).Build()

	s3c := s3fake.NewClient()
	r := &LogBucketReconciler{Client: c, Recorder: record.NewFakeRecorder(10), S3Client: s3c}

	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue on happy path, got %+v", res)
	}

	bucket := LogBucketName(cluster)
	s3c.AssertCalled(t, "EnsureBucket")
	s3c.AssertCalled(t, "EnsureLifecycleRule")
	s3c.AssertBucketExists(t, bucket)
	if got := s3c.Lifecycles[bucket]; got != 45 {
		t.Errorf("expected lifecycle retention=45 on bucket %q, got %d", bucket, got)
	}

	if cluster.Status.LogBucket != bucket {
		t.Errorf("expected status.logBucket=%q, got %q", bucket, cluster.Status.LogBucket)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionLogBucketReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != conditions.ReasonReady {
		t.Fatalf("expected LogBucketReady=True/Ready, got %+v", cond)
	}
}

func TestLogBucketReconciler_BucketEnsureError(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.Logging.Enabled = true
	cluster.Spec.Logging.RetentionDays = 30

	creds := logCredsSecret(cluster.Spec.ClusterName)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, creds).Build()

	s3c := s3fake.NewClient()
	s3c.Errors["EnsureBucket"] = errors.New("versitygw down")
	r := &LogBucketReconciler{Client: c, Recorder: record.NewFakeRecorder(10), S3Client: s3c}

	_, err := r.Reconcile(context.Background(), cluster)
	if err == nil {
		t.Fatalf("expected error when EnsureBucket fails")
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionLogBucketReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonError {
		t.Fatalf("expected LogBucketReady=False/Error, got %+v", cond)
	}

	if cluster.Status.LogBucket != "" {
		t.Errorf("expected status.logBucket empty on error, got %q", cluster.Status.LogBucket)
	}
}

func TestLogBucketReconciler_TwoClustersIsolated(t *testing.T) {
	scheme := newScheme(t)
	s3c := s3fake.NewClient()
	rec := record.NewFakeRecorder(20)

	for _, name := range []string{testClusterRed, testClusterBlue} {
		cluster := newCluster(name)
		cluster.Spec.Logging.Enabled = true
		cluster.Spec.Logging.RetentionDays = 14
		creds := logCredsSecret(name)
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(cluster, creds).Build()
		r := &LogBucketReconciler{Client: c, Recorder: rec, S3Client: s3c}
		if _, err := r.Reconcile(context.Background(), cluster); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}
	}

	redBucket := testClusterRed + "-logs"
	blueBucket := testClusterBlue + "-logs"
	if redBucket == blueBucket {
		t.Fatalf("two clusters share log bucket name %q", redBucket)
	}
	s3c.AssertBucketExists(t, redBucket)
	s3c.AssertBucketExists(t, blueBucket)
	if s3c.Lifecycles[redBucket] != 14 || s3c.Lifecycles[blueBucket] != 14 {
		t.Errorf("expected both buckets retention=14, got red=%d blue=%d",
			s3c.Lifecycles[redBucket], s3c.Lifecycles[blueBucket])
	}
}
