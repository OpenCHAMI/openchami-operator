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

const bucketRequeueAfter = 10 * time.Second

// BucketReconciler ensures the boot-images S3 bucket exists on VersityGW.
type BucketReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
	S3Client s3.Client
}

func (r *BucketReconciler) Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "bucket")
	log.Info("reconciling boot-images bucket")

	if r.S3Client == nil {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionBucketReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonError,
			Message:            "s3 client not configured",
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{RequeueAfter: bucketRequeueAfter}, nil
	}

	credsName := "openchami-" + cp.Spec.ClusterName + "-s3-credentials"
	creds := &corev1.Secret{}
	err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      credsName,
	}, creds)
	if apierrors.IsNotFound(err) {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionBucketReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonProvisioning,
			Message:            "waiting for VSO to materialize s3-credentials Secret",
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{RequeueAfter: bucketRequeueAfter}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting s3 credentials secret: %w", err)
	}

	bucket := BootBucketName(cp)
	if err := r.S3Client.EnsureBucket(ctx, bucket); err != nil {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionBucketReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonError,
			Message:            fmt.Sprintf("ensuring bucket: %v", err),
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{}, fmt.Errorf("ensuring bucket %s: %w", bucket, err)
	}

	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionBucketReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            fmt.Sprintf("bucket %s is ready", bucket),
		ObservedGeneration: cp.Generation,
	})
	return ctrl.Result{}, nil
}

func (r *BucketReconciler) Describe(_ *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
	// Bucket lives in S3, not Kubernetes — no objects to apply.
	return nil, nil
}
