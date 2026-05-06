// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
	tokensmithRequeueAfter = 30 * time.Second

	defaultTokensmithImage = "ghcr.io/openchami/tokensmith:latest"

	tokensmithPVCName    = "tokensmith-data"
	tokensmithDataVolume = "data"
	tokensmithDataPath   = "/var/lib/tokensmith"
	tokensmithPort       = int32(8080)
	tokensmithPortName   = "http"
	tokensmithHealthPath = "/health"

	tokensmithOIDCProviderVault    = "vault"
	tokensmithOIDCProviderExternal = "external"

	tokensmithOIDCIssuerSuffix = "/v1/identity/oidc/provider/default"

	tokensmithOIDCClientSecretKey = "client_secret"
	tokensmithOIDCIssuerEnvName   = "OIDC_ISSUER_URL"

	reasonOIDCConfigInvalid = "OIDCConfigInvalid"
)

// TokensmithReconciler ensures the tokensmith Deployment, PVC, Service, and PDB exist.
type TokensmithReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

func (r *TokensmithReconciler) Reconcile(ctx context.Context, cluster *openchamiv1alpha1.OpenCHAMICluster) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cluster, "tokensmith")

	if !ServiceDeployedInCluster(cluster, ServiceTokensmith) {
		log.Info("tokensmith not operator-managed (disabled or external), skipping deployment")
		return ctrl.Result{}, nil
	}

	ts := cluster.Spec.Services.Tokensmith
	if ts.OIDCProvider == tokensmithOIDCProviderExternal && ts.OIDCIssuerURL == "" {
		RecordConditionEvent(r.Recorder, cluster, corev1.EventTypeWarning,
			reasonOIDCConfigInvalid,
			"tokensmith oidcProvider=external requires oidcIssuerURL")
		return ctrl.Result{}, fmt.Errorf("tokensmith oidcProvider=external requires oidcIssuerURL")
	}

	pvc := r.buildPVC(cluster)
	logPVC := logging.EnrichWithResource(log, "PersistentVolumeClaim", pvc.Name)
	logPVC.Info("applying tokensmith PVC")
	if err := r.Client.Patch(ctx, pvc, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying tokensmith PVC: %w", err)
	}

	dep := r.buildDeployment(cluster)
	logDep := logging.EnrichWithResource(log, "Deployment", dep.Name)
	logDep.Info("applying tokensmith Deployment")
	if err := r.Client.Patch(ctx, dep, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying tokensmith Deployment: %w", err)
	}

	svc := r.buildService(cluster)
	logSvc := logging.EnrichWithResource(log, "Service", svc.Name)
	logSvc.Info("applying tokensmith Service")
	if err := r.Client.Patch(ctx, svc, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying tokensmith Service: %w", err)
	}

	pdb := r.buildPDB(cluster)
	logPDB := logging.EnrichWithResource(log, "PodDisruptionBudget", pdb.Name)
	logPDB.Info("applying tokensmith PodDisruptionBudget")
	if err := r.Client.Patch(ctx, pdb, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying tokensmith PodDisruptionBudget: %w", err)
	}

	ns := ClusterNamespace(cluster)
	current := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: ServiceTokensmith}, current); err != nil {
		return ctrl.Result{}, fmt.Errorf("reading tokensmith Deployment status: %w", err)
	}

	ready := current.Status.AvailableReplicas >= 1
	endpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", ServiceTokensmith, ns, tokensmithPort)
	message := "tokensmith Deployment available"
	if !ready {
		message = fmt.Sprintf("waiting for tokensmith Deployment (availableReplicas=%d)", current.Status.AvailableReplicas)
	}

	if cluster.Status.Services == nil {
		cluster.Status.Services = map[string]openchamiv1alpha1.ServiceStatus{}
	}
	cluster.Status.Services[ServiceTokensmith] = openchamiv1alpha1.ServiceStatus{
		Ready:    ready,
		Endpoint: endpoint,
		Message:  message,
	}

	if !ready {
		return ctrl.Result{RequeueAfter: tokensmithRequeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

func (r *TokensmithReconciler) Describe(cluster *openchamiv1alpha1.OpenCHAMICluster) ([]client.Object, error) {
	return []client.Object{
		r.buildPVC(cluster),
		r.buildDeployment(cluster),
		r.buildService(cluster),
		r.buildPDB(cluster),
	}, nil
}

// podLabels returns the canonical label set for tokensmith pods.
func tokensmithPodLabels(cluster *openchamiv1alpha1.OpenCHAMICluster) map[string]string {
	return map[string]string{
		labelAppName:   ServiceTokensmith,
		labelAppInst:   "openchami-" + cluster.Spec.ClusterName,
		labelManagedBy: managedByValue,
	}
}

func (r *TokensmithReconciler) buildPVC(cluster *openchamiv1alpha1.OpenCHAMICluster) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      tokensmithPVCName,
			Namespace: ClusterNamespace(cluster),
			Annotations: map[string]string{
				"helm.sh/resource-policy": "keep",
			},
			Labels: tokensmithPodLabels(cluster),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}
}

