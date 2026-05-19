// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"errors"
	"strings"
	"testing"

	cmv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	egv1alpha1 "github.com/envoyproxy/gateway/api/v1alpha1"
	vsov1beta1 "github.com/hashicorp/vault-secrets-operator/api/v1beta1"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/vault"
	vaultfake "github.com/openchami/openchami-operator/internal/vault/fake"
)

// Shared fixture cluster names used across reconciler tests. Extracted to
// satisfy goconst across sibling test files.
const (
	testControlPlaneRed  = "red"
	testControlPlaneBlue = "blue"
	testClusterAlpha     = "alpha"

	// Shared test fixture values used across the network/coredhcp/magellan
	// test files. Extracted to satisfy goconst.
	testProvisionSubnet = "10.0.0.0/24"
	testBMCSubnet       = "10.1.0.0/24"
	testNodeRoleKey     = "node-role"
	testNodeRoleDHCP    = "dhcp"
	testNodeRoleBMC     = "bmc"
	testPriorityClass   = "system-node-critical"
	testProbeLabelTrue  = "true"
	testNodeAName       = "node-a"
	testProbeContainer  = "probe"

	// testS3Endpoint is the placeholder ObjectStorage.Endpoint baked into
	// newControlPlane(). Centralised here so test asserts can reference the
	// same value without tripping goconst.
	testS3Endpoint = "http://s3.test:9000"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgo scheme: %v", err)
	}
	if err := openchamiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding openchami scheme: %v", err)
	}
	if err := vsov1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding vso scheme: %v", err)
	}
	if err := gwapiv1.Install(scheme); err != nil {
		t.Fatalf("adding gateway-api scheme: %v", err)
	}
	if err := cmv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding cert-manager scheme: %v", err)
	}
	if err := egv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding envoy gateway scheme: %v", err)
	}
	if err := monitoringv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding prometheus-operator monitoring scheme: %v", err)
	}
	return scheme
}

func newControlPlane(name string) *openchamiv1alpha1.OpenCHAMIControlPlane {
	return &openchamiv1alpha1.OpenCHAMIControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: 1},
		Spec: openchamiv1alpha1.OpenCHAMIControlPlaneSpec{
			ClusterName: name,
			Domain:      name + ".test.local",
			Platform: openchamiv1alpha1.PlatformSpec{
				Vault: openchamiv1alpha1.VaultSpec{
					Address:    "http://vault.test:8200",
					AuthMethod: openchamiv1alpha1.VaultAuthMethodKubernetes,
				},
				ObjectStorage: openchamiv1alpha1.ObjectStorageSpec{
					Endpoint: testS3Endpoint,
				},
			},
			// Mirror the post-admission default-enabled state so reconciler
			// unit tests reflect what the API server hands the controller in
			// production. Tests that want a service disabled override this.
			Services: openchamiv1alpha1.ServicesSpec{
				SMD: openchamiv1alpha1.SMDSpec{
					ServiceDefaults: openchamiv1alpha1.ServiceDefaults{Enabled: true},
				},
				Tokensmith: openchamiv1alpha1.TokensmithSpec{
					ServiceDefaults: openchamiv1alpha1.ServiceDefaults{Enabled: true},
					OIDCProvider:    "vault",
				},
				BootService: openchamiv1alpha1.BootServiceSpec{
					ServiceDefaults: openchamiv1alpha1.ServiceDefaults{Enabled: true},
				},
				MetadataService: openchamiv1alpha1.MetadataServiceSpec{
					ServiceDefaults: openchamiv1alpha1.ServiceDefaults{Enabled: true},
				},
			},
		},
	}
}

func TestVaultReconciler_HappyPath(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	v := vaultfake.NewClient()

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	paths := vault.Paths("alpha")
	v.AssertCalled(t, "IsReachable")
	v.AssertCalled(t, "EnsureKVMount")
	v.AssertSecretExists(t, paths.DBSMDCredentials)
	v.AssertSecretExists(t, paths.DBBootServiceCredentials)
	v.AssertSecretExists(t, paths.S3Credentials)
	v.AssertSecretExists(t, paths.LogCredentials)
	v.AssertSecretExists(t, paths.TokensmithOIDC)
	v.AssertPolicyExists(t, paths.PolicyServices)
	v.AssertCalled(t, "EnsureKubernetesRole")
	v.AssertCalled(t, "EnsureOIDCConfig")

	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionVaultConfigured)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected VaultConfigured=True, got %+v", cond)
	}
	if cp.Status.VaultPathPrefix != paths.SecretPrefix {
		t.Errorf("expected VaultPathPrefix=%q, got %q", paths.SecretPrefix, cp.Status.VaultPathPrefix)
	}
}

