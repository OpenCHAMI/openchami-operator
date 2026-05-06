// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
)

const (
	funicularRequeueAfter = 30 * time.Second

	// defaultFunicularImage is the placeholder ghcr path. As of 2026-05
	// no public image is published at this address — `kubelet` will get
	// 403 from the anonymous-token endpoint. The reconciler treats the
	// absence of `LoggingSpec.Image` as `ImageNotConfigured` and refuses
	// to schedule the DaemonSet, so the placeholder is only ever pulled
	// when an operator deliberately overrides Image to this value.
	defaultFunicularImage = "ghcr.io/openchami/funicular-collector:latest"

	funicularHostLogPath = "/var/log/pods"
	funicularLogVolume   = "varlogpods"

	// reasonImageNotConfigured is set on LogCollectorReady when logging is
	// enabled but no funicular image override is supplied. Surfaces in
	// `kubectl describe openchamicluster` so the missing config is obvious.
	reasonImageNotConfigured = "ImageNotConfigured"
)

// FunicularReconciler ensures the legendary-funicular collector DaemonSet
// runs on every node in the cluster, forwarding container logs into the
// per-cluster log bucket on VersityGW.
//
// The collector is a passthrough: this reconciler has no knowledge of what
// the services log (per invariant #10). It only wires endpoints, bucket,
// and credentials.
type FunicularReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

// Reconcile applies the funicular-collector DaemonSet (when logging is
// enabled) and reports ConditionLogCollectorReady from the DaemonSet's
// NumberReady status.
func (r *FunicularReconciler) Reconcile(ctx context.Context, cluster *openchamiv1alpha1.OpenCHAMICluster) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cluster, "funicular")

	if !cluster.Spec.Logging.Enabled {
		log.Info("logging disabled, skipping funicular collector")
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionLogCollectorReady,
			Status:             metav1.ConditionTrue,
			Reason:             conditions.ReasonReady,
			Message:            "logging disabled",
			ObservedGeneration: cluster.Generation,
		})
		return ctrl.Result{}, nil
	}

	// Refuse to schedule the DaemonSet when no image override is supplied.
	// The placeholder `ghcr.io/openchami/funicular-collector:latest` does
	// not resolve to a real image; pulling it would put the pod in
	// ImagePullBackOff with no clear hint that the *operator* expected
	// the user to set `.spec.logging.image`. Surface that as an explicit
	// condition reason instead of letting kubelet's pull error speak for us.
	if cluster.Spec.Logging.Image == nil {
		log.Info("logging enabled but spec.logging.image is unset; skipping DaemonSet apply")
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionLogCollectorReady,
			Status:             metav1.ConditionFalse,
			Reason:             reasonImageNotConfigured,
			Message:            "spec.logging.image is required when spec.logging.enabled=true (no default funicular-collector image is published yet)",
			ObservedGeneration: cluster.Generation,
		})
		return ctrl.Result{RequeueAfter: funicularRequeueAfter}, nil
	}

	ds := r.buildDaemonSet(cluster)
	dsLog := logging.EnrichWithResource(log, kindDaemonSet, ds.Name)
	dsLog.Info("applying funicular-collector DaemonSet")
	if err := r.Client.Patch(ctx, ds, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying funicular-collector DaemonSet: %w", err)
	}

	current := &appsv1.DaemonSet{}
	getErr := r.Client.Get(ctx, types.NamespacedName{Namespace: ds.Namespace, Name: ds.Name}, current)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return ctrl.Result{}, fmt.Errorf("reading funicular-collector DaemonSet status: %w", getErr)
	}
	numberReady := int32(0)
	if getErr == nil {
		numberReady = current.Status.NumberReady
	}

	endpoint := fmt.Sprintf("daemonset/%s.%s", ds.Name, ds.Namespace)
	if cluster.Status.Services == nil {
		cluster.Status.Services = map[string]openchamiv1alpha1.ServiceStatus{}
	}

	if numberReady == 0 {
		cluster.Status.Services[ServiceFunicular] = openchamiv1alpha1.ServiceStatus{
			Ready:    false,
			Endpoint: endpoint,
			Message:  "waiting for funicular-collector DaemonSet pods to become ready",
		}
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionLogCollectorReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonProvisioning,
			Message:            "waiting for funicular-collector DaemonSet pods to become ready",
			ObservedGeneration: cluster.Generation,
		})
		return ctrl.Result{RequeueAfter: funicularRequeueAfter}, nil
	}

	cluster.Status.Services[ServiceFunicular] = openchamiv1alpha1.ServiceStatus{
		Ready:    true,
		Endpoint: endpoint,
		Message:  fmt.Sprintf("funicular-collector DaemonSet ready (numberReady=%d)", numberReady),
	}
	apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionLogCollectorReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            fmt.Sprintf("funicular-collector DaemonSet ready (numberReady=%d)", numberReady),
		ObservedGeneration: cluster.Generation,
	})
	return ctrl.Result{}, nil
}

