// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/logging"
)

const (
	logqCompactorRequeueAfter = 30 * time.Second

	// reasonCompactorImageNotConfigured is set on LogCompactorReady when
	// compactor is enabled but no image override is supplied.
	reasonCompactorImageNotConfigured = "CompactorImageNotConfigured"
)

// LogqCompactorReconciler ensures the logq-compactor CronJob exists to convert
// NDJSON logs to Parquet daily.
//
// The compactor runs as a scheduled job that reads raw NDJSON from the log
// bucket and writes compacted Parquet files back to the same bucket.
type LogqCompactorReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

// Reconcile applies the logq-compactor CronJob and reports ConditionLogCompactorReady.
func (r *LogqCompactorReconciler) Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "logq-compactor")

	if !cp.Spec.Logging.Enabled || !cp.Spec.Logging.CompactorEnabled {
		log.Info("logq-compactor disabled, skipping")
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionLogCompactorReady,
			Status:             metav1.ConditionTrue,
			Reason:             conditions.ReasonReady,
			Message:            "logq-compactor disabled",
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{}, nil
	}

	// Refuse to schedule the CronJob when no image override is supplied.
	// Similar to funicular collector, we require explicit image configuration.
	if cp.Spec.Logging.CompactorImage == nil {
		log.Info("compactor enabled but spec.logging.compactorImage is unset; skipping CronJob apply")
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionLogCompactorReady,
			Status:             metav1.ConditionFalse,
			Reason:             reasonCompactorImageNotConfigured,
			Message:            "spec.logging.compactorImage is required when spec.logging.compactorEnabled=true",
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{RequeueAfter: logqCompactorRequeueAfter}, nil
	}

	// Wait for log bucket to be ready before deploying compactor
	if !apimeta.IsStatusConditionTrue(cp.Status.Conditions, conditions.ConditionLogBucketReady) {
		log.Info("waiting for log bucket before deploying compactor")
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionLogCompactorReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonProvisioning,
			Message:            "waiting for LogBucketReady before scheduling compactor",
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{RequeueAfter: logqCompactorRequeueAfter}, nil
	}

	cj := r.buildCronJob(cp)
	cjLog := logging.EnrichWithResource(log, kindCronJob, cj.Name)
	cjLog.Info("applying logq-compactor CronJob")
	if err := r.Client.Patch(ctx, cj, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying logq-compactor CronJob: %w", err)
	}

	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionLogCompactorReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            "logq-compactor CronJob applied",
		ObservedGeneration: cp.Generation,
	})
	return ctrl.Result{}, nil
}

// Describe returns the Kubernetes objects this reconciler would apply.
// Returns an empty (but non-nil) slice when compactor is disabled.
func (r *LogqCompactorReconciler) Describe(cp *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
	if !cp.Spec.Logging.Enabled || !cp.Spec.Logging.CompactorEnabled {
		return []client.Object{}, nil
	}
	return []client.Object{r.buildCronJob(cp)}, nil
}

// logqCompactorPodLabels returns the canonical label set for logq-compactor pods.
func logqCompactorPodLabels(cp *openchamiv1alpha1.OpenCHAMIControlPlane) map[string]string {
	return map[string]string{
		labelAppName:   ServiceLogqCompactor,
		labelAppInst:   "openchami-" + cp.Spec.ClusterName,
		labelManagedBy: managedByValue,
	}
}

func (r *LogqCompactorReconciler) buildCronJob(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *batchv1.CronJob {
	labels := logqCompactorPodLabels(cp)
	tmpVol, tmpMount := TmpVolume()

	ns := ControlPlaneNamespace(cp)
	bucket := LogBucketName(cp)
	credsSecret := SecretName(cp, SuffixLogCredentials)

	env := []corev1.EnvVar{
		{Name: "LOGQ_CLUSTER_NAME", Value: cp.Spec.ClusterName},
		{Name: "LOGQ_NAMESPACE", Value: ns},
		{Name: "LOGQ_S3_ENDPOINT", Value: cp.Spec.Platform.ObjectStorage.Endpoint},
		{Name: "LOGQ_S3_BUCKET", Value: bucket},
		{
			Name: "LOGQ_S3_ACCESS_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: credsSecret},
					Key:                  s3AccessKeyKey,
				},
			},
		},
		{
			Name: "LOGQ_S3_SECRET_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: credsSecret},
					Key:                  s3SecretKeyKey,
				},
			},
		},
	}

	image, pullPolicy := ResolveImage(cp, ServiceLogqCompactor)
	container := corev1.Container{
		Name:            ServiceLogqCompactor,
		Image:           image,
		ImagePullPolicy: pullPolicy,
		SecurityContext: CommonSecurityContext(),
		Env:             env,
		VolumeMounts:    []corev1.VolumeMount{tmpMount},
	}

	schedule := cp.Spec.Logging.CompactorSchedule
	if schedule == "" {
		schedule = "0 2 * * *" // Default to 2 AM daily
	}

	return &batchv1.CronJob{
		TypeMeta: metav1.TypeMeta{APIVersion: batchAPIVersion, Kind: kindCronJob},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceLogqCompactor,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: batchv1.CronJobSpec{
			Schedule:          schedule,
			ConcurrencyPolicy: batchv1.ForbidConcurrent,
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec: corev1.PodSpec{
							ServiceAccountName: ServiceLogqCompactor,
							EnableServiceLinks: DisableServiceLinks(),
							RestartPolicy:      corev1.RestartPolicyOnFailure,
							SecurityContext:    CommonPodSecurityContext(),
							Containers:         []corev1.Container{container},
							Volumes:            []corev1.Volume{tmpVol},
						},
					},
				},
			},
		},
	}
}
