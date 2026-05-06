// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package status

import (
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
)

// Metric label constants. Centralised so tests and updaters reference the
// same strings.
const (
	labelCluster    = "cluster"
	labelService    = "service"
	labelReconciler = "reconciler"
	labelPhase      = "phase"
)

// allPhases enumerates every ClusterPhase the reporter writes. The phase
// gauge sets the current phase to 1.0 and every other phase to 0.0 on each
// reconcile so dashboards never observe stale 1.0 values from prior phases.
var allPhases = []openchamiv1alpha1.ClusterPhase{
	openchamiv1alpha1.PhaseProvisioning,
	openchamiv1alpha1.PhaseReady,
	openchamiv1alpha1.PhaseDegraded,
	openchamiv1alpha1.PhaseDeleting,
	openchamiv1alpha1.PhaseFailed,
}

// Metric definitions. Registered against controller-runtime's metrics
// registry so they are exposed on the existing /metrics endpoint without
// touching the global default registerer (which is not the one
// controller-runtime serves).
var (
	clusterReady = promauto.With(metrics.Registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "openchami_cluster_ready",
			Help: "1 when the cluster phase is Ready, 0 otherwise.",
		},
		[]string{labelCluster},
	)

	clusterPhase = promauto.With(metrics.Registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "openchami_cluster_phase",
			Help: "1 for the cluster's current phase, 0 for every other phase.",
		},
		[]string{labelCluster, labelPhase},
	)

	clusterServiceReady = promauto.With(metrics.Registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "openchami_cluster_service_ready",
			Help: "1 when the named service is Ready, 0 otherwise.",
		},
		[]string{labelCluster, labelService},
	)

	clusterVaultReachable = promauto.With(metrics.Registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "openchami_cluster_vault_reachable",
			Help: "1 when ConditionVaultConfigured=True, 0 otherwise.",
		},
		[]string{labelCluster},
	)

	clusterCertExpirySeconds = promauto.With(metrics.Registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "openchami_cluster_cert_expiry_seconds",
			Help: "Unix epoch seconds of the soonest-expiring TLS certificate; 0 when unknown.",
		},
		[]string{labelCluster},
	)

	clusterReconcileDuration = promauto.With(metrics.Registry).NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "openchami_cluster_reconcile_duration_seconds",
			Help:    "Duration of each sub-reconciler invocation, in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{labelCluster, labelReconciler},
	)

	clusterDHCPNodes = promauto.With(metrics.Registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "openchami_cluster_dhcp_nodes",
			Help: "Number of nodes eligible for CoreDHCP scheduling (network probe provision-passed).",
		},
		[]string{labelCluster},
	)

	clusterProbeNodesProvision = promauto.With(metrics.Registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "openchami_cluster_probe_nodes_provision",
			Help: "Number of nodes that pass the provision-network probe.",
		},
		[]string{labelCluster},
	)

	clusterProbeNodesBMC = promauto.With(metrics.Registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "openchami_cluster_probe_nodes_bmc",
			Help: "Number of nodes that pass the BMC-network probe.",
		},
		[]string{labelCluster},
	)
)

// UpdateMetrics refreshes every operator-owned gauge for the given cluster.
// Call after ComputeAndSetPhase so the phase gauge matches the just-decided
// phase.
//
// The reconcile-duration histogram is updated separately by ObserveReconcile;
// histograms accumulate observations and are not "set" each pass.
func (r *Reporter) UpdateMetrics(cluster *openchamiv1alpha1.OpenCHAMICluster) {
	name := cluster.Spec.ClusterName

	// Ready gauge.
	if cluster.Status.Phase == openchamiv1alpha1.PhaseReady {
		clusterReady.WithLabelValues(name).Set(1)
	} else {
		clusterReady.WithLabelValues(name).Set(0)
	}

	// Phase gauge: 1 for the current phase, 0 for the rest. Setting the
	// inactive phases explicitly avoids stale 1.0 readings across phase
	// transitions.
	for _, p := range allPhases {
		v := 0.0
		if cluster.Status.Phase == p {
			v = 1.0
		}
		clusterPhase.WithLabelValues(name, string(p)).Set(v)
	}

	// Per-service readiness.
	for svc, st := range cluster.Status.Services {
		v := 0.0
		if st.Ready {
			v = 1.0
		}
		clusterServiceReady.WithLabelValues(name, svc).Set(v)
	}

	// Vault reachable: derived from ConditionVaultConfigured=True.
	vaultV := 0.0
	if apimeta.IsStatusConditionPresentAndEqual(
		cluster.Status.Conditions,
		conditions.ConditionVaultConfigured,
		metav1.ConditionTrue,
	) {
		vaultV = 1.0
	}
	clusterVaultReachable.WithLabelValues(name).Set(vaultV)

	// Certificate expiry: parse status.CertExpiryTime (RFC3339); 0 when
	// missing or unparseable so dashboards can detect the absence.
	expiry := 0.0
	if cluster.Status.CertExpiryTime != "" {
		if t, err := time.Parse(time.RFC3339, cluster.Status.CertExpiryTime); err == nil {
			expiry = float64(t.Unix())
		}
	}
	clusterCertExpirySeconds.WithLabelValues(name).Set(expiry)

	// Network-probe nodes. DHCP currently piggybacks on the provision-probe
	// set: the CoreDHCP DaemonSet's effective node selector is the
	// provision-network-ready label when probing is enabled, so the count of
	// provision-passed nodes is the right proxy for "DHCP nodes".
	provision := 0
	bmc := 0
	if cluster.Status.NetworkProbe != nil {
		provision = len(cluster.Status.NetworkProbe.NodesWithProvisionAccess)
		bmc = len(cluster.Status.NetworkProbe.NodesWithBMCAccess)
	}
	clusterDHCPNodes.WithLabelValues(name).Set(float64(provision))
	clusterProbeNodesProvision.WithLabelValues(name).Set(float64(provision))
	clusterProbeNodesBMC.WithLabelValues(name).Set(float64(bmc))
}

// ObserveReconcile records a single sub-reconciler invocation duration.
// Called by the controller around each sub.Reconcile call so we get a
// per-reconciler latency histogram without each sub-reconciler having to
// import this package.
func ObserveReconcile(clusterName, reconciler string, duration time.Duration) {
	clusterReconcileDuration.
		WithLabelValues(clusterName, reconciler).
		Observe(duration.Seconds())
}
