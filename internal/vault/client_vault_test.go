// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package vault

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"
)

const (
	// Shared test literals, centralised so repeated uses don't trip goconst.
	testProviderPath = "/v1/identity/oidc/provider/openchami"
	testAlphaIssuer  = "https://alpha.example.test"
)

func TestVaultClient_EnsureOIDCConfigCreatesConfidentialAndPublicClients(t *testing.T) {
	t.Parallel()

	writes := map[string]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode %s: %v", r.URL.Path, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			writes[r.URL.Path] = body
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{}}`))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/identity/oidc/client/openchami-alpha-tokensmith" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"client_id":"tokensmith-id","client_secret":"tokensmith-secret"}}`))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/identity/oidc/client/openchami-alpha-cli" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"client_id":"cli-id"}}`))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == testProviderPath {
			// Reflect whatever was last written so the read-before-write and
			// post-create read-back see a consistent issuer. 404 until created.
			// Real Vault returns the EFFECTIVE provider issuer (base + provider
			// path), not the base the operator wrote, so mirror that here
			// (issue #62).
			if prev, ok := writes[testProviderPath]; ok {
				w.Header().Set("Content-Type", "application/json")
				data := map[string]any{}
				for k, v := range prev {
					data[k] = v
				}
				if base, ok := data["issuer"].(string); ok {
					data["issuer"] = base + testProviderPath
				}
				resp := map[string]any{"data": data}
				_ = json.NewEncoder(w).Encode(resp)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	apiConfig := vaultapi.DefaultConfig()
	apiConfig.Address = server.URL
	api, err := vaultapi.NewClient(apiConfig)
	if err != nil {
		t.Fatalf("new Vault API client: %v", err)
	}
	client := &vaultClient{api: api}

	credentials, err := client.EnsureOIDCConfig(context.Background(), "alpha", OIDCConfig{
		IssuerURL:       testAlphaIssuer,
		ScopesSupported: []string{"groups"},
		CLIRedirectURIs: []string{"http://127.0.0.1:8250/callback"},
		CLIAssignments:  []string{vaultOIDCDefaultAssignment},
	})
	if err != nil {
		t.Fatalf("EnsureOIDCConfig: %v", err)
	}
	if credentials.ClientID != "tokensmith-id" || credentials.ClientSecret != "tokensmith-secret" {
		t.Fatalf("unexpected TokenSmith credentials: %+v", credentials)
	}
	if credentials.CLIClientID != "cli-id" {
		t.Fatalf("unexpected CLI client_id: %q", credentials.CLIClientID)
	}

	// The Vault-GLOBAL identity/oidc/config must NOT be written (issue #58):
	// the operator only writes the named provider's issuer.
	if _, wrote := writes["/v1/identity/oidc/config"]; wrote {
		t.Error("operator must not write the global identity/oidc/config")
	}

	tokensmith := writes["/v1/identity/oidc/client/openchami-alpha-tokensmith"]
	if tokensmith["client_type"] != "confidential" {
		t.Errorf("TokenSmith client_type = %v, want confidential", tokensmith["client_type"])
	}
	cli := writes["/v1/identity/oidc/client/openchami-alpha-cli"]
	if cli["client_type"] != "public" {
		t.Errorf("CLI client_type = %v, want public", cli["client_type"])
	}
	if _, exists := cli["client_secret"]; exists {
		t.Error("public CLI client request must not include a client_secret")
	}
	assertJSONStrings(t, cli["redirect_uris"], []string{"http://127.0.0.1:8250/callback"})
	assertJSONStrings(t, cli["assignments"], []string{vaultOIDCDefaultAssignment})

	// The shared named provider is written with a fixed wildcard
	// allowed_client_ids (issue #57): no per-control-plane read-modify-write.
	provider := writes[testProviderPath]
	if provider == nil {
		t.Fatal("expected a write to the named openchami provider")
	}
	if provider["issuer"] != testAlphaIssuer {
		t.Errorf("provider issuer = %v, want https://alpha.example.test", provider["issuer"])
	}
	assertJSONStrings(t, provider["allowed_client_ids"], []string{"*"})
	assertJSONStrings(t, provider["scopes_supported"], []string{"groups"})
}

