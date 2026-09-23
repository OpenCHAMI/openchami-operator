// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

// Package vault provides a Vault API client interface and supporting types.
// The interface is implemented by client_vault.go (real) and fake/client.go (test).
package vault

import "context"

// Client is the interface the vault sub-reconciler uses to interact with Vault.
// All methods are idempotent: calling them twice has no additional effect.
// Implementations must be safe for concurrent use.
type Client interface {
	// IsReachable checks Vault connectivity without authenticating.
	// Returns nil if Vault is reachable and unsealed.
	IsReachable(ctx context.Context) error

	// EnsureKVMount ensures a KV v2 secrets engine is mounted at path.
	// No-ops if the mount already exists.
	EnsureKVMount(ctx context.Context, mount string) error

	// EnsureSecret creates or updates a secret at path.
	// When overwrite is false, existing secrets are left unchanged.
	// When overwrite is true, the secret is replaced unconditionally.
	EnsureSecret(ctx context.Context, path string, data map[string]any, overwrite bool) error

	// ReadSecret reads a secret from path. Returns nil, nil if not found.
	ReadSecret(ctx context.Context, path string) (map[string]any, error)

	// EnsurePolicy creates or replaces an HCL policy.
	EnsurePolicy(ctx context.Context, name, hcl string) error

	// EnsureAppRole creates or updates an AppRole with the given config.
	// Returns the role_id. Does not rotate an existing secret_id.
	EnsureAppRole(ctx context.Context, name string, cfg AppRoleConfig) (roleID string, err error)

	// GenerateSecretID generates a new secret_id for an AppRole.
	// The caller is responsible for storing the returned secret_id securely.
	GenerateSecretID(ctx context.Context, roleName string) (secretID string, err error)

	// EnsureKubernetesRole creates or updates a Kubernetes auth role.
	EnsureKubernetesRole(ctx context.Context, name string, cfg KubernetesRoleConfig) error

	// EnsureOIDCConfig ensures the shared named "openchami" OIDC provider
	// exists (with a stable issuer and wildcard allowed_client_ids) and
	// provisions two per-cluster clients: a confidential TokenSmith client and
	// a public CLI client that requires PKCE. Access is scoped per-client via
	// assignments rather than by mutating the provider's client list, so
	// concurrent reconciliation of multiple control planes cannot race. It does
	// NOT write the Vault-global identity/oidc/config. It returns the
	// confidential client's generated client_id and client_secret (for the
	// TokenSmith Kubernetes Secret) and the public CLI client_id (safe to hand
	// to users; the client_secret never crosses to CLI config). The caller must
	// pass the Vault-address-derived issuer so Vault-minted `iss` values match
	// TokenSmith's provider URL.
	// Only called when tokensmith.oidcProvider=vault. Idempotent: repeated
	// calls return the same client_id and (Vault-preserved) client_secret.
	EnsureOIDCConfig(ctx context.Context, clusterName string, cfg OIDCConfig) (OIDCClientCredentials, error)

	// DeleteClusterPaths deletes all KV paths under prefix.
	// Used during cluster deletion when cleanup annotation is set.
	DeleteClusterPaths(ctx context.Context, prefix string) error

	// ListPaths lists all paths under prefix.
	ListPaths(ctx context.Context, prefix string) ([]string, error)
}

// OIDCClientCredentials holds the credentials Vault generates for a cluster's
// identity/oidc clients. All fields are assigned by Vault when the clients are
// created; the operator never chooses them.
type OIDCClientCredentials struct {
	// ClientID is the Vault-generated OAuth2 client_id for the confidential
	// TokenSmith client.
	ClientID string
	// ClientSecret is the Vault-generated OAuth2 client_secret for the
	// confidential TokenSmith client.
	ClientSecret string
	// CLIClientID is the Vault-generated OAuth2 client_id for the public CLI
	// client. Public clients have no secret (they use PKCE), so this is safe to
	// surface to end users.
	CLIClientID string
}

// OIDCConfig describes the issuer, provider scopes, and public CLI client
// configuration used to provision a cluster's Vault identity/oidc clients.
type OIDCConfig struct {
	// IssuerURL is the scheme+host(+port) with no path that Vault stamps as the
	// scheme://host:port component of minted `iss` claims for the shared
	// "openchami" provider.
	IssuerURL string
	// ScopesSupported are the scopes advertised on the shared provider. Because
	// the provider is shared across all control planes, this set is global to
	// the Vault instance; passing an empty slice leaves the provider's existing
	// scopes unchanged.
	ScopesSupported []string
	CLIRedirectURIs []string
	CLIAssignments  []string
}

// PublicOIDCClient describes the secretless CLI client exposed by the fake
// Vault implementation for behavioral assertions.
type PublicOIDCClient struct {
	Name         string
	ClientID     string
	RedirectURIs []string
	Assignments  []string
}

// AppRoleConfig configures an AppRole.
type AppRoleConfig struct {
	// Policies is the list of policy names to attach.
	Policies []string
	// TokenTTL is the default token TTL, e.g. "15m".
	TokenTTL string
	// TokenMaxTTL is the maximum token TTL, e.g. "1h".
	TokenMaxTTL string
	// SecretIDTTL is how long generated secret_ids are valid.
	// "0" means non-expiring.
	SecretIDTTL string
}

// KubernetesRoleConfig configures a Kubernetes auth role.
type KubernetesRoleConfig struct {
	// BoundServiceAccountNames are the ServiceAccount names allowed to use this role.
	BoundServiceAccountNames []string
	// BoundServiceAccountNamespaces are the namespaces those accounts must be in.
	BoundServiceAccountNamespaces []string
	// Policies is the list of policy names to attach.
	Policies []string
	// TokenTTL is the default token TTL.
	TokenTTL string
}
