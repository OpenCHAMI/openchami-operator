// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestBootstrapTokenSpecsAreDistinct is a low-cost invariant guard
// against a future refactor that accidentally aliases the per-service
// bootstrap-token specs.
//
// boot-service and metadata-service must NEVER share a bootstrap token.
// tokensmith bootstrap tokens are single-use and subject-scoped: the
// `sub` claim baked into the token is what tokensmith's policy engine
// uses to authorize the exchanged service-token's scopes. If
// boot-service ever consumed metadata-service's bootstrap (or vice
// versa) we'd either over-scope one service or wedge both into
// CrashLoopBackOff with "already consumed" — both bad in different
// ways. The Secret-name, subject, and consumer-Deployment-name fields
// of the two specs are therefore required to differ.
func TestBootstrapTokenSpecsAreDistinct(t *testing.T) {
	if bootServiceBootstrap.secretSuffix == metadataServiceBootstrap.secretSuffix {
		t.Errorf("bootstrap-token Secret suffix collision: both specs use %q — "+
			"each service must get its own per-cluster Secret",
			bootServiceBootstrap.secretSuffix)
	}
	if bootServiceBootstrap.subject == metadataServiceBootstrap.subject {
		t.Errorf("bootstrap-token subject collision: both specs use %q — "+
			"tokensmith policy lookups are keyed by subject; aliasing would "+
			"hand metadata-service boot-service's write scopes (or vice versa)",
			bootServiceBootstrap.subject)
	}
	if bootServiceBootstrap.appName == metadataServiceBootstrap.appName {
		t.Errorf("bootstrap-token consumer Deployment name collision: both specs "+
			"use %q — the per-service Secret labels would collide", bootServiceBootstrap.appName)
	}
}

func TestIsBootstrapTokenFresh(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name      string
		mintedAt  string
		hasToken  bool
		wantFresh bool
	}{
		{
			name:      "fresh — minted just now",
			mintedAt:  now.Format(time.RFC3339),
			hasToken:  true,
			wantFresh: true,
		},
		{
			name:      "fresh — just inside the refresh window",
			mintedAt:  now.Add(-(bootstrapTokenRefreshAge - 30*time.Second)).Format(time.RFC3339),
			hasToken:  true,
			wantFresh: true,
		},
		{
			name:      "stale — older than the refresh window",
			mintedAt:  now.Add(-(bootstrapTokenRefreshAge + time.Minute)).Format(time.RFC3339),
			hasToken:  true,
			wantFresh: false,
		},
		{
			name:      "stale — missing mintedAt annotation",
			mintedAt:  "",
			hasToken:  true,
			wantFresh: false,
		},
		{
			name:      "stale — unparseable timestamp",
			mintedAt:  "not-a-timestamp",
			hasToken:  true,
			wantFresh: false,
		},
		{
			name:      "stale — token data missing even if annotation looks valid",
			mintedAt:  now.Format(time.RFC3339),
			hasToken:  false,
			wantFresh: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
				Data: map[string][]byte{},
			}
			if tc.mintedAt != "" {
				s.Annotations[BootstrapTokenMintedAtAnnotation] = tc.mintedAt
			}
			if tc.hasToken {
				s.Data[BootstrapTokenKey] = []byte("opaque-token-bytes")
			}
			got := isBootstrapTokenFresh(s)
			if got != tc.wantFresh {
				t.Errorf("isBootstrapTokenFresh: got %v, want %v", got, tc.wantFresh)
			}
		})
	}
}

// TestProvisionServiceBootstrapTokens_SkipsWhenNoRESTConfig
// documents the test-mode behaviour: a reconciler with nil RESTConfig
// (i.e. envtest / unit-test paths) must not attempt the pods/exec
// dance. The provisioner returns nil immediately and does not mutate
// any cluster state.
func TestProvisionServiceBootstrapTokens_SkipsWhenNoRESTConfig(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &TokensmithReconciler{
		Client:   c,
		Recorder: record.NewFakeRecorder(10),
		// RESTConfig deliberately nil.
	}
	if err := r.provisionServiceBootstrapTokens(context.Background(), cp); err != nil {
		t.Fatalf("expected nil return when RESTConfig is unset (unit-test mode), got %v", err)
	}
}
