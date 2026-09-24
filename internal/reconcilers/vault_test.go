// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	cmv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	egv1alpha1 "github.com/envoyproxy/gateway/api/v1alpha1"
	vsov1beta1 "github.com/hashicorp/vault-secrets-operator/api/v1beta1"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
					OIDCProvider:    tokensmithOIDCProviderVault,
					CLIOIDC: openchamiv1alpha1.CLIOIDCConfig{
						RedirectURIs: []string{openchamiv1alpha1.DefaultTokensmithCLIRedirectURI},
						Assignments:  []string{openchamiv1alpha1.DefaultTokensmithCLIAssignment},
					},
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
	// The issuer must be the Vault address scheme+host (no path) — the same
	// source tokensmith derives TOKENSMITH_OIDC_PROVIDER from — NOT spec.domain.
	want := VaultOIDCIssuerBase(cp)
	if issuer != want {
		t.Errorf("expected issuer = %q (Vault address scheme+host), got %q", want, issuer)
	}
	if strings.Contains(strings.TrimPrefix(strings.TrimPrefix(issuer, "https://"), "http://"), "/") {
		t.Errorf("issuer URL must not contain a path component (Vault rejects it), got %q", issuer)
	}
}

// TestVaultReconciler_OIDCIssuerMatchesTokensmithProvider is the regression
// test for the iss mismatch found in end-to-end Vault→tokensmith testing: the
// Vault config issuer used to be derived from spec.domain while tokensmith's
// TOKENSMITH_OIDC_PROVIDER was derived from the Vault address, so the minted
// `iss` never matched what tokensmith validated against and every exchange
// failed. Both sides must now resolve to the SAME issuer.
func TestVaultReconciler_OIDCIssuerMatchesTokensmithProvider(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	v := vaultfake.NewClient()

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	vaultConfigIssuer := v.OIDCConfigs[cp.Spec.ClusterName]
	// Vault appends the named provider path to the provider issuer when minting `iss`.
	mintedISS := vaultConfigIssuer + "/v1/identity/oidc/provider/openchami"

	// What tokensmith is configured to expect.
	tokensmithProvider := VaultOIDCProviderURL(cp)

	if mintedISS != tokensmithProvider {
		t.Errorf("issuer mismatch: Vault mints iss=%q but tokensmith expects %q",
			mintedISS, tokensmithProvider)
	}
}

// TestVaultReconciler_OIDCClientCredentials is a regression test for issue #38:
// when oidcProvider=vault the operator must provision a Vault OIDC client and
// store BOTH the generated client_id and client_secret in the tokensmith OIDC
// KV path so VSO materializes them into the tokensmith Secret. Previously only
// client_secret was written, leaving OIDC_CLIENT_ID unresolvable.
func TestVaultReconciler_OIDCClientCredentials(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	v := vaultfake.NewClient()

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	paths := vault.Paths("alpha")
	data, err := v.ReadSecret(context.Background(), paths.TokensmithOIDC)
	if err != nil {
		t.Fatalf("reading tokensmith oidc secret: %v", err)
	}
	if data == nil {
		t.Fatalf("expected tokensmith oidc secret at %q", paths.TokensmithOIDC)
	}
	clientID, _ := data["client_id"].(string)
	if clientID == "" {
		t.Errorf("expected non-empty client_id in %q, got %+v", paths.TokensmithOIDC, data)
	}
	clientSecret, _ := data["client_secret"].(string)
	if clientSecret == "" {
		t.Errorf("expected non-empty client_secret in %q, got %+v", paths.TokensmithOIDC, data)
	}
	// The credentials must match what EnsureOIDCConfig returned for the cluster.
	want := v.OIDCClients[cp.Spec.ClusterName]
	if clientID != want.ClientID || clientSecret != want.ClientSecret {
		t.Errorf("stored credentials %q/%q do not match Vault OIDC client %q/%q",
			clientID, clientSecret, want.ClientID, want.ClientSecret)
	}
}

