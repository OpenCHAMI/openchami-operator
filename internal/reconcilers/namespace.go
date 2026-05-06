// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

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

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/logging"
)

// NamespaceReconciler ensures the per-cluster namespace exists with correct labels.
type NamespaceReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

func (r *NamespaceReconciler) Reconcile(ctx context.Context, cluster *openchamiv1alpha1.OpenCHAMICluster) (ctrl.Result, error) {
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

func (r *NamespaceReconciler) Describe(cluster *openchamiv1alpha1.OpenCHAMICluster) ([]client.Object, error) {
	return []client.Object{r.buildNamespace(cluster)}, nil
}

func (r *NamespaceReconciler) buildNamespace(cluster *openchamiv1alpha1.OpenCHAMICluster) *corev1.Namespace {
	name := ClusterNamespace(cluster)
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Namespace",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				kubernetesMetadataNameLabel: name,
				"openchami.org/cluster":     cluster.Spec.ClusterName,
				// PSA enforce=privileged because the namespace hosts three
				// workloads that PSA `baseline` (the next-stricter floor)
				// rejects:
				//   * coredhcp DaemonSet — hostNetwork=true, hostPort 67
				//     (DHCP is fundamentally a host-network protocol)
				//   * funicular-collector DaemonSet — hostPath mount of
				//     /var/log/pods (log collection)
				//   * network-probe DaemonSet — hostNetwork=true
				// `baseline` forbids host namespaces, hostPath, and host
				// ports outright; there is no PSA level between baseline
				// and privileged that permits any of them. Per-pod
				// admission (Kyverno, OPA Gatekeeper, ValidatingAdmission-
				// Policy) is the right tool for fine-grained constraints
				// inside a privileged namespace; PSA itself cannot express
				// "this pod yes, that pod no" without splitting namespaces.
				// warn/audit stay at restricted so the API server still
				// surfaces pods that drift away from a restricted-fit
				// profile (smd/tokensmith/boot-service/metadata-service
				// all fit restricted today).
				"pod-security.kubernetes.io/enforce": "privileged",
				"pod-security.kubernetes.io/warn":    "restricted",
				"pod-security.kubernetes.io/audit":   "restricted",
			},
		},
	}
}
