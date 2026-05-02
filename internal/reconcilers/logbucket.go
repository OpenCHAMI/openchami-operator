/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package reconcilers

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openahamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/logging"
	"github.com/openchami/openchami-operator/internal/s3"
)

const logBucketRequeueAfter = 10 * time.Second

// LogBucketReconciler ensures the legendary-funicular log bucket exists on
// VersityGW with the configured retention lifecycle rule applied.
//
// The bucket lives outside Kubernetes (S3), so Describe() returns no
// objects; the reconciler logs the resolved bucket parameters during
// Reconcile() instead.
type LogBucketReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
	S3Client s3.Client
}

// Reconcile waits for VSO to materialise the log-credentials Secret, then
// ensures the log bucket and its delete-after-N-days lifecycle rule exist.
// Idempotent: every call produces the same end state.
func (r *LogBucketReconciler) Reconcile(ctx context.Context, cluster *openahamiv1alpha1.OpenCHAMICluster) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cluster, "logbucket")

	if !cluster.Spec.Logging.Enabled {
		log.Info("logging disabled, skipping log bucket")
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionLogBucketReady,
			Status:             metav1.ConditionTrue,
			Reason:             conditions.ReasonReady,
			Message:            "logging disabled",
			ObservedGeneration: cluster.Generation,
		})
		return ctrl.Result{}, nil
	}

	if r.S3Client == nil {
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionLogBucketReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonError,
			Message:            "s3 client not configured",
			ObservedGeneration: cluster.Generation,
		})
		return ctrl.Result{RequeueAfter: logBucketRequeueAfter}, nil
	}

	credsName := SecretName(cluster, SuffixLogCredentials)
	creds := &corev1.Secret{}
	err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: ClusterNamespace(cluster),
		Name:      credsName,
	}, creds)
	if apierrors.IsNotFound(err) {
		log.Info("waiting for VSO to sync log credentials Secret",
			"secret", credsName)
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionLogBucketReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonProvisioning,
			Message:            "waiting for VSO to sync log credentials",
			ObservedGeneration: cluster.Generation,
		})
		return ctrl.Result{RequeueAfter: logBucketRequeueAfter}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting log credentials secret: %w", err)
	}

	bucket := LogBucketName(cluster)
	retention := cluster.Spec.Logging.RetentionDays
	log.Info("ensuring log bucket on VersityGW",
		"bucket", bucket, "retentionDays", retention)

	if err := r.S3Client.EnsureBucket(ctx, bucket); err != nil {
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionLogBucketReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonError,
			Message:            fmt.Sprintf("ensuring log bucket: %v", err),
			ObservedGeneration: cluster.Generation,
		})
		return ctrl.Result{}, fmt.Errorf("ensuring log bucket %s: %w", bucket, err)
	}

	if err := r.S3Client.EnsureLifecycleRule(ctx, bucket, retention); err != nil {
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionLogBucketReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonError,
			Message:            fmt.Sprintf("ensuring log bucket lifecycle rule: %v", err),
			ObservedGeneration: cluster.Generation,
		})
		return ctrl.Result{}, fmt.Errorf("ensuring log bucket lifecycle rule on %s: %w", bucket, err)
	}

	cluster.Status.LogBucket = bucket
	apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionLogBucketReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            fmt.Sprintf("log bucket %s ready (retention %d days)", bucket, retention),
		ObservedGeneration: cluster.Generation,
	})
	return ctrl.Result{}, nil
}

// Describe returns no Kubernetes objects: the log bucket is provisioned
// directly against VersityGW (S3) and has no in-cluster representation.
func (r *LogBucketReconciler) Describe(_ *openahamiv1alpha1.OpenCHAMICluster) ([]client.Object, error) {
	return []client.Object{}, nil
}
