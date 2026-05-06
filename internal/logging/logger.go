// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

// Package logging provides structured log enrichment for sub-reconcilers.
// All sub-reconcilers must use these helpers. Direct calls to
// log.FromContext in reconciler files are a lint violation.
package logging

import (
	"context"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

// Enrich returns a logger pre-seeded with standard fields for a sub-reconciler.
// Call once at the very start of each Reconcile() method:
//
//	log := logging.Enrich(ctx, cluster, "vault")
func Enrich(ctx context.Context, cluster *openchamiv1alpha1.OpenCHAMICluster, reconcilerName string) logr.Logger {
	return log.FromContext(ctx).WithValues(
		"cluster", cluster.Spec.ClusterName,
		"reconciler", reconcilerName,
		"generation", cluster.Generation,
		"namespace", "openchami-"+cluster.Spec.ClusterName,
	)
}

// EnrichWithResource adds resource-specific fields to an existing logger.
// Call when operating on a specific Kubernetes object:
//
//	log = logging.EnrichWithResource(log, "Deployment", "smd")
func EnrichWithResource(l logr.Logger, kind, name string) logr.Logger {
	return l.WithValues(
		"resource.kind", kind,
		"resource.name", name,
	)
}

// EnrichWithNode adds a node name field. Used in the network probe reconciler.
func EnrichWithNode(l logr.Logger, nodeName string) logr.Logger {
	return l.WithValues("node", nodeName)
}

// ClusterNamespace returns the canonical namespace name for a cluster.
// Centralised here to avoid string formatting scattered across reconcilers.
func ClusterNamespace(clusterName string) string {
	return "openchami-" + clusterName
}