// Describe returns the Kubernetes objects this reconciler would apply.
// Returns an empty (but non-nil) slice when logging is disabled.
func (r *FunicularReconciler) Describe(cluster *openchamiv1alpha1.OpenCHAMICluster) ([]client.Object, error) {
	if !cluster.Spec.Logging.Enabled {
		return []client.Object{}, nil
	}
	return []client.Object{r.buildDaemonSet(cluster)}, nil
}

// funicularImage resolves the container image, preferring the per-cluster
// spec override and falling back to defaultFunicularImage. The reconciler
// guards against an unset Image elsewhere; this helper only runs after
// that guard, so it always has a non-nil ImageSpec to read from.
func funicularImage(cluster *openchamiv1alpha1.OpenCHAMICluster) string {
	img := cluster.Spec.Logging.Image
	if img == nil {
		return defaultFunicularImage
	}
	repo, tag := img.Repository, img.Tag
	switch {
	case repo == "" && tag == "":
		return defaultFunicularImage
	case repo == "":
		return "ghcr.io/openchami/funicular-collector:" + tag
	case tag == "":
		return repo + ":latest"
	default:
		return repo + ":" + tag
	}
}

// funicularPodLabels returns the canonical label set for funicular pods.
func funicularPodLabels(cluster *openchamiv1alpha1.OpenCHAMICluster) map[string]string {
	return map[string]string{
		labelAppName:   ServiceFunicular,
		labelAppInst:   "openchami-" + cluster.Spec.ClusterName,
		labelManagedBy: managedByValue,
	}
}

func (r *FunicularReconciler) buildDaemonSet(cluster *openchamiv1alpha1.OpenCHAMICluster) *appsv1.DaemonSet {
	labels := funicularPodLabels(cluster)
	tmpVol, tmpMount := TmpVolume()

	ns := ClusterNamespace(cluster)
	bucket := LogBucketName(cluster)
	credsSecret := SecretName(cluster, SuffixLogCredentials)
	flushInterval := strconv.Itoa(int(cluster.Spec.Logging.FlushIntervalSeconds))
	includeServices := strings.Join(cluster.Spec.Logging.IncludeServices, ",")

	hostPathType := corev1.HostPathDirectory
	logVol := corev1.Volume{
		Name: funicularLogVolume,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: funicularHostLogPath,
				Type: &hostPathType,
			},
		},
	}
	readOnlyMount := corev1.VolumeMount{
		Name:      funicularLogVolume,
		MountPath: funicularHostLogPath,
		ReadOnly:  true,
	}

	env := []corev1.EnvVar{
		{Name: "FUNICULAR_CLUSTER_NAME", Value: cluster.Spec.ClusterName},
		{Name: "FUNICULAR_NAMESPACE", Value: ns},
		{Name: "FUNICULAR_S3_ENDPOINT", Value: cluster.Spec.Platform.ObjectStorage.Endpoint},
		{Name: "FUNICULAR_S3_BUCKET", Value: bucket},
		{Name: "FUNICULAR_FLUSH_INTERVAL", Value: flushInterval},
		{
			Name: "FUNICULAR_ACCESS_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: credsSecret},
					Key:                  s3AccessKeyKey,
				},
			},
		},
		{
			Name: "FUNICULAR_SECRET_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: credsSecret},
					Key:                  s3SecretKeyKey,
				},
			},
		},
		{Name: "FUNICULAR_INCLUDE_SERVICES", Value: includeServices},
	}

	container := corev1.Container{
		Name:            ServiceFunicular,
		Image:           funicularImage(cluster),
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: CommonSecurityContext(),
		Env:             env,
		VolumeMounts:    []corev1.VolumeMount{tmpMount, readOnlyMount},
	}

	return &appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{APIVersion: appsAPIVersion, Kind: kindDaemonSet},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceFunicular,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: ServiceFunicular,
					SecurityContext:    CommonPodSecurityContext(),
					// Collect logs from every node, including control-plane.
					Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Containers:  []corev1.Container{container},
					Volumes:     []corev1.Volume{tmpVol, logVol},
				},
			},
		},
	}
}
