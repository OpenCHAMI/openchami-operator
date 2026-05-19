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

// psaLevelRestricted is the PodSecurityAdmission "restricted" level —
// used for warn/audit labels on operator-managed namespaces. Extracted
// from a string literal so it can be reused from tests.
const psaLevelRestricted = "restricted"

// NamespaceReconciler ensures the per-cluster namespace exists with correct labels.
type NamespaceReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

func (r *NamespaceReconciler) Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "namespace")

	ns := r.buildNamespace(cp)
	log = logging.EnrichWithResource(log, "Namespace", ns.Name)
	log.Info("reconciling namespace")

	if err := r.Client.Patch(ctx, ns, client.Apply, //nolint:staticcheck // SSA via Patch is the supported pattern; new client.Apply API requires runtime.ApplyConfiguration
		client.ForceOwnership, client.FieldOwner("openchami-operator")); err != nil {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionNamespaceReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonError,
			Message:            fmt.Sprintf("failed to reconcile namespace: %v", err),
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{}, fmt.Errorf("reconciling namespace: %w", err)
	}

	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionNamespaceReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            "Namespace exists with correct labels",
		ObservedGeneration: cp.Generation,
	})
	cp.Status.Namespace = ns.Name
	return ctrl.Result{}, nil
}

func (r *NamespaceReconciler) Describe(cp *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
	return []client.Object{r.buildNamespace(cp)}, nil
}

func (r *NamespaceReconciler) buildNamespace(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *corev1.Namespace {
	name := ControlPlaneNamespace(cp)
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Namespace",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				kubernetesMetadataNameLabel: name,
				"openchami.org/cluster":     cp.Spec.ClusterName,
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
				"pod-security.kubernetes.io/warn":    psaLevelRestricted,
				"pod-security.kubernetes.io/audit":   psaLevelRestricted,
			},
		},
	}
}
