// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package vault

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
)

const (
	testOIDCConfigPath   = "/v1/identity/oidc/config"
	testOIDCProviderPath = "/v1/identity/oidc/provider/default"
	testCLIRedirectURI   = "http://127.0.0.1:8250/callback"
	testAdminClientID    = "admin-client"
	testClientIDKey      = "client_id"
)

func TestVaultClient_EnsureOIDCConfigCreatesConfidentialAndPublicClients(t *testing.T) {
	t.Parallel()

	writes := map[string]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == testOIDCConfigPath {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{}}`))
			return
		}
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
		if r.Method == http.MethodGet && r.URL.Path == testOIDCProviderPath {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"allowed_client_ids":["admin-client"]}}`))
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
		CLIRedirectURIs: []string{testCLIRedirectURI},
		CLIAssignments:  []string{vaultOIDCDefaultAssignment},
	})
	if err != nil {
		t.Fatalf("EnsureOIDCConfig: %v", err)
	}
	if credentials.ClientID != "tokensmith-id" || credentials.ClientSecret != "tokensmith-secret" {
		t.Fatalf("unexpected TokenSmith credentials: %+v", credentials)
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
	assertJSONStrings(t, cli["redirect_uris"], []string{testCLIRedirectURI})
	assertJSONStrings(t, cli["assignments"], []string{vaultOIDCDefaultAssignment})
	provider := writes[testOIDCProviderPath]
	assertJSONStrings(t, provider["allowed_client_ids"], []string{testAdminClientID, "cli-id"})
}

func TestVaultClient_EnsureOIDCConfigRejectsConflictingIssuer(t *testing.T) {
	t.Parallel()

	writes := map[string]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == testOIDCConfigPath {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"issuer":"https://vault-a.example.test"}}`))
			return
		}
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

	_, err = client.EnsureOIDCConfig(context.Background(), "beta", OIDCConfig{
		IssuerURL:       "https://vault-b.example.test",
		CLIRedirectURIs: []string{testCLIRedirectURI},
		CLIAssignments:  []string{vaultOIDCDefaultAssignment},
	})
	if err == nil {
		t.Fatal("expected issuer conflict")
	}
	if _, ok := writes[testOIDCConfigPath]; ok {
		t.Fatal("conflict must not overwrite identity/oidc/config")
	}
	if _, ok := writes["/v1/identity/oidc/key/openchami-beta"]; ok {
		t.Fatal("conflict must stop before cluster OIDC resources are mutated")
	}
}

func TestVaultClient_EnsureOIDCConfigSerializesProviderUpdates(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	oidcIssuer := ""
	allowedClientIDs := []string{testAdminClientID}
	firstProviderWriteStarted := make(chan struct{})
	releaseFirstProviderWrite := make(chan struct{})
	var firstProviderWrite sync.Once

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == testOIDCConfigPath {
			mu.Lock()
			issuer := oidcIssuer
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"issuer":"` + issuer + `"}}`))
			return
		}

		if r.Method == http.MethodPut && r.URL.Path == testOIDCConfigPath {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode %s: %v", r.URL.Path, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			oidcIssuer, _ = body["issuer"].(string)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{}}`))
			return
		}

		if r.Method == http.MethodGet && r.URL.Path == testOIDCProviderPath {
			mu.Lock()
			ids := append([]string(nil), allowedClientIDs...)
			mu.Unlock()
			writeVaultData(w, map[string]any{"allowed_client_ids": ids})
			return
		}

		if r.Method == http.MethodPut && r.URL.Path == testOIDCProviderPath {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode %s: %v", r.URL.Path, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			waitForRelease := false
			firstProviderWrite.Do(func() {
				waitForRelease = true
				close(firstProviderWriteStarted)
			})
			if waitForRelease {
				<-releaseFirstProviderWrite
			}
			ids, ok := body["allowed_client_ids"].([]any)
			if !ok {
				t.Errorf("allowed_client_ids = %T(%v), want JSON array", body["allowed_client_ids"], body["allowed_client_ids"])
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			allowedClientIDs = allowedClientIDs[:0]
			for _, id := range ids {
				if s, ok := id.(string); ok {
					allowedClientIDs = append(allowedClientIDs, s)
				}
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{}}`))
			return
		}

		if r.Method == http.MethodGet {
			switch r.URL.Path {
			case "/v1/identity/oidc/client/openchami-alpha-tokensmith":
				writeVaultData(w, map[string]any{testClientIDKey: "alpha-tokensmith-id", "client_secret": "alpha-secret"})
				return
			case "/v1/identity/oidc/client/openchami-alpha-cli":
				writeVaultData(w, map[string]any{testClientIDKey: "alpha-cli-id"})
				return
			case "/v1/identity/oidc/client/openchami-beta-tokensmith":
				writeVaultData(w, map[string]any{testClientIDKey: "beta-tokensmith-id", "client_secret": "beta-secret"})
				return
			case "/v1/identity/oidc/client/openchami-beta-cli":
				writeVaultData(w, map[string]any{testClientIDKey: "beta-cli-id"})
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

	errs := make(chan error, 2)
	go func() {
		errs <- ensureTestOIDCConfig(client, "alpha")
	}()
	select {
	case <-firstProviderWriteStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first provider write")
	}

	go func() {
		errs <- ensureTestOIDCConfig(client, "beta")
	}()
	time.Sleep(25 * time.Millisecond)
	close(releaseFirstProviderWrite)

	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("EnsureOIDCConfig: %v", err)
		}
	}

	mu.Lock()
	ids := append([]string(nil), allowedClientIDs...)
	mu.Unlock()
	for _, want := range []string{testAdminClientID, "alpha-cli-id", "beta-cli-id"} {
		if !slices.Contains(ids, want) {
			t.Fatalf("expected allowed_client_ids to contain %q, got %v", want, ids)
		}
	}
}

func ensureTestOIDCConfig(client *vaultClient, clusterName string) error {
	_, err := client.EnsureOIDCConfig(context.Background(), clusterName, OIDCConfig{
		IssuerURL:       "https://vault.example.test",
		CLIRedirectURIs: []string{testCLIRedirectURI},
		CLIAssignments:  []string{vaultOIDCDefaultAssignment},
	})
	return err
}

func writeVaultData(w http.ResponseWriter, data map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
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
