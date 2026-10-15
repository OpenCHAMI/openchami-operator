// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
)

// legacyMagellanCronJob represents the no-command workload shipped before #44.
func legacyMagellanCronJob(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name: ServiceMagellan, Namespace: ControlPlaneNamespace(cp),
			Labels: map[string]string{
				labelManagedBy: managedByValue, labelAppName: ServiceMagellan,
				labelAppInst: "openchami-" + cp.Spec.ClusterName,
			},
			Annotations: map[string]string{"openchami.org/probe-original-suspend": "false"},
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "*/30 * * * *", ConcurrencyPolicy: batchv1.ForbidConcurrent,
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: ServiceMagellan, RestartPolicy: corev1.RestartPolicyOnFailure,
					NodeSelector: map[string]string{"legacy-bmc-node": probeLabelValueTrue},
					Containers:   []corev1.Container{{Name: ServiceMagellan, Image: "ghcr.io/openchami/magellan:v0.5.1"}},
				},
			}}},
		},
	}
}

func assertMagellanCondition(t *testing.T, cp *openchamiv1alpha1.OpenCHAMIControlPlane, result ctrl.Result) {
	t.Helper()
	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionMagellanReady)
	status, reason := metav1.ConditionTrue, conditions.ReasonReady
	if cp.Spec.Services.Magellan.Enabled {
		status, reason = metav1.ConditionFalse, conditions.ReasonNotConfigured
	}
	if cond == nil || cond.Status != status || cond.Reason != reason || cond.ObservedGeneration != cp.Generation {
		t.Fatalf("expected MagellanReady=%s/%s at generation %d, got %+v", status, reason, cp.Generation, cond)
	}
	if result.RequeueAfter != magellanRequeueAfter {
		t.Fatalf("expected retry to enforce suspension, got %+v", result)
	}
}

func TestMagellanReconciler_DoesNotCreateCronJob(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, probeReady := range []bool{false, true} {
			t.Run(fmt.Sprintf("enabled=%v/probeReady=%v", enabled, probeReady), func(t *testing.T) {
				cp := newControlPlane("alpha")
				cp.Generation = 3
				cp.Spec.Services.Magellan.Enabled = enabled
				cp.Spec.NetworkProbe.Enabled = true
				probeStatus := metav1.ConditionFalse
				if probeReady {
					probeStatus = metav1.ConditionTrue
				}
				apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
					Type: conditions.ConditionNetworkProbeReady, Status: probeStatus,
					Reason: conditions.ReasonReady, ObservedGeneration: cp.Generation,
				})
				c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
				r := &MagellanReconciler{Client: c}
				for range 2 {
					result, err := r.Reconcile(context.Background(), cp)
					if err != nil {
						t.Fatal(err)
					}
					assertMagellanCondition(t, cp, result)
				}
				err := c.Get(context.Background(), types.NamespacedName{
					Name: ServiceMagellan, Namespace: ControlPlaneNamespace(cp),
				}, &batchv1.CronJob{})
				if !apierrors.IsNotFound(err) {
					t.Fatalf("expected no CronJob, got %v", err)
				}
				objects, err := r.Describe(cp)
				if err != nil || objects == nil || len(objects) != 0 {
					t.Fatalf("Describe must return no new resources: %v, %v", objects, err)
				}
			})
		}
	}
}