func (r *TokensmithReconciler) buildDeployment(cluster *openchamiv1alpha1.OpenCHAMICluster) *appsv1.Deployment {
	labels := tokensmithPodLabels(cluster)
	// Replicas always 1 — tokensmith holds key material, never scaled.
	replicas := int32(1)

	image := defaultTokensmithImage
	if ts := cluster.Spec.Services.Tokensmith; ts.Image != nil {
		repo := ts.Image.Repository
		tag := ts.Image.Tag
		if repo != "" && tag != "" {
			image = repo + ":" + tag
		} else if repo != "" {
			image = repo
		}
	}
	var pullPolicy corev1.PullPolicy
	if ts := cluster.Spec.Services.Tokensmith; ts.Image != nil {
		pullPolicy = ts.Image.PullPolicy
	}

	tmpVol, tmpMount := TmpVolume()

	env := []corev1.EnvVar{
		{
			Name: "OIDC_CLIENT_SECRET",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: SecretName(cluster, SuffixTokensmithOIDC),
					},
					Key: tokensmithOIDCClientSecretKey,
				},
			},
		},
	}

	switch cluster.Spec.Services.Tokensmith.OIDCProvider {
	case tokensmithOIDCProviderVault:
		base := strings.TrimSuffix(cluster.Spec.Platform.Vault.Address, "/")
		env = append(env, corev1.EnvVar{
			Name:  tokensmithOIDCIssuerEnvName,
			Value: base + tokensmithOIDCIssuerSuffix,
		})
	case tokensmithOIDCProviderExternal:
		env = append(env, corev1.EnvVar{
			Name:  tokensmithOIDCIssuerEnvName,
			Value: cluster.Spec.Services.Tokensmith.OIDCIssuerURL,
		})
	}

	probe := func(period, failures int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: tokensmithHealthPath,
					Port: intstr.FromString(tokensmithPortName),
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
		Name:            ServiceTokensmith,
		Image:           image,
		ImagePullPolicy: pullPolicy,
		SecurityContext: CommonSecurityContext(),
		Ports: []corev1.ContainerPort{{
			Name:          tokensmithPortName,
			ContainerPort: tokensmithPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		Env: env,
		VolumeMounts: []corev1.VolumeMount{
			tmpMount,
			{
				Name:      tokensmithDataVolume,
				MountPath: tokensmithDataPath,
			},
		},
		StartupProbe:   startup,
		LivenessProbe:  liveness,
		ReadinessProbe: readiness,
	}
	if res := cluster.Spec.Services.Tokensmith.Resources; res != nil {
		container.Resources = *res
	}

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: appsAPIVersion, Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceTokensmith,
			Namespace: ClusterNamespace(cluster),
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: ServiceTokensmith,
					SecurityContext:    CommonPodSecurityContext(),
					Containers:         []corev1.Container{container},
					Volumes: []corev1.Volume{
						tmpVol,
						{
							Name: tokensmithDataVolume,
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: tokensmithPVCName,
								},
							},
						},
					},
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

func (r *TokensmithReconciler) buildService(cluster *openchamiv1alpha1.OpenCHAMICluster) *corev1.Service {
	labels := tokensmithPodLabels(cluster)
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: coreAPIVersion, Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceTokensmith,
			Namespace: ClusterNamespace(cluster),
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       tokensmithPortName,
				Port:       tokensmithPort,
				TargetPort: intstr.FromString(tokensmithPortName),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

func (r *TokensmithReconciler) buildPDB(cluster *openchamiv1alpha1.OpenCHAMICluster) *policyv1.PodDisruptionBudget {
	labels := tokensmithPodLabels(cluster)
	minAvail := intstr.FromInt32(1)
	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{APIVersion: policyAPIVersion, Kind: "PodDisruptionBudget"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceTokensmith,
			Namespace: ClusterNamespace(cluster),
			Labels:    labels,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvail,
			Selector:     &metav1.LabelSelector{MatchLabels: labels},
		},
	}
}
