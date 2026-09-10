// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package controller

// PROTOTYPE — per-CR Vault/S3 client construction (issue #15).
//
// This file prototypes the "longer-term fix": instead of the operator
// building a single, process-wide Vault/S3 client from environment
// variables at startup (see cmd/operator/main.go buildVaultClient /
// buildS3Client), each reconcile derives the clients it needs from the
// reconciled CR's own spec (spec.platform.vault / spec.platform.objectStorage)
// plus the in-cluster Secrets those specs reference.
//
// Why this is desirable:
//   - Multi-tenancy: different OpenCHAMIControlPlane CRs can point at
//     different Vault/S3 backends. Impossible with one global env client.
//   - Fixes issue #15: a freshly deployed operator no longer needs
//     hand-injected VAULT_ADDR / AWS_* env vars on the Deployment; the
//     connection details live in the CR the user already authors.
//   - The bucket reconcilers ALREADY fetch the per-cluster s3-credentials
//     Secret (bucket.go / logbucket.go) — they just never used it. This
//     wires that Secret into the client that actually does the work.
//
// Design notes for reviewers:
//   - Graceful degradation is preserved. When a client cannot be built
//     (missing spec, missing Secret, unreachable) we return (nil, nil) so
//     the existing nil-guards in vault.go / bucket.go / logbucket.go report
//     the *Configured / *Ready condition False and requeue, rather than
//     crashing the operator.
//   - The struct-level VaultClient / S3Client fields on the reconciler are
//     kept as an OPTIONAL OVERRIDE (used by unit tests / fakes and as a
//     dev escape hatch). When set, the factory returns them unchanged so
//     existing tests keep passing untouched.
//   - The dev-only "token" Vault auth method is NOT expressible in the CRD
//     enum (kubernetes|appRole). Dev continues to rely on the env-built
//     override client injected by `make dev-run` / `make dev-deploy`.
//
// OPEN QUESTIONS (flagged for discussion, not resolved in this prototype):
//   - Should S3 credentials come from the VSO-synced `s3-credentials`
//     Secret (convention, used here) or a new explicit
//     spec.platform.objectStorage.credentialsSecretRef? The former reuses
//     existing plumbing; the latter is more discoverable.
//   - Should we cache the Vault client per CR to avoid a login round-trip
//     on every reconcile? Left uncached here for clarity.
//   - AppRole role_id/secret_id resolution from spec.platform.vault.
//     appRoleSecretRef is stubbed (see vaultClientFor) — the current
//     env path reads VAULT_APPROLE_ROLE_ID/SECRET_ID instead.

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/reconcilers"
	s3client "github.com/openchami/openchami-operator/internal/s3"
	"github.com/openchami/openchami-operator/internal/vault"
)

