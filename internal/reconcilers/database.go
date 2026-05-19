// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/logging"
)

const (
	databaseRequeueAfter = 30 * time.Second

	cnpgImage      = "ghcr.io/cloudnative-pg/postgresql:16.3"
	defaultStorage = "20Gi"

	cnpgAPIVersion   = "postgresql.cnpg.io/v1"
	cnpgKindCluster  = "Cluster"
	cnpgKindDatabase = "Database"

	// managedRolesHashAnnotation carries a content hash of the
	// dbRoleSpecs the operator declares as managed.roles. When the set
	// of roles changes the hash changes, the annotation updates, and
	// the CNPG instance manager's RoleSynchronizer (which watches the
	// Cluster object) sees a meaningful event — guards against the
	// dormant-synchronizer race where byte-identical SSA re-applies
	// don't bump .metadata.generation and never re-trigger role sync.
	managedRolesHashAnnotation = "openchami.org/managed-roles-hash"
)

// dbRoleSpec lists every per-service role + database the operator
// declares to CNPG. Each entry produces:
//   - a managed.roles entry on the CNPG Cluster (CNPG creates the role
//     and applies the password from the referenced Secret),
//   - a postgresql.cnpg.io/v1 Database CR (CNPG creates the database
//     owned by the role).
//
// Role and Database are PostgreSQL identifiers and may contain underscores
// (boot-service runs with role `boot_service` because CNPG normalises a
// hyphen to an underscore in SQL and we want the on-the-wire role name to
// match the service's BSS_DBUSER env directly). CRSuffix is the hyphenated
// suffix used to name the Kubernetes Database CR, which must be an RFC 1123
// subdomain — no underscores. Add a new consumer service by appending to
// this slice; no SQL or post-init Job changes required.
type dbRoleSpec struct {
	Role       string
	Database   string
	CRSuffix   string
	SecretName func(cp *openchamiv1alpha1.OpenCHAMIControlPlane) string
}

var dbRoleSpecs = []dbRoleSpec{
	{
		Role:       ServiceSMD,
		Database:   ServiceSMD,
		CRSuffix:   ServiceSMD,
		SecretName: func(c *openchamiv1alpha1.OpenCHAMIControlPlane) string { return SecretName(c, SuffixSMDDB) },
	},
	{
		Role:       bootServiceDBName,  // "boot_service" — SQL identifier
		Database:   bootServiceDBName,  // "boot_service" — SQL identifier
		CRSuffix:   ServiceBootService, // "boot-service" — RFC 1123 CR suffix
		SecretName: func(c *openchamiv1alpha1.OpenCHAMIControlPlane) string { return SecretName(c, SuffixBootServiceDB) },
	},
}

// DatabaseReconciler ensures a CloudNativePG Cluster exists with the
// per-service roles managed by CNPG, and one Database CR per service.
//
// 2026-05-08: rewritten to use CNPG's declarative managed.roles +
// kind: Database (modeled on kube-deploy). The previous post-init Job
// (which ran a shell script as the smd role to CREATE ROLE / GRANT /
// CREATE DATABASE) was deleted; CNPG owns role and database lifecycle
// directly.
type DatabaseReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