func TestVaultReconciler_ProvisionsSeparatePublicCLIClient(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.Tokensmith.CLIOIDC = openchamiv1alpha1.CLIOIDCConfig{
		RedirectURIs: []string{"https://cli.alpha.test/callback"},
		Assignments:  []string{"alpha-operators"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	v := vaultfake.NewClient()

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	cli, ok := v.PublicOIDCClients[cp.Spec.ClusterName]
	if !ok {
		t.Fatalf("expected public CLI client for %q", cp.Spec.ClusterName)
	}
	if cli.Name != "openchami-alpha-cli" || cli.ClientID == "" {
		t.Errorf("unexpected public CLI client: %+v", cli)
	}
	if len(cli.RedirectURIs) != 1 || cli.RedirectURIs[0] != "https://cli.alpha.test/callback" {
		t.Errorf("CLI redirect URIs = %v", cli.RedirectURIs)
	}
	if len(cli.Assignments) != 1 || cli.Assignments[0] != "alpha-operators" {
		t.Errorf("CLI assignments = %v", cli.Assignments)
	}
	if cli.ClientID == v.OIDCClients[cp.Spec.ClusterName].ClientID {
		t.Error("public CLI client_id must differ from the confidential TokenSmith client_id")
	}
}

// TestVaultReconciler_OIDCProviderUsesWildcard asserts the shared named
// provider is configured with a wildcard allowed_client_ids rather than a
// per-control-plane client list. This is the fix for the #57 lost-update race:
// there is no shared mutable list to read-modify-write, so concurrent control
// planes cannot remove each other's clients. Access is scoped per-client via
// assignments instead.
func TestVaultReconciler_OIDCProviderUsesWildcard(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	v := vaultfake.NewClient()

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(v.OIDCProviderAllowedClientIDs) != 1 || v.OIDCProviderAllowedClientIDs[0] != "*" {
		t.Errorf("expected provider allowed_client_ids=[*], got %v", v.OIDCProviderAllowedClientIDs)
	}
	if v.OIDCProviderIssuer != VaultOIDCIssuerBase(cp) {
		t.Errorf("expected provider issuer %q, got %q", VaultOIDCIssuerBase(cp), v.OIDCProviderIssuer)
	}
}

// TestVaultReconciler_OIDCConcurrentReconcileConverges is the regression test
// for issue #57. Two control planes sharing one Vault reconcile concurrently;
// because the shared provider is written with a fixed wildcard payload (never a
// read-modify-write on a per-control-plane list), the final provider state is
// deterministic and neither control plane's clients are lost. Each control
// plane's own confidential + public clients are still provisioned.
func TestVaultReconciler_OIDCConcurrentReconcileConverges(t *testing.T) {
	scheme := newScheme(t)
	cpA := newControlPlane("alpha")
	cpB := newControlPlane("beta")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cpA, cpB).Build()
	// A single shared fake Vault, as when both control planes point at one Vault.
	v := vaultfake.NewClient()

	rA := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	rB := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}

	var wg sync.WaitGroup
	for range 25 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := rA.Reconcile(context.Background(), cpA.DeepCopy()); err != nil {
				t.Errorf("reconcile alpha: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := rB.Reconcile(context.Background(), cpB.DeepCopy()); err != nil {
				t.Errorf("reconcile beta: %v", err)
			}
		}()
	}
	wg.Wait()

	// Regardless of interleaving, the shared provider is always the fixed
	// wildcard — no lost update is possible.
	if len(v.OIDCProviderAllowedClientIDs) != 1 || v.OIDCProviderAllowedClientIDs[0] != "*" {
		t.Errorf("expected provider allowed_client_ids=[*] after concurrent reconciles, got %v",
			v.OIDCProviderAllowedClientIDs)
	}
	// Both control planes' clients survive: neither was removed by the other.
	if _, ok := v.PublicOIDCClients["alpha"]; !ok {
		t.Error("alpha CLI client missing after concurrent reconcile")
	}
	if _, ok := v.PublicOIDCClients["beta"]; !ok {
		t.Error("beta CLI client missing after concurrent reconcile")
	}
	if _, ok := v.OIDCClients["alpha"]; !ok {
		t.Error("alpha tokensmith client missing after concurrent reconcile")
	}
	if _, ok := v.OIDCClients["beta"]; !ok {
		t.Error("beta tokensmith client missing after concurrent reconcile")
	}
}

