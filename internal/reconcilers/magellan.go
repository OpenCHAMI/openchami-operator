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
	magellanRequeueAfter = 30 * time.Second

	defaultMagellanImage = "ghcr.io/openchami/magellan:latest"
)

// MagellanReconciler ensures the Magellan BMC-discovery CronJob exists.
//
// The reconciler intentionally does not parse Magellan output or surface any
// HPC inventory: that is SMD's responsibility (see invariant #10).
type MagellanReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

// Reconcile applies the Magellan CronJob and reports MagellanReady.
func (r *MagellanReconciler) Reconcile(ctx context.Context, cluster *openchamiv1alpha1.OpenCHAMICluster) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cluster, "magellan")

	if !cluster.Spec.Services.Magellan.Enabled {
		log.Info("magellan disabled, skipping")
		return ctrl.Result{}, nil
	}

	if cluster.Spec.NetworkProbe.Enabled &&
		!apimeta.IsStatusConditionTrue(cluster.Status.Conditions, conditions.ConditionNetworkProbeReady) {
		log.Info("waiting for network probe before deploying magellan")
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionMagellanReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonWaitingForProbe,
			Message:            "waiting for NetworkProbeReady before scheduling magellan",
			ObservedGeneration: cluster.Generation,
		})
		return ctrl.Result{RequeueAfter: magellanRequeueAfter}, nil
	}

	cj := r.buildCronJob(cluster)
	cjLog := logging.EnrichWithResource(log, kindCronJob, cj.Name)
	cjLog.Info("applying magellan CronJob")
	if err := r.Client.Patch(ctx, cj, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying magellan CronJob: %w", err)
	}

	apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionMagellanReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            "magellan CronJob applied",
		ObservedGeneration: cluster.Generation,
	})
	return ctrl.Result{}, nil
}

// Describe returns the Kubernetes objects this reconciler would apply.
// Returns an empty (but non-nil) slice when Magellan is disabled.
func (r *MagellanReconciler) Describe(cluster *openchamiv1alpha1.OpenCHAMICluster) ([]client.Object, error) {
	if !cluster.Spec.Services.Magellan.Enabled {
		return []client.Object{}, nil
	}
	return []client.Object{r.buildCronJob(cluster)}, nil
}

// magellanPodLabels returns the canonical label set for magellan pods.
func magellanPodLabels(cluster *openchamiv1alpha1.OpenCHAMICluster) map[string]string {
	return map[string]string{
		labelAppName:   ServiceMagellan,
		labelAppInst:   "openchami-" + cluster.Spec.ClusterName,
		labelManagedBy: managedByValue,
	}
}

// magellanImage resolves the container image, preferring per-cluster spec
// override, falling back to defaultMagellanImage.
func magellanImage(cluster *openchamiv1alpha1.OpenCHAMICluster) string {
	img := cluster.Spec.Services.Magellan.Image
	if img == nil {
		return defaultMagellanImage
	}
	repo, tag := img.Repository, img.Tag
	switch {
	case repo == "" && tag == "":
		return defaultMagellanImage
	case repo == "":
		return "ghcr.io/openchami/magellan:" + tag
	case tag == "":
		return repo + ":latest"
	default:
		return repo + ":" + tag
	}
}

func (r *MagellanReconciler) buildCronJob(cluster *openchamiv1alpha1.OpenCHAMICluster) *batchv1.CronJob {
	labels := magellanPodLabels(cluster)
	tmpVol, tmpMount := TmpVolume()
	mag := cluster.Spec.Services.Magellan

	env := []corev1.EnvVar{
		{Name: "MAGELLAN_BMC_SUBNET", Value: mag.BMCSubnet},
		fieldRefEnv("NODE_NAME", "spec.nodeName"),
	}

	container := corev1.Container{
		Name:            ServiceMagellan,
		Image:           magellanImage(cluster),
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: CommonSecurityContext(),
		Env:             env,
		VolumeMounts:    []corev1.VolumeMount{tmpMount},
	}
	if mag.Resources != nil {
		container.Resources = *mag.Resources
	}

	return &batchv1.CronJob{
		TypeMeta: metav1.TypeMeta{APIVersion: batchAPIVersion, Kind: kindCronJob},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceMagellan,
			Namespace: ClusterNamespace(cluster),
			Labels:    labels,
		},
		Spec: batchv1.CronJobSpec{
			Schedule:          mag.Schedule,
			ConcurrencyPolicy: mag.ConcurrencyPolicy,
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec: corev1.PodSpec{
							ServiceAccountName: ServiceMagellan,
							RestartPolicy:      corev1.RestartPolicyOnFailure,
							NodeSelector:       EffectiveNodeSelector(cluster, probeTypeBMC),
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
