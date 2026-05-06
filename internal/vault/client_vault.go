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

func (c *vaultClient) EnsureOIDCConfig(ctx context.Context, clusterName, issuerURL string) error {
	data := map[string]any{"issuer": issuerURL}
	_, err := c.api.Logical().WriteWithContext(ctx,
		"identity/oidc/config", data)
	if err != nil {
		return fmt.Errorf("configuring oidc issuer: %w", err)
	}
	keyName := "openchami-" + clusterName
	keyData := map[string]any{
		"rotation_period":    "24h",
		"verification_ttl":   "24h",
		"allowed_client_ids": []string{"*"},
	}
	if _, err := c.api.Logical().WriteWithContext(ctx,
		"identity/oidc/key/"+keyName, keyData); err != nil {
		return fmt.Errorf("creating oidc key: %w", err)
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