// TestVaultReconciler_OIDCProviderIdempotent asserts repeated reconciles keep
// the shared provider at the fixed wildcard (no drift, no duplication).
func TestVaultReconciler_OIDCProviderIdempotent(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	v := vaultfake.NewClient()

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	for range 3 {
		if _, err := r.Reconcile(context.Background(), cp); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	if len(v.OIDCProviderAllowedClientIDs) != 1 || v.OIDCProviderAllowedClientIDs[0] != "*" {
		t.Errorf("expected provider allowed_client_ids=[*] after repeated reconciles, got %v",
			v.OIDCProviderAllowedClientIDs)
	}
}

// TestVaultReconciler_OIDCCLIClientIDPersisted asserts the public CLI client_id
// is written to the CLI OIDC KV path so CLI tooling can discover it. The public
// client has no secret, so only client_id is stored.
func TestVaultReconciler_OIDCCLIClientIDPersisted(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	v := vaultfake.NewClient()

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	paths := vault.Paths("alpha")
	data, err := v.ReadSecret(context.Background(), paths.CLIOIDC)
	if err != nil {
		t.Fatalf("reading cli oidc secret: %v", err)
	}
	if data == nil {
		t.Fatalf("expected cli oidc secret at %q", paths.CLIOIDC)
	}
	clientID, _ := data["client_id"].(string)
	want := v.PublicOIDCClients["alpha"].ClientID
	if clientID == "" || clientID != want {
		t.Errorf("cli client_id = %q, want %q", clientID, want)
	}
	if _, hasSecret := data["client_secret"]; hasSecret {
		t.Error("public CLI KV entry must not contain a client_secret")
	}
}

// TestVaultReconciler_OIDCIssuerSeparateFromAddress asserts that when
// spec.platform.vault.oidcIssuer is set, the operator still dials Vault at
// spec.platform.vault.address, but the OIDC provider issuer and tokensmith's
// TOKENSMITH_OIDC_PROVIDER are derived from oidcIssuer instead.
func TestVaultReconciler_OIDCIssuerSeparateFromAddress(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Platform.Vault.Address = "https://vault.vault.svc.cluster.local:8200"
	cp.Spec.Platform.Vault.OIDCIssuer = "https://vault.example.org"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	v := vaultfake.NewClient()

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Operator dial address is unchanged.
	if got := VaultAddress(cp); got != "https://vault.vault.svc.cluster.local:8200" {
		t.Errorf("VaultAddress = %q, want the .svc dial address", got)
	}
	// Provider issuer comes from oidcIssuer, not address.
	if v.OIDCProviderIssuer != "https://vault.example.org" {
		t.Errorf("provider issuer = %q, want https://vault.example.org", v.OIDCProviderIssuer)
	}
	if got := VaultOIDCIssuerBase(cp); got != "https://vault.example.org" {
		t.Errorf("VaultOIDCIssuerBase = %q, want https://vault.example.org", got)
	}
	// TokenSmith validates against the oidcIssuer-derived provider URL.
	wantProvider := "https://vault.example.org/v1/identity/oidc/provider/openchami"
	if got := VaultOIDCProviderURL(cp); got != wantProvider {
		t.Errorf("VaultOIDCProviderURL = %q, want %q", got, wantProvider)
	}
}

// TestVaultReconciler_OIDCIssuerFallsBackToAddress asserts that when oidcIssuer
// is omitted the issuer falls back to the Vault address, preserving the
// pre-oidcIssuer behavior.
func TestVaultReconciler_OIDCIssuerFallsBackToAddress(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Platform.Vault.Address = "https://vault.example.test:8200"
	cp.Spec.Platform.Vault.OIDCIssuer = ""
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	v := vaultfake.NewClient()

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if v.OIDCProviderIssuer != "https://vault.example.test:8200" {
		t.Errorf("provider issuer = %q, want the Vault address (fallback)", v.OIDCProviderIssuer)
	}
	if got := VaultOIDCIssuerBase(cp); got != "https://vault.example.test:8200" {
		t.Errorf("VaultOIDCIssuerBase = %q, want the Vault address (fallback)", got)
	}
}

// TestVaultReconciler_OIDCSharedIssuerAgreement asserts two control planes
// sharing one Vault that agree on the OIDC issuer both reconcile successfully.
func TestVaultReconciler_OIDCSharedIssuerAgreement(t *testing.T) {
	scheme := newScheme(t)
	cpA := newControlPlane("alpha")
	cpB := newControlPlane("beta")
	// Both advertise the same canonical issuer even though they could dial
	// Vault differently.
	cpA.Spec.Platform.Vault.OIDCIssuer = "https://vault.example.org"
	cpB.Spec.Platform.Vault.OIDCIssuer = "https://vault.example.org"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cpA, cpB).Build()
	v := vaultfake.NewClient()

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	if _, err := r.Reconcile(context.Background(), cpA); err != nil {
		t.Fatalf("reconcile alpha: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), cpB); err != nil {
		t.Fatalf("reconcile beta: %v", err)
	}
	if v.OIDCProviderIssuer != "https://vault.example.org" {
		t.Errorf("provider issuer = %q, want https://vault.example.org", v.OIDCProviderIssuer)
	}
	// Both control planes' clients exist.
	if _, ok := v.OIDCClients["alpha"]; !ok {
		t.Error("alpha tokensmith client missing")
	}
	if _, ok := v.OIDCClients["beta"]; !ok {
		t.Error("beta tokensmith client missing")
	}
}

// TestVaultReconciler_OIDCConflictingIssuerRejected asserts that a second
// control plane requesting a different issuer than the one already pinned on
// the shared provider fails with a clear error and does NOT change the issuer.
func TestVaultReconciler_OIDCConflictingIssuerRejected(t *testing.T) {
	scheme := newScheme(t)
	cpA := newControlPlane("alpha")
	cpB := newControlPlane("beta")
	cpA.Spec.Platform.Vault.OIDCIssuer = "https://vault-a.example.org"
	cpB.Spec.Platform.Vault.OIDCIssuer = "https://vault-b.example.org"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cpA, cpB).Build()
	v := vaultfake.NewClient()

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	// A wins first and pins the provider issuer.
	if _, err := r.Reconcile(context.Background(), cpA); err != nil {
		t.Fatalf("reconcile alpha: %v", err)
	}
	// B disagrees: must fail, and must not have changed the pinned issuer.
	_, err := r.Reconcile(context.Background(), cpB)
	if err == nil {
		t.Fatal("expected beta reconcile to fail on issuer conflict")
	}
	if v.OIDCProviderIssuer != "https://vault-a.example.org" {
		t.Errorf("provider issuer changed to %q; must remain the first-pinned issuer", v.OIDCProviderIssuer)
	}
	cond := apimeta.FindStatusCondition(cpB.Status.Conditions, conditions.ConditionVaultConfigured)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected VaultConfigured=False on conflict, got %+v", cond)
	}
	if !strings.Contains(cond.Message, "issuer") {
		t.Errorf("expected conflict message to mention issuer, got %q", cond.Message)
	}
}

