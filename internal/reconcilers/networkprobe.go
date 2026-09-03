// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"fmt"
	"sort"
	"strconv"
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
	networkProbeRequeueAfter = 30 * time.Second

	// kindDaemonSet is the Kind name for apps/v1 DaemonSet.
	// Defined here (rather than in smd.go) because it is first used in
	// Phase 6; reused by coredhcp.go.
	kindDaemonSet = "DaemonSet"

	// kindCronJob is the Kind name for batch/v1 CronJob. First used by magellan.
	kindCronJob = "CronJob"

	// batchAPIVersion is the GroupVersion for batch/v1 (CronJob).
	batchAPIVersion = "batch/v1"

	// probeNetworkReadyLabelFmt is the label key written by the probe binary
	// onto each node. Format args are clusterName, probeType ("provision"|"bmc").
	//
	// Kubernetes label keys allow at most one '/' (prefix/name), so the
	// cluster name and probe type are joined into the qualified-name
	// segment: openchami.org/<cluster>-<probe>-network-ready.
	probeNetworkReadyLabelFmt = "openchami.org/%s-%s-network-ready"
	// probeLabelValueTrue is the "passes probe" value of the probe-applied label.
	probeLabelValueTrue = "true"

	probeTypeProvision = "provision"
	probeTypeBMC       = "bmc"

	// priorityClassSystemNodeCritical pins host-critical DaemonSets so they
	// are not evicted ahead of workload pods.
	priorityClassSystemNodeCritical = "system-node-critical"

	// containerNameProbe is the container name inside the probe DaemonSet
	// pod template (also referenced as the binary subcommand argument).
	containerNameProbe = "probe"
)

// NetworkProbeReconciler ensures the network-probe DaemonSet exists and
// translates per-node probe labels into cluster-level status.
type NetworkProbeReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

// Reconcile applies the probe DaemonSet (if probing is enabled), reads back
// node labels written by the probe binary, and sets ConditionNetworkProbeReady.
func (r *NetworkProbeReconciler) Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "networkprobe")

	if !cp.Spec.NetworkProbe.Enabled {
		log.Info("network probe disabled, manual nodeSelectors in use")
		// Trivially satisfy the condition so downstream sub-reconcilers
		// (CoreDHCP, Magellan) don't gate on a probe that isn't running.
		cp.Status.NetworkProbe = nil
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionNetworkProbeReady,
			Status:             metav1.ConditionTrue,
			Reason:             conditions.ReasonReady,
			Message:            "probe disabled, manual nodeSelectors in use",
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{}, nil
	}

	ds := r.buildDaemonSet(cp)
	dsLog := logging.EnrichWithResource(log, kindDaemonSet, ds.Name)
	dsLog.Info("applying network-probe DaemonSet")
	if err := r.Client.Patch(ctx, ds, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying network-probe DaemonSet: %w", err)
	}

	current := &appsv1.DaemonSet{}
	getErr := r.Client.Get(ctx, types.NamespacedName{Namespace: ds.Namespace, Name: ds.Name}, current)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return ctrl.Result{}, fmt.Errorf("reading network-probe DaemonSet status: %w", getErr)
	}
	numberReady := int32(0)
	if getErr == nil {
		numberReady = current.Status.NumberReady
	}

	if numberReady == 0 {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionNetworkProbeReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonProvisioning,
			Message:            "waiting for network-probe DaemonSet pods to become ready",
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{RequeueAfter: networkProbeRequeueAfter}, nil
	}

	// Read all nodes and tally those with each probe-applied label.
	nodes := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodes); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing nodes for probe status: %w", err)
	}

	provisionLabel := fmt.Sprintf(probeNetworkReadyLabelFmt, cp.Spec.ClusterName, probeTypeProvision)
	bmcLabel := fmt.Sprintf(probeNetworkReadyLabelFmt, cp.Spec.ClusterName, probeTypeBMC)

	provisionNodes := []string{}
	bmcNodes := []string{}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if v, ok := n.Labels[provisionLabel]; ok && v == probeLabelValueTrue {
			provisionNodes = append(provisionNodes, n.Name)
		}
		if v, ok := n.Labels[bmcLabel]; ok && v == probeLabelValueTrue {
			bmcNodes = append(bmcNodes, n.Name)
		}
	}
	sort.Strings(provisionNodes)
	sort.Strings(bmcNodes)

	bmcConfigured := cp.Spec.NetworkProbe.BMCNetwork != nil
	provisionConfigured := cp.Spec.NetworkProbe.ProvisionNetwork != nil

	provisionOK := !provisionConfigured || len(provisionNodes) > 0
	bmcOK := !bmcConfigured || len(bmcNodes) > 0
	probeReady := provisionOK && bmcOK

	cp.Status.NetworkProbe = &openchamiv1alpha1.NetworkProbeStatus{
		NodesWithProvisionAccess: provisionNodes,
		NodesWithBMCAccess:       bmcNodes,
		ProbeReady:               probeReady,
	}

	if !probeReady {
		missing := ""
		switch {
		case provisionConfigured && len(provisionNodes) == 0 && bmcConfigured && len(bmcNodes) == 0:
			missing = "no nodes pass provision probe; no nodes pass BMC probe"
		case provisionConfigured && len(provisionNodes) == 0:
			missing = "no nodes pass provision probe"
		case bmcConfigured && len(bmcNodes) == 0:
			missing = "no nodes pass BMC probe"
		default:
			missing = "no eligible nodes"
		}
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionNetworkProbeReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonNoEligibleNodes,
			Message:            missing,
			ObservedGeneration: cp.Generation,
		})
		RecordConditionEvent(r.Recorder, cp,
			corev1.EventTypeWarning, "NoEligibleNodes",
			"network-probe DaemonSet running but no nodes meet probe requirements: "+missing)
		return ctrl.Result{RequeueAfter: networkProbeRequeueAfter}, nil
	}

	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionNetworkProbeReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            fmt.Sprintf("provisionNodes=%d bmcNodes=%d", len(provisionNodes), len(bmcNodes)),
		ObservedGeneration: cp.Generation,
	})
	return ctrl.Result{}, nil
}

