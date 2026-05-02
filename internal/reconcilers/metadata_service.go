/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package reconcilers

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openahamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/logging"
)

const (
	metadataServiceRequeueAfter = 30 * time.Second

	defaultMetadataServiceImage = "ghcr.io/openchami/metadata-service:latest"

	metadataServicePort       = int32(8081)
	metadataServicePortName   = "http"
	metadataServiceHealthPath = "/cloud-init/health"
)

// MetadataServiceReconciler ensures the metadata-service Deployment, Service,
// and PodDisruptionBudget exist for the cluster.
type MetadataServiceReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

func (r *MetadataServiceReconciler) Reconcile(ctx context.Context, cluster *openahamiv1alpha1.OpenCHAMICluster) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cluster, "metadata-service")

	if !cluster.Spec.Services.MetadataService.Enabled {
		log.Info("metadata-service disabled, skipping")
		return ctrl.Result{}, nil
	}

	dep := r.buildDeployment(cluster)
	logDep := logging.EnrichWithResource(log, "Deployment", dep.Name)
	logDep.Info("applying metadata-service Deployment")
	if err := r.Client.Patch(ctx, dep, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying metadata-service Deployment: %w", err)
	}

	svc := r.buildService(cluster)
	logSvc := logging.EnrichWithResource(log, "Service", svc.Name)
	logSvc.Info("applying metadata-service Service")
	if err := r.Client.Patch(ctx, svc, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying metadata-service Service: %w", err)
	}

	pdb := r.buildPDB(cluster)
	logPDB := logging.EnrichWithResource(log, "PodDisruptionBudget", pdb.Name)
	logPDB.Info("applying metadata-service PodDisruptionBudget")
	if err := r.Client.Patch(ctx, pdb, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying metadata-service PodDisruptionBudget: %w", err)
	}

	ns := ClusterNamespace(cluster)
	current := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: ServiceMetadataService}, current); err != nil {
		return ctrl.Result{}, fmt.Errorf("reading metadata-service Deployment status: %w", err)
	}

	ready := current.Status.AvailableReplicas >= 1
	endpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", ServiceMetadataService, ns, metadataServicePort)
	message := "metadata-service Deployment available"
	if !ready {
		message = fmt.Sprintf("waiting for metadata-service Deployment (availableReplicas=%d)", current.Status.AvailableReplicas)
	}

	if cluster.Status.Services == nil {
		cluster.Status.Services = map[string]openahamiv1alpha1.ServiceStatus{}
	}
	cluster.Status.Services[ServiceMetadataService] = openahamiv1alpha1.ServiceStatus{
		Ready:    ready,
		Endpoint: endpoint,
		Message:  message,
	}

	if !ready {
		return ctrl.Result{RequeueAfter: metadataServiceRequeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

func (r *MetadataServiceReconciler) Describe(cluster *openahamiv1alpha1.OpenCHAMICluster) ([]client.Object, error) {
	return []client.Object{
		r.buildDeployment(cluster),
		r.buildService(cluster),
		r.buildPDB(cluster),
	}, nil
}

// metadataServicePodLabels returns the canonical label set for metadata-service pods.
func metadataServicePodLabels(cluster *openahamiv1alpha1.OpenCHAMICluster) map[string]string {
	return map[string]string{
		labelAppName:   ServiceMetadataService,
		labelAppInst:   "openchami-" + cluster.Spec.ClusterName,
		labelManagedBy: managedByValue,
	}
}

func (r *MetadataServiceReconciler) buildDeployment(cluster *openahamiv1alpha1.OpenCHAMICluster) *appsv1.Deployment {
	labels := metadataServicePodLabels(cluster)

	replicas := max(cluster.Spec.Services.MetadataService.Replicas, 1)

	image := defaultMetadataServiceImage
	if ms := cluster.Spec.Services.MetadataService; ms.Image != nil {
		repo := ms.Image.Repository
		tag := ms.Image.Tag
		switch {
		case repo != "" && tag != "":
			image = repo + ":" + tag
		case repo != "":
			image = repo
		}
	}
	var pullPolicy corev1.PullPolicy
	if ms := cluster.Spec.Services.MetadataService; ms.Image != nil {
		pullPolicy = ms.Image.PullPolicy
	}

	tmpVol, tmpMount := TmpVolume()

	ns := ClusterNamespace(cluster)
	smdURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:27779", ServiceSMD, ns)
	jwksURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:8080/.well-known/jwks.json", ServiceTokensmith, ns)

	env := []corev1.EnvVar{
		{Name: "METADATA_CLUSTER_NAME", Value: cluster.Spec.ClusterName},
		{Name: "METADATA_SMD_URL", Value: smdURL},
		{Name: "METADATA_JWKS_URL", Value: jwksURL},
	}

	probe := func(period, failures int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: metadataServiceHealthPath,
					Port: intstr.FromString(metadataServicePortName),
				},
			},
			PeriodSeconds:    period,
			FailureThreshold: failures,
		}
	}
	startup := probe(5, 30)
	liveness := probe(20, 3)
	readiness := probe(10, 3)

	container := corev1.Container{
		Name:            ServiceMetadataService,
		Image:           image,
		ImagePullPolicy: pullPolicy,
		SecurityContext: CommonSecurityContext(),
		Ports: []corev1.ContainerPort{{
			Name:          metadataServicePortName,
			ContainerPort: metadataServicePort,
			Protocol:      corev1.ProtocolTCP,
		}},
		Env:            env,
		VolumeMounts:   []corev1.VolumeMount{tmpMount},
		StartupProbe:   startup,
		LivenessProbe:  liveness,
		ReadinessProbe: readiness,
	}
	if res := cluster.Spec.Services.MetadataService.Resources; res != nil {
		container.Resources = *res
	}

	maxUnavailable := intstr.FromInt32(0)
	maxSurge := intstr.FromInt32(1)

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: appsAPIVersion, Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceMetadataService,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: &maxUnavailable,
					MaxSurge:       &maxSurge,
				},
			},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: ServiceMetadataService,
					SecurityContext:    CommonPodSecurityContext(),
					Containers:         []corev1.Container{container},
					Volumes:            []corev1.Volume{tmpVol},
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
						MaxSkew:           1,
						TopologyKey:       topologyHostKey,
						WhenUnsatisfiable: corev1.ScheduleAnyway,
						LabelSelector:     &metav1.LabelSelector{MatchLabels: labels},
					}},
				},
			},
		},
	}
}

func (r *MetadataServiceReconciler) buildService(cluster *openahamiv1alpha1.OpenCHAMICluster) *corev1.Service {
	labels := metadataServicePodLabels(cluster)
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: coreAPIVersion, Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceMetadataService,
			Namespace: ClusterNamespace(cluster),
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       metadataServicePortName,
				Port:       metadataServicePort,
				TargetPort: intstr.FromString(metadataServicePortName),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

func (r *MetadataServiceReconciler) buildPDB(cluster *openahamiv1alpha1.OpenCHAMICluster) *policyv1.PodDisruptionBudget {
	labels := metadataServicePodLabels(cluster)
	minAvail := intstr.FromInt32(1)
	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{APIVersion: policyAPIVersion, Kind: "PodDisruptionBudget"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceMetadataService,
			Namespace: ClusterNamespace(cluster),
			Labels:    labels,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvail,
			Selector:     &metav1.LabelSelector{MatchLabels: labels},
		},
	}
}