// vaultClientFor returns the Vault client to use for this CR.
//
// Resolution order:
//  1. If a process-wide override client is set on the reconciler
//     (r.VaultClient, e.g. injected by tests or by `make dev-run`), use it.
//  2. Otherwise build one from cp.Spec.Platform.Vault.
//
// Returns (nil, nil) when no address is configured or when auth material
// cannot be resolved, so the vault sub-reconciler degrades gracefully.
func (r *OpenCHAMIControlPlaneReconciler) vaultClientFor(
	ctx context.Context,
	cp *openchamiv1alpha1.OpenCHAMIControlPlane,
) (vault.Client, error) {
	// (1) explicit override (tests / dev escape hatch) wins.
	if r.VaultClient != nil {
		return r.VaultClient, nil
	}

	vs := cp.Spec.Platform.Vault
	if vs.Address == "" {
		// Nothing to build. Sub-reconciler reports VaultConfigured=False.
		return nil, nil
	}

	cfg := vault.Config{
		Address:    vs.Address,
		AuthMethod: string(vs.AuthMethod),
	}
	if cfg.AuthMethod == "" {
		cfg.AuthMethod = string(openchamiv1alpha1.VaultAuthMethodKubernetes)
	}

	switch vs.AuthMethod {
	case openchamiv1alpha1.VaultAuthMethodAppRole:
		if vs.AppRoleSecretRef == nil {
			// Spec says appRole but named no Secret; degrade gracefully.
			return nil, nil
		}
		roleID, secretID, err := r.resolveAppRole(ctx, cp, vs.AppRoleSecretRef.Name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Secret not materialised yet — requeue via nil client.
				return nil, nil
			}
			return nil, err
		}
		cfg.AppRoleID = roleID
		cfg.AppRoleSecretID = secretID

	case openchamiv1alpha1.VaultAuthMethodKubernetes:
		// The operator authenticates with its own ServiceAccount token.
		// K8sRole defaults inside vault.NewClient when empty; a future
		// revision may surface a spec.platform.vault.kubernetesRole field.

	default:
		// Unknown / unexpressible (e.g. dev-only "token") — degrade.
		return nil, nil
	}

	c, err := vault.NewClient(ctx, cfg)
	if err != nil {
		// Auth/connect failure: return nil so the sub-reconciler reports
		// VaultConfigured=False/Unreachable and requeues, matching the
		// existing env-path semantics (buildVaultClient logs + continues).
		return nil, nil //nolint:nilerr // intentional graceful degradation
	}
	return c, nil
}

// resolveAppRole reads role_id / secret_id from the referenced Secret in the
// control-plane namespace. Key names mirror Vault's AppRole response fields.
func (r *OpenCHAMIControlPlaneReconciler) resolveAppRole(
	ctx context.Context,
	cp *openchamiv1alpha1.OpenCHAMIControlPlane,
	secretName string,
) (roleID, secretID string, err error) {
	s := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: reconcilers.ControlPlaneNamespace(cp),
		Name:      secretName,
	}, s); err != nil {
		return "", "", err
	}
	roleID = string(s.Data["role_id"])
	secretID = string(s.Data["secret_id"])
	if roleID == "" || secretID == "" {
		return "", "", fmt.Errorf("appRole secret %q missing role_id/secret_id", secretName)
	}
	return roleID, secretID, nil
}

// s3ClientFor returns the S3 client to use for this CR.
//
// Resolution order mirrors vaultClientFor:
//  1. Process-wide override (r.S3Client) wins.
//  2. Otherwise build from cp.Spec.Platform.ObjectStorage + the VSO-synced
//     `s3-credentials` Secret in the control-plane namespace.
//
// Returns (nil, nil) when the endpoint is unconfigured or the credentials
// Secret has not been materialised yet, so the bucket sub-reconcilers
// degrade gracefully (they already wait for that Secret).
func (r *OpenCHAMIControlPlaneReconciler) s3ClientFor(
	ctx context.Context,
	cp *openchamiv1alpha1.OpenCHAMIControlPlane,
) (s3client.Client, error) {
	if r.S3Client != nil {
		return r.S3Client, nil
	}

	os := cp.Spec.Platform.ObjectStorage
	if os.Endpoint == "" {
		return nil, nil
	}

	// Credentials come from the per-cluster s3-credentials Secret that the
	// vault reconciler provisions via VSO. Keys match what boot_service.go /
	// funicular.go already consume (access_key / secret_key).
	creds := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: reconcilers.ControlPlaneNamespace(cp),
		Name:      reconcilers.SecretName(cp, reconcilers.SuffixS3Credentials),
	}, creds); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil // waiting for VSO — sub-reconciler requeues
		}
		return nil, err
	}

	cfg := s3client.Config{
		Endpoint:    os.Endpoint,
		AccessKey:   string(creds.Data["access_key"]),
		SecretKey:   string(creds.Data["secret_key"]),
		Region:      os.Region,
		TLSInsecure: os.TLSInsecure,
	}

	c, err := s3client.NewClient(ctx, cfg)
	if err != nil {
		return nil, nil //nolint:nilerr // intentional graceful degradation
	}
	return c, nil
}