// TestVaultReconciler_OIDCIssuerHasNoPath is a regression test for the bug
// fixed on 2026-05-04: the reconciler used to build
// `https://<domain>/oidc/<clusterName>` and pass it to Vault's
// identity/oidc/config, which Vault rejects with
// "invalid issuer, which must include only a scheme, host, and optional port".
// Vault hosts the issuer at `<base>/v1/identity/oidc/...` itself, so the
// operator must pass only the base URL. The cluster-name partition is carried
// by the OIDC key (`openchami-<clusterName>`), not the issuer URL.
func TestVaultReconciler_OIDCIssuerHasNoPath(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	v := vaultfake.NewClient()

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	issuer, ok := v.OIDCConfigs[cp.Spec.ClusterName]
	if !ok {
		t.Fatalf("expected OIDCConfig recorded for cluster %q, got %+v", cp.Spec.ClusterName, v.OIDCConfigs)
	}
	want := "https://" + cp.Spec.Domain
	if issuer != want {
		t.Errorf("expected issuer = %q (scheme + host only), got %q", want, issuer)
	}
	if strings.Contains(strings.TrimPrefix(issuer, "https://"), "/") {
		t.Errorf("issuer URL must not contain a path component (Vault rejects it), got %q", issuer)
	}
}

func TestVaultReconciler_Unreachable(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("beta")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	v := vaultfake.NewClient()
	v.Errors["IsReachable"] = errors.New("connection refused")

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue when vault unreachable, got %+v", res)
	}

	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionVaultConfigured)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonUnreachable {
		t.Fatalf("expected VaultConfigured=False/Unreachable, got %+v", cond)
	}
	v.AssertNotCalled(t, "EnsureKVMount")
}

func TestVaultReconciler_NoOverwriteExistingSecrets(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("gamma")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	v := vaultfake.NewClient()

	paths := vault.Paths("gamma")
	v.Secrets[paths.DBSMDCredentials] = map[string]any{VaultKeyDBPassword: "original-value"}

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := v.Secrets[paths.DBSMDCredentials]
	if got[VaultKeyDBPassword] != "original-value" {
		t.Errorf("expected original DB password preserved, got %v", got[VaultKeyDBPassword])
	}
}

func TestVaultReconciler_TwoClustersIsolated(t *testing.T) {
	scheme := newScheme(t)
	v := vaultfake.NewClient()
	rec := record.NewFakeRecorder(20)

	for _, name := range []string{testControlPlaneRed, testControlPlaneBlue} {
		cp := newControlPlane(name)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
		r := &VaultReconciler{Client: c, Recorder: rec, VaultClient: v}
		if _, err := r.Reconcile(context.Background(), cp); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}
	}

	red := vault.Paths(testControlPlaneRed)
	blue := vault.Paths(testControlPlaneBlue)
	if red.SecretPrefix == blue.SecretPrefix {
		t.Fatalf("two clusters share secret prefix %q", red.SecretPrefix)
	}
	v.AssertSecretExists(t, red.DBSMDCredentials)
	v.AssertSecretExists(t, blue.DBSMDCredentials)
	if v.Secrets[red.DBSMDCredentials][VaultKeyDBPassword] == v.Secrets[blue.DBSMDCredentials][VaultKeyDBPassword] {
		t.Errorf("expected distinct random passwords across clusters")
	}
}

// TestVaultStaticSecretSuffixesAllResolveToNonEmptyPaths is a regression test
// for the parallel-list bug fixed on 2026-05-04: vssNames and pathByName were
// two separate sources of truth, so adding a suffix to one without the other
// silently produced a VaultStaticSecret with an empty `path:` field.
//
// vssEntries is now the single source of truth and this test asserts every
// entry's PathFn returns a non-empty path under a sample cluster's Paths.
func TestVaultStaticSecretSuffixesAllResolveToNonEmptyPaths(t *testing.T) {
	paths := vault.Paths("audit-cluster")
	if len(vssEntries) == 0 {
		t.Fatal("vssEntries is empty — at least the per-service DB credentials must be present")
	}
	seen := make(map[string]struct{}, len(vssEntries))
	for _, e := range vssEntries {
		if _, dup := seen[e.Suffix]; dup {
			t.Errorf("duplicate suffix %q in vssEntries", e.Suffix)
		}
		seen[e.Suffix] = struct{}{}

		got := e.PathFn(paths)
		if got == "" {
			t.Errorf("PathFn for suffix %q returned empty path", e.Suffix)
		}
		// Every cluster-scoped path must include the cluster name to honour
		// invariant 3 (Vault path isolation).
		if got != "" && !strings.Contains(got, "audit-cluster") {
			t.Errorf("PathFn for suffix %q returned %q which does not include the cluster name (invariant 3)", e.Suffix, got)
		}
	}
}

// TestBuildVaultStaticSecret_UnknownSuffixPanics asserts that a typo in a
// caller passing an unrecognised suffix is caught loudly rather than silently
// producing an empty-path VSS. The runtime panic is the safety net behind
// vssEntries being the single source of truth.
func TestBuildVaultStaticSecret_UnknownSuffixPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for unknown suffix")
		}
	}()
	r := &VaultReconciler{}
	cp := newControlPlane("audit-cluster")
	_ = r.buildVaultStaticSecret(cp, "no-such-suffix", vault.Paths("audit-cluster"))
}
