// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/logging"
)

const (
	logqQueryRequeueAfter = 30 * time.Second

	logqQueryPortName   = "http"
	logqQueryHealthPath = "/health"
)

// LogqQueryReconciler ensures the logq-query Deployment and Service exist to
// provide a query API for Parquet logs.
//
// The query service exposes an HTTP API that can query Parquet files directly
// from the log bucket using DuckDB.
type LogqQueryReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

// Reconcile applies the logq-query Deployment and Service. It records readiness
// in cluster.Status.Services["logq-query"].
func (r *LogqQueryReconciler) Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "logq-query")

	if !cp.Spec.Logging.Enabled || !cp.Spec.Logging.QueryEnabled {
		log.Info("logq-query disabled, skipping")
		return ctrl.Result{}, nil
	}

	// Refuse to deploy when no image override is supplied.
	if cp.Spec.Logging.QueryImage == nil {
		log.Info("query enabled but spec.logging.queryImage is unset; skipping deployment")
		return ctrl.Result{RequeueAfter: logqQueryRequeueAfter}, nil
	}

	// Wait for log bucket to be ready before deploying query service
	if cp.Status.LogBucket == "" {
		log.Info("waiting for log bucket before deploying query service")
		return ctrl.Result{RequeueAfter: logqQueryRequeueAfter}, nil
	}

	ns := ControlPlaneNamespace(cp)

	dep := r.buildDeployment(cp)
	depLog := logging.EnrichWithResource(log, kindDeployment, dep.Name)
	depLog.Info("applying logq-query deployment")
	if err := r.Client.Patch(ctx, dep, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying logq-query deployment: %w", err)
	}

	svc := r.buildService(cp)
	svcLog := logging.EnrichWithResource(log, kindService, svc.Name)
	svcLog.Info("applying logq-query service")
	if err := r.Client.Patch(ctx, svc, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying logq-query service: %w", err)
	}

	current := &appsv1.Deployment{}
	getErr := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: ServiceLogqQuery}, current)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return ctrl.Result{}, fmt.Errorf("reading logq-query deployment status: %w", getErr)
	}

	available := int32(0)
	if getErr == nil {
		available = current.Status.AvailableReplicas
	}

	queryPort := cp.Spec.Logging.QueryPort
	if queryPort == 0 {
		queryPort = 8080
	}
	endpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", ServiceLogqQuery, ns, queryPort)
	ready := available >= 1
	msg := fmt.Sprintf("availableReplicas=%d", available)
	if ready {
		msg = "logq-query deployment available"
	}

	if cp.Status.Services == nil {
		cp.Status.Services = map[string]openchamiv1alpha1.ServiceStatus{}
	}
	cp.Status.Services[ServiceLogqQuery] = openchamiv1alpha1.ServiceStatus{
		Ready:    ready,
		Endpoint: endpoint,
		Message:  msg,
	}

	if !ready {
		return ctrl.Result{RequeueAfter: logqQueryRequeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

// Describe returns the Kubernetes objects this reconciler would apply.
// Returns an empty (but non-nil) slice when query is disabled.
func (r *LogqQueryReconciler) Describe(cp *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
	if !cp.Spec.Logging.Enabled || !cp.Spec.Logging.QueryEnabled {
		return []client.Object{}, nil
	}
	return []client.Object{r.buildDeployment(cp), r.buildService(cp)}, nil
}

// logqQueryPodLabels returns the canonical label set for logq-query pods.
func logqQueryPodLabels(cp *openchamiv1alpha1.OpenCHAMIControlPlane) map[string]string {
	return map[string]string{
		labelAppName:   ServiceLogqQuery,
		labelAppInst:   "openchami-" + cp.Spec.ClusterName,
		labelManagedBy: managedByValue,
	}
}

func (r *LogqQueryReconciler) buildDeployment(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *appsv1.Deployment {
	labels := logqQueryPodLabels(cp)
	tmpVol, tmpMount := TmpVolume()

	ns := ControlPlaneNamespace(cp)
	bucket := LogBucketName(cp)
	credsSecret := SecretName(cp, SuffixLogCredentials)

	queryPort := cp.Spec.Logging.QueryPort
	if queryPort == 0 {
		queryPort = 8080
	}

	env := []corev1.EnvVar{
		{Name: "LOGQ_CLUSTER_NAME", Value: cp.Spec.ClusterName},
		{Name: "LOGQ_NAMESPACE", Value: ns},
		{Name: "LOGQ_S3_ENDPOINT", Value: cp.Spec.Platform.ObjectStorage.Endpoint},
		{Name: "LOGQ_S3_BUCKET", Value: bucket},
		{Name: "LOGQ_HTTP_PORT", Value: fmt.Sprintf("%d", queryPort)},
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

	image, pullPolicy := ResolveImage(cp, ServiceLogqQuery)
	container := corev1.Container{
		Name:            ServiceLogqQuery,
		Image:           image,
		ImagePullPolicy: pullPolicy,
		SecurityContext: CommonSecurityContext(),
		Env:             env,
		Ports: []corev1.ContainerPort{{
			Name:          logqQueryPortName,
			ContainerPort: queryPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: logqQueryHealthPath,
					Port: intstr.FromString(logqQueryPortName),
				},
			},
			InitialDelaySeconds: 10,
			PeriodSeconds:       30,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: logqQueryHealthPath,
					Port: intstr.FromString(logqQueryPortName),
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
		VolumeMounts: []corev1.VolumeMount{tmpMount},
	}

	replicas := cp.Spec.Logging.QueryReplicas
	if replicas == 0 {
		replicas = 1
	}

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: appsAPIVersion, Kind: kindDeployment},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceLogqQuery,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: ServiceLogqQuery,
					EnableServiceLinks: DisableServiceLinks(),
					SecurityContext:    CommonPodSecurityContext(),
					Containers:         []corev1.Container{container},
					Volumes:            []corev1.Volume{tmpVol},
				},
			},
		},
	}
}

func (r *LogqQueryReconciler) buildService(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *corev1.Service {
	labels := logqQueryPodLabels(cp)

	queryPort := cp.Spec.Logging.QueryPort
	if queryPort == 0 {
		queryPort = 8080
	}

	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: coreAPIVersion, Kind: kindService},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceLogqQuery,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       logqQueryPortName,
				Port:       queryPort,
				TargetPort: intstr.FromString(logqQueryPortName),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}