func (r *DatabaseReconciler) Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "database")
	ns := ControlPlaneNamespace(cp)

	// Wait for VSO to materialize every per-service db Secret. CNPG
	// reads the password Secret as part of role reconciliation; if
	// any are missing the Cluster never becomes healthy.
	for _, spec := range dbRoleSpecs {
		secretName := spec.SecretName(cp)
		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretName}, &corev1.Secret{}); apierrors.IsNotFound(err) {
			apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditions.ConditionDatabaseReady,
				Status:             metav1.ConditionFalse,
				Reason:             conditions.ReasonProvisioning,
				Message:            fmt.Sprintf("waiting for VSO to materialize Secret %s", secretName),
				ObservedGeneration: cp.Generation,
			})
			return ctrl.Result{RequeueAfter: databaseRequeueAfter}, nil
		} else if err != nil {
			return ctrl.Result{}, fmt.Errorf("getting db secret %s: %w", secretName, err)
		}
	}

	cnpg := r.buildCNPGCluster(cp)
	log.Info("applying CNPG cluster", "name", cnpg.Name)
	if err := r.Client.Patch(ctx, cnpg, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying cnpg cluster: %w", err)
	}

	current := &cnpgv1.Cluster{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: cnpg.Name}, current); err != nil {
		return ctrl.Result{}, fmt.Errorf("reading cnpg cluster status: %w", err)
	}
	if current.Status.Phase != cnpgv1.PhaseHealthy {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionDatabaseReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonProvisioning,
			Message:            fmt.Sprintf("cnpg phase: %q", current.Status.Phase),
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{RequeueAfter: databaseRequeueAfter}, nil
	}

	// Cluster phase=Healthy does NOT imply CNPG has reconciled the
	// managed roles into Postgres. The RoleSynchronizer in the instance
	// manager runs independently and lags behind the cluster going
	// healthy; if we apply the Database CRs before the roles are
	// reconciled, CNPG's database controller fails with `ERROR: role
	// "<x>" does not exist (SQLSTATE 42704)` and the Database CR is
	// stuck Applied=false. Gate on Status.ManagedRolesStatus reporting
	// every spec role as `reconciled` before proceeding.
	if missing := unreconciledManagedRoles(current); len(missing) > 0 {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionDatabaseReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonProvisioning,
			Message:            fmt.Sprintf("waiting for CNPG to reconcile managed roles: %s", strings.Join(missing, ", ")),
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{RequeueAfter: databaseRequeueAfter}, nil
	}

	// CNPG Cluster is healthy and managed roles are reconciled. Apply
	// per-service Database CRs and wait for each one to reach
	// Applied=true before the operator's overall DatabaseReady flips True.
	allReady := true
	for _, spec := range dbRoleSpecs {
		db := r.buildDatabase(cp, spec)
		if err := r.Client.Patch(ctx, db, client.Apply, //nolint:staticcheck // SSA via Patch
			client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
			return ctrl.Result{}, fmt.Errorf("applying database %s: %w", db.Name, err)
		}
		live := &cnpgv1.Database{}
		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: db.Name}, live); err != nil {
			return ctrl.Result{}, fmt.Errorf("reading database %s status: %w", db.Name, err)
		}
		if !databaseReady(live) {
			allReady = false
		}
	}
	if !allReady {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionDatabaseReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonProvisioning,
			Message:            "waiting for CNPG Database CRs to reach Ready=True",
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{RequeueAfter: databaseRequeueAfter}, nil
	}

	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionDatabaseReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            fmt.Sprintf("CNPG cluster healthy; %d managed databases ready", len(dbRoleSpecs)),
		ObservedGeneration: cp.Generation,
	})
	return ctrl.Result{}, nil
}

func (r *DatabaseReconciler) Describe(cp *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
	objs := make([]client.Object, 0, 1+len(dbRoleSpecs))
	objs = append(objs, r.buildCNPGCluster(cp))
	for _, spec := range dbRoleSpecs {
		objs = append(objs, r.buildDatabase(cp, spec))
	}
	return objs, nil
}

