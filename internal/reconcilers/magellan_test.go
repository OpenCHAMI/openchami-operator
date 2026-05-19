// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"fmt"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
)

func TestMagellanReconciler_DisabledSkips(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.Magellan.Enabled = false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &MagellanReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when disabled, got %+v", res)
	}

	cj := &batchv1.CronJob{}
	getErr := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      ServiceMagellan,
	}, cj)
	if !apierrors.IsNotFound(getErr) {
		t.Errorf("expected magellan CronJob to be absent when disabled, got err=%v", getErr)
	}
}

func TestMagellanReconciler_WaitsForProbe(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.Magellan.Enabled = true
	cp.Spec.NetworkProbe.Enabled = true
	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:    conditions.ConditionNetworkProbeReady,
		Status:  metav1.ConditionFalse,
		Reason:  conditions.ReasonProvisioning,
		Message: "probe still spinning up",
	})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	r := &MagellanReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while waiting for probe, got %+v", res)
	}

	cj := &batchv1.CronJob{}
	getErr := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      ServiceMagellan,
	}, cj)
	if !apierrors.IsNotFound(getErr) {
		t.Errorf("expected magellan CronJob to be absent while waiting for probe, got err=%v", getErr)
	}

	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionMagellanReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonWaitingForProbe {
		t.Fatalf("expected MagellanReady=False/WaitingForNetworkProbe, got %+v", cond)
	}
}

func TestMagellanReconciler_AppliesCronJob(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.NetworkProbe.Enabled = false
	cp.Spec.Services.Magellan = openchamiv1alpha1.MagellanSpec{
		Enabled:           true,
		NodeSelector:      map[string]string{testNodeRoleKey: testNodeRoleBMC},
		Schedule:          "*/15 * * * *",
		BMCSubnet:         testBMCSubnet,
		ConcurrencyPolicy: batchv1.ForbidConcurrent,
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	r := &MagellanReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cj := &batchv1.CronJob{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      ServiceMagellan,
	}, cj); err != nil {
		t.Fatalf("getting magellan CronJob: %v", err)
	}

	if cj.Spec.Schedule != "*/15 * * * *" {
		t.Errorf("expected schedule=*/15 * * * *, got %q", cj.Spec.Schedule)
	}
	if cj.Spec.ConcurrencyPolicy != batchv1.ForbidConcurrent {
		t.Errorf("expected ConcurrencyPolicy=Forbid, got %q", cj.Spec.ConcurrencyPolicy)
	}

	pod := cj.Spec.JobTemplate.Spec.Template.Spec
	if pod.ServiceAccountName != ServiceMagellan {
		t.Errorf("expected SA=%q, got %q", ServiceMagellan, pod.ServiceAccountName)
	}
	if pod.NodeSelector[testNodeRoleKey] != testNodeRoleBMC {
		t.Errorf("expected nodeSelector node-role=bmc, got %+v", pod.NodeSelector)
	}
	if len(pod.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(pod.Containers))
	}
	var foundSubnet bool
	for _, e := range pod.Containers[0].Env {
		if e.Name == "MAGELLAN_BMC_SUBNET" {
			foundSubnet = true
			if e.Value != testBMCSubnet {
				t.Errorf("expected MAGELLAN_BMC_SUBNET=%q, got %q", testBMCSubnet, e.Value)
			}
		}
	}
	if !foundSubnet {
		t.Errorf("expected MAGELLAN_BMC_SUBNET env var")
	}
}

func TestMagellanReconciler_ReadyOnceCreated(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.NetworkProbe.Enabled = false
	cp.Spec.Services.Magellan = openchamiv1alpha1.MagellanSpec{
		Enabled:           true,
		NodeSelector:      map[string]string{testNodeRoleKey: testNodeRoleBMC},
		Schedule:          "*/30 * * * *",
		BMCSubnet:         testBMCSubnet,
		ConcurrencyPolicy: batchv1.ForbidConcurrent,
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	r := &MagellanReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %+v", res)
	}

	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionMagellanReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected MagellanReady=True, got %+v", cond)
	}
}

func TestMagellanReconciler_UsesProbeNodeSelector(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.Magellan.Enabled = true
	cp.Spec.Services.Magellan.Schedule = "*/30 * * * *"
	cp.Spec.Services.Magellan.ConcurrencyPolicy = batchv1.ForbidConcurrent
	cp.Spec.NetworkProbe.Enabled = true
	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:   conditions.ConditionNetworkProbeReady,
		Status: metav1.ConditionTrue,
		Reason: conditions.ReasonReady,
	})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	r := &MagellanReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cj := &batchv1.CronJob{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      ServiceMagellan,
	}, cj); err != nil {
		t.Fatalf("getting magellan CronJob: %v", err)
	}

	wantKey := fmt.Sprintf(probeNetworkReadyLabelFmt, "alpha", probeTypeBMC)
	got := cj.Spec.JobTemplate.Spec.Template.Spec.NodeSelector
	if got[wantKey] != testProbeLabelTrue {
		t.Errorf("expected nodeSelector %s=true, got %+v", wantKey, got)
	}
}
