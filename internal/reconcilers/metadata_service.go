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
	metadataServiceRequeueAfter = 30 * time.Second

	metadataServicePort     = int32(8081)
	metadataServicePortName = "http"
	// metadataServiceDataDir is where the binary's file backend writes
	// (--data-dir default). Mounted as a writable emptyDir because the
	// pod has readOnlyRootFilesystem=true. Also where the WireGuard
	// state file lives by default.
	metadataServiceDataDir = "/data"
	// The metadata-service binary serves its liveness endpoint at /health
	// (see cmd/server/main.go in OpenCHAMI/metadata-service). A short-lived
	// pr-14 build briefly exposed /healthz instead; pr-15 reverted to
	// /health. /healthz currently 404s against the published image.
	metadataServiceHealthPath = "/health"
)

// MetadataServiceReconciler ensures the metadata-service Deployment, Service,
// and PodDisruptionBudget exist for the cluster.
type MetadataServiceReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

func (r *MetadataServiceReconciler) Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "metadata-service")

	if !ServiceDeployedInCluster(cp, ServiceMetadataService) {
		log.Info("metadata-service not operator-managed (disabled or external), skipping deployment")
		return ctrl.Result{}, nil
	}

	dep := r.buildDeployment(cp)
	logDep := logging.EnrichWithResource(log, "Deployment", dep.Name)
	logDep.Info("applying metadata-service Deployment")
	if err := r.Client.Patch(ctx, dep, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying metadata-service Deployment: %w", err)
	}

	svc := r.buildService(cp)
	logSvc := logging.EnrichWithResource(log, "Service", svc.Name)
	logSvc.Info("applying metadata-service Service")
	if err := r.Client.Patch(ctx, svc, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying metadata-service Service: %w", err)
	}

	pdb := r.buildPDB(cp)
	logPDB := logging.EnrichWithResource(log, "PodDisruptionBudget", pdb.Name)
	logPDB.Info("applying metadata-service PodDisruptionBudget")
	if err := r.Client.Patch(ctx, pdb, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying metadata-service PodDisruptionBudget: %w", err)
	}

	ns := ControlPlaneNamespace(cp)
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

	if cp.Status.Services == nil {
		cp.Status.Services = map[string]openchamiv1alpha1.ServiceStatus{}
	}
	cp.Status.Services[ServiceMetadataService] = openchamiv1alpha1.ServiceStatus{
		Ready:    ready,
		Endpoint: endpoint,
		Message:  message,
	}

	if !ready {
		return ctrl.Result{RequeueAfter: metadataServiceRequeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

func (r *MetadataServiceReconciler) Describe(cp *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
	return []client.Object{
		r.buildDeployment(cp),
		r.buildService(cp),
		r.buildPDB(cp),
	}, nil
}

// metadataServicePodLabels returns the canonical label set for metadata-service pods.
func metadataServicePodLabels(cp *openchamiv1alpha1.OpenCHAMIControlPlane) map[string]string {
	return map[string]string{
		labelAppName:   ServiceMetadataService,
		labelAppInst:   "openchami-" + cp.Spec.ClusterName,
		labelManagedBy: managedByValue,
	}
}

func (r *MetadataServiceReconciler) buildDeployment(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *appsv1.Deployment {
	labels := metadataServicePodLabels(cp)

	replicas := max(cp.Spec.Services.MetadataService.Replicas, 1)

	image, pullPolicy := ResolveImage(cp, ServiceMetadataService)

	tmpVol, tmpMount := TmpVolume()
	// metadata-service's new (post-#8 fabrica refactor) binary writes file
	// storage to `--data-dir` which defaults to /data — an absolute path.
	// With readOnlyRootFilesystem=true we have to mount a writable volume
	// at exactly that path. The WireGuard state file default
	// /data/wireguard/state.yaml lands inside the same mount.
	dataVol, dataMount := DataVolumeAt(metadataServiceDataDir)

	// metadata-service reads its SMD client config via direct os.Getenv
	// (no viper prefix). SMD_URL switches the binary from the in-memory
	// mock client to a real HTTP client against SMD. The TOKENSMITH_*
	// + JWKS_URL block mirrors boot-service's tokensmith wiring: the
	// container exchanges the bootstrap token for an aud=hsm JWT and
	// authenticates SMD reads as `sub=metadata-service`. Optional=true
	// on the SecretKeyRef so the Deployment can come up before
	// tokensmith finishes minting the first token (the tokensmith
	// reconciler back-fills it post-Ready).
	tokensmithURL := ServiceURL(cp, ServiceTokensmith)
	if tokensmithMTLSEnabled(cp) {
		tokensmithURL = TokensmithBaseURL(cp)
	}
	env := []corev1.EnvVar{
		{Name: "SMD_URL", Value: ServiceURL(cp, ServiceSMD)},
		{Name: "TOKENSMITH_URL", Value: tokensmithURL},
		{Name: "TOKENSMITH_TARGET_SERVICE", Value: bootstrapTokenAudience},
		{
			Name: "TOKENSMITH_BOOTSTRAP_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: SecretName(cp, SuffixMetadataServiceBootstr),
					},
					Key:      BootstrapTokenKey,
					Optional: ptrBool(true),
				},
			},
		},
		{Name: "JWKS_URL", Value: tokensmithURL + "/.well-known/jwks.json"},
	}

	// mTLS service-identity wiring: see boot_service.go for the
	// equivalent. metadata-service uses unprefixed
	// TOKENSMITH_SERVICE_IDENTITY_* env vars (per the upstream
	// prompt — its cobra flags don't share boot-service's
	// BOOT_SERVICE_ viper prefix).
	identityVols, identityMounts, identityWired := serviceIdentityVolumesAndMounts(cp, ServiceMetadataService)
	if identityWired && tokensmithMTLSEnabled(cp) {
		env = append(env,
			corev1.EnvVar{Name: "TOKENSMITH_SERVICE_IDENTITY_CERT", Value: serviceIdentityCertFilePath()},
			corev1.EnvVar{Name: "TOKENSMITH_SERVICE_IDENTITY_KEY", Value: serviceIdentityKeyFilePath()},
		)
	}

	// The post-#8 binary uses cobra flags with explicit arguments rather
	// than the OCHAMI_METADATA_* viper-prefix env conventions of the
	// previous image — we pass --port and --data-dir directly so the
	// behaviour is independent of any viper alias bug. The ENTRYPOINT
	// already runs `ochami-metadata-server serve`; we only need to
	// append flags.
	args := []string{
		"serve",
		"--port", fmt.Sprintf("%d", metadataServicePort),
		"--data-dir", metadataServiceDataDir,
	}
	ns := ControlPlaneNamespace(cp)

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

	mounts := []corev1.VolumeMount{tmpMount, dataMount}
	volumes := []corev1.Volume{tmpVol, dataVol}
	if identityWired && tokensmithMTLSEnabled(cp) {
		mounts = append(mounts, identityMounts...)
		volumes = append(volumes, identityVols...)
	}

	container := corev1.Container{
		Name:            ServiceMetadataService,
		Image:           image,
		ImagePullPolicy: pullPolicy,
		SecurityContext: CommonSecurityContext(),
		Args:            args,
		Ports: []corev1.ContainerPort{{
			Name:          metadataServicePortName,
			ContainerPort: metadataServicePort,
			Protocol:      corev1.ProtocolTCP,
		}},
		Env:            env,
		VolumeMounts:   mounts,
		StartupProbe:   startup,
		LivenessProbe:  liveness,
		ReadinessProbe: readiness,
	}
	if res := cp.Spec.Services.MetadataService.Resources; res != nil {
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
					EnableServiceLinks: DisableServiceLinks(),
					SecurityContext:    CommonPodSecurityContext(),
					Containers:         []corev1.Container{container},
					Volumes:            volumes,
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

func (r *MetadataServiceReconciler) buildService(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *corev1.Service {
	labels := metadataServicePodLabels(cp)
	spec := corev1.ServiceSpec{
		Type:     corev1.ServiceTypeClusterIP,
		Selector: labels,
		Ports: []corev1.ServicePort{{
			Name:       metadataServicePortName,
			Port:       metadataServicePort,
			TargetPort: intstr.FromString(metadataServicePortName),
			Protocol:   corev1.ProtocolTCP,
		}},
	}
	// kube-deploy convention (kustomize/services/cloud-init/base/service.yaml):
	// cloud-init looks up the requesting node by source IP, so any path that
	// would let kube-proxy SNAT the request would break recognition. When the
	// site has no WireGuard tunnel network bringing per-node identity into
	// the cluster, expose metadata-service as a NodePort with
	// externalTrafficPolicy=Local so the node-network source IP survives.
	if cp.Spec.Services.MetadataService.PreserveClientIP {
		spec.Type = corev1.ServiceTypeNodePort
		spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
	}
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: coreAPIVersion, Kind: kindService},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceMetadataService,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    labels,
		},
		Spec: spec,
	}
}

func (r *MetadataServiceReconciler) buildPDB(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *policyv1.PodDisruptionBudget {
	labels := metadataServicePodLabels(cp)
	minAvail := intstr.FromInt32(1)
	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{APIVersion: policyAPIVersion, Kind: "PodDisruptionBudget"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceMetadataService,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    labels,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvail,
			Selector:     &metav1.LabelSelector{MatchLabels: labels},
		},
	}
}
