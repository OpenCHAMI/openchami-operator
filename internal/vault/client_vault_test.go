// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package vault

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"
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
		IssuerURL:       "https://alpha.example.test",
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
	provider := writes["/v1/identity/oidc/provider/openchami"]
	if provider == nil {
		t.Fatal("expected a write to the named openchami provider")
	}
	if provider["issuer"] != "https://alpha.example.test" {
		t.Errorf("provider issuer = %v, want https://alpha.example.test", provider["issuer"])
	}
	assertJSONStrings(t, provider["allowed_client_ids"], []string{"*"})
	assertJSONStrings(t, provider["scopes_supported"], []string{"groups"})
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
