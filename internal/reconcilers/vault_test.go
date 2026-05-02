/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package reconcilers

import (
	"context"
	"errors"
	"testing"

	vsov1beta1 "github.com/hashicorp/vault-secrets-operator/api/v1beta1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openahamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/vault"
	vaultfake "github.com/openchami/openchami-operator/internal/vault/fake"
)

// Shared fixture cluster names used across reconciler tests. Extracted to
// satisfy goconst across sibling test files.
const (
	testClusterRed  = "red"
	testClusterBlue = "blue"

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
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgo scheme: %v", err)
	}
	if err := openahamiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding openchami scheme: %v", err)
	}
	if err := vsov1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding vso scheme: %v", err)
	}
	return scheme
}

func newCluster(name string) *openahamiv1alpha1.OpenCHAMICluster {
	return &openahamiv1alpha1.OpenCHAMICluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: 1},
		Spec: openahamiv1alpha1.OpenCHAMIClusterSpec{
			ClusterName: name,
			Domain:      name + ".test.local",
			Platform: openahamiv1alpha1.PlatformSpec{
				Vault: openahamiv1alpha1.VaultSpec{
					Address:    "http://vault.test:8200",
					AuthMethod: openahamiv1alpha1.VaultAuthMethodKubernetes,
				},
				ObjectStorage: openahamiv1alpha1.ObjectStorageSpec{
					Endpoint: "http://s3.test:9000",
				},
			},
			Services: openahamiv1alpha1.ServicesSpec{
				Tokensmith: openahamiv1alpha1.TokensmithSpec{OIDCProvider: "vault"},
			},
		},
	}
}

func TestVaultReconciler_HappyPath(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	v := vaultfake.NewClient()

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	paths := vault.Paths("alpha")
	v.AssertCalled(t, "IsReachable")
	v.AssertCalled(t, "EnsureKVMount")
	v.AssertSecretExists(t, paths.DBCredentials)
	v.AssertSecretExists(t, paths.S3Credentials)
	v.AssertSecretExists(t, paths.LogCredentials)
	v.AssertSecretExists(t, paths.TokensmithOIDC)
	v.AssertPolicyExists(t, paths.PolicyServices)
	v.AssertCalled(t, "EnsureKubernetesRole")
	v.AssertCalled(t, "EnsureOIDCConfig")

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionVaultConfigured)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected VaultConfigured=True, got %+v", cond)
	}
	if cluster.Status.VaultPathPrefix != paths.SecretPrefix {
		t.Errorf("expected VaultPathPrefix=%q, got %q", paths.SecretPrefix, cluster.Status.VaultPathPrefix)
	}
}

func TestVaultReconciler_Unreachable(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("beta")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	v := vaultfake.NewClient()
	v.Errors["IsReachable"] = errors.New("connection refused")

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue when vault unreachable, got %+v", res)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionVaultConfigured)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonUnreachable {
		t.Fatalf("expected VaultConfigured=False/Unreachable, got %+v", cond)
	}
	v.AssertNotCalled(t, "EnsureKVMount")
}

func TestVaultReconciler_NoOverwriteExistingSecrets(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("gamma")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	v := vaultfake.NewClient()

	paths := vault.Paths("gamma")
	v.Secrets[paths.DBCredentials] = map[string]any{VaultKeySMDPassword: "original-value"}

	r := &VaultReconciler{Client: c, Recorder: record.NewFakeRecorder(10), VaultClient: v}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := v.Secrets[paths.DBCredentials]
	if got[VaultKeySMDPassword] != "original-value" {
		t.Errorf("expected original DB password preserved, got %v", got[VaultKeySMDPassword])
	}
}

func TestVaultReconciler_TwoClustersIsolated(t *testing.T) {
	scheme := newScheme(t)
	v := vaultfake.NewClient()
	rec := record.NewFakeRecorder(20)

	for _, name := range []string{testClusterRed, testClusterBlue} {
		cluster := newCluster(name)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
		r := &VaultReconciler{Client: c, Recorder: rec, VaultClient: v}
		if _, err := r.Reconcile(context.Background(), cluster); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}
	}

	red := vault.Paths(testClusterRed)
	blue := vault.Paths(testClusterBlue)
	if red.SecretPrefix == blue.SecretPrefix {
		t.Fatalf("two clusters share secret prefix %q", red.SecretPrefix)
	}
	v.AssertSecretExists(t, red.DBCredentials)
	v.AssertSecretExists(t, blue.DBCredentials)
	if v.Secrets[red.DBCredentials][VaultKeySMDPassword] == v.Secrets[blue.DBCredentials][VaultKeySMDPassword] {
		t.Errorf("expected distinct random passwords across clusters")
	}
}
