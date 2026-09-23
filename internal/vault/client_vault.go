// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package vault

import (
	"context"
	"fmt"
	"strings"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/api/auth/approle"
	"github.com/hashicorp/vault/api/auth/kubernetes"
)

const (
	vaultOIDCDefaultAssignment = "allow_all"
	vaultOIDCTokenTTL          = "30m"
	// vaultOIDCProviderName is the single, shared named OIDC provider the
	// operator manages. Every OpenCHAMIControlPlane sharing a Vault instance
	// uses this same provider; per-control-plane isolation is carried entirely
	// by the per-cluster clients and signing key, never by mutating shared
	// provider state. See internal/reconcilers/helpers.go
	// (vaultOIDCProviderPathSuffix) which must reference the same name so the
	// minted `iss` matches what tokensmith validates.
	vaultOIDCProviderName = "openchami"
)

// Config holds connection parameters for a real Vault client.
type Config struct {
	Address string
	// AuthMethod is "kubernetes", "appRole", or "token". The "token"
	// method is intended for `vault server -dev` style dev clusters
	// only — production deployments use kubernetes or appRole.
	AuthMethod string
	// K8sRole is the Vault Kubernetes auth role to use when AuthMethod=kubernetes.
	K8sRole string
	// K8sMountPath is the Kubernetes auth mount path. Defaults to "kubernetes".
	K8sMountPath string
	// AppRoleID and AppRoleSecretID are used when AuthMethod=appRole.
	AppRoleID       string
	AppRoleSecretID string
	// AppRoleMountPath is the AppRole auth mount path. Defaults to "approle".
	AppRoleMountPath string
	// Token is the bearer token used when AuthMethod=token. Reads from
	// VAULT_TOKEN in the dev-run flow (see Makefile).
	Token string
}

// vaultClient implements Client against a real Vault server.
type vaultClient struct {
	api *vaultapi.Client
	cfg Config
}

// NewClient builds a Vault client and authenticates using cfg.
func NewClient(ctx context.Context, cfg Config) (Client, error) {
	apiCfg := vaultapi.DefaultConfig()
	apiCfg.Address = cfg.Address
	c, err := vaultapi.NewClient(apiCfg)
	if err != nil {
		return nil, fmt.Errorf("creating vault api client: %w", err)
	}

	vc := &vaultClient{api: c, cfg: cfg}
	if err := vc.authenticate(ctx); err != nil {
		return nil, fmt.Errorf("authenticating to vault: %w", err)
	}
	return vc, nil
}

func (c *vaultClient) authenticate(ctx context.Context) error {
	switch c.cfg.AuthMethod {
	case "kubernetes":
		mount := c.cfg.K8sMountPath
		if mount == "" {
			mount = "kubernetes"
		}
		auth, err := kubernetes.NewKubernetesAuth(c.cfg.K8sRole,
			kubernetes.WithMountPath(mount))
		if err != nil {
			return fmt.Errorf("constructing kubernetes auth: %w", err)
		}
		secret, err := c.api.Auth().Login(ctx, auth)
		if err != nil {
			return fmt.Errorf("kubernetes login: %w", err)
		}
		if secret == nil || secret.Auth == nil {
			return fmt.Errorf("kubernetes login returned no token")
		}
		return nil

	case "appRole":
		mount := c.cfg.AppRoleMountPath
		if mount == "" {
			mount = "approle"
		}
		auth, err := approle.NewAppRoleAuth(c.cfg.AppRoleID,
			&approle.SecretID{FromString: c.cfg.AppRoleSecretID},
			approle.WithMountPath(mount))
		if err != nil {
			return fmt.Errorf("constructing approle auth: %w", err)
		}
		secret, err := c.api.Auth().Login(ctx, auth)
		if err != nil {
			return fmt.Errorf("approle login: %w", err)
		}
		if secret == nil || secret.Auth == nil {
			return fmt.Errorf("approle login returned no token")
		}
		return nil

	case "token":
		// Dev-only path: hand the bearer token to the API client
		// directly. No login round-trip is required (a token already
		// embodies the authenticated session). Production must use
		// kubernetes or appRole — see the Config.AuthMethod doc.
		if c.cfg.Token == "" {
			return fmt.Errorf("token auth requires a non-empty token")
		}
		c.api.SetToken(c.cfg.Token)
		return nil

	default:
		return fmt.Errorf("unsupported auth method %q", c.cfg.AuthMethod)
	}
}

