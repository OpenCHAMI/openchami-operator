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

	// EnsureOIDCConfig configures Vault's identity/oidc engine for a cluster
	// and provisions an OIDC client for tokensmith. It configures the issuer,
	// creates the signing key, an assignment, and an OIDC client, then returns
	// the client's generated client_id and client_secret so the caller can
	// materialize them into the tokensmith OIDC Kubernetes Secret.
	// redirectURIs sets the client's allowed redirect (callback) URIs for the
	// authorization-code flow; an empty slice leaves the client with no
	// allowed redirects (authorization requests will fail with
	// invalid_redirect_uri). Only called when tokensmith.oidcProvider=vault.
	// Idempotent: repeated calls return the same client_id and
	// (Vault-preserved) client_secret and reconcile redirect_uris to the
	// supplied list.
	EnsureOIDCConfig(ctx context.Context, clusterName, issuerURL string, redirectURIs []string) (OIDCClientCredentials, error)

	// DeleteClusterPaths deletes all KV paths under prefix.
	// Used during cluster deletion when cleanup annotation is set.
	DeleteClusterPaths(ctx context.Context, prefix string) error

	// ListPaths lists all paths under prefix.
	ListPaths(ctx context.Context, prefix string) ([]string, error)
}

// OIDCClientCredentials holds the credentials Vault generates for an
// identity/oidc client. Both fields are assigned by Vault when the client is
// created; the operator never chooses them.
type OIDCClientCredentials struct {
	// ClientID is the Vault-generated OAuth2 client_id.
	ClientID string
	// ClientSecret is the Vault-generated OAuth2 client_secret.
	ClientSecret string
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
