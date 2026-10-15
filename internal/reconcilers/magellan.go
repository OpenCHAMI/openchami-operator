// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/logging"
)

const magellanRequeueAfter = 30 * time.Second

// MagellanReconciler prevents no-op discovery Jobs until the command,
// BMC credentials, and SMD authentication are wired end to end.
// Inventory processing belongs to Magellan and SMD (invariant #10).
type MagellanReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

// Reconcile suspends legacy schedules and reports discovery as unavailable.
// Existing Jobs are retained for diagnosis. Probe recovery must not resume
// schedules while functional discovery remains unimplemented.
func (r *MagellanReconciler) Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.EnrichWithResource(logging.Enrich(ctx, cp, "magellan"), kindCronJob, ServiceMagellan)
	if err := r.suspendExistingCronJob(ctx, cp); err != nil {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionMagellanReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonError,
			Message:            err.Error(),
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{}, err
	}

	condition := metav1.Condition{
		Type:               conditions.ConditionMagellanReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            "magellan disabled; existing schedules suspended",
		ObservedGeneration: cp.Generation,
	}
	if cp.Spec.Services.Magellan.Enabled {
		condition.Status = metav1.ConditionFalse
		condition.Reason = conditions.ReasonNotConfigured
		condition.Message = "magellan discovery is not implemented: command pipeline, BMC credentials, and SMD authentication are not wired; scheduling suspended"
		log.Info("magellan discovery unavailable; scheduling suspended")
	} else {
		log.Info("magellan disabled; scheduling suspended")
	}
	apimeta.SetStatusCondition(&cp.Status.Conditions, condition)
	return ctrl.Result{RequeueAfter: magellanRequeueAfter}, nil
}

// suspendExistingCronJob changes only suspend, using a dedicated field manager
// so omitting the legacy workload fields does not prune them during SSA.
func (r *MagellanReconciler) suspendExistingCronJob(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) error {
	current := &batchv1.CronJob{}
	key := types.NamespacedName{Namespace: ControlPlaneNamespace(cp), Name: ServiceMagellan}
	if err := r.Client.Get(ctx, key, current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("reading existing magellan CronJob: %w", err)
	}
	if current.Labels[labelManagedBy] != managedByValue ||
		current.Labels[labelAppInst] != "openchami-"+cp.Spec.ClusterName ||
		current.Labels[labelAppName] != ServiceMagellan {
		return fmt.Errorf("refusing to suspend magellan CronJob %s without matching operator and cluster labels", key)
	}
	patch := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": batchAPIVersion,
		"kind":       kindCronJob,
		"metadata":   map[string]any{"name": key.Name, "namespace": key.Namespace},
		"spec":       map[string]any{"suspend": true},
	}}
	if err := r.Client.Patch(ctx, patch, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager+"-magellan-suspension")); err != nil {
		return fmt.Errorf("suspending existing magellan CronJob: %w", err)
	}
	return nil
}

// Describe returns no new resources while discovery is unavailable. Legacy
// CronJobs are suspended by Reconcile; Describe cannot inspect live resources.
func (r *MagellanReconciler) Describe(_ *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
	return []client.Object{}, nil
}
