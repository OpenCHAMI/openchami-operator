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

// bootServiceTokensmithURL returns the URL boot-service dials for token
// exchange. When mTLS service-identity is up the operator switches the
// tokensmith Service to HTTPS, so consumers must dial https://. The
// site can still override either via spec.services.tokensmith.externalEndpoint.
func bootServiceTokensmithURL(cp *openchamiv1alpha1.OpenCHAMIControlPlane) string {
	if tokensmithMTLSEnabled(cp) {
		return TokensmithBaseURL(cp)
	}
	return ServiceURL(cp, ServiceTokensmith)
}

const (
	bootServiceRequeueAfter = 30 * time.Second

	bootServicePort       = int32(27778)
	bootServicePortName   = "http"
	bootServiceHealthPath = "/boot/v1/service/status"

	bootServiceDBName = "boot_service"
	bootServiceDBUser = "boot_service"
	bootServiceDBPort = "5432"

	// S3 secret keys (shared across services).
	s3AccessKeyKey = "access_key"
	s3SecretKeyKey = "secret_key"

	// In-cluster Postgres host template. The "%s" placeholders are the
	// cluster name. Service URLs are resolved via ServiceURL() in helpers.go
	// so that spec.services.<svc>.externalEndpoint overrides are honoured.
	postgresRWHostTemplate = "openchami-%s-postgres-rw.openchami-%s.svc.cluster.local"
)

// BootServiceReconciler ensures the boot-service Deployment, Service, and PDB exist.
type BootServiceReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