// TestVaultReconciler_OIDCPreexistingConflictingProvider asserts that if the
// shared provider already exists with a conflicting issuer (set by an admin or
// another control plane before this one reconciled), reconciliation fails and
// the existing provider is not modified.
func TestVaultReconciler_OIDCPreexistingConflictingProvider(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Platform.Vault.OIDCIssuer = "https://vault-new.example.org"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	v := vaultfake.NewClient()
	// Seed a pre-existing provider with a different issuer.
	v.OIDCProviderIssuer = "https://vault-existing.example.org"

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	_, err := r.Reconcile(context.Background(), cp)
	if err == nil {
		t.Fatal("expected reconcile to fail against a pre-existing conflicting provider")
	}
	if v.OIDCProviderIssuer != "https://vault-existing.example.org" {
		t.Errorf("existing provider issuer was modified to %q", v.OIDCProviderIssuer)
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

// newAppRoleControlPlane returns a cluster configured for appRole auth with a
// conventional AppRoleSecretRef, mirroring what `ochami-admin init --vault-auth
// appRole` produces.
func newAppRoleControlPlane(name string) *openchamiv1alpha1.OpenCHAMIControlPlane {
	cp := newControlPlane(name)
	cp.Spec.Platform.Vault.AuthMethod = openchamiv1alpha1.VaultAuthMethodAppRole
	cp.Spec.Platform.Vault.AppRoleSecretRef = &corev1.LocalObjectReference{
		Name: name + "-vault-approle",
	}
	return cp
}

// TestVaultReconciler_AppRoleSecretIDBootstrap is the primary regression test
// for issue #54: on a fresh appRole install the operator must generate a
// SecretID and create the Kubernetes Secret VSO reads (keyed `id`) so VSO can
// authenticate. Previously the operator only set the RoleID in VaultAuth and
// left the SecretID Secret to an out-of-band admin step, so VSO logins 403'd.
func TestVaultReconciler_AppRoleSecretIDBootstrap(t *testing.T) {
	scheme := newScheme(t)
	cp := newAppRoleControlPlane("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	v := vaultfake.NewClient()

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	paths := vault.Paths("alpha")
	v.AssertCalled(t, "EnsureAppRole")
	v.AssertCalled(t, "GenerateSecretID")
	if got := v.CallCount("GenerateSecretID"); got != 1 {
		t.Fatalf("expected exactly one GenerateSecretID call, got %d", got)
	}

	var sec corev1.Secret
	key := types.NamespacedName{Namespace: ControlPlaneNamespace(cp), Name: cp.Spec.Platform.Vault.AppRoleSecretRef.Name}
	if err := c.Get(context.Background(), key, &sec); err != nil {
		t.Fatalf("expected approle secret %s to exist: %v", key, err)
	}
	id := string(sec.Data[appRoleSecretIDKey])
	if id == "" {
		t.Fatalf("expected non-empty %q key in approle secret, got %+v", appRoleSecretIDKey, sec.Data)
	}
	if want := "fake-secret-id-" + paths.AppRoleServices; id != want {
		t.Errorf("expected secret-id %q, got %q", want, id)
	}
	if got := sec.Annotations[appRoleRoleIDAnnotation]; got != "fake-role-id-"+paths.AppRoleServices {
		t.Errorf("expected RoleID binding annotation, got %q", got)
	}
	if got := sec.Labels[labelManagedBy]; got != managedByValue {
		t.Errorf("expected managed-by label %q, got %q", managedByValue, got)
	}
}

// TestVaultReconciler_AppRoleSecretIDIdempotent asserts a valid SecretID is
// preserved across reconciles: the operator must not rotate a working SecretID
// (which would churn VSO's cached login) when the Secret already carries one
// bound to the current RoleID.
func TestVaultReconciler_AppRoleSecretIDIdempotent(t *testing.T) {
	scheme := newScheme(t)
	cp := newAppRoleControlPlane("beta")

	// The fake EnsureAppRole returns "fake-role-id-<role>"; the stored
	// SecretID must be annotated with that same RoleID to be considered bound
	// to the current AppRole incarnation.
	paths := vault.Paths("beta")
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cp.Spec.Platform.Vault.AppRoleSecretRef.Name,
			Namespace: ControlPlaneNamespace(cp),
			Annotations: map[string]string{
				appRoleRoleIDAnnotation: "fake-role-id-" + paths.AppRoleServices,
			},
		},
		Data: map[string][]byte{appRoleSecretIDKey: []byte("preexisting-secret-id")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp, existing).Build()
	v := vaultfake.NewClient()

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	v.AssertNotCalled(t, "GenerateSecretID")

	var sec corev1.Secret
	key := types.NamespacedName{Namespace: ControlPlaneNamespace(cp), Name: cp.Spec.Platform.Vault.AppRoleSecretRef.Name}
	if err := c.Get(context.Background(), key, &sec); err != nil {
		t.Fatalf("reading approle secret: %v", err)
	}
	if got := string(sec.Data[appRoleSecretIDKey]); got != "preexisting-secret-id" {
		t.Errorf("expected existing secret-id preserved, got %q", got)
	}
}

// TestVaultReconciler_AppRoleSecretIDRegeneratesOnRoleIDMismatch covers the
// AppRole/Vault recreation case: the Kubernetes Secret survives (e.g. the
// namespace was untouched) but Vault was reset, so the recreated AppRole has a
// new RoleID. The stored SecretID was minted against the OLD RoleID and would
// now 403. The operator must detect the mismatch via the binding annotation
// and regenerate.
func TestVaultReconciler_AppRoleSecretIDRegeneratesOnRoleIDMismatch(t *testing.T) {
	scheme := newScheme(t)
	cp := newAppRoleControlPlane("epsilon")

	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cp.Spec.Platform.Vault.AppRoleSecretRef.Name,
			Namespace: ControlPlaneNamespace(cp),
			Annotations: map[string]string{
				appRoleRoleIDAnnotation: "old-role-id-from-a-previous-vault",
			},
		},
		Data: map[string][]byte{appRoleSecretIDKey: []byte("stale-secret-id")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp, stale).Build()
	v := vaultfake.NewClient()

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	v.AssertCalled(t, "GenerateSecretID")
	paths := vault.Paths("epsilon")
	var sec corev1.Secret
	key := types.NamespacedName{Namespace: ControlPlaneNamespace(cp), Name: cp.Spec.Platform.Vault.AppRoleSecretRef.Name}
	if err := c.Get(context.Background(), key, &sec); err != nil {
		t.Fatalf("reading approle secret: %v", err)
	}
	if got := string(sec.Data[appRoleSecretIDKey]); got == "stale-secret-id" || got == "" {
		t.Errorf("expected a freshly generated secret-id, got %q", got)
	}
	if got := sec.Annotations[appRoleRoleIDAnnotation]; got != "fake-role-id-"+paths.AppRoleServices {
		t.Errorf("expected RoleID annotation rebound to current RoleID, got %q", got)
	}
}

// TestVaultReconciler_AppRoleSecretIDAdoptsUnboundSecret covers a Secret an
// administrator created by hand (or a pre-upgrade operator wrote) that carries
// an `id` but no RoleID binding annotation. Its provenance is unknown, so the
// operator regenerates and stamps the binding annotation, taking ownership.
func TestVaultReconciler_AppRoleSecretIDAdoptsUnboundSecret(t *testing.T) {
	scheme := newScheme(t)
	cp := newAppRoleControlPlane("zeta")

	unbound := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cp.Spec.Platform.Vault.AppRoleSecretRef.Name,
			Namespace: ControlPlaneNamespace(cp),
		},
		Data: map[string][]byte{appRoleSecretIDKey: []byte("admin-supplied-id")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp, unbound).Build()
	v := vaultfake.NewClient()

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	v.AssertCalled(t, "GenerateSecretID")
	paths := vault.Paths("zeta")
	var sec corev1.Secret
	key := types.NamespacedName{Namespace: ControlPlaneNamespace(cp), Name: cp.Spec.Platform.Vault.AppRoleSecretRef.Name}
	if err := c.Get(context.Background(), key, &sec); err != nil {
		t.Fatalf("reading approle secret: %v", err)
	}
	if got := sec.Annotations[appRoleRoleIDAnnotation]; got != "fake-role-id-"+paths.AppRoleServices {
		t.Errorf("expected RoleID binding annotation stamped, got %q", got)
	}
}

