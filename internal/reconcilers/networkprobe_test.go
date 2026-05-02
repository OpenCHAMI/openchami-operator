/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package reconcilers

import (
	"context"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openahamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
)

// alphaProbeDS is the canonical DaemonSet name for the "alpha" probe.
// Mirrors the formula used by NetworkProbeReconciler.buildDaemonSet().
const alphaProbeDS = "openchami-alpha-network-probe"

func TestNetworkProbeReconciler_DisabledTriviallyReady(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.NetworkProbe.Enabled = false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	r := &NetworkProbeReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when probe disabled, got %+v", res)
	}

	ds := &appsv1.DaemonSet{}
	getErr := c.Get(context.Background(), types.NamespacedName{
		Namespace: ClusterNamespace(cluster),
		Name:      alphaProbeDS,
	}, ds)
	if !apierrors.IsNotFound(getErr) {
		t.Errorf("expected probe DaemonSet to be absent when probe disabled, got err=%v", getErr)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionNetworkProbeReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != conditions.ReasonReady {
		t.Fatalf("expected NetworkProbeReady=True/Ready, got %+v", cond)
	}
	if cluster.Status.NetworkProbe != nil {
		t.Errorf("expected status.networkProbe to be cleared when disabled, got %+v",
			cluster.Status.NetworkProbe)
	}
}

func TestNetworkProbeReconciler_AppliesDaemonSet(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.NetworkProbe = openahamiv1alpha1.NetworkProbeSpec{
		Enabled:         true,
		IntervalSeconds: 60,
		ProvisionNetwork: &openahamiv1alpha1.NetworkProbeTarget{
			Subnet:          testProvisionSubnet,
			ValidateHost:    "10.0.0.1",
			ValidatePort:    80,
			ValidateTimeout: "3s",
		},
		BMCNetwork: &openahamiv1alpha1.NetworkProbeTarget{
			Subnet: testBMCSubnet,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	r := &NetworkProbeReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ds := &appsv1.DaemonSet{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ClusterNamespace(cluster),
		Name:      alphaProbeDS,
	}, ds); err != nil {
		t.Fatalf("getting probe DaemonSet: %v", err)
	}

	if ds.Spec.Template.Spec.ServiceAccountName != ServiceNetworkProbe {
		t.Errorf("expected SA=%q, got %q", ServiceNetworkProbe, ds.Spec.Template.Spec.ServiceAccountName)
	}
	if ds.Spec.Template.Spec.PriorityClassName != testPriorityClass {
		t.Errorf("expected priority=system-node-critical, got %q", ds.Spec.Template.Spec.PriorityClassName)
	}
	if ds.Spec.Template.Spec.NodeSelector != nil {
		t.Errorf("expected no nodeSelector (probe runs everywhere), got %+v", ds.Spec.Template.Spec.NodeSelector)
	}
	if ds.Spec.Template.Spec.HostNetwork {
		t.Errorf("expected HostNetwork=false, got true")
	}
	if len(ds.Spec.Template.Spec.Tolerations) != 1 || ds.Spec.Template.Spec.Tolerations[0].Operator != corev1.TolerationOpExists {
		t.Errorf("expected single Exists toleration, got %+v", ds.Spec.Template.Spec.Tolerations)
	}

	if len(ds.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(ds.Spec.Template.Spec.Containers))
	}
	container := ds.Spec.Template.Spec.Containers[0]
	if container.Image != defaultNetworkProbeImage {
		t.Errorf("expected image=%q, got %q", defaultNetworkProbeImage, container.Image)
	}
	if len(container.Args) != 1 || container.Args[0] != testProbeContainer {
		t.Errorf("expected args=[probe], got %+v", container.Args)
	}

	envs := map[string]corev1.EnvVar{}
	for _, e := range container.Env {
		envs[e.Name] = e
	}
	must := func(name, want string) {
		t.Helper()
		got, ok := envs[name]
		if !ok {
			t.Errorf("missing env %q", name)
			return
		}
		if got.Value != want {
			t.Errorf("env %q: expected %q, got %q", name, want, got.Value)
		}
	}
	must("PROBE_CLUSTER_NAME", "alpha")
	must("PROBE_INTERVAL_SECONDS", "60")
	must("PROBE_PROVISION_SUBNET", testProvisionSubnet)
	must("PROBE_PROVISION_HOST", "10.0.0.1")
	must("PROBE_PROVISION_PORT", "80")
	must("PROBE_PROVISION_TIMEOUT", "3s")
	must("PROBE_BMC_SUBNET", testBMCSubnet)

	nn, ok := envs["NODE_NAME"]
	if !ok {
		t.Fatalf("missing NODE_NAME env")
	}
	if nn.ValueFrom == nil || nn.ValueFrom.FieldRef == nil ||
		nn.ValueFrom.FieldRef.FieldPath != "spec.nodeName" {
		t.Errorf("expected NODE_NAME from fieldRef spec.nodeName, got %+v", nn)
	}
}