// TestVaultClient_EnsureOIDCConfigIssuerConflict asserts that when the shared
// openchami provider already exists with a different issuer, EnsureOIDCConfig
// returns an OIDCIssuerConflictError and does NOT overwrite the provider.
func TestVaultClient_EnsureOIDCConfigIssuerConflict(t *testing.T) {
	t.Parallel()

	providerWrites := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == testProviderPath {
			// Provider already pinned to a different issuer by another CP. Vault
			// reports the EFFECTIVE issuer (base + provider path) (issue #62).
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"issuer":"https://other.example.test/v1/identity/oidc/provider/openchami","allowed_client_ids":["*"]}}`))
			return
		}
		if r.Method == http.MethodPut && r.URL.Path == testProviderPath {
			providerWrites++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{}}`))
			return
		}
		if r.Method == http.MethodPut {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	apiConfig := vaultapi.DefaultConfig()
	apiConfig.Address = server.URL
	api, err := vaultapi.NewClient(apiConfig)
	if err != nil {
		t.Fatalf("new Vault API client: %v", err)
	}
	client := &vaultClient{api: api}

	_, err = client.EnsureOIDCConfig(context.Background(), "alpha", OIDCConfig{
		IssuerURL: testAlphaIssuer,
	})
	var conflict *OIDCIssuerConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected OIDCIssuerConflictError, got %v", err)
	}
	if conflict.Existing != "https://other.example.test/v1/identity/oidc/provider/openchami" ||
		conflict.Requested != testAlphaIssuer+testProviderPath {
		t.Errorf("unexpected conflict detail: %+v", conflict)
	}
	if providerWrites != 0 {
		t.Errorf("provider must not be written on issuer conflict, got %d writes", providerWrites)
	}
}

// TestVaultClient_EnsureOIDCConfigReadBackConflict covers the concurrent
// initial-creation race: the pre-write GET sees no provider (404), the write
// succeeds, but the post-write read-back observes a DIFFERENT issuer because
// another control plane won the create race. The read-back must catch this and
// return an OIDCIssuerConflictError rather than reporting success.
func TestVaultClient_EnsureOIDCConfigReadBackConflict(t *testing.T) {
	t.Parallel()

	provGets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testProviderPath {
			switch r.Method {
			case http.MethodGet:
				provGets++
				if provGets == 1 {
					// Pre-write: provider does not exist yet.
					w.WriteHeader(http.StatusNotFound)
					return
				}
				// Post-write read-back: a concurrent creator won with a
				// different issuer. Vault reports the EFFECTIVE issuer
				// (base + provider path) (issue #62).
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{"issuer":"https://winner.example.test/v1/identity/oidc/provider/openchami","allowed_client_ids":["*"]}}`))
				return
			case http.MethodPut:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{}}`))
				return
			}
		}
		if r.Method == http.MethodPut {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	apiConfig := vaultapi.DefaultConfig()
	apiConfig.Address = server.URL
	api, err := vaultapi.NewClient(apiConfig)
	if err != nil {
		t.Fatalf("new Vault API client: %v", err)
	}
	client := &vaultClient{api: api}

	_, err = client.EnsureOIDCConfig(context.Background(), "alpha", OIDCConfig{
		IssuerURL: testAlphaIssuer,
	})
	var conflict *OIDCIssuerConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected OIDCIssuerConflictError from read-back, got %v", err)
	}
	if conflict.Existing != "https://winner.example.test/v1/identity/oidc/provider/openchami" ||
		conflict.Requested != testAlphaIssuer+testProviderPath {
		t.Errorf("unexpected conflict detail: %+v", conflict)
	}
}

// TestVaultClient_EnsureOIDCConfigReadBackMissingIssuer asserts that a
// read-back which returns no observable issuer is treated as a verification
// failure (the write cannot be confirmed), not silent success.
func TestVaultClient_EnsureOIDCConfigReadBackMissingIssuer(t *testing.T) {
	t.Parallel()

	provGets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testProviderPath {
			switch r.Method {
			case http.MethodGet:
				provGets++
				if provGets == 1 {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				// Post-write read-back returns data but no issuer.
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{"allowed_client_ids":["*"]}}`))
				return
			case http.MethodPut:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{}}`))
				return
			}
		}
		if r.Method == http.MethodPut {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	apiConfig := vaultapi.DefaultConfig()
	apiConfig.Address = server.URL
	api, err := vaultapi.NewClient(apiConfig)
	if err != nil {
		t.Fatalf("new Vault API client: %v", err)
	}
	client := &vaultClient{api: api}

	_, err = client.EnsureOIDCConfig(context.Background(), "alpha", OIDCConfig{
		IssuerURL: testAlphaIssuer,
	})
	if err == nil {
		t.Fatal("expected an error when read-back returns no issuer")
	}
	if _, ok := errors.AsType[*OIDCIssuerConflictError](err); ok {
		t.Fatalf("missing issuer must be a verification error, not a conflict: %v", err)
	}
}

// TestExpectedOIDCProviderIssuer verifies the base issuer is turned into the
// effective provider issuer Vault reports, tolerating a trailing slash on the
// configured base.
func TestExpectedOIDCProviderIssuer(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		base string
		want string
	}{
		{
			name: "no trailing slash",
			base: "http://vault.vault.svc.cluster.local:8200",
			want: "http://vault.vault.svc.cluster.local:8200/v1/identity/oidc/provider/openchami",
		},
		{
			name: "trailing slash trimmed",
			base: "https://vault.example.com/",
			want: "https://vault.example.com/v1/identity/oidc/provider/openchami",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := expectedOIDCProviderIssuer(tc.base); got != tc.want {
				t.Errorf("expectedOIDCProviderIssuer(%q) = %q, want %q", tc.base, got, tc.want)
			}
		})
	}
}