// Describe returns the Kubernetes objects this reconciler would apply.
// Returns an empty (but non-nil) slice when the probe is disabled so callers
// can iterate without nil checks.
func (r *NetworkProbeReconciler) Describe(cp *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
	if !cp.Spec.NetworkProbe.Enabled {
		return []client.Object{}, nil
	}
	return []client.Object{r.buildDaemonSet(cp)}, nil
}

// networkProbePodLabels returns the canonical label set for probe pods.
func networkProbePodLabels(cp *openchamiv1alpha1.OpenCHAMIControlPlane) map[string]string {
	return map[string]string{
		labelAppName:   ServiceNetworkProbe,
		labelAppInst:   "openchami-" + cp.Spec.ClusterName,
		labelManagedBy: managedByValue,
	}
}

// fieldRefEnv returns an EnvVar whose value is sourced from a pod fieldRef.
func fieldRefEnv(name, fieldPath string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: fieldPath},
		},
	}
}

// targetEnvVars expands a NetworkProbeTarget into four PROBE_<prefix>_*
// environment variables. Always emits all four (empty when target is nil)
// so the probe binary sees a deterministic env shape.
func targetEnvVars(prefix string, t *openchamiv1alpha1.NetworkProbeTarget) []corev1.EnvVar {
	subnet, host, port, timeout := "", "", "", ""
	if t != nil {
		subnet = t.Subnet
		host = t.ValidateHost
		if t.ValidatePort != 0 {
			port = strconv.FormatInt(int64(t.ValidatePort), 10)
		}
		timeout = t.ValidateTimeout
	}
	return []corev1.EnvVar{
		{Name: "PROBE_" + prefix + "_SUBNET", Value: subnet},
		{Name: "PROBE_" + prefix + "_HOST", Value: host},
		{Name: "PROBE_" + prefix + "_PORT", Value: port},
		{Name: "PROBE_" + prefix + "_TIMEOUT", Value: timeout},
	}
}

func (r *NetworkProbeReconciler) buildDaemonSet(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *appsv1.DaemonSet {
	labels := networkProbePodLabels(cp)
	tmpVol, tmpMount := TmpVolume()

	intervalSeconds := cp.Spec.NetworkProbe.IntervalSeconds
	if intervalSeconds == 0 {
		intervalSeconds = 300
	}

	env := make([]corev1.EnvVar, 0, 11)
	env = append(env,
		corev1.EnvVar{Name: "PROBE_CLUSTER_NAME", Value: cp.Spec.ClusterName},
		corev1.EnvVar{Name: "PROBE_INTERVAL_SECONDS", Value: strconv.FormatInt(int64(intervalSeconds), 10)},
		fieldRefEnv("NODE_NAME", "spec.nodeName"),
	)
	env = append(env, targetEnvVars("PROVISION", cp.Spec.NetworkProbe.ProvisionNetwork)...)
	env = append(env, targetEnvVars("BMC", cp.Spec.NetworkProbe.BMCNetwork)...)

	hostNetwork := false

	image, pullPolicy := ResolveImage(cp, ServiceNetworkProbe)
	container := corev1.Container{
		Name:            containerNameProbe,
		Image:           image,
		ImagePullPolicy: pullPolicy,
		Command:         []string{"/probe"},
		SecurityContext: CommonSecurityContext(),
		Env:             env,
		VolumeMounts:    []corev1.VolumeMount{tmpMount},
	}

	return &appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{APIVersion: appsAPIVersion, Kind: kindDaemonSet},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openchami-" + cp.Spec.ClusterName + "-network-probe",
			Namespace: ControlPlaneNamespace(cp),
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: ServiceNetworkProbe,
					EnableServiceLinks: DisableServiceLinks(),
					PriorityClassName:  priorityClassSystemNodeCritical,
					HostNetwork:        hostNetwork,
					SecurityContext:    CommonPodSecurityContext(),
					// Probe must run on every node, including control-plane.
					Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Containers:  []corev1.Container{container},
					Volumes:     []corev1.Volume{tmpVol},
				},
			},
		},
	}
}
