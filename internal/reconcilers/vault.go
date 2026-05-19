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
	{SuffixSMDDB, func(p vault.VaultPaths) string { return p.DBSMDCredentials }},
	{SuffixBootServiceDB, func(p vault.VaultPaths) string { return p.DBBootServiceCredentials }},
	{SuffixS3Credentials, func(p vault.VaultPaths) string { return p.S3Credentials }},
	{SuffixLogCredentials, func(p vault.VaultPaths) string { return p.LogCredentials }},
	{SuffixTokensmithOIDC, func(p vault.VaultPaths) string { return p.TokensmithOIDC }},
}

// VaultReconciler ensures Vault paths, policies, and VSO resources exist.
type VaultReconciler struct {
	Client      client.Client
	Recorder    record.EventRecorder
	VaultClient vault.Client
}

func (r *VaultReconciler) Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "vault")
	log.Info("reconciling vault configuration")

	if r.VaultClient == nil {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionVaultConfigured,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonError,
			Message:            "vault client not configured",
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{RequeueAfter: vaultRequeueAfter}, nil
	}

	if err := r.VaultClient.IsReachable(ctx); err != nil {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionVaultConfigured,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonUnreachable,
			Message:            fmt.Sprintf("vault unreachable: %v", err),
			ObservedGeneration: cp.Generation,
		})
		RecordConditionEvent(r.Recorder, cp, corev1.EventTypeWarning,
			conditions.ReasonUnreachable, "Vault is unreachable; will retry")
		return ctrl.Result{RequeueAfter: vaultRequeueAfter}, nil
	}

	paths := vault.Paths(cp.Spec.ClusterName)

	if err := r.VaultClient.EnsureKVMount(ctx, paths.KVMount); err != nil {
		return r.fail(cp, fmt.Errorf("ensuring kv mount: %w", err))
	}

	if err := r.ensureClusterSecrets(ctx, paths); err != nil {
		return r.fail(cp, err)
	}

	if err := r.VaultClient.EnsurePolicy(ctx, paths.PolicyServices,
		vault.ServicesPolicy(cp.Spec.ClusterName)); err != nil {
		return r.fail(cp, fmt.Errorf("ensuring policy: %w", err))
	}

	var appRoleID string
	switch cp.Spec.Platform.Vault.AuthMethod {
	case openchamiv1alpha1.VaultAuthMethodAppRole:
		// EnsureAppRole returns the role_id UUID Vault assigned. We thread
		// it through to applyVSOResources so the VaultAuth CR carries the
		// actual UUID rather than the AppRole name — VSO uses RoleID as
		// the credential and rejects logins keyed on a name.
		roleID, err := r.VaultClient.EnsureAppRole(ctx, paths.AppRoleServices, vault.AppRoleConfig{
			Policies:    []string{paths.PolicyServices},
			TokenTTL:    "15m",
			TokenMaxTTL: "1h",
			SecretIDTTL: "0",
		})
		if err != nil {
			return r.fail(cp, fmt.Errorf("ensuring approle: %w", err))
		}
		appRoleID = roleID
	default: // kubernetes
		if err := r.VaultClient.EnsureKubernetesRole(ctx, paths.K8sRoleServices, vault.KubernetesRoleConfig{
			BoundServiceAccountNames:      serviceAccountNames,
			BoundServiceAccountNamespaces: []string{ControlPlaneNamespace(cp)},
			Policies:                      []string{paths.PolicyServices},
			TokenTTL:                      "15m",
		}); err != nil {
			return r.fail(cp, fmt.Errorf("ensuring k8s role: %w", err))
		}
	}

	if cp.Spec.Services.Tokensmith.OIDCProvider == "vault" {
		// Vault rejects issuer URLs that contain a path: it accepts only
		// scheme + host (+ optional port) and appends the OIDC provider
		// path itself. The cluster-name partition is carried by the OIDC
		// key (`openchami-<clusterName>`) created inside EnsureOIDCConfig,
		// not by the issuer URL.
		issuer := fmt.Sprintf("https://%s", cp.Spec.Domain)
		if err := r.VaultClient.EnsureOIDCConfig(ctx, cp.Spec.ClusterName, issuer); err != nil {
			return r.fail(cp, fmt.Errorf("ensuring oidc config: %w", err))
		}
	}

	if err := r.applyVSOResources(ctx, cp, paths, appRoleID); err != nil {
		return r.fail(cp, err)
	}

	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionVaultConfigured,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            "Vault paths, policies, and VSO resources configured",
		ObservedGeneration: cp.Generation,
	})
	cp.Status.VaultPathPrefix = paths.SecretPrefix
	return ctrl.Result{}, nil
}