func TestNetworkProbeReconciler_NoEligibleNodes(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.NetworkProbe = openahamiv1alpha1.NetworkProbeSpec{
		Enabled:          true,
		ProvisionNetwork: &openahamiv1alpha1.NetworkProbeTarget{Subnet: testProvisionSubnet},
		BMCNetwork:       &openahamiv1alpha1.NetworkProbeTarget{Subnet: testBMCSubnet},
	}

	// Pre-create a DaemonSet whose status reports two pods Ready, but no
	// nodes carry the probe-applied labels.
	existingDS := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      alphaProbeDS,
			Namespace: ClusterNamespace(cluster),
		},
		Status: appsv1.DaemonSetStatus{NumberReady: 2},
	}
	plainNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n0"}}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&appsv1.DaemonSet{}).
		WithObjects(existingDS, plainNode).
		Build()

	rec := record.NewFakeRecorder(10)
	r := &NetworkProbeReconciler{Client: c, Recorder: rec}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue when no eligible nodes, got %+v", res)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionNetworkProbeReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonNoEligibleNodes {
		t.Fatalf("expected NetworkProbeReady=False/NoEligibleNodes, got %+v", cond)
	}

	// Verify a Warning Event was recorded with the expected reason.
	select {
	case ev := <-rec.Events:
		if ev == "" {
			t.Errorf("expected non-empty event")
		}
	default:
		t.Errorf("expected an Event to be recorded")
	}

	if cluster.Status.NetworkProbe == nil {
		t.Fatalf("expected status.networkProbe to be set")
	}
	if cluster.Status.NetworkProbe.ProbeReady {
		t.Errorf("expected probeReady=false, got true")
	}
}

func TestNetworkProbeReconciler_PopulatesProbeStatus(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.NetworkProbe = openahamiv1alpha1.NetworkProbeSpec{
		Enabled:          true,
		ProvisionNetwork: &openahamiv1alpha1.NetworkProbeTarget{Subnet: testProvisionSubnet},
		BMCNetwork:       &openahamiv1alpha1.NetworkProbeTarget{Subnet: testBMCSubnet},
	}

	existingDS := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      alphaProbeDS,
			Namespace: ClusterNamespace(cluster),
		},
		Status: appsv1.DaemonSetStatus{NumberReady: 1},
	}

	provLabel := fmt.Sprintf(probeNetworkReadyLabelFmt, "alpha", probeTypeProvision)
	bmcLabel := fmt.Sprintf(probeNetworkReadyLabelFmt, "alpha", probeTypeBMC)
	eligible := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNodeAName,
			Labels: map[string]string{
				provLabel: testProbeLabelTrue,
				bmcLabel:  testProbeLabelTrue,
			},
		},
	}
	provisionOnly := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-b",
			Labels: map[string]string{provLabel: testProbeLabelTrue},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&appsv1.DaemonSet{}).
		WithObjects(existingDS, eligible, provisionOnly).
		Build()

	r := &NetworkProbeReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when probe ready, got %+v", res)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionNetworkProbeReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected NetworkProbeReady=True, got %+v", cond)
	}

	if cluster.Status.NetworkProbe == nil {
		t.Fatalf("expected status.networkProbe to be set")
	}
	if !cluster.Status.NetworkProbe.ProbeReady {
		t.Errorf("expected probeReady=true")
	}
	want := map[string]bool{testNodeAName: true, "node-b": true}
	got := map[string]bool{}
	for _, n := range cluster.Status.NetworkProbe.NodesWithProvisionAccess {
		got[n] = true
	}
	for n := range want {
		if !got[n] {
			t.Errorf("expected node %q in NodesWithProvisionAccess, got %+v",
				n, cluster.Status.NetworkProbe.NodesWithProvisionAccess)
		}
	}
	if len(cluster.Status.NetworkProbe.NodesWithBMCAccess) != 1 ||
		cluster.Status.NetworkProbe.NodesWithBMCAccess[0] != testNodeAName {
		t.Errorf("expected NodesWithBMCAccess=[node-a], got %+v",
			cluster.Status.NetworkProbe.NodesWithBMCAccess)
	}
}
