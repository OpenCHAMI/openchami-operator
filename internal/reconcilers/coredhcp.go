// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	coreDHCPRequeueAfter = 30 * time.Second

	coreDHCPPort     int32 = 67
	coreDHCPPortName       = "dhcp-server"

	// coreDHCPTFTPPort is the coresmd plugin's built-in TFTP server
	// (tftp_port, default 69) that serves the iPXE binaries bundled in
	// the coresmd image. Declared so a hostPort conflict is visible to
	// the scheduler; with single_port=false coresmd still answers from
	// ephemeral high ports, which hostNetwork permits.
	coreDHCPTFTPPort     int32 = 69
	coreDHCPTFTPPortName       = "tftp"

	// coreDHCPConfigMapName is the operator-managed ConfigMap mounted
	// into the DaemonSet. Its content is either rendered from the spec
	// or copied from spec.services.coreDHCP.configMapRef.
	coreDHCPConfigMapName = ServiceCoreDHCP + "-config"

	// coreDHCPConfigSourceAnnotation records where the mirrored config
	// came from ("generated" or "<namespace>/<name>:<key>").
	coreDHCPConfigSourceAnnotation = "openchami.org/coredhcp-config-source"
	coreDHCPConfigSourceGenerated  = "generated"

	// coreDHCPConfigHashAnnotation is stamped on the pod template so a
	// config change triggers a DaemonSet rollout.
	coreDHCPConfigHashAnnotation = "openchami.org/coredhcp-config-hash"

	// coreDHCPCAMountPath / coreDHCPCAFile are where the gateway TLS
	// Secret's ca.crt is mounted, matching the path the upstream coresmd
	// examples use for `ca_cert=`. Point coresmd at it with
	// `ca_cert=/root_ca/root_ca.crt` so it can validate the gateway's
	// certificate when svc_base_uri is https://<spec.domain>.
	coreDHCPCAMountPath = "/root_ca"
	coreDHCPCAFile      = "root_ca.crt"
	coreDHCPCAVolume    = "gateway-ca"
	gatewayTLSCAKey     = "ca.crt"

	// coreDHCPConfigMountPath is where coredhcp's config-search loop looks
	// for `config.{yml,yaml}`. The upstream binary searches `/`, `/coredhcp`,
	// `/.coredhcp`, and `/etc/coredhcp` — we mount in the last of those so
	// the operator-rendered ConfigMap takes precedence over any bundled
	// defaults the image might ship.
	coreDHCPConfigMountPath = "/etc/coredhcp"
	coreDHCPConfigKey       = "config.yml"
	coreDHCPConfigVolume    = "config"
)

// CoreDHCPReconciler ensures the CoreDHCP DaemonSet exists on nodes that
// pass the provision-network probe (or are manually selected).
type CoreDHCPReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