func (r *BootServiceReconciler) Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "boot-service")

	if !ServiceDeployedInCluster(cp, ServiceBootService) {
		log.Info("boot-service not operator-managed (disabled or external), skipping deployment")
		return ctrl.Result{}, nil
	}

	dep := r.buildDeployment(cp)
	logDep := logging.EnrichWithResource(log, "Deployment", dep.Name)
	logDep.Info("applying boot-service Deployment")
	if err := r.Client.Patch(ctx, dep, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying boot-service Deployment: %w", err)
	}

	svc := r.buildService(cp)
	logSvc := logging.EnrichWithResource(log, "Service", svc.Name)
	logSvc.Info("applying boot-service Service")
	if err := r.Client.Patch(ctx, svc, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying boot-service Service: %w", err)
	}

	pdb := r.buildPDB(cp)
	logPDB := logging.EnrichWithResource(log, "PodDisruptionBudget", pdb.Name)
	logPDB.Info("applying boot-service PodDisruptionBudget")
	if err := r.Client.Patch(ctx, pdb, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying boot-service PodDisruptionBudget: %w", err)
	}

	ns := ControlPlaneNamespace(cp)
	current := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: ServiceBootService}, current); err != nil {
		return ctrl.Result{}, fmt.Errorf("reading boot-service Deployment status: %w", err)
	}

	ready := current.Status.AvailableReplicas >= 1
	scheme := listenerHTTP
	if cp.Spec.Services.BootService.TLS.Enabled {
		scheme = listenerHTTPS
	}
	endpoint := fmt.Sprintf("%s://%s.%s.svc.cluster.local:%d", scheme, ServiceBootService, ns, bootServicePort)
	message := "boot-service Deployment available"
	if !ready {
		message = fmt.Sprintf("waiting for boot-service Deployment (availableReplicas=%d)", current.Status.AvailableReplicas)
	}

	if cp.Status.Services == nil {
		cp.Status.Services = map[string]openchamiv1alpha1.ServiceStatus{}
	}
	cp.Status.Services[ServiceBootService] = openchamiv1alpha1.ServiceStatus{
		Ready:    ready,
		Endpoint: endpoint,
		Message:  message,
	}

	if !ready {
		return ctrl.Result{RequeueAfter: bootServiceRequeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

func (r *BootServiceReconciler) Describe(cp *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
	return []client.Object{
		r.buildDeployment(cp),
		r.buildService(cp),
		r.buildPDB(cp),
	}, nil
}

// bootServicePodLabels returns the canonical label set for boot-service pods.
func bootServicePodLabels(cp *openchamiv1alpha1.OpenCHAMIControlPlane) map[string]string {
	return map[string]string{
		labelAppName:   ServiceBootService,
		labelAppInst:   "openchami-" + cp.Spec.ClusterName,
		labelManagedBy: managedByValue,
	}
}

func (r *BootServiceReconciler) buildDeployment(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *appsv1.Deployment {
	labels := bootServicePodLabels(cp)
	bs := cp.Spec.Services.BootService
	replicas := bs.Replicas

	image, pullPolicy := ResolveImage(cp, ServiceBootService)

	tmpVol, tmpMount := TmpVolume()
	// boot-service's image WORKDIR is /app, so `./data` resolves to
	// /app/data. Mount the writable emptyDir there so the binary's
	// default storage path becomes writable. See DataVolumeAt for why
	// --data-dir / BOOT_SERVICE_DATA_DIR cannot be used to override this.
	dataVol, dataMount := DataVolumeAt("/app/data")

	dbCredsName := SecretName(cp, SuffixBootServiceDB)
	s3CredsName := SecretName(cp, SuffixS3Credentials)
	name := cp.Spec.ClusterName

	env := []corev1.EnvVar{
		// Override the default :8080 listen port to match the operator's
		// service definition. boot-service uses viper.SetEnvPrefix("BOOT_SERVICE")
		// and the `port` mapstructure tag matches the cobra flag's viper key
		// directly, so this env var binds cleanly. Without it the binary
		// listens on 8080 while the Service/probe target 27778 — startup
		// probe fails and the pod CrashLoops.
		{Name: "BOOT_SERVICE_PORT", Value: fmt.Sprintf("%d", bootServicePort)},
		{Name: "BOOT_SERVICE_DBHOST", Value: fmt.Sprintf(postgresRWHostTemplate, name, name)},
		{Name: "BOOT_SERVICE_DBPORT", Value: bootServiceDBPort},
		{Name: "BOOT_SERVICE_DBNAME", Value: bootServiceDBName},
		{Name: "BOOT_SERVICE_DBUSER", Value: bootServiceDBUser},
		{
			Name: "BOOT_SERVICE_DBPASS",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: dbCredsName},
					Key:                  VaultKeyDBPassword,
				},
			},
		},
		// boot-service v0.1.5 reads its HSM/SMD upstream from
		// `--hsm-url` (mapstructure: hsm_url). With viper prefix
		// BOOT_SERVICE_ and dash→underscore replacement that resolves
		// to BOOT_SERVICE_HSM_URL. The pre-2026-05 BOOT_SERVICE_SMD_URL
		// env var the operator used to set is silently ignored — the
		// binary never read it, which is why the background HSM sync
		// loop reported `hsm=false` at startup. See:
		// https://github.com/OpenCHAMI/boot-service/blob/v0.1.5/cmd/server/main.go
		{Name: "BOOT_SERVICE_HSM_URL", Value: ServiceURL(cp, ServiceSMD)},
		// boot-service v0.1.5 gates the outbound tokensmith service-token
		// exchange on enable_auth — without this set true the binary
		// logs "tokensmith URL ignored, auth disabled" and the
		// BOOT_SERVICE_TOKENSMITH_* env vars below have no effect, so
		// HSM calls go out anonymous and SMD's auth-gated reads (group
		// membership in particular) come back 401. Inbound auth is a
		// no-op in v0.1.5 — no middleware is mounted on /boot/v1/* —
		// so iPXE bootscript fetches remain unauthenticated.
		{Name: "BOOT_SERVICE_ENABLE_AUTH", Value: "true"}, //nolint:goconst // env value, not a label
		// Tokensmith wiring for the RFC 8693 exchange boot-service
		// performs against the bootstrap token in
		// BOOT_SERVICE_TOKENSMITH_BOOTSTRAP_TOKEN. target_service=hsm
		// matches the aud claim the bootstrap-token was minted with.
		// The bootstrap-token Secret is kept as a fallback even when
		// mTLS is enabled: the tokensmith client library's GetToken
		// flow prefers (refresh → cert → bootstrap) automatically, so
		// a cert-bearing pod uses the cert path and the env var goes
		// unread. Removing the bootstrap envelope entirely would mean
		// any pod that started before service-identity was Ready had
		// no path to authentication.
		{Name: "BOOT_SERVICE_TOKENSMITH_URL", Value: bootServiceTokensmithURL(cp)},
		{Name: "BOOT_SERVICE_TOKENSMITH_TARGET_SERVICE", Value: bootstrapTokenAudience},
		{
			Name: "BOOT_SERVICE_TOKENSMITH_BOOTSTRAP_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: SecretName(cp, SuffixBootServiceBootstr)},
					Key:                  BootstrapTokenKey,
					Optional:             &boolTrue,
				},
			},
		},
		{Name: "BOOT_SERVICE_JWKS_URL", Value: bootServiceTokensmithURL(cp) + "/.well-known/jwks.json"},
		{Name: "BOOT_SERVICE_S3_ENDPOINT", Value: cp.Spec.Platform.ObjectStorage.Endpoint},
		{Name: "BOOT_SERVICE_S3_BUCKET", Value: BootBucketName(cp)},
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

	// mTLS service-identity wiring: per the upstream prompt
	// (boot-service/docs/portable-service-identity-prompt.md) the
	// consumer reads its cert+key from two new flags. With the cert
	// in place the tokensmith client library's GetToken flow
	// transparently prefers the mTLS service-identity endpoint over
	// the single-use bootstrap token, so we never have to mint a
	// fresh bootstrap on pod restart.
	identityVols, identityMounts, identityWired := serviceIdentityVolumesAndMounts(cp, ServiceBootService)
	if identityWired && tokensmithMTLSEnabled(cp) {
		env = append(env,
			corev1.EnvVar{Name: "BOOT_SERVICE_TOKENSMITH_SERVICE_IDENTITY_CERT", Value: serviceIdentityCertFilePath()},
			corev1.EnvVar{Name: "BOOT_SERVICE_TOKENSMITH_SERVICE_IDENTITY_KEY", Value: serviceIdentityKeyFilePath()},
		)
	}

	probe := func(period, failures int32) *corev1.Probe {
		scheme := corev1.URISchemeHTTP
		if cp.Spec.Services.BootService.TLS.Enabled {
			scheme = corev1.URISchemeHTTPS
		}
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   bootServiceHealthPath,
					Port:   intstr.FromString(bootServicePortName),
					Scheme: scheme,
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
		VolumeMounts:   mounts,
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
			Namespace: ControlPlaneNamespace(cp),
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

func (r *BootServiceReconciler) buildService(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *corev1.Service {
	labels := bootServicePodLabels(cp)
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: coreAPIVersion, Kind: kindService},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceBootService,
			Namespace: ControlPlaneNamespace(cp),
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

func (r *BootServiceReconciler) buildPDB(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *policyv1.PodDisruptionBudget {
	labels := bootServicePodLabels(cp)
	minAvail := intstr.FromInt32(1)
	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{APIVersion: policyAPIVersion, Kind: kindPodDisruptionBudget},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceBootService,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    labels,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvail,
			Selector:     &metav1.LabelSelector{MatchLabels: labels},
		},
	}
}