func TestMagellanReconciler_SuspendsLegacyCronJob(t *testing.T) {
	for _, initialSuspend := range []*bool{nil, new(false), new(true)} {
		t.Run(fmt.Sprintf("suspend=%v", initialSuspend), func(t *testing.T) {
			ctx := context.Background()
			cp := newControlPlane("alpha")
			cp.Spec.Services.Magellan.Enabled = true
			cp.Spec.NetworkProbe.Enabled = true
			existing := legacyMagellanCronJob(cp)
			existing.Spec.Suspend = initialSuspend
			other := legacyMagellanCronJob(newControlPlane("beta"))
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "magellan-previous", Namespace: existing.Namespace},
				Spec:       existing.Spec.JobTemplate.Spec,
				Status:     batchv1.JobStatus{Succeeded: 1},
			}
			// The fake client converts a partial CronJob apply to a typed
			// object and injects an empty required schedule. Check the actual
			// apply payload, then emulate its merge here. The controller
			// envtest suite verifies preservation against a real API server.
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(existing, other, job).
				WithInterceptorFuncs(interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						data, err := patch.Data(obj)
						if err != nil {
							return err
						}
						var applied map[string]any
						if err := json.Unmarshal(data, &applied); err != nil {
							return err
						}
						if patch.Type() != types.ApplyPatchType || !reflect.DeepEqual(applied["spec"], map[string]any{"suspend": true}) {
							t.Fatalf("expected apply of suspend only, got %s", data)
						}
						return c.Patch(ctx, obj, client.RawPatch(types.MergePatchType, data))
					},
				}).Build()
			r := &MagellanReconciler{Client: c}
			key := client.ObjectKeyFromObject(existing)
			// Probe recovery, disabling probing, disabling Magellan, and re-enabling
			// it must all keep the old no-op schedule suspended.
			for step := range 5 {
				cp.Generation++
				if step == 1 {
					apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
						Type: conditions.ConditionNetworkProbeReady, Status: metav1.ConditionTrue,
						Reason: conditions.ReasonReady, ObservedGeneration: cp.Generation,
					})
				}
				if step == 2 {
					cp.Spec.NetworkProbe.Enabled = false
				}
				cp.Spec.Services.Magellan.Enabled = step != 3
				for range 2 {
					result, err := r.Reconcile(ctx, cp)
					if err != nil {
						t.Fatal(err)
					}
					assertMagellanCondition(t, cp, result)
					got := &batchv1.CronJob{}
					if err := c.Get(ctx, key, got); err != nil {
						t.Fatal(err)
					}
					wantSpec := existing.Spec.DeepCopy()
					wantSpec.Suspend = new(true)
					if !reflect.DeepEqual(got.Spec, *wantSpec) || !reflect.DeepEqual(got.Annotations, existing.Annotations) {
						t.Fatal("suspension must preserve the legacy template, schedule, and annotations")
					}
				}
			}
			gotJob := &batchv1.Job{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(job), gotJob); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotJob.Spec, job.Spec) || !reflect.DeepEqual(gotJob.Status, job.Status) {
				t.Fatal("existing Job changed")
			}
			gotOther := &batchv1.CronJob{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(other), gotOther); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotOther.Spec, other.Spec) {
				t.Fatal("another cluster's CronJob changed")
			}
		})
	}
}

func TestMagellanReconciler_ReportsSuspensionFailures(t *testing.T) {
	for _, operation := range []string{"read", "apply"} {
		t.Run(operation, func(t *testing.T) {
			cp := newControlPlane("alpha")
			cp.Spec.Services.Magellan.Enabled = true
			wantErr := errors.New("access denied")
			hooks := interceptor.Funcs{}
			if operation == "read" {
				hooks.Get = func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return wantErr
				}
			} else {
				hooks.Patch = func(_ context.Context, _ client.WithWatch, _ client.Object, patch client.Patch, opts ...client.PatchOption) error {
					options := &client.PatchOptions{}
					for _, opt := range opts {
						opt.ApplyToPatch(options)
					}
					if patch.Type() != types.ApplyPatchType || options.FieldManager == fieldManager || options.FieldManager == probeGateFieldManager {
						t.Fatal("suspension must use SSA with its own field manager to preserve the legacy workload")
					}
					return wantErr
				}
			}
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(legacyMagellanCronJob(cp)).WithInterceptorFuncs(hooks).Build()
			r := &MagellanReconciler{Client: c}
			_, err := r.Reconcile(context.Background(), cp)
			if !errors.Is(err, wantErr) {
				t.Fatalf("expected wrapped failure, got %v", err)
			}
			cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionMagellanReady)
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonError {
				t.Fatalf("expected failure condition, got %+v", cond)
			}
		})
	}
}

func TestMagellanReconciler_RefusesUnmanagedCronJob(t *testing.T) {
	cp := newControlPlane("alpha")
	existing := legacyMagellanCronJob(cp)
	delete(existing.Labels, labelManagedBy)
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(existing).Build()
	r := &MagellanReconciler{Client: c}
	if _, err := r.Reconcile(context.Background(), cp); err == nil {
		t.Fatal("expected refusal to change an unmanaged CronJob")
	}
	got := &batchv1.CronJob{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(existing), got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Suspend != nil {
		t.Fatal("unmanaged CronJob was suspended")
	}
}
