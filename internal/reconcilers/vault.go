// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	vsov1beta1 "github.com/hashicorp/vault-secrets-operator/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/logging"
	"github.com/openchami/openchami-operator/internal/vault"
)

const (
	vaultRequeueAfter = 30 * time.Second

	vsoAPIVersion            = "secrets.hashicorp.com/v1beta1"
	vsoKindVaultConnection   = "VaultConnection"
	vsoKindVaultAuth         = "VaultAuth"
	vsoKindVaultStaticSecret = "VaultStaticSecret"
)

// vssEntries lists every VaultStaticSecret produced by this reconciler and
// the function that returns its full Vault path. Adding a new entry here is
// the single source of truth — the suffix-only list, the path-by-suffix map,
// and the produced VSS objects all derive from this slice. A test
// (TestVaultStaticSecretSuffixesAllResolveToNonEmptyPaths) asserts that every
// entry's pathFn returns a non-empty path for a sample cluster, so a future
// addition to vault.VaultPaths cannot silently produce a VSS with an empty
// `path:`.
type vssEntry struct {
	Suffix string
	PathFn func(vault.VaultPaths) string
}

var vssEntries = []vssEntry{
	{SuffixDBCredentials, func(p vault.VaultPaths) string { return p.DBCredentials }},
	{SuffixS3Credentials, func(p vault.VaultPaths) string { return p.S3Credentials }},
	{SuffixLogCredentials, func(p vault.VaultPaths) string { return p.LogCredentials }},
	{SuffixTokensmithOIDC, func(p vault.VaultPaths) string { return p.TokensmithOIDC }},
}

// vssNames returns just the suffixes — used by callers that only need names
// (e.g. metric/log enumeration of expected secrets).
func vssNames() []string {
	out := make([]string, len(vssEntries))
	for i, e := range vssEntries {
		out[i] = e.Suffix
	}
	return out
}

// VaultReconciler ensures Vault paths, policies, and VSO resources exist.
type VaultReconciler struct {
	Client      client.Client
	Recorder    record.EventRecorder
	VaultClient vault.Client
}

func (r *VaultReconciler) Reconcile(ctx context.Context, cluster *openchamiv1alpha1.OpenCHAMICluster) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cluster, "vault")
	log.Info("reconciling vault configuration")

	if r.VaultClient == nil {
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionVaultConfigured,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonError,
			Message:            "vault client not configured",
			ObservedGeneration: cluster.Generation,
		})
		return ctrl.Result{RequeueAfter: vaultRequeueAfter}, nil
	}

	if err := r.VaultClient.IsReachable(ctx); err != nil {
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionVaultConfigured,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonUnreachable,
			Message:            fmt.Sprintf("vault unreachable: %v", err),
			ObservedGeneration: cluster.Generation,
		})
		RecordConditionEvent(r.Recorder, cluster, corev1.EventTypeWarning,
			conditions.ReasonUnreachable, "Vault is unreachable; will retry")
		return ctrl.Result{RequeueAfter: vaultRequeueAfter}, nil
	}

	paths := vault.Paths(cluster.Spec.ClusterName)

	if err := r.VaultClient.EnsureKVMount(ctx, paths.KVMount); err != nil {
		return r.fail(cluster, fmt.Errorf("ensuring kv mount: %w", err))
	}

	if err := r.ensureClusterSecrets(ctx, paths); err != nil {
		return r.fail(cluster, err)
	}

	if err := r.VaultClient.EnsurePolicy(ctx, paths.PolicyServices,
		vault.ServicesPolicy(cluster.Spec.ClusterName)); err != nil {
		return r.fail(cluster, fmt.Errorf("ensuring policy: %w", err))
	}

	switch cluster.Spec.Platform.Vault.AuthMethod {
	case openchamiv1alpha1.VaultAuthMethodAppRole:
		if _, err := r.VaultClient.EnsureAppRole(ctx, paths.AppRoleServices, vault.AppRoleConfig{
			Policies:    []string{paths.PolicyServices},
			TokenTTL:    "15m",
			TokenMaxTTL: "1h",
			SecretIDTTL: "0",
		}); err != nil {
			return r.fail(cluster, fmt.Errorf("ensuring approle: %w", err))
		}
	default: // kubernetes
		if err := r.VaultClient.EnsureKubernetesRole(ctx, paths.K8sRoleServices, vault.KubernetesRoleConfig{
			BoundServiceAccountNames:      serviceAccountNames,
			BoundServiceAccountNamespaces: []string{ClusterNamespace(cluster)},
			Policies:                      []string{paths.PolicyServices},
			TokenTTL:                      "15m",
		}); err != nil {
			return r.fail(cluster, fmt.Errorf("ensuring k8s role: %w", err))
		}
	}

	if cluster.Spec.Services.Tokensmith.OIDCProvider == "vault" {
		// Vault rejects issuer URLs that contain a path: it accepts only
		// scheme + host (+ optional port) and appends the OIDC provider
		// path itself. The cluster-name partition is carried by the OIDC
		// key (`openchami-<clusterName>`) created inside EnsureOIDCConfig,
		// not by the issuer URL.
		issuer := fmt.Sprintf("https://%s", cluster.Spec.Domain)
		if err := r.VaultClient.EnsureOIDCConfig(ctx, cluster.Spec.ClusterName, issuer); err != nil {
			return r.fail(cluster, fmt.Errorf("ensuring oidc config: %w", err))
		}
	}

	if err := r.applyVSOResources(ctx, cluster, paths); err != nil {
		return r.fail(cluster, err)
	}

	apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionVaultConfigured,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            "Vault paths, policies, and VSO resources configured",
		ObservedGeneration: cluster.Generation,
	})
	cluster.Status.VaultPathPrefix = paths.SecretPrefix
	return ctrl.Result{}, nil
}