// TestVaultClient_EnsureOIDCProviderIdempotentOnEffectiveIssuer is the core
// regression test for issue #62: a provider previously created by the operator
// with the configured BASE issuer is read back by Vault as the EFFECTIVE issuer
// (base + provider path). The operator must treat these as equivalent and NOT
// report a conflict on the next reconcile.
func TestVaultClient_EnsureOIDCProviderIdempotentOnEffectiveIssuer(t *testing.T) {
	t.Parallel()

	const base = "https://vault.example.com"
	providerWrites := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testProviderPath {
			switch r.Method {
			case http.MethodGet:
				// Provider already exists, created earlier by the operator with
				// the base issuer; Vault serves the effective issuer.
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{"issuer":"https://vault.example.com/v1/identity/oidc/provider/openchami","allowed_client_ids":["*"]}}`))
				return
			case http.MethodPut:
				providerWrites++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{}}`))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	apiConfig := vaultapi.DefaultConfig()
	apiConfig.Address = server.URL
	api, err := vaultapi.NewClient(apiConfig)
	if err != nil {
		t.Fatalf("new Vault API client: %v", err)
	}
	client := &vaultClient{api: api}

	if err := client.ensureOIDCProvider(context.Background(), OIDCConfig{IssuerURL: base}); err != nil {
		t.Fatalf("ensureOIDCProvider on operator-created provider must succeed, got: %v", err)
	}
	if providerWrites == 0 {
		t.Error("expected the provider fields to be reconciled (at least one write)")
	}
}

// TestVaultClient_EnsureOIDCProviderRealConflict asserts a genuine cross-Vault
// conflict is still detected: the effective issuer served by Vault has a
// DIFFERENT host than the configured base, so it must be an
// OIDCIssuerConflictError.
func TestVaultClient_EnsureOIDCProviderRealConflict(t *testing.T) {
	t.Parallel()

	const base = "https://vault-a.example.com"
	providerWrites := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testProviderPath {
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{"issuer":"https://vault-b.example.com/v1/identity/oidc/provider/openchami","allowed_client_ids":["*"]}}`))
				return
			case http.MethodPut:
				providerWrites++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{}}`))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	apiConfig := vaultapi.DefaultConfig()
	apiConfig.Address = server.URL
	api, err := vaultapi.NewClient(apiConfig)
	if err != nil {
		t.Fatalf("new Vault API client: %v", err)
	}
	client := &vaultClient{api: api}

	err = client.ensureOIDCProvider(context.Background(), OIDCConfig{IssuerURL: base})
	var conflict *OIDCIssuerConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected OIDCIssuerConflictError, got %v", err)
	}
	if conflict.Existing != "https://vault-b.example.com/v1/identity/oidc/provider/openchami" ||
		conflict.Requested != base+testProviderPath {
		t.Errorf("unexpected conflict detail: %+v", conflict)
	}
	if providerWrites != 0 {
		t.Errorf("provider must not be written on issuer conflict, got %d writes", providerWrites)
	}
}

// TestVaultClient_EnsureOIDCProviderCreateReconcileReconcile exercises the
// full lifecycle from issue #62: the provider does not exist, is created by the
// operator, and two subsequent reconciles must both accept the provider Vault
// now serves under its effective issuer.
func TestVaultClient_EnsureOIDCProviderCreateReconcileReconcile(t *testing.T) {
	t.Parallel()

	const base = "http://vault.vault.svc.cluster.local:8200"
	created := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testProviderPath {
			switch r.Method {
			case http.MethodGet:
				if !created {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				// Once created, Vault serves the effective issuer.
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{"issuer":"http://vault.vault.svc.cluster.local:8200/v1/identity/oidc/provider/openchami","allowed_client_ids":["*"]}}`))
				return
			case http.MethodPut:
				created = true
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{}}`))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	apiConfig := vaultapi.DefaultConfig()
	apiConfig.Address = server.URL
	api, err := vaultapi.NewClient(apiConfig)
	if err != nil {
		t.Fatalf("new Vault API client: %v", err)
	}
	client := &vaultClient{api: api}

	// Create, then reconcile twice; all three must succeed without a conflict.
	for i := 0; i < 3; i++ {
		if err := client.ensureOIDCProvider(context.Background(), OIDCConfig{IssuerURL: base}); err != nil {
			t.Fatalf("ensureOIDCProvider iteration %d must succeed, got: %v", i, err)
		}
	}
}

func assertJSONStrings(t *testing.T, got any, want []string) {
	t.Helper()
	values, ok := got.([]any)
	if !ok || len(values) != len(want) {
		t.Fatalf("got %T(%v), want %v", got, got, want)
	}
	for i := range want {
		if values[i] != want[i] {
			t.Errorf("value[%d] = %v, want %q", i, values[i], want[i])
		}
	}
}
