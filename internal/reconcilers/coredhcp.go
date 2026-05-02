/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package reconcilers

import (
	"context"
	"encoding/json"
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

	openahamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/logging"
)

const (
	coreDHCPRequeueAfter = 30 * time.Second

	// defaultCoreDHCPImage is the fallback container image for the CoreDHCP
	// DaemonSet. Per Phase 13 the operator's ImageConfig will eventually
	// resolve image overrides; until then this is the only source of truth.
	defaultCoreDHCPImage = "ghcr.io/openchami/coresmd:latest"

	coreDHCPPort     int32 = 67
	coreDHCPPortName       = "dhcp-server"
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
func (r *CoreDHCPReconciler) Reconcile(ctx context.Context, cluster *openahamiv1alpha1.OpenCHAMICluster) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cluster, "coredhcp")

	if !cluster.Spec.Services.CoreDHCP.Enabled {
		log.Info("coredhcp disabled, skipping")
		return ctrl.Result{}, nil
	}

	// Probe gate: when probing is enabled, we must wait for the probe
	// reconciler to report at least one eligible node before scheduling.
	if cluster.Spec.NetworkProbe.Enabled &&
		!apimeta.IsStatusConditionTrue(cluster.Status.Conditions, conditions.ConditionNetworkProbeReady) {
		log.Info("waiting for network probe before deploying coredhcp")
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionDHCPReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonWaitingForProbe,
			Message:            "waiting for NetworkProbeReady before scheduling coredhcp",
			ObservedGeneration: cluster.Generation,
		})
		return ctrl.Result{RequeueAfter: coreDHCPRequeueAfter}, nil
	}

	// TODO(phase06b): mint coredhcp-smd-token after tokensmith ready.
	//   1. Skip if Secret openchami-{cluster}-coredhcp-smd-token already exists.
	//   2. POST to tokensmith /token endpoint with the coredhcp ServiceAccount
	//      JWT to receive a scoped SMD-write token.
	//   3. Server-side apply the resulting token into the Secret above.

	ds := r.buildDaemonSet(cluster)
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
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionDHCPReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonProvisioning,
			Message:            "waiting for coredhcp DaemonSet pods to become ready",
			ObservedGeneration: cluster.Generation,
		})
		return ctrl.Result{RequeueAfter: coreDHCPRequeueAfter}, nil
	}

	apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionDHCPReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            fmt.Sprintf("coredhcp DaemonSet ready (numberReady=%d)", numberReady),
		ObservedGeneration: cluster.Generation,
	})
	return ctrl.Result{}, nil
}

// Describe returns the Kubernetes objects this reconciler would apply.
// Returns an empty (but non-nil) slice when CoreDHCP is disabled.
func (r *CoreDHCPReconciler) Describe(cluster *openahamiv1alpha1.OpenCHAMICluster) ([]client.Object, error) {
	if !cluster.Spec.Services.CoreDHCP.Enabled {
		return []client.Object{}, nil
	}
	return []client.Object{r.buildDaemonSet(cluster)}, nil
}

// coreDHCPPodLabels returns the canonical label set for coredhcp pods.
func coreDHCPPodLabels(cluster *openahamiv1alpha1.OpenCHAMICluster) map[string]string {
	return map[string]string{
		labelAppName:   ServiceCoreDHCP,
		labelAppInst:   "openchami-" + cluster.Spec.ClusterName,
		labelManagedBy: managedByValue,
	}
}

// coreDHCPImage resolves the container image, preferring the per-cluster
// spec override and falling back to defaultCoreDHCPImage.
func coreDHCPImage(cluster *openahamiv1alpha1.OpenCHAMICluster) string {
	img := cluster.Spec.Services.CoreDHCP.Image
	if img == nil {
		return defaultCoreDHCPImage
	}
	repo, tag := img.Repository, img.Tag
	switch {
	case repo == "" && tag == "":
		return defaultCoreDHCPImage
	case repo == "":
		return "ghcr.io/openchami/coresmd:" + tag
	case tag == "":
		return repo + ":latest"
	default:
		return repo + ":" + tag
	}
}

// dhcpSecurityContext is CommonSecurityContext() with NET_BIND_SERVICE added
// so that the coredhcp container can bind to UDP/67. We build a fresh struct
// rather than mutate the shared object.
func dhcpSecurityContext() *corev1.SecurityContext {
	sc := CommonSecurityContext()
	sc.Capabilities = &corev1.Capabilities{
		Drop: []corev1.Capability{"ALL"},
		Add:  []corev1.Capability{"NET_BIND_SERVICE"},
	}
	return sc
}

func (r *CoreDHCPReconciler) buildDaemonSet(cluster *openahamiv1alpha1.OpenCHAMICluster) *appsv1.DaemonSet {
	labels := coreDHCPPodLabels(cluster)
	tmpVol, tmpMount := TmpVolume()
	dhcp := cluster.Spec.Services.CoreDHCP

	// LeaseRanges as JSON for the container to consume. Marshal failures fall
	// back to "[]" — they cannot occur for the well-typed input but we guard
	// to keep the binary's contract simple.
	leaseRangesJSON := "[]"
	if buf, err := json.Marshal(dhcp.LeaseRanges); err == nil {
		leaseRangesJSON = string(buf)
	}

	env := []corev1.EnvVar{
		{Name: "CLUSTER_NAME", Value: cluster.Spec.ClusterName},
		{Name: "LEASE_RANGES_JSON", Value: leaseRangesJSON},
		{Name: "UNKNOWN_LEASE_DURATION", Value: dhcp.UnknownLeaseDuration},
		{Name: "KNOWN_LEASE_DURATION", Value: dhcp.KnownLeaseDuration},
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

	container := corev1.Container{
		Name:            ServiceCoreDHCP,
		Image:           coreDHCPImage(cluster),
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: dhcpSecurityContext(),
		Ports: []corev1.ContainerPort{{
			Name:          coreDHCPPortName,
			ContainerPort: coreDHCPPort,
			HostPort:      coreDHCPPort,
			Protocol:      corev1.ProtocolUDP,
		}},
		Env:          env,
		VolumeMounts: []corev1.VolumeMount{tmpMount},
		Lifecycle:    preStop,
	}
	if dhcp.Resources != nil {
		container.Resources = *dhcp.Resources
	}

	return &appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{APIVersion: appsAPIVersion, Kind: kindDaemonSet},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceCoreDHCP,
			Namespace: ClusterNamespace(cluster),
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: ServiceCoreDHCP,
					PriorityClassName:  priorityClassSystemNodeCritical,
					HostNetwork:        true,
					DNSPolicy:          corev1.DNSClusterFirstWithHostNet,
					NodeSelector:       EffectiveNodeSelector(cluster, probeTypeProvision),
					Tolerations:        dhcp.Tolerations,
					SecurityContext:    CommonPodSecurityContext(),
					Containers:         []corev1.Container{container},
					Volumes:            []corev1.Volume{tmpVol},
				},
			},
		},
	}
}