func (c *vaultClient) IsReachable(ctx context.Context) error {
	health, err := c.api.Sys().HealthWithContext(ctx)
	if err != nil {
		return fmt.Errorf("vault health check: %w", err)
	}
	if health.Sealed {
		return fmt.Errorf("vault is sealed")
	}
	return nil
}

func (c *vaultClient) EnsureKVMount(ctx context.Context, mount string) error {
	mounts, err := c.api.Sys().ListMountsWithContext(ctx)
	if err != nil {
		return fmt.Errorf("listing mounts: %w", err)
	}
	if _, exists := mounts[mount+"/"]; exists {
		return nil
	}
	return c.api.Sys().MountWithContext(ctx, mount, &vaultapi.MountInput{
		Type:    "kv",
		Options: map[string]string{"version": "2"},
	})
}

func (c *vaultClient) EnsureSecret(ctx context.Context, path string, data map[string]any, overwrite bool) error {
	mount, sub := splitKVPath(path)
	kv := c.api.KVv2(mount)

	if !overwrite {
		existing, err := kv.Get(ctx, sub)
		if err == nil && existing != nil {
			return nil
		}
		if err != nil && !isNotFound(err) {
			return fmt.Errorf("checking existing secret %s: %w", path, err)
		}
	}

	if _, err := kv.Put(ctx, sub, data); err != nil {
		return fmt.Errorf("writing secret %s: %w", path, err)
	}
	return nil
}

func (c *vaultClient) ReadSecret(ctx context.Context, path string) (map[string]any, error) {
	mount, sub := splitKVPath(path)
	secret, err := c.api.KVv2(mount).Get(ctx, sub)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading secret %s: %w", path, err)
	}
	if secret == nil {
		return nil, nil
	}
	return secret.Data, nil
}

func (c *vaultClient) EnsurePolicy(ctx context.Context, name, hcl string) error {
	return c.api.Sys().PutPolicyWithContext(ctx, name, hcl)
}

func (c *vaultClient) EnsureAppRole(ctx context.Context, name string, cfg AppRoleConfig) (string, error) {
	rolePath := fmt.Sprintf("auth/approle/role/%s", name)
	data := map[string]any{
		"token_policies": cfg.Policies,
		"token_ttl":      cfg.TokenTTL,
		"token_max_ttl":  cfg.TokenMaxTTL,
		"secret_id_ttl":  cfg.SecretIDTTL,
	}
	if _, err := c.api.Logical().WriteWithContext(ctx, rolePath, data); err != nil {
		return "", fmt.Errorf("writing approle %s: %w", name, err)
	}

	idResp, err := c.api.Logical().ReadWithContext(ctx, rolePath+"/role-id")
	if err != nil {
		return "", fmt.Errorf("reading approle id: %w", err)
	}
	if idResp == nil {
		return "", fmt.Errorf("approle %s returned no role-id", name)
	}
	id, _ := idResp.Data["role_id"].(string)
	return id, nil
}

func (c *vaultClient) GenerateSecretID(ctx context.Context, roleName string) (string, error) {
	resp, err := c.api.Logical().WriteWithContext(ctx,
		fmt.Sprintf("auth/approle/role/%s/secret-id", roleName), nil)
	if err != nil {
		return "", fmt.Errorf("generating secret-id for %s: %w", roleName, err)
	}
	if resp == nil {
		return "", fmt.Errorf("secret-id generation returned no data")
	}
	id, _ := resp.Data["secret_id"].(string)
	return id, nil
}

func (c *vaultClient) EnsureKubernetesRole(ctx context.Context, name string, cfg KubernetesRoleConfig) error {
	data := map[string]any{
		"bound_service_account_names":      cfg.BoundServiceAccountNames,
		"bound_service_account_namespaces": cfg.BoundServiceAccountNamespaces,
		"token_policies":                   cfg.Policies,
		"token_ttl":                        cfg.TokenTTL,
	}
	_, err := c.api.Logical().WriteWithContext(ctx,
		fmt.Sprintf("auth/kubernetes/role/%s", name), data)
	if err != nil {
		return fmt.Errorf("writing kubernetes role %s: %w", name, err)
	}
	return nil
}

