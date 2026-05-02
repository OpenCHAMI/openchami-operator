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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	smdRequeueAfter = 30 * time.Second

	// defaultSMDImage is the fallback container image when the spec does not
	// override it. Phase 13 wires per-cluster image overrides via the operator
	// ImageConfig; until then this constant is the only source of truth.
	defaultSMDImage = "ghcr.io/openchami/smd:latest"

	smdPort         int32 = 27779
	smdPortName           = "http"
	smdHealthPath         = "/hsm/v2/service/ready"
	smdJWKSPortName       = "8080"

	// Common label/key strings extracted to constants to satisfy goconst.
	labelAppName    = "app.kubernetes.io/name"
	labelAppInst    = "app.kubernetes.io/instance"
	labelManagedBy  = "openchami.org/managed-by"
	managedByValue  = "operator"
	topologyHostKey = "kubernetes.io/hostname"

	appsAPIVersion   = "apps/v1"
	coreAPIVersion   = "v1"
	policyAPIVersion = "policy/v1"

	// Kind name constants shared across the service reconcilers in this package.
	// Other sub-reconcilers (boot_service, tokensmith, metadata_service) reference
	// these so that goconst is satisfied without each file redefining them.
	kindDeployment          = "Deployment"
	kindService             = "Service"
	kindPodDisruptionBudget = "PodDisruptionBudget"
	kindPDB                 = kindPodDisruptionBudget
)

// SMDReconciler ensures the SMD Deployment, Service, and PodDisruptionBudget exist.
type SMDReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