// TestVaultReconciler_AppRoleSecretIDRegeneratesWhenEmpty asserts the operator
// re-provisions a SecretID when the Secret exists but its `id` key is missing
// or empty — the exact namespace-recreation scenario in issue #54 where the
// Secret may be recreated blank while the Vault-side AppRole persists.
func TestVaultReconciler_AppRoleSecretIDRegeneratesWhenEmpty(t *testing.T) {
	scheme := newScheme(t)
	cp := newAppRoleControlPlane("gamma")

	blank := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cp.Spec.Platform.Vault.AppRoleSecretRef.Name,
			Namespace: ControlPlaneNamespace(cp),
		},
		Data: map[string][]byte{appRoleSecretIDKey: []byte("")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp, blank).Build()
	v := vaultfake.NewClient()

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	v.AssertCalled(t, "GenerateSecretID")
	var sec corev1.Secret
	key := types.NamespacedName{Namespace: ControlPlaneNamespace(cp), Name: cp.Spec.Platform.Vault.AppRoleSecretRef.Name}
	if err := c.Get(context.Background(), key, &sec); err != nil {
		t.Fatalf("reading approle secret: %v", err)
	}
	if got := string(sec.Data[appRoleSecretIDKey]); got == "" {
		t.Errorf("expected a freshly generated secret-id, got empty")
	}
}