func (c *vaultClient) EnsureOIDCConfig(ctx context.Context, clusterName string, cfg OIDCConfig) (OIDCClientCredentials, error) {
	// NOTE: We intentionally do NOT write identity/oidc/config. That endpoint
	// is a Vault-GLOBAL singleton (one issuer per Vault instance); writing it
	// per-control-plane made whichever control plane reconciled last silently
	// own every other control plane's issuer (issue #58). The issuer is instead
	// pinned on the named `openchami` OIDC provider below, which every control
	// plane writes identically.
	keyName := "openchami-" + clusterName
	keyData := map[string]any{
		"rotation_period":    "24h",
		"verification_ttl":   "24h",
		"allowed_client_ids": []string{"*"},
	}
	if _, err := c.api.Logical().WriteWithContext(ctx,
		"identity/oidc/key/"+keyName, keyData); err != nil {
		return OIDCClientCredentials{}, fmt.Errorf("creating oidc key: %w", err)
	}

	// Ensure the shared named provider exists with a stable issuer and a
	// wildcard allowed_client_ids. Writing ["*"] means the provider authorizes
	// ANY client the operator creates without a read-modify-write on a shared
	// list (issue #57): access is instead scoped per-client via `assignments`
	// on each individual client below. Every control plane writes this same
	// fixed payload, so concurrent reconciles converge instead of racing.
	if err := c.ensureOIDCProvider(ctx, cfg); err != nil {
		return OIDCClientCredentials{}, err
	}

	// Provision the OIDC client tokensmith authenticates as. Vault generates
	// the client_id and client_secret on creation; we read them back below so
	// the caller can surface both to tokensmith. An assignment scopes which
	// entities/groups may obtain tokens; the built-in "allow_all" assignment
	// (shipped with every Vault) is sufficient for the operator's single-tenant
	// per-cluster model.
	clientName := "openchami-" + clusterName + "-tokensmith"
	clientData := map[string]any{
		"key":              keyName,
		"client_type":      "confidential",
		"assignments":      []string{vaultOIDCDefaultAssignment},
		"id_token_ttl":     vaultOIDCTokenTTL,
		"access_token_ttl": vaultOIDCTokenTTL,
	}
	if _, err := c.api.Logical().WriteWithContext(ctx,
		"identity/oidc/client/"+clientName, clientData); err != nil {
		return OIDCClientCredentials{}, fmt.Errorf("creating oidc client: %w", err)
	}

	// The CLI client is public and therefore has no distributable secret. Vault
	// enforces PKCE for this client type; only its generated client_id is safe to
	// hand to users. It is deliberately distinct from TokenSmith's confidential
	// client above so the TokenSmith client_secret never crosses into CLI config.
	cliClientName := "openchami-" + clusterName + "-cli"
	cliClientData := map[string]any{
		"key":              keyName,
		"client_type":      "public",
		"redirect_uris":    cfg.CLIRedirectURIs,
		"assignments":      cfg.CLIAssignments,
		"id_token_ttl":     vaultOIDCTokenTTL,
		"access_token_ttl": vaultOIDCTokenTTL,
	}
	if _, err := c.api.Logical().WriteWithContext(ctx,
		"identity/oidc/client/"+cliClientName, cliClientData); err != nil {
		return OIDCClientCredentials{}, fmt.Errorf("creating public CLI oidc client: %w", err)
	}

	resp, err := c.api.Logical().ReadWithContext(ctx,
		"identity/oidc/client/"+clientName)
	if err != nil {
		return OIDCClientCredentials{}, fmt.Errorf("reading oidc client: %w", err)
	}
	if resp == nil || resp.Data == nil {
		return OIDCClientCredentials{}, fmt.Errorf("oidc client %q returned no data", clientName)
	}
	creds := OIDCClientCredentials{}
	creds.ClientID, _ = resp.Data["client_id"].(string)
	creds.ClientSecret, _ = resp.Data["client_secret"].(string)
	if creds.ClientID == "" {
		return OIDCClientCredentials{}, fmt.Errorf("oidc client %q returned empty client_id", clientName)
	}
	cliResp, err := c.api.Logical().ReadWithContext(ctx,
		"identity/oidc/client/"+cliClientName)
	if err != nil {
		return OIDCClientCredentials{}, fmt.Errorf("reading public CLI oidc client: %w", err)
	}
	if cliResp == nil || cliResp.Data == nil {
		return OIDCClientCredentials{}, fmt.Errorf("public CLI oidc client %q returned no data", cliClientName)
	}
	cliClientID, _ := cliResp.Data["client_id"].(string)
	if cliClientID == "" {
		return OIDCClientCredentials{}, fmt.Errorf("public CLI oidc client %q returned empty client_id", cliClientName)
	}
	creds.CLIClientID = cliClientID

	return creds, nil
}

