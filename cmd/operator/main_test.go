// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package main

import "testing"

func TestVaultConfigFromEnv(t *testing.T) {
	tests := []struct {
		name        string
		env         map[string]string
		wantOK      bool
		wantMethod  string
		wantRoleID  string
		wantSecret  string
		wantToken   string
		wantK8sRole string
	}{
		{
			name:   "no address disables vault",
			env:    map[string]string{},
			wantOK: false,
		},
		{
			name:       "defaults to kubernetes",
			env:        map[string]string{"VAULT_ADDR": "http://vault:8200", "VAULT_KUBERNETES_ROLE": "operator"},
			wantOK:     true,
			wantMethod: "kubernetes",
			wantK8sRole: "operator",
		},
		{
			name: "approle lowercase with VAULT_ROLE_ID/VAULT_SECRET_ID",
			env: map[string]string{
				"VAULT_ADDR":        "http://vault.vault.svc.cluster.local:8200",
				"VAULT_AUTH_METHOD": "approle",
				"VAULT_ROLE_ID":     "rid-123",
				"VAULT_SECRET_ID":   "sid-456",
			},
			wantOK:     true,
			wantMethod: "approle",
			wantRoleID: "rid-123",
			wantSecret: "sid-456",
		},
		{
			name: "appRole camelCase still works",
			env: map[string]string{
				"VAULT_ADDR":        "http://vault:8200",
				"VAULT_AUTH_METHOD": "appRole",
				"VAULT_ROLE_ID":     "rid",
				"VAULT_SECRET_ID":   "sid",
			},
			wantOK:     true,
			wantMethod: "appRole",
			wantRoleID: "rid",
			wantSecret: "sid",
		},
		{
			name: "app-role hyphenated works",
			env: map[string]string{
				"VAULT_ADDR":        "http://vault:8200",
				"VAULT_AUTH_METHOD": "app-role",
				"VAULT_ROLE_ID":     "rid",
				"VAULT_SECRET_ID":   "sid",
			},
			wantOK:     true,
			wantMethod: "app-role",
			wantRoleID: "rid",
			wantSecret: "sid",
		},
		{
			name: "legacy VAULT_APPROLE_* env vars still honored",
			env: map[string]string{
				"VAULT_ADDR":              "http://vault:8200",
				"VAULT_AUTH_METHOD":       "approle",
				"VAULT_APPROLE_ROLE_ID":   "legacy-rid",
				"VAULT_APPROLE_SECRET_ID": "legacy-sid",
			},
			wantOK:     true,
			wantMethod: "approle",
			wantRoleID: "legacy-rid",
			wantSecret: "legacy-sid",
		},
		{
			name: "VAULT_ROLE_ID takes precedence over legacy name",
			env: map[string]string{
				"VAULT_ADDR":            "http://vault:8200",
				"VAULT_AUTH_METHOD":     "approle",
				"VAULT_ROLE_ID":         "new-rid",
				"VAULT_APPROLE_ROLE_ID": "legacy-rid",
			},
			wantOK:     true,
			wantMethod: "approle",
			wantRoleID: "new-rid",
		},
		{
			name: "token auth",
			env: map[string]string{
				"VAULT_ADDR":        "http://vault:8200",
				"VAULT_AUTH_METHOD": "token",
				"VAULT_TOKEN":       "dev-root-token",
			},
			wantOK:     true,
			wantMethod: "token",
			wantToken:  "dev-root-token",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string { return tc.env[k] }
			cfg, ok := vaultConfigFromEnv(getenv)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if cfg.AuthMethod != tc.wantMethod {
				t.Errorf("AuthMethod = %q, want %q", cfg.AuthMethod, tc.wantMethod)
			}
			if cfg.AppRoleID != tc.wantRoleID {
				t.Errorf("AppRoleID = %q, want %q", cfg.AppRoleID, tc.wantRoleID)
			}
			if cfg.AppRoleSecretID != tc.wantSecret {
				t.Errorf("AppRoleSecretID = %q, want %q", cfg.AppRoleSecretID, tc.wantSecret)
			}
			if cfg.Token != tc.wantToken {
				t.Errorf("Token = %q, want %q", cfg.Token, tc.wantToken)
			}
			if cfg.K8sRole != tc.wantK8sRole {
				t.Errorf("K8sRole = %q, want %q", cfg.K8sRole, tc.wantK8sRole)
			}
		})
	}
}

func TestNormalizeVaultAuthMethod(t *testing.T) {
	cases := map[string]string{
		"kubernetes": "kubernetes",
		"Kubernetes": "kubernetes",
		"k8s":        "kubernetes",
		"approle":    "appRole",
		"appRole":    "appRole",
		"APPROLE":    "appRole",
		"app-role":   "appRole",
		"app_role":   "appRole",
		"token":      "token",
		"TOKEN":      "token",
		"bogus":      "bogus",
	}
	for in, want := range cases {
		if got := normalizeVaultAuthMethod(in); got != want {
			t.Errorf("normalizeVaultAuthMethod(%q) = %q, want %q", in, got, want)
		}
	}
}