func (r *VaultReconciler) Describe(cluster *openchamiv1alpha1.OpenCHAMICluster) ([]client.Object, error) {
	paths := vault.Paths(cluster.Spec.ClusterName)
	objs := make([]client.Object, 0, 2+len(vssEntries))
	objs = append(objs, r.buildVaultConnection(cluster), r.buildVaultAuth(cluster))
	for _, e := range vssEntries {
		objs = append(objs, r.buildVaultStaticSecret(cluster, e.Suffix, paths))
	}
	return objs, nil
}

func (r *VaultReconciler) fail(cluster *openchamiv1alpha1.OpenCHAMICluster, err error) (ctrl.Result, error) {
	apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionVaultConfigured,
		Status:             metav1.ConditionFalse,
		Reason:             conditions.ReasonError,
		Message:            err.Error(),
		ObservedGeneration: cluster.Generation,
	})
	return ctrl.Result{}, err
}

// ensureClusterSecrets writes seed credentials into Vault, never overwriting
// values that already exist.
func (r *VaultReconciler) ensureClusterSecrets(ctx context.Context, paths vault.VaultPaths) error {
	type seed struct {
		path string
		keys []string
	}
	for _, s := range []seed{
		{paths.DBCredentials, []string{VaultKeySMDPassword, VaultKeyBootServicePassword}},
		{paths.S3Credentials, []string{"access_key", "secret_key"}},
		{paths.LogCredentials, []string{"access_key", "secret_key"}},
		{paths.TokensmithOIDC, []string{"client_secret"}},
	} {
		data := make(map[string]any, len(s.keys))
		for _, k := range s.keys {
			v, err := randomHex(32)
			if err != nil {
				return fmt.Errorf("generating random for %s: %w", k, err)
			}
			data[k] = v
		}
		if err := r.VaultClient.EnsureSecret(ctx, s.path, data, false); err != nil {
			return fmt.Errorf("ensuring secret %s: %w", s.path, err)
		}
	}
	return nil
}

func (r *VaultReconciler) applyVSOResources(ctx context.Context, cluster *openchamiv1alpha1.OpenCHAMICluster, paths vault.VaultPaths) error {
	objs := make([]client.Object, 0, 2+len(vssEntries))
	objs = append(objs, r.buildVaultConnection(cluster), r.buildVaultAuth(cluster))
	for _, e := range vssEntries {
		objs = append(objs, r.buildVaultStaticSecret(cluster, e.Suffix, paths))
	}

	for _, obj := range objs {
		if err := r.Client.Patch(ctx, obj, client.Apply, //nolint:staticcheck // SSA via Patch
			client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
			return fmt.Errorf("applying %s/%s: %w",
				obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
		}
	}
	return nil
}

func (r *VaultReconciler) buildVaultConnection(cluster *openchamiv1alpha1.OpenCHAMICluster) *vsov1beta1.VaultConnection {
	return &vsov1beta1.VaultConnection{
		TypeMeta: metav1.TypeMeta{APIVersion: vsoAPIVersion, Kind: vsoKindVaultConnection},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openchami-" + cluster.Spec.ClusterName,
			Namespace: ClusterNamespace(cluster),
		},
		Spec: vsov1beta1.VaultConnectionSpec{
			Address: cluster.Spec.Platform.Vault.Address,
		},
	}
}

