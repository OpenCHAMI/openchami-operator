/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package reconcilers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openahamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/logging"
)

// NamespaceReconciler ensures the per-cluster namespace exists with correct labels.
type NamespaceReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

func (r *NamespaceReconciler) Reconcile(ctx context.Context, cluster *openahamiv1alpha1.OpenCHAMICluster) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cluster, "namespace")

	ns := r.buildNamespace(cluster)
	log = logging.EnrichWithResource(log, "Namespace", ns.Name)
	log.Info("reconciling namespace")

	if err := r.Client.Patch(ctx, ns, client.Apply, //nolint:staticcheck // SSA via Patch is the supported pattern; new client.Apply API requires runtime.ApplyConfiguration
		client.ForceOwnership, client.FieldOwner("openchami-operator")); err != nil {
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionNamespaceReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonError,
			Message:            fmt.Sprintf("failed to reconcile namespace: %v", err),
			ObservedGeneration: cluster.Generation,
		})
		return ctrl.Result{}, fmt.Errorf("reconciling namespace: %w", err)
	}

	apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionNamespaceReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            "Namespace exists with correct labels",
		ObservedGeneration: cluster.Generation,
	})
	cluster.Status.Namespace = ns.Name
	return ctrl.Result{}, nil
}

func (r *NamespaceReconciler) Describe(cluster *openahamiv1alpha1.OpenCHAMICluster) ([]client.Object, error) {
	return []client.Object{r.buildNamespace(cluster)}, nil
}

func (r *NamespaceReconciler) buildNamespace(cluster *openahamiv1alpha1.OpenCHAMICluster) *corev1.Namespace {
	name := ClusterNamespace(cluster)
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Namespace",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"kubernetes.io/metadata.name":        name,
				"openchami.org/cluster":              cluster.Spec.ClusterName,
				"pod-security.kubernetes.io/enforce": "restricted",
			},
		},
	}
}
