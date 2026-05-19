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
	policyv1 "k8s.io/api/policy/v1"
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
	smdRequeueAfter = 30 * time.Second

	smdPort       int32 = 27779
	smdPortName         = "http"
	smdHealthPath       = "/hsm/v2/service/ready"

	smdDBPassEnvName = "SMD_DBPASS"

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
func (r *SMDReconciler) Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "smd")

	if !ServiceDeployedInCluster(cp, ServiceSMD) {
		// Either SMD is disabled, or the site has supplied an external
		// endpoint via spec.services.smd.externalEndpoint — in either case
		// the operator must not produce in-cluster objects for it.
		log.Info("smd not operator-managed (disabled or external), skipping deployment")
		return ctrl.Result{}, nil
	}

	ns := ControlPlaneNamespace(cp)

	dep := r.buildDeployment(cp)
	depLog := logging.EnrichWithResource(log, "Deployment", dep.Name)
	depLog.Info("applying smd deployment")
	if err := r.Client.Patch(ctx, dep, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying smd deployment: %w", err)
	}

	svc := r.buildService(cp)
	svcLog := logging.EnrichWithResource(log, "Service", svc.Name)
	svcLog.Info("applying smd service")
	if err := r.Client.Patch(ctx, svc, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying smd service: %w", err)
	}

	pdb := r.buildPDB(cp)
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

	if cp.Status.Services == nil {
		cp.Status.Services = map[string]openchamiv1alpha1.ServiceStatus{}
	}
	cp.Status.Services[ServiceSMD] = openchamiv1alpha1.ServiceStatus{
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
func (r *SMDReconciler) Describe(cp *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
	return []client.Object{
		r.buildDeployment(cp),
		r.buildService(cp),
		r.buildPDB(cp),
	}, nil
}

// podLabels returns the canonical pod selector labels for the SMD Deployment.
func smdPodLabels(cp *openchamiv1alpha1.OpenCHAMIControlPlane) map[string]string {
	return map[string]string{
		labelAppName:   ServiceSMD,
		labelAppInst:   "openchami-" + cp.Spec.ClusterName,
		labelManagedBy: managedByValue,
	}
}

func (r *SMDReconciler) buildDeployment(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *appsv1.Deployment {
	ns := ControlPlaneNamespace(cp)
	labels := smdPodLabels(cp)
	replicas := cp.Spec.Services.SMD.Replicas
	if replicas == 0 {
		replicas = 2
	}

	tmpVol, tmpMount := TmpVolume()
	maxUnavailable := intstr.FromInt32(0)
	maxSurge := intstr.FromInt32(1)

	dbHost := fmt.Sprintf("openchami-%s-postgres-rw.%s.svc.cluster.local",
		cp.Spec.ClusterName, ns)
	jwksURL := ServiceURL(cp, ServiceTokensmith) + "/.well-known/jwks.json"
	dbCredsSecret := SecretName(cp, SuffixSMDDB)

	image, pullPolicy := ResolveImage(cp, ServiceSMD)
	container := corev1.Container{
		Name:            ServiceSMD,
		Image:           image,
		ImagePullPolicy: pullPolicy,
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
				Name: smdDBPassEnvName,
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: dbCredsSecret},
						Key:                  VaultKeyDBPassword,
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
	if cp.Spec.Services.SMD.Resources != nil {
		container.Resources = *cp.Spec.Services.SMD.Resources
	}

	// SMD's image ships /smd-init at the root: a one-shot binary that
	// applies SQL migrations from /persistent_migrations. Without it the
	// `smd` database (created by CNPG bootstrap) is empty and the main
	// SMD process loops on `Schema check failed: relation "system" does
	// not exist`. Run it as an init container before SMD starts. It uses
	// flag args, not env vars: -dbhost, -dbport, -dbname, -dbuser; the
	// password comes from the same SMD_DBPASS env var SMD itself reads.
	initContainer := corev1.Container{
		Name:            ServiceSMD + "-init",
		Image:           image, // same image as the main container
		ImagePullPolicy: pullPolicy,
		SecurityContext: CommonSecurityContext(),
		Command:         []string{"/smd-init"},
		Args: []string{
			"-dbhost", dbHost,
			"-dbport", "5432",
			"-dbname", ServiceSMD,
			"-dbuser", ServiceSMD,
			"-migrationsdir", "/persistent_migrations",
		},
		Env: []corev1.EnvVar{
			{
				Name: smdDBPassEnvName,
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: dbCredsSecret},
						Key:                  VaultKeyDBPassword,
					},
				},
			},
		},
		VolumeMounts: []corev1.VolumeMount{tmpMount},
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
					EnableServiceLinks: DisableServiceLinks(),
					SecurityContext:    CommonPodSecurityContext(),
					InitContainers:     []corev1.Container{initContainer},
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

func (r *SMDReconciler) buildService(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *corev1.Service {
	labels := smdPodLabels(cp)
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: coreAPIVersion, Kind: kindService},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceSMD,
			Namespace: ControlPlaneNamespace(cp),
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

func (r *SMDReconciler) buildPDB(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *policyv1.PodDisruptionBudget {
	labels := smdPodLabels(cp)
	minAvailable := intstr.FromInt32(1)
	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{APIVersion: policyAPIVersion, Kind: kindPodDisruptionBudget},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceSMD,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    labels,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector:     &metav1.LabelSelector{MatchLabels: labels},
		},
	}
}
