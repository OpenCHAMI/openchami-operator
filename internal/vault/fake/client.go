/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

// Package fake provides an in-memory implementation of vault.Client for tests.
package fake

import (
	"context"
	"maps"
	"strings"
	"sync"
	"testing"

	"github.com/openchami/openchami-operator/internal/vault"
)

// Client is an in-memory implementation of vault.Client.
// All operations are thread-safe.
type Client struct {
	mu sync.Mutex

	// Calls records every method invocation as a list of argument slices.
	// Key is the method name (e.g. "EnsureSecret"), value is one slice per call.
	Calls map[string][][]any

	// Mounts records KV mounts created via EnsureKVMount.
	Mounts map[string]bool

	// Secrets stores secret data keyed by full path.
	Secrets map[string]map[string]any

	// Policies stores policy HCL keyed by name.
	Policies map[string]string

	// AppRoles stores AppRole configurations keyed by role name.
	AppRoles map[string]vault.AppRoleConfig

	// SecretIDs stores generated secret_ids keyed by role name.
	SecretIDs map[string]string

	// K8sRoles stores Kubernetes auth role configurations keyed by name.
	K8sRoles map[string]vault.KubernetesRoleConfig

	// OIDCConfigs stores configured OIDC issuer URLs keyed by cluster name.
	OIDCConfigs map[string]string

	// Errors injects errors keyed by method name.
	// Set Errors["EnsureSecret"] = errors.New(...) to make every
	// EnsureSecret call return that error.
	Errors map[string]error
}

// NewClient returns an empty FakeClient ready for use.
func NewClient() *Client {
	return &Client{
		Calls:       map[string][][]any{},
		Mounts:      map[string]bool{},
		Secrets:     map[string]map[string]any{},
		Policies:    map[string]string{},
		AppRoles:    map[string]vault.AppRoleConfig{},
		SecretIDs:   map[string]string{},
		K8sRoles:    map[string]vault.KubernetesRoleConfig{},
		OIDCConfigs: map[string]string{},
		Errors:      map[string]error{},
	}
}

func (c *Client) record(method string, args ...any) {
	c.Calls[method] = append(c.Calls[method], args)
}

func (c *Client) IsReachable(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("IsReachable")
	return c.Errors["IsReachable"]
}

func (c *Client) EnsureKVMount(_ context.Context, mount string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("EnsureKVMount", mount)
	if err := c.Errors["EnsureKVMount"]; err != nil {
		return err
	}
	c.Mounts[mount] = true
	return nil
}

func (c *Client) EnsureSecret(_ context.Context, path string, data map[string]any, overwrite bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("EnsureSecret", path, data, overwrite)
	if err := c.Errors["EnsureSecret"]; err != nil {
		return err
	}
	if _, exists := c.Secrets[path]; exists && !overwrite {
		return nil
	}
	stored := make(map[string]any, len(data))
	maps.Copy(stored, data)
	c.Secrets[path] = stored
	return nil
}

func (c *Client) ReadSecret(_ context.Context, path string) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("ReadSecret", path)
	if err := c.Errors["ReadSecret"]; err != nil {
		return nil, err
	}
	data, ok := c.Secrets[path]
	if !ok {
		return nil, nil
	}
	out := make(map[string]any, len(data))
	maps.Copy(out, data)
	return out, nil
}

func (c *Client) EnsurePolicy(_ context.Context, name, hcl string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("EnsurePolicy", name, hcl)
	if err := c.Errors["EnsurePolicy"]; err != nil {
		return err
	}
	c.Policies[name] = hcl
	return nil
}

func (c *Client) EnsureAppRole(_ context.Context, name string, cfg vault.AppRoleConfig) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("EnsureAppRole", name, cfg)
	if err := c.Errors["EnsureAppRole"]; err != nil {
		return "", err
	}
	c.AppRoles[name] = cfg
	return "fake-role-id-" + name, nil
}

func (c *Client) GenerateSecretID(_ context.Context, roleName string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("GenerateSecretID", roleName)
	if err := c.Errors["GenerateSecretID"]; err != nil {
		return "", err
	}
	id := "fake-secret-id-" + roleName
	c.SecretIDs[roleName] = id
	return id, nil
}

func (c *Client) EnsureKubernetesRole(_ context.Context, name string, cfg vault.KubernetesRoleConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("EnsureKubernetesRole", name, cfg)
	if err := c.Errors["EnsureKubernetesRole"]; err != nil {
		return err
	}
	c.K8sRoles[name] = cfg
	return nil
}

func (c *Client) EnsureOIDCConfig(_ context.Context, clusterName, issuerURL string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("EnsureOIDCConfig", clusterName, issuerURL)
	if err := c.Errors["EnsureOIDCConfig"]; err != nil {
		return err
	}
	c.OIDCConfigs[clusterName] = issuerURL
	return nil
}

func (c *Client) DeleteClusterPaths(_ context.Context, prefix string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("DeleteClusterPaths", prefix)
	if err := c.Errors["DeleteClusterPaths"]; err != nil {
		return err
	}
	for path := range c.Secrets {
		if strings.HasPrefix(path, prefix) {
			delete(c.Secrets, path)
		}
	}
	return nil
}

func (c *Client) ListPaths(_ context.Context, prefix string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("ListPaths", prefix)
	if err := c.Errors["ListPaths"]; err != nil {
		return nil, err
	}
	var paths []string
	for path := range c.Secrets {
		if strings.HasPrefix(path, prefix) {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// AssertCalled fails t if method was not called.
func (c *Client) AssertCalled(t *testing.T, method string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.Calls[method]) == 0 {
		t.Errorf("expected vault.%s to have been called", method)
	}
}

// AssertNotCalled fails t if method was called.
func (c *Client) AssertNotCalled(t *testing.T, method string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.Calls[method]) > 0 {
		t.Errorf("expected vault.%s to not have been called (called %d times)", method, len(c.Calls[method]))
	}
}

// AssertSecretExists fails t if no secret exists at path.
func (c *Client) AssertSecretExists(t *testing.T, path string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.Secrets[path]; !ok {
		t.Errorf("expected vault secret at %s to exist", path)
	}
}

// AssertPolicyExists fails t if no policy with name exists.
func (c *Client) AssertPolicyExists(t *testing.T, name string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.Policies[name]; !ok {
		t.Errorf("expected vault policy %s to exist", name)
	}
}

// CallCount returns how many times method has been called.
func (c *Client) CallCount(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.Calls[method])
}
