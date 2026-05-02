/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

// Package status provides the post-reconcile status reporter and the custom
// Prometheus metrics exposed by the operator.
//
// The Reporter is intentionally pure-data with respect to the Kubernetes API:
// its methods mutate the in-memory cluster object, and the controller patches
// .status as a single transaction at the end of each reconcile (invariant #6).
// Sub-reconcilers continue to write their own conditions today; the Reporter
// is the canonical home for the post-aggregation phase computation, the cert
// expiry parser, and the metric updater. It is wired in additively so that
// future reconcilers can route through it without churning the existing call
// sites.
package status

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openahamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/reconcilers"
)

// Reporter centralises status mutation for an OpenCHAMICluster. Methods on
// Reporter mutate the in-memory cluster only; the caller is responsible for
// patching .status (invariant #6: status is patched last by the controller).
//
// Reporter is safe to share across reconciles because every method takes the
// cluster pointer as input and uses no internal per-cluster state.
type Reporter struct {
	Client   client.Client
	Recorder record.EventRecorder
}

// SetCondition sets a condition on the cluster's status with ObservedGeneration
// stamped to cluster.Generation. It is a thin wrapper around
// apimeta.SetStatusCondition that exists so future plumbing (event emission,
// metric annotation) can be added in one place.
func (r *Reporter) SetCondition(
	cluster *openahamiv1alpha1.OpenCHAMICluster,
	condType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: cluster.Generation,
	})
}

// UpdateServiceStatus records readiness, endpoint, and message for a single
// service in cluster.Status.Services. Initialises the map on first use.
func (r *Reporter) UpdateServiceStatus(
	cluster *openahamiv1alpha1.OpenCHAMICluster,
	svcName string,
	ready bool,
	endpoint, message string,
) {
	if cluster.Status.Services == nil {
		cluster.Status.Services = map[string]openahamiv1alpha1.ServiceStatus{}
	}
	cluster.Status.Services[svcName] = openahamiv1alpha1.ServiceStatus{
		Ready:    ready,
		Endpoint: endpoint,
		Message:  message,
	}
}

// ComputeAndSetPhase aggregates conditions and per-service readiness into a
// single ClusterPhase. Mutates cluster.Status.Phase and may emit Warning
// Events for service readiness regressions and NoEligibleNodes.
//
// Decision precedence (highest wins):
//
//  1. Failed       — any condition False with Reason containing "Error".
//  2. Degraded     — CertificatesValid=False, or a service flipped Ready→NotReady,
//     or NetworkProbeReady=False/NoEligibleNodes.
//  3. Provisioning — any condition False with Reason containing "Provisioning".
//  4. Ready        — all conditions True and all enabled services Ready.
//
// Falls back to the previous phase when none of the rules match (typical case:
// a transient state where conditions are still being evaluated).
func (r *Reporter) ComputeAndSetPhase(cluster *openahamiv1alpha1.OpenCHAMICluster) {
	previous := cluster.Status.Phase

	// Detect service Ready→NotReady regressions before we overwrite anything.
	regressed := r.detectServiceRegressions(cluster, previous)

	// Walk the conditions once, gathering everything the ladder needs.
	var (
		hasFailed         bool
		hasDegradedCert   bool
		hasNoEligible     bool
		hasProvisioning   bool
		anyConditionFalse bool
	)
	for _, c := range cluster.Status.Conditions {
		if c.Status != metav1.ConditionFalse {
			continue
		}
		anyConditionFalse = true
		if strings.Contains(c.Reason, "Error") {
			hasFailed = true
		}
		if c.Type == conditions.ConditionCertificatesValid {
			hasDegradedCert = true
		}
		if c.Type == conditions.ConditionNetworkProbeReady && c.Reason == conditions.ReasonNoEligibleNodes {
			hasNoEligible = true
		}
		if strings.Contains(c.Reason, conditions.ReasonProvisioning) {
			hasProvisioning = true
		}
	}

	allServicesReady := r.allEnabledServicesReady(cluster)
	allConditionsTrue := !anyConditionFalse

	// Emit warning events for the degraded sub-cases. Done here (not at each
	// rule branch) so a state that triggers multiple rules still surfaces them.
	if regressed != "" {
		reconcilers.RecordConditionEvent(r.Recorder, cluster, corev1.EventTypeWarning,
			"ServiceDegraded",
			fmt.Sprintf("service %q transitioned Ready→NotReady", regressed))
	}
	if hasNoEligible {
		reconcilers.RecordConditionEvent(r.Recorder, cluster, corev1.EventTypeWarning,
			conditions.ReasonNoEligibleNodes,
			"network probe found no eligible nodes; cluster is degraded")
	}

	switch {
	case hasFailed:
		cluster.Status.Phase = openahamiv1alpha1.PhaseFailed
	case hasDegradedCert, regressed != "", hasNoEligible:
		cluster.Status.Phase = openahamiv1alpha1.PhaseDegraded
	case hasProvisioning, !allServicesReady:
		cluster.Status.Phase = openahamiv1alpha1.PhaseProvisioning
	case allConditionsTrue && allServicesReady:
		cluster.Status.Phase = openahamiv1alpha1.PhaseReady
	default:
		// Nothing matched; preserve the previous phase rather than blanking it.
		if previous != "" {
			cluster.Status.Phase = previous
		} else {
			cluster.Status.Phase = openahamiv1alpha1.PhaseProvisioning
		}
	}
}

