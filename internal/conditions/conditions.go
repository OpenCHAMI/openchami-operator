// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

// Package conditions defines condition type constants for OpenCHAMIControlPlane.
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

	// ConditionNetworkPoliciesReady is true when all per-cluster
	// NetworkPolicies have been applied to the cluster namespace.
	ConditionNetworkPoliciesReady = "NetworkPoliciesReady"

	// ConditionServiceIdentityReady is true when the per-cluster
	// service-identity CA has issued the tokensmith server cert and
	// every per-service client cert that consumer Deployments mount
	// for mTLS to tokensmith. Used to gate tokensmith's HTTPS listener
	// rollout and the BackendTLSPolicy that lets envoy trust the CA
	// when fetching JWKS over HTTPS.
	ConditionServiceIdentityReady = "ServiceIdentityReady"
)

// Reasons used across multiple conditions.
const (
	ReasonProvisioning        = "Provisioning"
	ReasonReady               = "Ready"
	ReasonError               = "Error"
	ReasonUnreachable         = "Unreachable"
	ReasonNoEligibleNodes     = "NoEligibleNodes"
	ReasonVersionPinned       = "VersionPinned"
	ReasonWaitingForProbe     = "WaitingForNetworkProbe"
	ReasonExpirationImminent  = "ExpirationImminent"
	ReasonExpired             = "Expired"
	ReasonAwaitingCertificate = "AwaitingCertificate"
	ReasonNotProgrammed       = "NotProgrammed"
	// ReasonAwaitingTokensmith is set on ConditionGatewayReady when the
	// gateway reconciler has held back JWT-protected HTTPRoutes +
	// SecurityPolicies because tokensmith isn't Ready yet. Applying the
	// SecurityPolicies before tokensmith can serve `/.well-known/jwks.json`
	// poisons envoy's JWT-filter cache: the async JWKS fetch fails at
	// envoy startup and never recovers, leaving every JWT-gated route
	// returning 500 `direct_response`. So we wait.
	ReasonAwaitingTokensmith = "AwaitingTokensmith"
	// ReasonAwaitingServiceIdentity is set on ConditionServiceIdentityReady
	// (and on dependent conditions) while cert-manager has not yet
	// populated the per-cluster CA / server / client Secrets that the
	// mTLS service-identity flow depends on. Idempotent: clears once
	// every Secret is present and well-formed.
	ReasonAwaitingServiceIdentity = "AwaitingServiceIdentity"

	// ReasonMissingCRD is set on ConditionGatewayReady when a required
	// CRD is absent from the cluster — most commonly
	// gateway.networking.k8s.io/v1 BackendTLSPolicy, which gateway-api
	// promoted from experimental to standard in v1.4. The operator
	// still works without it (JWT-gated routes get deferred the same
	// way they would if tokensmith were down), but the user must
	// install the missing CRD before envoy can validate the
	// in-namespace CA on the JWKS fetch.
	ReasonMissingCRD = "MissingCRD"
)