// ensureOIDCProvider creates or updates the shared named OIDC provider. The
// payload is identical for every control plane: a stable issuer derived from
// the Vault address and a wildcard allowed_client_ids. Because the write is a
// fixed create-or-update (never a read-modify-write on a per-control-plane
// list) concurrent reconciliations converge on the same value instead of
// losing each other's updates (issue #57).
//
// The issuer set here is scheme+host(+port) with no path; Vault appends
// `/v1/identity/oidc/provider/<name>` itself when minting the `iss` claim, and
// tokensmith validates against that exact URL (see helpers.go).
func (c *vaultClient) ensureOIDCProvider(ctx context.Context, cfg OIDCConfig) error {
	data := map[string]any{
		"issuer":             cfg.IssuerURL,
		"allowed_client_ids": []string{"*"},
	}
	if len(cfg.ScopesSupported) > 0 {
		data["scopes_supported"] = cfg.ScopesSupported
	}
	if _, err := c.api.Logical().WriteWithContext(ctx,
		"identity/oidc/provider/"+vaultOIDCProviderName, data); err != nil {
		return fmt.Errorf("configuring oidc provider %q: %w", vaultOIDCProviderName, err)
	}
	return nil
}

func (c *vaultClient) DeleteClusterPaths(ctx context.Context, prefix string) error {
	mount, sub := splitKVPath(prefix)
	listing, err := c.api.Logical().ListWithContext(ctx,
		fmt.Sprintf("%s/metadata/%s", mount, sub))
	if err != nil {
		return fmt.Errorf("listing %s: %w", prefix, err)
	}
	if listing == nil {
		return nil
	}
	keys, _ := listing.Data["keys"].([]any)
	for _, k := range keys {
		key, _ := k.(string)
		full := strings.TrimSuffix(prefix, "/") + "/" + strings.TrimSuffix(key, "/")
		if strings.HasSuffix(key, "/") {
			if err := c.DeleteClusterPaths(ctx, full+"/"); err != nil {
				return err
			}
			continue
		}
		if err := c.api.KVv2(mount).DeleteMetadata(ctx,
			strings.TrimPrefix(full, mount+"/")); err != nil {
			return fmt.Errorf("deleting %s: %w", full, err)
		}
	}
	return nil
}

func (c *vaultClient) ListPaths(ctx context.Context, prefix string) ([]string, error) {
	mount, sub := splitKVPath(prefix)
	listing, err := c.api.Logical().ListWithContext(ctx,
		fmt.Sprintf("%s/metadata/%s", mount, sub))
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", prefix, err)
	}
	if listing == nil {
		return nil, nil
	}
	keys, _ := listing.Data["keys"].([]any)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if s, ok := k.(string); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// splitKVPath splits a full path like "openchami/foo/db/credentials" into
// the mount ("openchami") and sub path ("foo/db/credentials"). The full
// path must include the mount as its first segment.
func splitKVPath(path string) (mount, sub string) {
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		return path, ""
	}
	return parts[0], parts[1]
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var rerr *vaultapi.ResponseError
	if asResponseError(err, &rerr) {
		return rerr.StatusCode == 404
	}
	return strings.Contains(err.Error(), "secret not found")
}

func asResponseError(err error, target **vaultapi.ResponseError) bool {
	if err == nil {
		return false
	}
	type unwrapper interface{ Unwrap() error }
	for cur := err; cur != nil; {
		if re, ok := cur.(*vaultapi.ResponseError); ok {
			*target = re
			return true
		}
		u, ok := cur.(unwrapper)
		if !ok {
			return false
		}
		cur = u.Unwrap()
	}
	return false
}