func (r *VaultReconciler) Describe(cp *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
	paths := vault.Paths(cp.Spec.ClusterName)
	objs := make([]client.Object, 0, 2+len(vssEntries))
	// Describe must not contact external services (per SubReconciler contract),
	// so we don't have the live AppRole UUID. The role NAME is emitted instead
	// as a placeholder; the real UUID is filled in by Reconcile.
	objs = append(objs, r.buildVaultConnection(cp), r.buildVaultAuth(cp, ""))
	for _, e := range vssEntries {
		objs = append(objs, r.buildVaultStaticSecret(cp, e.Suffix, paths))
	}
	return objs, nil
}

func (r *VaultReconciler) fail(cp *openchamiv1alpha1.OpenCHAMIControlPlane, err error) (ctrl.Result, error) {
	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionVaultConfigured,
		Status:             metav1.ConditionFalse,
		Reason:             conditions.ReasonError,
		Message:            err.Error(),
		ObservedGeneration: cp.Generation,
	})
	return ctrl.Result{}, err
}

// ensureClusterSecrets writes seed credentials into Vault, never overwriting
// values that already exist.
//
// One Vault path per consumer: each per-service db path carries a
// `username`/`password` pair (the kubernetes.io/basic-auth shape). VSO
// materializes each path as a Secret in the cluster namespace; CNPG's
// managed.roles references the Secret by name to set the role's
// password, and the consumer Deployment references the same Secret for
// its DB env vars.
//
// CNPG **validates** that the Secret's `username` key matches the
// role name declared in managed.roles, refusing to reconcile the role
// otherwise (error: `wrong username '<seen>' in secret, expected
// '<role>'`). Seeds therefore derive the username from dbRoleSpecs
// directly — same slice the DatabaseReconciler hands to CNPG — so the
// two sides cannot drift. The single source of truth is
// dbRoleSpec.Role.
func (r *VaultReconciler) ensureClusterSecrets(ctx context.Context, paths vault.VaultPaths) error {
	// vaultPathByRole maps each dbRoleSpec to the Vault path that
	// stores its credentials. Kept here (rather than as a field on
	// dbRoleSpec) so the reconcilers package doesn't have to import
	// vault.VaultPaths.
	vaultPathByRole := map[string]string{
		ServiceSMD:        paths.DBSMDCredentials,
		bootServiceDBName: paths.DBBootServiceCredentials,
	}
	for _, spec := range dbRoleSpecs {
		path, ok := vaultPathByRole[spec.Role]
		if !ok {
			return fmt.Errorf("no vault path registered for db role %q (extend vaultPathByRole)", spec.Role)
		}
		password, err := randomHex(32)
		if err != nil {
			return fmt.Errorf("generating password for %s: %w", path, err)
		}
		data := map[string]any{
			VaultKeyDBUsername: spec.Role, // must match managed.roles[].name exactly
			VaultKeyDBPassword: password,
		}
		if err := r.VaultClient.EnsureSecret(ctx, path, data, false); err != nil {
			return fmt.Errorf("ensuring secret %s: %w", path, err)
		}
	}

	type seed struct {
		path string
		keys []string
	}
	for _, s := range []seed{
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

func (r *VaultReconciler) applyVSOResources(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane, paths vault.VaultPaths, appRoleID string) error {
	objs := make([]client.Object, 0, 2+len(vssEntries))
	objs = append(objs, r.buildVaultConnection(cp), r.buildVaultAuth(cp, appRoleID))
	for _, e := range vssEntries {
		objs = append(objs, r.buildVaultStaticSecret(cp, e.Suffix, paths))
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

func (r *VaultReconciler) buildVaultConnection(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *vsov1beta1.VaultConnection {
	return &vsov1beta1.VaultConnection{
		TypeMeta: metav1.TypeMeta{APIVersion: vsoAPIVersion, Kind: vsoKindVaultConnection},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openchami-" + cp.Spec.ClusterName,
			Namespace: ControlPlaneNamespace(cp),
		},
		Spec: vsov1beta1.VaultConnectionSpec{
			Address: cp.Spec.Platform.Vault.Address,
		},
	}
}

// buildVaultAuth produces the VaultAuth CR for VSO. appRoleID is the UUID
// returned by Vault for the AppRole — Reconcile threads it through after
// EnsureAppRole. When called from Describe (which must not contact Vault),
// appRoleID is "" and the AppRole name is emitted as an informational
// placeholder; the live VaultAuth always carries the UUID.
func (r *VaultReconciler) buildVaultAuth(cp *openchamiv1alpha1.OpenCHAMIControlPlane, appRoleID string) *vsov1beta1.VaultAuth {
	paths := vault.Paths(cp.Spec.ClusterName)
	connRef := "openchami-" + cp.Spec.ClusterName
	auth := &vsov1beta1.VaultAuth{
		TypeMeta: metav1.TypeMeta{APIVersion: vsoAPIVersion, Kind: vsoKindVaultAuth},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openchami-" + cp.Spec.ClusterName,
			Namespace: ControlPlaneNamespace(cp),
		},
		Spec: vsov1beta1.VaultAuthSpec{
			VaultConnectionRef: connRef,
		},
	}
	if cp.Spec.Platform.Vault.AuthMethod == openchamiv1alpha1.VaultAuthMethodAppRole {
		auth.Spec.Method = "appRole"
		auth.Spec.Mount = "approle"
		ref := ""
		if cp.Spec.Platform.Vault.AppRoleSecretRef != nil {
			ref = cp.Spec.Platform.Vault.AppRoleSecretRef.Name
		}
		roleID := appRoleID
		if roleID == "" {
			roleID = paths.AppRoleServices // Describe-only placeholder
		}
		auth.Spec.AppRole = &vsov1beta1.VaultAuthConfigAppRole{
			RoleID:    roleID,
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

func (r *VaultReconciler) buildVaultStaticSecret(cp *openchamiv1alpha1.OpenCHAMIControlPlane, suffix string, paths vault.VaultPaths) *vsov1beta1.VaultStaticSecret {
	for _, e := range vssEntries {
		if e.Suffix == suffix {
			return r.buildVaultStaticSecretAt(cp, suffix, paths.KVMount, e.PathFn(paths))
		}
	}
	// Caller passed a suffix not in vssEntries — fail loudly rather than
	// silently returning a VSS with an empty path. The unit test
	// TestVaultStaticSecretSuffixesAllResolveToNonEmptyPaths catches this in CI.
	panic(fmt.Sprintf("buildVaultStaticSecret: unknown suffix %q (add to vssEntries in vault.go)", suffix))
}

func (r *VaultReconciler) buildVaultStaticSecretAt(cp *openchamiv1alpha1.OpenCHAMIControlPlane, suffix, mount, fullPath string) *vsov1beta1.VaultStaticSecret {
	// fullPath includes the mount; VSO expects mount and sub-path separately.
	sub := fullPath
	if len(fullPath) > len(mount)+1 {
		sub = fullPath[len(mount)+1:]
	}
	return &vsov1beta1.VaultStaticSecret{
		TypeMeta: metav1.TypeMeta{APIVersion: vsoAPIVersion, Kind: vsoKindVaultStaticSecret},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openchami-" + cp.Spec.ClusterName + "-" + suffix,
			Namespace: ControlPlaneNamespace(cp),
		},
		Spec: vsov1beta1.VaultStaticSecretSpec{
			VaultAuthRef: "openchami-" + cp.Spec.ClusterName,
			RefreshAfter: "1h",
			Destination: vsov1beta1.Destination{
				Name:   "openchami-" + cp.Spec.ClusterName + "-" + suffix,
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
