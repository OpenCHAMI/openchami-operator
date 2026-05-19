// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

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

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
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
func (r *LogBucketReconciler) Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "logbucket")

	if !cp.Spec.Logging.Enabled {
		log.Info("logging disabled, skipping log bucket")
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionLogBucketReady,
			Status:             metav1.ConditionTrue,
			Reason:             conditions.ReasonReady,
			Message:            "logging disabled",
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{}, nil
	}

	if r.S3Client == nil {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionLogBucketReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonError,
			Message:            "s3 client not configured",
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{RequeueAfter: logBucketRequeueAfter}, nil
	}

	credsName := SecretName(cp, SuffixLogCredentials)
	creds := &corev1.Secret{}
	err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      credsName,
	}, creds)
	if apierrors.IsNotFound(err) {
		log.Info("waiting for VSO to sync log credentials Secret",
			"secret", credsName)
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionLogBucketReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonProvisioning,
			Message:            "waiting for VSO to sync log credentials",
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{RequeueAfter: logBucketRequeueAfter}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting log credentials secret: %w", err)
	}

	bucket := LogBucketName(cp)
	retention := cp.Spec.Logging.RetentionDays
	log.Info("ensuring log bucket on VersityGW",
		"bucket", bucket, "retentionDays", retention)

	if err := r.S3Client.EnsureBucket(ctx, bucket); err != nil {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionLogBucketReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonError,
			Message:            fmt.Sprintf("ensuring log bucket: %v", err),
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{}, fmt.Errorf("ensuring log bucket %s: %w", bucket, err)
	}

	if err := r.S3Client.EnsureLifecycleRule(ctx, bucket, retention); err != nil {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionLogBucketReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonError,
			Message:            fmt.Sprintf("ensuring log bucket lifecycle rule: %v", err),
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{}, fmt.Errorf("ensuring log bucket lifecycle rule on %s: %w", bucket, err)
	}

	cp.Status.LogBucket = bucket
	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionLogBucketReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            fmt.Sprintf("log bucket %s ready (retention %d days)", bucket, retention),
		ObservedGeneration: cp.Generation,
	})
	return ctrl.Result{}, nil
}

// Describe returns no Kubernetes objects: the log bucket is provisioned
// directly against VersityGW (S3) and has no in-cluster representation.
func (r *LogBucketReconciler) Describe(_ *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
	return []client.Object{}, nil
}
