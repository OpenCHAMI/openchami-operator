// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package status

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/reconcilers"
)

// metricsCluster builds a happy-path cluster with phase=Ready, every
// condition True, every service Ready, a NetworkProbe status with non-zero
// node counts, and a known cert expiry. Each metric assertion in this file
// touches a different aspect of the resulting state.
func metricsCluster(name string) *openchamiv1alpha1.OpenCHAMIControlPlane {
	c := newTestCluster(name)
	addAllConditionsTrue(c)
	allServicesReady(c)
	c.Status.Phase = openchamiv1alpha1.PhaseReady
	c.Status.NetworkProbe = &openchamiv1alpha1.NetworkProbeStatus{
		NodesWithProvisionAccess: []string{"node-a", "node-b", "node-c"},
		NodesWithBMCAccess:       []string{"node-a"},
		ProbeReady:               true,
	}
	return c
}

func TestMetrics_UpdateMetrics_HappyPath(t *testing.T) {
	c := metricsCluster("metric-happy")
	// Known cert expiry so cert-expiry-seconds has a deterministic value.
	expiry := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	c.Status.CertExpiryTime = expiry.Format(time.RFC3339)

	r := &Reporter{Recorder: record.NewFakeRecorder(2)}
	r.UpdateMetrics(c)

	if got := testutil.ToFloat64(clusterReady.WithLabelValues("metric-happy")); got != 1 {
		t.Errorf("openchami_cluster_ready: want 1, got %v", got)
	}
	if got := testutil.ToFloat64(clusterVaultReachable.WithLabelValues("metric-happy")); got != 1 {
		t.Errorf("openchami_cluster_vault_reachable: want 1, got %v", got)
	}
	if got := testutil.ToFloat64(clusterDHCPNodes.WithLabelValues("metric-happy")); got != 3 {
		t.Errorf("openchami_cluster_dhcp_nodes: want 3, got %v", got)
	}
	if got := testutil.ToFloat64(clusterProbeNodesProvision.WithLabelValues("metric-happy")); got != 3 {
		t.Errorf("openchami_cluster_probe_nodes_provision: want 3, got %v", got)
	}
	if got := testutil.ToFloat64(clusterProbeNodesBMC.WithLabelValues("metric-happy")); got != 1 {
		t.Errorf("openchami_cluster_probe_nodes_bmc: want 1, got %v", got)
	}
	for _, svc := range []string{
		reconcilers.ServiceSMD,
		reconcilers.ServiceTokensmith,
		reconcilers.ServiceBootService,
		reconcilers.ServiceMetadataService,
	} {
		if got := testutil.ToFloat64(clusterServiceReady.WithLabelValues("metric-happy", svc)); got != 1 {
			t.Errorf("openchami_cluster_service_ready{service=%q}: want 1, got %v", svc, got)
		}
	}
}

func TestMetrics_PhaseGauge_OnlyCurrentIsOne(t *testing.T) {
	c := metricsCluster("metric-phase")
	r := &Reporter{Recorder: record.NewFakeRecorder(2)}
	r.UpdateMetrics(c)

	for _, p := range allPhases {
		want := 0.0
		if p == openchamiv1alpha1.PhaseReady {
			want = 1.0
		}
		if got := testutil.ToFloat64(clusterPhase.WithLabelValues("metric-phase", string(p))); got != want {
			t.Errorf("phase=%q: want %v, got %v", p, want, got)
		}
	}

	// Flip to Degraded; the previous Ready=1 must now read 0 and
	// Degraded must read 1.
	c.Status.Phase = openchamiv1alpha1.PhaseDegraded
	r.UpdateMetrics(c)
	if got := testutil.ToFloat64(clusterPhase.WithLabelValues("metric-phase", string(openchamiv1alpha1.PhaseReady))); got != 0 {
		t.Errorf("after transition: phase=Ready should be 0, got %v", got)
	}
	if got := testutil.ToFloat64(clusterPhase.WithLabelValues("metric-phase", string(openchamiv1alpha1.PhaseDegraded))); got != 1 {
		t.Errorf("after transition: phase=Degraded should be 1, got %v", got)
	}
}

func TestMetrics_ObserveReconcile_RecordsObservation(t *testing.T) {
	// Histograms accumulate across tests; what we check is that an
	// observation is registered for our distinct labels.
	const name = "metric-observe"
	const reconciler = "FakeReconciler"
	before := testutil.CollectAndCount(clusterReconcileDuration)
	ObserveReconcile(name, reconciler, 25*time.Millisecond)
	after := testutil.CollectAndCount(clusterReconcileDuration)
	if after <= before {
		t.Errorf("expected histogram to accumulate at least one new series; before=%d after=%d", before, after)
	}
}

func TestMetrics_CertExpirySeconds_MatchesEpoch(t *testing.T) {
	c := metricsCluster("metric-cert")
	expiry := time.Date(2030, 6, 15, 12, 0, 0, 0, time.UTC)
	c.Status.CertExpiryTime = expiry.Format(time.RFC3339)

	r := &Reporter{Recorder: record.NewFakeRecorder(2)}
	r.UpdateMetrics(c)

	want := float64(expiry.Unix())
	if got := testutil.ToFloat64(clusterCertExpirySeconds.WithLabelValues("metric-cert")); got != want {
		t.Errorf("openchami_cluster_cert_expiry_seconds: want %v, got %v", want, got)
	}

	// Empty CertExpiryTime → 0.
	c.Status.CertExpiryTime = ""
	r.UpdateMetrics(c)
	if got := testutil.ToFloat64(clusterCertExpirySeconds.WithLabelValues("metric-cert")); got != 0 {
		t.Errorf("empty CertExpiryTime should produce 0, got %v", got)
	}
}

func TestMetrics_VaultReachable_FalseWhenConfiguredFalse(t *testing.T) {
	c := metricsCluster("metric-vault")
	addCondition(c, conditions.ConditionVaultConfigured, metav1.ConditionFalse, conditions.ReasonUnreachable)

	r := &Reporter{Recorder: record.NewFakeRecorder(2)}
	r.UpdateMetrics(c)
	if got := testutil.ToFloat64(clusterVaultReachable.WithLabelValues("metric-vault")); got != 0 {
		t.Errorf("openchami_cluster_vault_reachable: want 0 when not configured, got %v", got)
	}
}
