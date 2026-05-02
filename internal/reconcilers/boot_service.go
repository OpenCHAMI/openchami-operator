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
	bootServiceRequeueAfter = 30 * time.Second

	defaultBootServiceImage = "ghcr.io/openchami/boot-service:latest"

	bootServicePort       = int32(27778)
	bootServicePortName   = "http"
	bootServiceHealthPath = "/boot/v1/service/status"

	bootServiceDBName = "boot_service"
	bootServiceDBUser = "boot_service"
	bootServiceDBPort = "5432"

	// S3 secret keys (shared across services).
	s3AccessKeyKey = "access_key"
	s3SecretKeyKey = "secret_key"

	// In-cluster service host templates. The "%s" is the cluster name.
	postgresRWHostTemplate = "openchami-%s-postgres-rw.openchami-%s.svc.cluster.local"
	smdHostTemplate        = "http://smd.openchami-%s.svc.cluster.local:27779"
	tokensmithJWKSTemplate = "http://tokensmith.openchami-%s.svc.cluster.local:8080/.well-known/jwks.json"
)

// BootServiceReconciler ensures the boot-service Deployment, Service, and PDB exist.
type BootServiceReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

func (r *BootServiceReconciler) Reconcile(ctx context.Context, cluster *openahamiv1alpha1.OpenCHAMICluster) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cluster, "boot-service")

	if !cluster.Spec.Services.BootService.Enabled {
		log.Info("boot-service disabled, skipping")
		return ctrl.Result{}, nil
	}

	dep := r.buildDeployment(cluster)
	logDep := logging.EnrichWithResource(log, "Deployment", dep.Name)
	logDep.Info("applying boot-service Deployment")
	if err := r.Client.Patch(ctx, dep, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying boot-service Deployment: %w", err)
	}

	svc := r.buildService(cluster)
	logSvc := logging.EnrichWithResource(log, "Service", svc.Name)
	logSvc.Info("applying boot-service Service")
	if err := r.Client.Patch(ctx, svc, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying boot-service Service: %w", err)
	}

	pdb := r.buildPDB(cluster)
	logPDB := logging.EnrichWithResource(log, "PodDisruptionBudget", pdb.Name)
	logPDB.Info("applying boot-service PodDisruptionBudget")
	if err := r.Client.Patch(ctx, pdb, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying boot-service PodDisruptionBudget: %w", err)
	}

	ns := ClusterNamespace(cluster)
	current := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: ServiceBootService}, current); err != nil {
		return ctrl.Result{}, fmt.Errorf("reading boot-service Deployment status: %w", err)
	}

	ready := current.Status.AvailableReplicas >= 1
	endpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", ServiceBootService, ns, bootServicePort)
	message := "boot-service Deployment available"
	if !ready {
		message = fmt.Sprintf("waiting for boot-service Deployment (availableReplicas=%d)", current.Status.AvailableReplicas)
	}

	if cluster.Status.Services == nil {
		cluster.Status.Services = map[string]openahamiv1alpha1.ServiceStatus{}
	}
	cluster.Status.Services[ServiceBootService] = openahamiv1alpha1.ServiceStatus{
		Ready:    ready,
		Endpoint: endpoint,
		Message:  message,
	}

	if !ready {
		return ctrl.Result{RequeueAfter: bootServiceRequeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

func (r *BootServiceReconciler) Describe(cluster *openahamiv1alpha1.OpenCHAMICluster) ([]client.Object, error) {
	return []client.Object{
		r.buildDeployment(cluster),
		r.buildService(cluster),
		r.buildPDB(cluster),
	}, nil
}

// bootServicePodLabels returns the canonical label set for boot-service pods.
func bootServicePodLabels(cluster *openahamiv1alpha1.OpenCHAMICluster) map[string]string {
	return map[string]string{
		labelAppName:   ServiceBootService,
		labelAppInst:   "openchami-" + cluster.Spec.ClusterName,
		labelManagedBy: managedByValue,
	}
}

func (r *BootServiceReconciler) buildDeployment(cluster *openahamiv1alpha1.OpenCHAMICluster) *appsv1.Deployment {
	labels := bootServicePodLabels(cluster)
	bs := cluster.Spec.Services.BootService
	replicas := bs.Replicas

	image := defaultBootServiceImage
	var pullPolicy corev1.PullPolicy
	if bs.Image != nil {
		repo := bs.Image.Repository
		tag := bs.Image.Tag
		switch {
		case repo != "" && tag != "":
			image = repo + ":" + tag
		case repo != "":
			image = repo
		}
		pullPolicy = bs.Image.PullPolicy
	}

	tmpVol, tmpMount := TmpVolume()

	dbCredsName := SecretName(cluster, SuffixDBCredentials)
	s3CredsName := SecretName(cluster, SuffixS3Credentials)
	name := cluster.Spec.ClusterName

	env := []corev1.EnvVar{
		{Name: "BOOT_SERVICE_DBHOST", Value: fmt.Sprintf(postgresRWHostTemplate, name, name)},
		{Name: "BOOT_SERVICE_DBPORT", Value: bootServiceDBPort},
		{Name: "BOOT_SERVICE_DBNAME", Value: bootServiceDBName},
		{Name: "BOOT_SERVICE_DBUSER", Value: bootServiceDBUser},
		{
			Name: "BOOT_SERVICE_DBPASS",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: dbCredsName},
					Key:                  VaultKeyBootServicePassword,
				},
			},
		},
		{Name: "BOOT_SERVICE_SMD_URL", Value: fmt.Sprintf(smdHostTemplate, name)},
		{Name: "BOOT_SERVICE_JWKS_URL", Value: fmt.Sprintf(tokensmithJWKSTemplate, name)},
		{Name: "BOOT_SERVICE_S3_ENDPOINT", Value: cluster.Spec.Platform.ObjectStorage.Endpoint},
		{Name: "BOOT_SERVICE_S3_BUCKET", Value: BootBucketName(cluster)},
		{
			Name: "BOOT_SERVICE_S3_ACCESS_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: s3CredsName},
					Key:                  s3AccessKeyKey,
				},
			},
		},
		{
			Name: "BOOT_SERVICE_S3_SECRET_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: s3CredsName},
					Key:                  s3SecretKeyKey,
				},
			},
		},
	}

	probe := func(period, failures int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: bootServiceHealthPath,
					Port: intstr.FromString(bootServicePortName),
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
		Name:            ServiceBootService,
		Image:           image,
		ImagePullPolicy: pullPolicy,
		SecurityContext: CommonSecurityContext(),
		Ports: []corev1.ContainerPort{{
			Name:          bootServicePortName,
			ContainerPort: bootServicePort,
			Protocol:      corev1.ProtocolTCP,
		}},
		Env:            env,
		VolumeMounts:   []corev1.VolumeMount{tmpMount},
		StartupProbe:   startup,
		LivenessProbe:  liveness,
		ReadinessProbe: readiness,
	}
	if res := bs.Resources; res != nil {
		container.Resources = *res
	}

	maxUnavailable := intstr.FromInt32(0)
	maxSurge := intstr.FromInt32(1)

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: appsAPIVersion, Kind: kindDeployment},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceBootService,
			Namespace: ClusterNamespace(cluster),
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
					ServiceAccountName: ServiceBootService,
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

func (r *BootServiceReconciler) buildService(cluster *openahamiv1alpha1.OpenCHAMICluster) *corev1.Service {
	labels := bootServicePodLabels(cluster)
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: coreAPIVersion, Kind: kindService},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceBootService,
			Namespace: ClusterNamespace(cluster),
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       bootServicePortName,
				Port:       bootServicePort,
				TargetPort: intstr.FromString(bootServicePortName),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

func (r *BootServiceReconciler) buildPDB(cluster *openahamiv1alpha1.OpenCHAMICluster) *policyv1.PodDisruptionBudget {
	labels := bootServicePodLabels(cluster)
	minAvail := intstr.FromInt32(1)
	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{APIVersion: policyAPIVersion, Kind: kindPodDisruptionBudget},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceBootService,
			Namespace: ClusterNamespace(cluster),
			Labels:    labels,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvail,
			Selector:     &metav1.LabelSelector{MatchLabels: labels},
		},
	}
}