func (r *VaultReconciler) buildVaultAuth(cluster *openchamiv1alpha1.OpenCHAMICluster) *vsov1beta1.VaultAuth {
	paths := vault.Paths(cluster.Spec.ClusterName)
	connRef := "openchami-" + cluster.Spec.ClusterName
	auth := &vsov1beta1.VaultAuth{
		TypeMeta: metav1.TypeMeta{APIVersion: vsoAPIVersion, Kind: vsoKindVaultAuth},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openchami-" + cluster.Spec.ClusterName,
			Namespace: ClusterNamespace(cluster),
		},
		Spec: vsov1beta1.VaultAuthSpec{
			VaultConnectionRef: connRef,
		},
	}
	if cluster.Spec.Platform.Vault.AuthMethod == openchamiv1alpha1.VaultAuthMethodAppRole {
		auth.Spec.Method = "appRole"
		auth.Spec.Mount = "approle"
		ref := ""
		if cluster.Spec.Platform.Vault.AppRoleSecretRef != nil {
			ref = cluster.Spec.Platform.Vault.AppRoleSecretRef.Name
		}
		auth.Spec.AppRole = &vsov1beta1.VaultAuthConfigAppRole{
			RoleID:    paths.AppRoleServices,
			SecretRef: ref,
		}
	} else {
		auth.Spec.Method = "kubernetes"
		auth.Spec.Mount = "kubernetes"
		auth.Spec.Kubernetes = &vsov1beta1.VaultAuthConfigKubernetes{
			Role:           paths.K8sRoleServices,
			ServiceAccount: configReaderName,
		}
	}
	return auth
}

func (r *VaultReconciler) buildVaultStaticSecret(cluster *openchamiv1alpha1.OpenCHAMICluster, suffix string, paths vault.VaultPaths) *vsov1beta1.VaultStaticSecret {
	for _, e := range vssEntries {
		if e.Suffix == suffix {
			return r.buildVaultStaticSecretAt(cluster, suffix, paths.KVMount, e.PathFn(paths))
		}
	}
	// Caller passed a suffix not in vssEntries — fail loudly rather than
	// silently returning a VSS with an empty path. The unit test
	// TestVaultStaticSecretSuffixesAllResolveToNonEmptyPaths catches this in CI.
	panic(fmt.Sprintf("buildVaultStaticSecret: unknown suffix %q (add to vssEntries in vault.go)", suffix))
}

func (r *VaultReconciler) buildVaultStaticSecretAt(cluster *openchamiv1alpha1.OpenCHAMICluster, suffix, mount, fullPath string) *vsov1beta1.VaultStaticSecret {
	// fullPath includes the mount; VSO expects mount and sub-path separately.
	sub := fullPath
	if len(fullPath) > len(mount)+1 {
		sub = fullPath[len(mount)+1:]
	}
	return &vsov1beta1.VaultStaticSecret{
		TypeMeta: metav1.TypeMeta{APIVersion: vsoAPIVersion, Kind: vsoKindVaultStaticSecret},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openchami-" + cluster.Spec.ClusterName + "-" + suffix,
			Namespace: ClusterNamespace(cluster),
		},
		Spec: vsov1beta1.VaultStaticSecretSpec{
			VaultAuthRef: "openchami-" + cluster.Spec.ClusterName,
			RefreshAfter: "1h",
			Destination: vsov1beta1.Destination{
				Name:   "openchami-" + cluster.Spec.ClusterName + "-" + suffix,
				Create: true,
			},
			VaultStaticSecretCommon: vsov1beta1.VaultStaticSecretCommon{
				Mount: mount,
				Path:  sub,
				Type:  "kv-v2",
			},
		},
	}
}

func randomHex(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