// TestVaultReconciler_AppRoleVaultAuthWiring asserts the produced VaultAuth
// carries the live RoleID (not the role name) and references the SecretID
// Secret by name — the two halves of the AppRole credential VSO needs.
func TestVaultReconciler_AppRoleVaultAuthWiring(t *testing.T) {
	scheme := newScheme(t)
	cp := newAppRoleControlPlane("delta")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	v := vaultfake.NewClient()

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var auth vsov1beta1.VaultAuth
	key := types.NamespacedName{Namespace: ControlPlaneNamespace(cp), Name: "openchami-" + cp.Spec.ClusterName}
	if err := c.Get(context.Background(), key, &auth); err != nil {
		t.Fatalf("reading VaultAuth: %v", err)
	}
	if auth.Spec.AppRole == nil {
		t.Fatalf("expected VaultAuth.spec.appRole to be set")
	}
	paths := vault.Paths("delta")
	if want := "fake-role-id-" + paths.AppRoleServices; auth.Spec.AppRole.RoleID != want {
		t.Errorf("expected RoleID %q, got %q", want, auth.Spec.AppRole.RoleID)
	}
	if auth.Spec.AppRole.SecretRef != cp.Spec.Platform.Vault.AppRoleSecretRef.Name {
		t.Errorf("expected SecretRef %q, got %q",
			cp.Spec.Platform.Vault.AppRoleSecretRef.Name, auth.Spec.AppRole.SecretRef)
	}
}