func (r *DatabaseReconciler) buildCNPGCluster(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *cnpgv1.Cluster {
	instances := int(cp.Spec.Database.Instances)
	if instances == 0 {
		instances = 3
	}
	storage := cp.Spec.Database.StorageSize
	if storage.IsZero() {
		storage = resource.MustParse(defaultStorage)
	}
	storageStr := storage.String()

	roles := make([]cnpgv1.RoleConfiguration, 0, len(dbRoleSpecs))
	for _, spec := range dbRoleSpecs {
		roles = append(roles, cnpgv1.RoleConfiguration{
			Name:           spec.Role,
			Ensure:         cnpgv1.EnsurePresent,
			Login:          true,
			Comment:        "managed by openchami-operator",
			PasswordSecret: &cnpgv1.LocalObjectReference{Name: spec.SecretName(cp)},
		})
	}

	annotations := argoSyncWaveAnnotation("15")
	annotations[managedRolesHashAnnotation] = managedRolesHash(cp, roles)

	return &cnpgv1.Cluster{
		TypeMeta: metav1.TypeMeta{APIVersion: cnpgAPIVersion, Kind: cnpgKindCluster},
		ObjectMeta: metav1.ObjectMeta{
			Name:        "openchami-" + cp.Spec.ClusterName + "-postgres",
			Namespace:   ControlPlaneNamespace(cp),
			Annotations: annotations,
		},
		Spec: cnpgv1.ClusterSpec{
			ImageName: cnpgImage,
			Instances: instances,
			// No bootstrap.InitDB.Secret: managed.roles handles role
			// creation declaratively. The default `app` role and
			// database that CNPG would otherwise create are not used;
			// each service's database is created by a Database CR
			// (below) owned by its dedicated role.
			Managed: &cnpgv1.ManagedConfiguration{
				Roles: roles,
			},
			StorageConfiguration: cnpgv1.StorageConfiguration{
				Size:         storageStr,
				StorageClass: storageClassPtr(cp.Spec.Database.StorageClass),
			},
		},
	}
}

func (r *DatabaseReconciler) buildDatabase(cp *openchamiv1alpha1.OpenCHAMIControlPlane, spec dbRoleSpec) *cnpgv1.Database {
	return &cnpgv1.Database{
		TypeMeta: metav1.TypeMeta{APIVersion: cnpgAPIVersion, Kind: cnpgKindDatabase},
		ObjectMeta: metav1.ObjectMeta{
			Name:        "openchami-" + cp.Spec.ClusterName + "-" + spec.CRSuffix,
			Namespace:   ControlPlaneNamespace(cp),
			Annotations: argoSyncWaveAnnotation("17"),
		},
		Spec: cnpgv1.DatabaseSpec{
			ClusterRef: corev1.LocalObjectReference{Name: "openchami-" + cp.Spec.ClusterName + "-postgres"},
			Name:       spec.Database,
			Owner:      spec.Role,
			Ensure:     cnpgv1.EnsurePresent,
		},
	}
}

// databaseReady reports whether a CNPG Database CR has reconciled
// successfully. CNPG sets `.status.applied=true` once the database has
// been created and owner assignment confirmed.
func databaseReady(db *cnpgv1.Database) bool {
	return db.Status.Applied != nil && *db.Status.Applied
}

func storageClassPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// argoSyncWaveAnnotation returns an annotation map carrying the Argo
// CD sync-wave hint (kube-deploy convention). Even without ArgoCD, the
// annotation documents apply order: 5=CNPG operator (cluster scope),
// 15=CNPG Cluster CR, 17=Database CRs, 20=migration jobs (smd-init),
// 30=service Deployments. Higher numbers wait for lower numbers.
func argoSyncWaveAnnotation(wave string) map[string]string {
	return map[string]string{"argocd.argoproj.io/sync-wave": wave}
}

// unreconciledManagedRoles returns the set of role names declared by
// dbRoleSpecs that do not appear in current.Status.ManagedRolesStatus
// .ByStatus[reconciled]. An empty slice means every managed role is
// reconciled in Postgres and Database CRs can safely refer to them as
// owners.
//
// We deliberately treat "missing from ByStatus entirely" the same as
// "present in pending-reconciliation" — the gate is binary, and the
// caller only needs to know whether to proceed or requeue.
func unreconciledManagedRoles(current *cnpgv1.Cluster) []string {
	reconciled := make(map[string]struct{})
	if current.Status.ManagedRolesStatus.ByStatus != nil {
		for _, name := range current.Status.ManagedRolesStatus.ByStatus[cnpgv1.RoleStatusReconciled] {
			reconciled[name] = struct{}{}
		}
	}
	var missing []string
	for _, spec := range dbRoleSpecs {
		if _, ok := reconciled[spec.Role]; !ok {
			missing = append(missing, spec.Role)
		}
	}
	sort.Strings(missing)
	return missing
}

// managedRolesHash returns a short content hash of the role
// configuration the operator declares on the CNPG Cluster. Stamped as
// an annotation so any change to the role list (add/remove a service,
// change a password Secret reference, swap auth options) shows up as
// a metadata delta on the Cluster object. The CNPG instance manager's
// RoleSynchronizer watches the Cluster via informer; even when SSA
// produces a byte-identical spec, an annotation change still fires
// the watch handler, waking the synchronizer.
//
// Hash inputs are sorted so adding a role at any list position
// produces the same hash for the same final set — avoids spurious
// churn when the slice is re-ordered.
func managedRolesHash(cp *openchamiv1alpha1.OpenCHAMIControlPlane, roles []cnpgv1.RoleConfiguration) string {
	parts := make([]string, 0, len(roles))
	for _, r := range roles {
		secret := ""
		if r.PasswordSecret != nil {
			secret = r.PasswordSecret.Name
		}
		parts = append(parts, fmt.Sprintf("%s|%s|%v|%v", r.Name, secret, r.Ensure, r.Login))
	}
	sort.Strings(parts)
	// Cluster name folded in so the hash is stable per-control-plane
	// even in the unlikely case of two clusters in the same namespace.
	h := sha256.Sum256([]byte(cp.Spec.ClusterName + ";" + strings.Join(parts, ";")))
	return hex.EncodeToString(h[:8]) // 16 hex chars — short, collision-resistant for this purpose
}
