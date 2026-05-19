// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

// Package reconcilers contains sub-reconcilers for each OpenCHAMIControlPlane concern.
package reconcilers

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

// SubReconciler is implemented by each concern-specific reconciler.
//
// Implementation rules (enforced by validate-invariants.sh):
//   - Must be idempotent: calling Reconcile twice has no additional effect.
//   - Must call logging.Enrich() at the top of Reconcile().
//   - Must update the relevant condition on cluster.Status before returning.
//   - Must use server-side apply (client.Apply). Never client.Create then client.Update.
//   - Must wrap errors: fmt.Errorf("reconciling X: %w", err).
//   - Must use helpers.RecordConditionEvent for all Events.
//   - Must not call log.FromContext directly.
//   - Must not call recorder.Event directly.
type SubReconciler interface {
	// Reconcile creates, updates, or deletes Kubernetes resources for this
	// sub-domain. Returns a ctrl.Result instructing the controller when to
	// requeue, and any error that should trigger an immediate requeue.
	Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error)

	// Describe returns the Kubernetes objects this reconciler would apply,
	// in apply order, without actually applying them. Used by
	// `ochami-admin describe`. Must not contact any external service.
	Describe(cp *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error)
}