// UpdateCertExpiry parses the leaf certificate from secret.Data["tls.crt"]
// and writes its NotAfter into cluster.Status.CertExpiryTime as RFC3339.
// Returns an error when the PEM is missing or malformed; on error the cluster
// status is not modified.
func (r *Reporter) UpdateCertExpiry(
	cluster *openahamiv1alpha1.OpenCHAMICluster,
	certSecret *corev1.Secret,
) error {
	if certSecret == nil {
		return fmt.Errorf("nil cert secret")
	}
	notAfter, err := parseLeafNotAfter(certSecret.Data[corev1.TLSCertKey])
	if err != nil {
		return fmt.Errorf("parsing leaf certificate: %w", err)
	}
	cluster.Status.CertExpiryTime = notAfter.UTC().Format(time.RFC3339)
	return nil
}

// detectServiceRegressions returns the name of a service whose Ready flag
// transitioned True → False since the previous reconcile, or "" when no such
// transition happened.
//
// The "previous" snapshot we have is just cluster.Status.Phase; we do not have
// a snapshot of the prior Services map. We therefore consider a regression to
// have happened when the phase was previously Ready (so every enabled service
// had been observed Ready) and at least one enabled service is now NotReady.
func (r *Reporter) detectServiceRegressions(
	cluster *openahamiv1alpha1.OpenCHAMICluster,
	previous openahamiv1alpha1.ClusterPhase,
) string {
	if previous != openahamiv1alpha1.PhaseReady {
		return ""
	}
	for _, name := range enabledServiceNames(cluster) {
		st, ok := cluster.Status.Services[name]
		if !ok || !st.Ready {
			return name
		}
	}
	return ""
}

// allEnabledServicesReady reports whether every enabled operator-managed
// service is currently Ready.
func (r *Reporter) allEnabledServicesReady(cluster *openahamiv1alpha1.OpenCHAMICluster) bool {
	for _, name := range enabledServiceNames(cluster) {
		st, ok := cluster.Status.Services[name]
		if !ok || !st.Ready {
			return false
		}
	}
	return true
}

// enabledServiceNames returns the canonical set of enabled, status-tracked
// services. Mirrors the aggregation set used by the controller's
// aggregateServicesReady so phase and ServicesReady stay in lockstep.
func enabledServiceNames(cluster *openahamiv1alpha1.OpenCHAMICluster) []string {
	pairs := []struct {
		name    string
		enabled bool
	}{
		{reconcilers.ServiceSMD, cluster.Spec.Services.SMD.Enabled},
		{reconcilers.ServiceTokensmith, cluster.Spec.Services.Tokensmith.Enabled},
		{reconcilers.ServiceBootService, cluster.Spec.Services.BootService.Enabled},
		{reconcilers.ServiceMetadataService, cluster.Spec.Services.MetadataService.Enabled},
	}
	names := make([]string, 0, len(pairs))
	for _, p := range pairs {
		if p.enabled {
			names = append(names, p.name)
		}
	}
	return names
}

// parseLeafNotAfter decodes the first PEM block in tls.crt and returns the
// certificate's NotAfter timestamp. Mirrors the helper in the certificates
// reconciler; duplicated intentionally so the reporter has no reverse
// dependency on internal/reconcilers' cert plumbing (which would create a
// cycle once the reconciler routes its writes through this package).
func parseLeafNotAfter(crtPEM []byte) (time.Time, error) {
	if len(crtPEM) == 0 {
		return time.Time{}, fmt.Errorf("tls.crt is empty")
	}
	block, _ := pem.Decode(crtPEM)
	if block == nil {
		return time.Time{}, fmt.Errorf("tls.crt does not contain a PEM block")
	}
	if block.Type != "CERTIFICATE" {
		return time.Time{}, fmt.Errorf("unexpected PEM block type %q", block.Type)
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing X.509 certificate: %w", err)
	}
	return parsed.NotAfter, nil
}