// Reconcile applies the CoreDHCP DaemonSet and reports DHCPReady.
//
// When the network probe is enabled but ConditionNetworkProbeReady is not
// True, this reconciler defers and requeues; the DaemonSet is only applied
// once the probe has identified eligible nodes.
func (r *CoreDHCPReconciler) Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "coredhcp")

	if !cp.Spec.Services.CoreDHCP.Enabled {
		log.Info("coredhcp disabled, skipping")
		return ctrl.Result{}, nil
	}

	// Probe gate: when probing is enabled, we must wait for the probe
	// reconciler to report at least one eligible node before scheduling.
	if cp.Spec.NetworkProbe.Enabled &&
		!apimeta.IsStatusConditionTrue(cp.Status.Conditions, conditions.ConditionNetworkProbeReady) {
		log.Info("waiting for network probe before deploying coredhcp")
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionDHCPReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonWaitingForProbe,
			Message:            "waiting for NetworkProbeReady before scheduling coredhcp",
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{RequeueAfter: coreDHCPRequeueAfter}, nil
	}

	// TODO(phase06b): mint coredhcp-smd-token after tokensmith ready.
	//   1. Skip if Secret openchami-{cluster}-coredhcp-smd-token already exists.
	//   2. POST to tokensmith /token endpoint with the coredhcp ServiceAccount
	//      JWT to receive a scoped SMD-write token.
	//   3. Server-side apply the resulting token into the Secret above.

	// Resolve the CoreDHCP config: either rendered from the spec or read
	// from the user-provided ConfigMap. Either way it is mirrored into
	// the operator-managed coredhcp-config ConfigMap in the control-plane
	// namespace, so the DaemonSet's volume never changes shape between
	// modes. When the source is unavailable we leave any existing
	// DaemonSet untouched (it keeps serving the last-good config) rather
	// than tearing DHCP down.
	data, source, reason, msg, err := r.resolveConfig(ctx, cp)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason != "" {
		log.Info("coredhcp config unavailable, not applying DaemonSet", "reason", reason, "detail", msg)
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionDHCPReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: cp.Generation,
		})
		RecordConditionEvent(r.Recorder, cp, corev1.EventTypeWarning, reason, msg)
		return ctrl.Result{RequeueAfter: coreDHCPRequeueAfter}, nil
	}

	// Apply the config ConfigMap before the DaemonSet so the pods
	// schedule with the volume already populated. SSA is idempotent
	// either way, but ordering this first avoids one self-correcting
	// requeue on the first reconcile.
	cm := r.buildConfigMap(cp, data, source)
	cmLog := logging.EnrichWithResource(log, kindConfigMap, cm.Name)
	cmLog.Info("applying coredhcp ConfigMap", "source", source)
	if err := r.Client.Patch(ctx, cm, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying coredhcp ConfigMap: %w", err)
	}

	ds := r.buildDaemonSet(cp, coreDHCPConfigHash(data))
	dsLog := logging.EnrichWithResource(log, kindDaemonSet, ds.Name)
	dsLog.Info("applying coredhcp DaemonSet")
	if err := r.Client.Patch(ctx, ds, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying coredhcp DaemonSet: %w", err)
	}

	current := &appsv1.DaemonSet{}
	getErr := r.Client.Get(ctx, types.NamespacedName{Namespace: ds.Namespace, Name: ds.Name}, current)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return ctrl.Result{}, fmt.Errorf("reading coredhcp DaemonSet status: %w", getErr)
	}
	numberReady := int32(0)
	if getErr == nil {
		numberReady = current.Status.NumberReady
	}

	// TODO(phase11): expose CoreDHCP node list on cluster.Status (no field yet).

	if numberReady == 0 {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionDHCPReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonProvisioning,
			Message:            "waiting for coredhcp DaemonSet pods to become ready",
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{RequeueAfter: coreDHCPRequeueAfter}, nil
	}

	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionDHCPReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            fmt.Sprintf("coredhcp DaemonSet ready (numberReady=%d)", numberReady),
		ObservedGeneration: cp.Generation,
	})
	return ctrl.Result{}, nil
}

// Describe returns the Kubernetes objects this reconciler would apply.
// Returns an empty (but non-nil) slice when CoreDHCP is disabled.
//
// Describe has no API client, so with a configMapRef the mirrored
// ConfigMap carries a placeholder noting the source instead of the
// user's content, and the DaemonSet's config-hash annotation is computed
// from that placeholder.
func (r *CoreDHCPReconciler) Describe(cp *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
	if !cp.Spec.Services.CoreDHCP.Enabled {
		return []client.Object{}, nil
	}
	var data, source string
	if ref := cp.Spec.Services.CoreDHCP.ConfigMapRef; ref != nil {
		source = coreDHCPConfigSourceRef(cp, ref)
		data = "# CoreDHCP config is copied at reconcile time from " + source + "\n"
	} else {
		rendered, err := renderCoreDHCPConfig(cp)
		if err != nil {
			return nil, err
		}
		data, source = rendered, coreDHCPConfigSourceGenerated
	}
	return []client.Object{
		r.buildConfigMap(cp, data, source),
		r.buildDaemonSet(cp, coreDHCPConfigHash(data)),
	}, nil
}

