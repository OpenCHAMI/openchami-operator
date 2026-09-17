// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package vault

import "testing"

func TestNormalizeAuthMethod(t *testing.T) {
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
		"":           "",
		"bogus":      "bogus",
	}
	for in, want := range cases {
		if got := normalizeAuthMethod(in); got != want {
			t.Errorf("normalizeAuthMethod(%q) = %q, want %q", in, got, want)
		}
	}
}
