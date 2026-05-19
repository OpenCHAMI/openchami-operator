// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"fmt"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
)

func TestCoreDHCPReconciler_DisabledSkips(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.CoreDHCP.Enabled = false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &CoreDHCPReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when disabled, got %+v", res)
	}

	ds := &appsv1.DaemonSet{}
	getErr := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      ServiceCoreDHCP,
	}, ds)
	if !apierrors.IsNotFound(getErr) {
		t.Errorf("expected coredhcp DaemonSet to be absent when disabled, got err=%v", getErr)
	}
}

func TestCoreDHCPReconciler_WaitsForProbe(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.CoreDHCP.Enabled = true
	cp.Spec.NetworkProbe.Enabled = true
	// Pre-set NetworkProbeReady=False on the cluster.
	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:    conditions.ConditionNetworkProbeReady,
		Status:  metav1.ConditionFalse,
		Reason:  conditions.ReasonProvisioning,
		Message: "probe still spinning up",
	})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	r := &CoreDHCPReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while waiting for probe, got %+v", res)
	}

	ds := &appsv1.DaemonSet{}
	getErr := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      ServiceCoreDHCP,
	}, ds)
	if !apierrors.IsNotFound(getErr) {
		t.Errorf("expected coredhcp DaemonSet to be absent while waiting for probe, got err=%v", getErr)
	}

	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionDHCPReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonWaitingForProbe {
		t.Fatalf("expected DHCPReady=False/WaitingForNetworkProbe, got %+v", cond)
	}
}

func TestCoreDHCPReconciler_AppliesDaemonSet(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.CoreDHCP = openchamiv1alpha1.CoreDHCPSpec{
		Enabled:      true,
		NodeSelector: map[string]string{testNodeRoleKey: testNodeRoleDHCP},
		LeaseRanges: []openchamiv1alpha1.DHCPLeaseRange{{
			Subnet: testProvisionSubnet,
			Start:  testLeaseRangeStartLarge,
			End:    testLeaseRangeEndLarge,
		}},
		UnknownLeaseDuration: "5m",
		KnownLeaseDuration:   "1h",
	}
	cp.Spec.NetworkProbe.Enabled = false

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	r := &CoreDHCPReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ds := &appsv1.DaemonSet{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      ServiceCoreDHCP,
	}, ds); err != nil {
		t.Fatalf("getting coredhcp DaemonSet: %v", err)
	}

	if got := ds.Spec.Template.Spec.NodeSelector; got[testNodeRoleKey] != testNodeRoleDHCP {
		t.Errorf("expected nodeSelector node-role=dhcp, got %+v", got)
	}
	if !ds.Spec.Template.Spec.HostNetwork {
		t.Errorf("expected HostNetwork=true")
	}
	if ds.Spec.Template.Spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
		t.Errorf("expected DNSPolicy=ClusterFirstWithHostNet, got %s", ds.Spec.Template.Spec.DNSPolicy)
	}
	if ds.Spec.Template.Spec.PriorityClassName != testPriorityClass {
		t.Errorf("expected priority=system-node-critical, got %q", ds.Spec.Template.Spec.PriorityClassName)
	}
	if ds.Spec.Template.Spec.ServiceAccountName != ServiceCoreDHCP {
		t.Errorf("expected SA=%q, got %q", ServiceCoreDHCP, ds.Spec.Template.Spec.ServiceAccountName)
	}

	if len(ds.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(ds.Spec.Template.Spec.Containers))
	}
	container := ds.Spec.Template.Spec.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.Capabilities == nil {
		t.Fatalf("expected container securityContext with capabilities")
	}
	if !slices.Contains(container.SecurityContext.Capabilities.Add, "NET_BIND_SERVICE") {
		t.Errorf("expected NET_BIND_SERVICE in caps.add, got %+v",
			container.SecurityContext.Capabilities.Add)
	}
	if !slices.Contains(container.SecurityContext.Capabilities.Drop, "ALL") {
		t.Errorf("expected ALL in caps.drop, got %+v",
			container.SecurityContext.Capabilities.Drop)
	}
	if len(container.Ports) != 1 || container.Ports[0].ContainerPort != 67 {
		t.Errorf("expected single port 67, got %+v", container.Ports)
	}

	envs := map[string]corev1.EnvVar{}
	for _, e := range container.Env {
		envs[e.Name] = e
	}
	if envs["LEASE_RANGES_JSON"].Value == "" || envs["LEASE_RANGES_JSON"].Value == "null" {
		t.Errorf("expected non-empty LEASE_RANGES_JSON, got %q",
			envs["LEASE_RANGES_JSON"].Value)
	}
	if envs["UNKNOWN_LEASE_DURATION"].Value != "5m" {
		t.Errorf("expected UNKNOWN_LEASE_DURATION=5m, got %q",
			envs["UNKNOWN_LEASE_DURATION"].Value)
	}

	if container.Lifecycle == nil || container.Lifecycle.PreStop == nil {
		t.Errorf("expected PreStop lifecycle hook")
	}
}

func TestCoreDHCPReconciler_ReadyWhenAvailable(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.CoreDHCP = openchamiv1alpha1.CoreDHCPSpec{
		Enabled:      true,
		NodeSelector: map[string]string{testNodeRoleKey: testNodeRoleDHCP},
		LeaseRanges: []openchamiv1alpha1.DHCPLeaseRange{{
			Subnet: testProvisionSubnet,
			Start:  testLeaseRangeStartLarge,
			End:    testLeaseRangeEndLarge,
		}},
	}
	cp.Spec.NetworkProbe.Enabled = false

	existingDS := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceCoreDHCP,
			Namespace: ControlPlaneNamespace(cp),
		},
		Status: appsv1.DaemonSetStatus{NumberReady: 1},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cp).
		WithStatusSubresource(&appsv1.DaemonSet{}).
		WithObjects(existingDS).
		Build()
	r := &CoreDHCPReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when ready, got %+v", res)
	}

	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionDHCPReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected DHCPReady=True, got %+v", cond)
	}
}

func TestCoreDHCPReconciler_UsesProbeNodeSelector(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.CoreDHCP.Enabled = true
	cp.Spec.Services.CoreDHCP.LeaseRanges = []openchamiv1alpha1.DHCPLeaseRange{{
		Subnet: testProvisionSubnet,
		Start:  testLeaseRangeStartLarge,
		End:    testLeaseRangeEndLarge,
	}}
	cp.Spec.NetworkProbe.Enabled = true
	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:   conditions.ConditionNetworkProbeReady,
		Status: metav1.ConditionTrue,
		Reason: conditions.ReasonReady,
	})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	r := &CoreDHCPReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ds := &appsv1.DaemonSet{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      ServiceCoreDHCP,
	}, ds); err != nil {
		t.Fatalf("getting coredhcp DaemonSet: %v", err)
	}

	wantKey := fmt.Sprintf(probeNetworkReadyLabelFmt, "alpha", probeTypeProvision)
	got := ds.Spec.Template.Spec.NodeSelector
	if got[wantKey] != testProbeLabelTrue {
		t.Errorf("expected nodeSelector %s=true, got %+v", wantKey, got)
	}
}
