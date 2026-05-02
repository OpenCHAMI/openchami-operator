/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

// Package conditions defines condition type constants for OpenCHAMICluster.
package conditions

const (
	// ConditionReady is true when all enabled services are available
	// and all infrastructure conditions are satisfied.
	ConditionReady = "Ready"

	// ConditionReconcileActive is false when the cluster is version-pinned
	// to a different operator version than currently running.
	ConditionReconcileActive = "ReconcileActive"

	// ConditionNamespaceReady is true when the cluster namespace exists
	// with correct labels and pod security standards.
	ConditionNamespaceReady = "NamespaceReady"

	// ConditionVaultConfigured is true when Vault paths, policies, roles,
	// and VSO resources are in place for this cluster.
	ConditionVaultConfigured = "VaultConfigured"

	// ConditionBucketReady is true when the boot-images S3 bucket exists
	// on VersityGW.
	ConditionBucketReady = "BucketReady"

	// ConditionLogBucketReady is true when the log S3 bucket exists
	// with the correct lifecycle rule applied.
	ConditionLogBucketReady = "LogBucketReady"

	// ConditionDatabaseReady is true when the CloudNativePG cluster
	// reports a healthy phase and both databases exist.
	ConditionDatabaseReady = "DatabaseReady"

	// ConditionNetworkProbeReady is true when the probe DaemonSet is
	// running and at least one node passes each configured probe type.
	// False with Reason=NoEligibleNodes when probes run but nothing passes.
	ConditionNetworkProbeReady = "NetworkProbeReady"

	// ConditionServicesReady is true when all enabled Deployments
	// report availableReplicas >= 1.
	ConditionServicesReady = "ServicesReady"

	// ConditionDHCPReady is true when the CoreDHCP DaemonSet has
	// NumberReady > 0.
	ConditionDHCPReady = "DHCPReady"

	// ConditionGatewayReady is true when the Envoy Gateway reports
	// Programmed=True.
	ConditionGatewayReady = "GatewayReady"

	// ConditionCertificatesValid is true when the nearest-expiring
	// certificate has more than 24 hours remaining.
	ConditionCertificatesValid = "CertificatesValid"

	// ConditionTopologyPublished is true when the topology ConfigMap
	// exists and its content hash matches the operator's computed value.
	ConditionTopologyPublished = "TopologyPublished"

	// ConditionLogCollectorReady is true when the legendary-funicular
	// collector DaemonSet has NumberReady > 0.
	ConditionLogCollectorReady = "LogCollectorReady"

	// ConditionMagellanReady is true when the Magellan CronJob exists.
	ConditionMagellanReady = "MagellanReady"
)

// Reasons used across multiple conditions.
const (
	ReasonProvisioning       = "Provisioning"
	ReasonReady              = "Ready"
	ReasonError              = "Error"
	ReasonUnreachable        = "Unreachable"
	ReasonNoEligibleNodes    = "NoEligibleNodes"
	ReasonVersionPinned      = "VersionPinned"
	ReasonWaitingForProbe    = "WaitingForNetworkProbe"
	ReasonExpirationImminent = "ExpirationImminent"
	ReasonExpired            = "Expired"
)