// Reconcile applies the SMD Deployment, Service, and PDB. It records readiness
// in cluster.Status.Services["smd"]. The aggregator owns ConditionServicesReady.
func (r *SMDReconciler) Reconcile(ctx context.Context, cluster *openahamiv1alpha1.OpenCHAMICluster) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cluster, "smd")

	if !cluster.Spec.Services.SMD.Enabled {
		log.Info("smd disabled, skipping")
		return ctrl.Result{}, nil
	}

	ns := ClusterNamespace(cluster)

	dep := r.buildDeployment(cluster)
	depLog := logging.EnrichWithResource(log, "Deployment", dep.Name)
	depLog.Info("applying smd deployment")
	if err := r.Client.Patch(ctx, dep, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying smd deployment: %w", err)
	}

	svc := r.buildService(cluster)
	svcLog := logging.EnrichWithResource(log, "Service", svc.Name)
	svcLog.Info("applying smd service")
	if err := r.Client.Patch(ctx, svc, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying smd service: %w", err)
	}

	pdb := r.buildPDB(cluster)
	pdbLog := logging.EnrichWithResource(log, "PodDisruptionBudget", pdb.Name)
	pdbLog.Info("applying smd pdb")
	if err := r.Client.Patch(ctx, pdb, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying smd pdb: %w", err)
	}

	current := &appsv1.Deployment{}
	getErr := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: ServiceSMD}, current)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return ctrl.Result{}, fmt.Errorf("reading smd deployment status: %w", getErr)
	}

	available := int32(0)
	if getErr == nil {
		available = current.Status.AvailableReplicas
	}

	endpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", ServiceSMD, ns, smdPort)
	ready := available >= 1
	msg := fmt.Sprintf("availableReplicas=%d", available)
	if ready {
		msg = "smd deployment available"
	}

	if cluster.Status.Services == nil {
		cluster.Status.Services = map[string]openahamiv1alpha1.ServiceStatus{}
	}
	cluster.Status.Services[ServiceSMD] = openahamiv1alpha1.ServiceStatus{
		Ready:    ready,
		Endpoint: endpoint,
		Message:  msg,
	}

	if !ready {
		return ctrl.Result{RequeueAfter: smdRequeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

// Describe returns the Kubernetes objects this reconciler would apply, in
// apply order, without contacting any external service.
func (r *SMDReconciler) Describe(cluster *openahamiv1alpha1.OpenCHAMICluster) ([]client.Object, error) {
	return []client.Object{
		r.buildDeployment(cluster),
		r.buildService(cluster),
		r.buildPDB(cluster),
	}, nil
}

// podLabels returns the canonical pod selector labels for the SMD Deployment.
func smdPodLabels(cluster *openahamiv1alpha1.OpenCHAMICluster) map[string]string {
	return map[string]string{
		labelAppName:   ServiceSMD,
		labelAppInst:   "openchami-" + cluster.Spec.ClusterName,
		labelManagedBy: managedByValue,
	}
}

// smdImage resolves the container image for the SMD container, preferring the
// per-cluster spec override, falling back to defaultSMDImage.
func smdImage(cluster *openahamiv1alpha1.OpenCHAMICluster) string {
	img := cluster.Spec.Services.SMD.Image
	if img == nil {
		return defaultSMDImage
	}
	repo := img.Repository
	tag := img.Tag
	switch {
	case repo == "" && tag == "":
		return defaultSMDImage
	case repo == "":
		// Use default repo with overridden tag.
		return "ghcr.io/openchami/smd:" + tag
	case tag == "":
		return repo + ":latest"
	default:
		return repo + ":" + tag
	}
}

func (r *SMDReconciler) buildDeployment(cluster *openahamiv1alpha1.OpenCHAMICluster) *appsv1.Deployment {
	ns := ClusterNamespace(cluster)
	labels := smdPodLabels(cluster)
	replicas := cluster.Spec.Services.SMD.Replicas
	if replicas == 0 {
		replicas = 2
	}

	tmpVol, tmpMount := TmpVolume()
	maxUnavailable := intstr.FromInt32(0)
	maxSurge := intstr.FromInt32(1)

	dbHost := fmt.Sprintf("openchami-%s-postgres-rw.%s.svc.cluster.local",
		cluster.Spec.ClusterName, ns)
	jwksURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%s/.well-known/jwks.json",
		ServiceTokensmith, ns, smdJWKSPortName)
	dbCredsSecret := SecretName(cluster, SuffixDBCredentials)

	container := corev1.Container{
		Name:            ServiceSMD,
		Image:           smdImage(cluster),
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: CommonSecurityContext(),
		Ports: []corev1.ContainerPort{{
			Name:          smdPortName,
			ContainerPort: smdPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		Env: []corev1.EnvVar{
			{Name: "SMD_DBHOST", Value: dbHost},
			{Name: "SMD_DBPORT", Value: "5432"},
			{Name: "SMD_DBNAME", Value: ServiceSMD},
			{Name: "SMD_DBUSER", Value: ServiceSMD},
			{
				Name: "SMD_DBPASS",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: dbCredsSecret},
						Key:                  VaultKeySMDPassword,
					},
				},
			},
			{Name: "SMD_JWKS_URL", Value: jwksURL},
		},
		VolumeMounts: []corev1.VolumeMount{tmpMount},
		StartupProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: smdHealthPath,
					Port: intstr.FromString(smdPortName),
				},
			},
			FailureThreshold: 30,
			PeriodSeconds:    5,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: smdHealthPath,
					Port: intstr.FromString(smdPortName),
				},
			},
			PeriodSeconds:    20,
			FailureThreshold: 3,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: smdHealthPath,
					Port: intstr.FromString(smdPortName),
				},
			},
			PeriodSeconds:    10,
			FailureThreshold: 3,
		},
	}
	if cluster.Spec.Services.SMD.Resources != nil {
		container.Resources = *cluster.Spec.Services.SMD.Resources
	}

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: appsAPIVersion, Kind: kindDeployment},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceSMD,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: &maxUnavailable,
					MaxSurge:       &maxSurge,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: ServiceSMD,
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

func (r *SMDReconciler) buildService(cluster *openahamiv1alpha1.OpenCHAMICluster) *corev1.Service {
	labels := smdPodLabels(cluster)
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: coreAPIVersion, Kind: kindService},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceSMD,
			Namespace: ClusterNamespace(cluster),
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       smdPortName,
				Port:       smdPort,
				TargetPort: intstr.FromString(smdPortName),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

func (r *SMDReconciler) buildPDB(cluster *openahamiv1alpha1.OpenCHAMICluster) *policyv1.PodDisruptionBudget {
	labels := smdPodLabels(cluster)
	minAvailable := intstr.FromInt32(1)
	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{APIVersion: policyAPIVersion, Kind: kindPodDisruptionBudget},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceSMD,
			Namespace: ClusterNamespace(cluster),
			Labels:    labels,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector:     &metav1.LabelSelector{MatchLabels: labels},
		},
	}
}