// resolveConfig returns the CoreDHCP config file contents and a short
// description of where they came from. When the config cannot be
// produced for a user-correctable reason, data is empty and reason/msg
// describe the problem for ConditionDHCPReady; err is reserved for
// transient API failures that should be retried with backoff.
func (r *CoreDHCPReconciler) resolveConfig(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (data, source, reason, msg string, err error) {
	ref := cp.Spec.Services.CoreDHCP.ConfigMapRef
	if ref == nil {
		rendered, renderErr := renderCoreDHCPConfig(cp)
		if renderErr != nil {
			return "", "", conditions.ReasonInvalidConfig,
				fmt.Sprintf("cannot generate coredhcp config: %v; set spec.services.coreDHCP.leaseRanges or configMapRef", renderErr), nil
		}
		return rendered, coreDHCPConfigSourceGenerated, "", "", nil
	}

	source = coreDHCPConfigSourceRef(cp, ref)
	user := &corev1.ConfigMap{}
	getErr := r.Client.Get(ctx, types.NamespacedName{Namespace: cp.Namespace, Name: ref.Name}, user)
	switch {
	case apierrors.IsNotFound(getErr):
		return "", source, conditions.ReasonConfigMapNotFound,
			fmt.Sprintf("coredhcp configMapRef %s not found", source), nil
	case getErr != nil:
		return "", source, "", "", fmt.Errorf("reading coredhcp configMapRef %s: %w", source, getErr)
	}
	content, ok := user.Data[ref.EffectiveKey()]
	if !ok || content == "" {
		return "", source, conditions.ReasonConfigMapNotFound,
			fmt.Sprintf("coredhcp configMapRef %s has no data key %q (or it is empty)", source, ref.EffectiveKey()), nil
	}
	return content, source, "", "", nil
}

// coreDHCPConfigSourceRef formats a configMapRef as namespace/name:key for
// logs, conditions and the mirrored ConfigMap's source annotation.
func coreDHCPConfigSourceRef(cp *openchamiv1alpha1.OpenCHAMIControlPlane, ref *openchamiv1alpha1.CoreDHCPConfigMapRef) string {
	return fmt.Sprintf("%s/%s:%s", cp.Namespace, ref.Name, ref.EffectiveKey())
}

// coreDHCPConfigHash returns a stable digest of the config contents. It
// is stamped on the DaemonSet pod template so a config change rolls the
// pods: coredhcp reads its config once at startup and never reloads it.
func coreDHCPConfigHash(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// buildConfigMap wraps the coredhcp YAML in a ConfigMap suitable for SSA.
// Lives in the cluster namespace and is mounted into the DS. source is
// recorded as an annotation so an admin inspecting the mirrored copy can
// tell whether it was generated or copied from a user ConfigMap.
func (r *CoreDHCPReconciler) buildConfigMap(cp *openchamiv1alpha1.OpenCHAMIControlPlane, data, source string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: coreAPIVersion, Kind: kindConfigMap},
		ObjectMeta: metav1.ObjectMeta{
			Name:        coreDHCPConfigMapName,
			Namespace:   ControlPlaneNamespace(cp),
			Labels:      coreDHCPPodLabels(cp),
			Annotations: map[string]string{coreDHCPConfigSourceAnnotation: source},
		},
		Data: map[string]string{coreDHCPConfigKey: data},
	}
}

// coreDHCPPodLabels returns the canonical label set for coredhcp pods.
func coreDHCPPodLabels(cp *openchamiv1alpha1.OpenCHAMIControlPlane) map[string]string {
	return map[string]string{
		labelAppName:   ServiceCoreDHCP,
		labelAppInst:   "openchami-" + cp.Spec.ClusterName,
		labelManagedBy: managedByValue,
	}
}

// dhcpSecurityContext is CommonSecurityContext() with the network
// capabilities coredhcp requires for L2 DHCP, forced to run as root
// (UID 0). We build a fresh struct rather than mutate the shared object.
//
//   - NET_BIND_SERVICE: bind the privileged UDP/67 server port.
//   - NET_RAW, NET_ADMIN: coredhcp's server4 opens a raw/broadcast socket to
//     answer clients that have no IP yet (DORA happens before the client has
//     an address). These match the caps documented by the upstream coresmd
//     coredhcp plugin (--cap-add=NET_ADMIN,NET_RAW).
//
// Why root: the coresmd image ships its coredhcp binary without file
// capabilities (no setcap), and Kubernetes has no way to grant ambient
// capabilities to a non-root process. Under a non-root UID the added caps
// are dropped on exec, so coredhcp cannot open the raw socket / bind UDP/67
// and dies with "cannot bind to port 67: permission denied" (see issue #21).
// Running as UID 0 keeps the file caps effective. This matches how upstream
// coresmd is run (its documented podman/quadlet examples run as root and
// mount into /root_ca). If a future coresmd image setcap's its binary, this
// pod can move back to the shared non-root UID by dropping the RunAsNonRoot /
// RunAsUser overrides here and in dhcpPodSecurityContext().
func dhcpSecurityContext() *corev1.SecurityContext {
	sc := CommonSecurityContext()
	nonRoot := false
	uid := int64(0)
	sc.RunAsNonRoot = &nonRoot
	sc.RunAsUser = &uid
	sc.Capabilities = &corev1.Capabilities{
		Drop: []corev1.Capability{"ALL"},
		Add:  []corev1.Capability{"NET_BIND_SERVICE", "NET_RAW", "NET_ADMIN"},
	}
	return sc
}

