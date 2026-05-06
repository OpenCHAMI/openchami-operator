// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"fmt"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/logging"
)

const (
	serviceMonitorAPIVersion = "monitoring.coreos.com/v1"
	kindServiceMonitor       = "ServiceMonitor"

	// labelAppPartOf is the part-of label used by the ServiceMonitor
	// selector to scrape every operator-managed service across the
	// cluster's namespace.
	labelAppPartOf      = "app.kubernetes.io/part-of"
	labelAppPartOfValue = "openchami"

	// serviceMonitorPort is the named port the operator's services expose
	// metrics on. Mirrors smdPortName so a single ServiceMonitor scrapes
	// every service without per-service endpoints.
	serviceMonitorPort     = "http"
	serviceMonitorPath     = "/metrics"
	serviceMonitorInterval = "30s"
)

// ServiceMonitorReconciler ensures a Prometheus ServiceMonitor exists for the
// cluster's services when prometheus-operator integration is enabled.
//
// The reconciler is intentionally a no-op when
// spec.observability.prometheusOperator is false: an existing ServiceMonitor
// is left in place so cluster operators can disable the feature without
// losing scrape configuration mid-flight. Production deletion is an explicit
// admin action via kubectl.
type ServiceMonitorReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

// Reconcile applies a single ServiceMonitor selecting every operator-managed
// service in the cluster's namespace.
func (r *ServiceMonitorReconciler) Reconcile(ctx context.Context, cluster *openchamiv1alpha1.OpenCHAMICluster) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cluster, "servicemonitor")

	if !cluster.Spec.Observability.PrometheusOperator {
		log.Info("prometheus-operator integration disabled, skipping (existing ServiceMonitor preserved)")
		return ctrl.Result{}, nil
	}

	sm := r.buildServiceMonitor(cluster)
	smLog := logging.EnrichWithResource(log, kindServiceMonitor, sm.Name)
	smLog.Info("applying ServiceMonitor")
	if err := r.Client.Patch(ctx, sm, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying ServiceMonitor: %w", err)
	}
	return ctrl.Result{}, nil
}

// Describe returns the ServiceMonitor this reconciler would apply, or an
// empty slice when prometheus-operator integration is disabled.
func (r *ServiceMonitorReconciler) Describe(cluster *openchamiv1alpha1.OpenCHAMICluster) ([]client.Object, error) {
	if !cluster.Spec.Observability.PrometheusOperator {
		return []client.Object{}, nil
	}
	return []client.Object{r.buildServiceMonitor(cluster)}, nil
}

// buildServiceMonitor returns the operator-managed ServiceMonitor object for
// the cluster. Selector labels are scoped to this cluster's instance so
// neighbouring clusters in the same Prometheus do not cross-scrape.
func (r *ServiceMonitorReconciler) buildServiceMonitor(cluster *openchamiv1alpha1.OpenCHAMICluster) *monitoringv1.ServiceMonitor {
	return &monitoringv1.ServiceMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: serviceMonitorAPIVersion, Kind: kindServiceMonitor},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openchami-" + cluster.Spec.ClusterName + "-services",
			Namespace: ClusterNamespace(cluster),
			Labels: map[string]string{
				labelAppPartOf: labelAppPartOfValue,
				labelAppInst:   "openchami-" + cluster.Spec.ClusterName,
				labelManagedBy: managedByValue,
			},
		},
		Spec: monitoringv1.ServiceMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					labelAppPartOf: labelAppPartOfValue,
					labelAppInst:   "openchami-" + cluster.Spec.ClusterName,
				},
			},
			Endpoints: []monitoringv1.Endpoint{
				{
					Port:     serviceMonitorPort,
					Path:     serviceMonitorPath,
					Interval: monitoringv1.Duration(serviceMonitorInterval),
				},
			},
		},
	}
}