// dhcpPodSecurityContext is CommonPodSecurityContext() forced to run as root
// (UID/GID 0) so the pod-level policy matches the container-level override in
// dhcpSecurityContext(). See that function for why coredhcp must run as root.
func dhcpPodSecurityContext() *corev1.PodSecurityContext {
	psc := CommonPodSecurityContext()
	nonRoot := false
	zero := int64(0)
	psc.RunAsNonRoot = &nonRoot
	psc.RunAsUser = &zero
	psc.RunAsGroup = &zero
	psc.FSGroup = &zero
	return psc
}

func (r *CoreDHCPReconciler) buildDaemonSet(cp *openchamiv1alpha1.OpenCHAMIControlPlane, configHash string) *appsv1.DaemonSet {
	labels := coreDHCPPodLabels(cp)
	tmpVol, tmpMount := TmpVolume()
	dhcp := cp.Spec.Services.CoreDHCP

	env := []corev1.EnvVar{
		fieldRefEnv("NODE_NAME", "spec.nodeName"),
	}

	preStop := &corev1.Lifecycle{
		PreStop: &corev1.LifecycleHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"/bin/sh", "-c",
					`echo "WARN: CoreDHCP stopping on $(hostname). ` +
						`Provision network DHCP may be interrupted. ` +
						`Runbook: https://openchami.org/docs/ops/coredhcp-node-drain" >&2`,
				},
			},
		},
	}

	configMount := corev1.VolumeMount{
		Name:      coreDHCPConfigVolume,
		MountPath: coreDHCPConfigMountPath,
		ReadOnly:  true,
	}
	// The gateway TLS Secret's ca.crt, for coresmd's ca_cert= when it
	// reaches SMD / boot-service through https://<spec.domain>. Mounted
	// as a single file so /root_ca contains only root_ca.crt. Optional:
	// cert-manager does not populate ca.crt for every issuer type (ACME
	// in particular), and a missing key must not block DHCP. Sites
	// without ca.crt point coresmd's ca_cert= at the image's system
	// bundle or bake their CA into a derived image (docs/crd-reference.md).
	caMount := corev1.VolumeMount{
		Name:      coreDHCPCAVolume,
		MountPath: coreDHCPCAMountPath,
		ReadOnly:  true,
	}
	optional := true

	image, pullPolicy := ResolveImage(cp, ServiceCoreDHCP)
	container := corev1.Container{
		Name:            ServiceCoreDHCP,
		Image:           image,
		ImagePullPolicy: pullPolicy,
		SecurityContext: dhcpSecurityContext(),
		// No explicit --config flag: the upstream image's ENTRYPOINT is
		// `tini --` and prepending an arg makes tini try to exec the
		// flag. Instead we mount the ConfigMap at /etc/coredhcp/, which
		// is already on the binary's built-in search path
		// (`/`, `/coredhcp`, `/.coredhcp`, `/etc/coredhcp`), so
		// auto-discovery picks up our config.yml without needing a flag.
		Ports: []corev1.ContainerPort{
			{
				Name:          coreDHCPPortName,
				ContainerPort: coreDHCPPort,
				HostPort:      coreDHCPPort,
				Protocol:      corev1.ProtocolUDP,
			},
			{
				Name:          coreDHCPTFTPPortName,
				ContainerPort: coreDHCPTFTPPort,
				HostPort:      coreDHCPTFTPPort,
				Protocol:      corev1.ProtocolUDP,
			},
		},
		Env:          env,
		VolumeMounts: []corev1.VolumeMount{tmpMount, configMount, caMount},
		Lifecycle:    preStop,
	}
	if dhcp.Resources != nil {
		container.Resources = *dhcp.Resources
	}

	return &appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{APIVersion: appsAPIVersion, Kind: kindDaemonSet},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceCoreDHCP,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{coreDHCPConfigHashAnnotation: configHash},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: ServiceCoreDHCP,
					EnableServiceLinks: DisableServiceLinks(),
					PriorityClassName:  priorityClassSystemNodeCritical,
					HostNetwork:        true,
					DNSPolicy:          corev1.DNSClusterFirstWithHostNet,
					NodeSelector:       EffectiveNodeSelector(cp, probeTypeProvision),
					Tolerations:        dhcp.Tolerations,
					SecurityContext:    dhcpPodSecurityContext(),
					Containers:         []corev1.Container{container},
					Volumes: []corev1.Volume{
						tmpVol,
						{
							Name: coreDHCPConfigVolume,
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: coreDHCPConfigMapName,
									},
								},
							},
						},
						{
							Name: coreDHCPCAVolume,
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: GatewayTLSSecretName(cp),
									Items: []corev1.KeyToPath{{
										Key:  gatewayTLSCAKey,
										Path: coreDHCPCAFile,
									}},
									Optional: &optional,
								},
							},
						},
					},
				},
			},
		},
	}
}
