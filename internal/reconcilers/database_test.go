// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
)

func newDBScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := newScheme(t)
	if err := cnpgv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding cnpg scheme: %v", err)
	}
	return scheme
}

// cnpgClusterName returns the CNPG Cluster CR name the operator builds for
// the "alpha" test cluster — the only cluster name used by the per-test
// assertions in this file. The TwoClustersIsolated test composes its own
// names inline since it iterates red/blue.
func cnpgClusterName() string {
	return "openchami-alpha-postgres"
}

// newServiceDBSecret returns the per-service VSO-synced Secret carrying
// `username` and `password` keys. CNPG's managed.roles reads `password`
// from the referenced Secret to set the role password.
func newServiceDBSecret(cp *openchamiv1alpha1.OpenCHAMIControlPlane, suffix, username, password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretName(cp, suffix),
			Namespace: ControlPlaneNamespace(cp),
		},
		Data: map[string][]byte{
			VaultKeyDBUsername: []byte(username),
			VaultKeyDBPassword: []byte(password),
		},
	}
}

func allDBSecretObjects(cp *openchamiv1alpha1.OpenCHAMIControlPlane) []client.Object {
	return []client.Object{
		newServiceDBSecret(cp, SuffixSMDDB, ServiceSMD, "smd-pw"),
		newServiceDBSecret(cp, SuffixBootServiceDB, "boot_service", "boot-pw"),
	}
}

func TestDatabaseReconciler_WaitsForCredentials(t *testing.T) {
	scheme := newDBScheme(t)
	cp := newControlPlane("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &DatabaseReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while waiting for db creds, got %+v", res)
	}

	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionDatabaseReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonProvisioning {
		t.Fatalf("expected DatabaseReady=False/Provisioning, got %+v", cond)
	}
}

// TestDatabaseReconciler_WaitsForEachServiceSecret asserts the controller
// keeps requeuing while any single per-service Secret is missing — not just
// the first one.
func TestDatabaseReconciler_WaitsForEachServiceSecret(t *testing.T) {
	scheme := newDBScheme(t)
	cp := newControlPlane("alpha")
	// Only seed the SMD secret; boot_service secret is still missing.
	smdSecret := newServiceDBSecret(cp, SuffixSMDDB, ServiceSMD, "smd-pw")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp, smdSecret).Build()

	r := &DatabaseReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while boot-service secret missing, got %+v", res)
	}
}

// TestDatabaseReconciler_CreatesCNPGClusterWithManagedRoles asserts the
// shape of the CNPG Cluster CR the operator applies once all per-service
// Secrets are present: every entry in dbRoleSpecs becomes a
// managed.roles[] entry referencing its own password Secret, and there
// is no legacy bootstrap.initdb block.
func TestDatabaseReconciler_CreatesCNPGClusterWithManagedRoles(t *testing.T) {
	scheme := newDBScheme(t)
	cp := newControlPlane("alpha")
	secrets := allDBSecretObjects(cp)
	objs := make([]client.Object, 0, 1+len(secrets))
	objs = append(objs, cp)
	objs = append(objs, secrets...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	r := &DatabaseReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &cnpgv1.Cluster{}
	key := types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      cnpgClusterName(),
	}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatalf("getting cnpg cluster: %v", err)
	}
	if got.Spec.Instances != 3 {
		t.Errorf("expected 3 instances by default, got %d", got.Spec.Instances)
	}
	if got.Spec.Bootstrap != nil && got.Spec.Bootstrap.InitDB != nil {
		t.Errorf("expected no bootstrap.initdb under managed.roles model, got %+v", got.Spec.Bootstrap.InitDB)
	}
	if got.Spec.Managed == nil {
		t.Fatalf("expected Spec.Managed to be set with declarative roles")
	}
	if len(got.Spec.Managed.Roles) != len(dbRoleSpecs) {
		t.Fatalf("expected %d managed roles, got %d", len(dbRoleSpecs), len(got.Spec.Managed.Roles))
	}
	byName := map[string]cnpgv1.RoleConfiguration{}
	for _, role := range got.Spec.Managed.Roles {
		byName[role.Name] = role
	}
	for _, spec := range dbRoleSpecs {
		role, ok := byName[spec.Role]
		if !ok {
			t.Errorf("expected managed role %q to be present", spec.Role)
			continue
		}
		if role.Ensure != cnpgv1.EnsurePresent {
			t.Errorf("role %q: expected Ensure=present, got %q", spec.Role, role.Ensure)
		}
		if !role.Login {
			t.Errorf("role %q: expected Login=true", spec.Role)
		}
		if role.PasswordSecret == nil || role.PasswordSecret.Name != spec.SecretName(cp) {
			t.Errorf("role %q: expected PasswordSecret %q, got %+v",
				spec.Role, spec.SecretName(cp), role.PasswordSecret)
		}
	}
}

func TestDatabaseReconciler_DegradedWhileNotHealthy(t *testing.T) {
	scheme := newDBScheme(t)
	cp := newControlPlane("alpha")
	secrets := allDBSecretObjects(cp)
	objs := make([]client.Object, 0, 1+len(secrets))
	objs = append(objs, cp)
	objs = append(objs, secrets...)
	cnpg := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cnpgClusterName(),
			Namespace: ControlPlaneNamespace(cp),
		},
		Status: cnpgv1.ClusterStatus{Phase: "Setting up primary"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&cnpgv1.Cluster{}).
		WithObjects(cnpg).
		Build()

	r := &DatabaseReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while not healthy")
	}
	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionDatabaseReady)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected DatabaseReady=False, got %+v", cond)
	}

	// Database CRs must not exist yet — the controller waits for the CNPG
	// Cluster to reach phase=Healthy before applying them.
	for _, spec := range dbRoleSpecs {
		db := &cnpgv1.Database{}
		err := c.Get(context.Background(), types.NamespacedName{
			Namespace: ControlPlaneNamespace(cp),
			Name:      "openchami-alpha-" + spec.CRSuffix,
		}, db)
		if err == nil {
			t.Errorf("expected Database CR for %q to be absent before CNPG is healthy", spec.Database)
		}
	}
}

// TestDatabaseReconciler_AppliesDatabaseCRsOnceHealthy is the success
// path. The CNPG Cluster reports phase=Healthy *and* every managed
// role reports `reconciled`; the operator applies a Database CR for
// each entry in dbRoleSpecs, but since none have `.status.applied=true`
// yet, DatabaseReady stays False/Provisioning.
func TestDatabaseReconciler_AppliesDatabaseCRsOnceHealthy(t *testing.T) {
	scheme := newDBScheme(t)
	cp := newControlPlane("alpha")
	secrets := allDBSecretObjects(cp)
	objs := make([]client.Object, 0, 1+len(secrets))
	objs = append(objs, cp)
	objs = append(objs, secrets...)
	cnpg := healthyCNPGClusterWithReconciledRoles(cp)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&cnpgv1.Cluster{}, &cnpgv1.Database{}).
		WithObjects(cnpg).
		Build()

	r := &DatabaseReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while databases not yet applied")
	}

	// Each Database CR exists with the expected ClusterRef/Owner shape.
	for _, spec := range dbRoleSpecs {
		db := &cnpgv1.Database{}
		key := types.NamespacedName{
			Namespace: ControlPlaneNamespace(cp),
			Name:      "openchami-alpha-" + spec.CRSuffix,
		}
		if err := c.Get(context.Background(), key, db); err != nil {
			t.Fatalf("getting Database %s: %v", key.Name, err)
		}
		if db.Spec.ClusterRef.Name != cnpgClusterName() {
			t.Errorf("[%s] expected ClusterRef %q, got %q",
				spec.Database, cnpgClusterName(), db.Spec.ClusterRef.Name)
		}
		if db.Spec.Name != spec.Database {
			t.Errorf("[%s] expected Spec.Name %q, got %q", spec.Database, spec.Database, db.Spec.Name)
		}
		if db.Spec.Owner != spec.Role {
			t.Errorf("[%s] expected Owner %q, got %q", spec.Database, spec.Role, db.Spec.Owner)
		}
		if db.Spec.Ensure != cnpgv1.EnsurePresent {
			t.Errorf("[%s] expected Ensure=present, got %q", spec.Database, db.Spec.Ensure)
		}
	}

	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionDatabaseReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonProvisioning {
		t.Fatalf("expected Ready=False/Provisioning while Databases pending, got %+v", cond)
	}
}

// TestDatabaseReconciler_ReadyWhenAllDatabasesApplied flips
// `.status.applied=true` on every Database CR (the field CNPG sets when
// the database has been created and owner assignment confirmed) and
// asserts DatabaseReady transitions to True.
func TestDatabaseReconciler_ReadyWhenAllDatabasesApplied(t *testing.T) {
	scheme := newDBScheme(t)
	cp := newControlPlane("alpha")
	secrets := allDBSecretObjects(cp)
	objs := make([]client.Object, 0, 1+len(secrets))
	objs = append(objs, cp)
	objs = append(objs, secrets...)
	cnpg := healthyCNPGClusterWithReconciledRoles(cp)
	applied := true
	dbObjs := make([]client.Object, 0, len(dbRoleSpecs))
	for _, spec := range dbRoleSpecs {
		dbObjs = append(dbObjs, &cnpgv1.Database{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "openchami-alpha-" + spec.CRSuffix,
				Namespace: ControlPlaneNamespace(cp),
			},
			Status: cnpgv1.DatabaseStatus{Applied: &applied},
		})
	}

	builder := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&cnpgv1.Cluster{}, &cnpgv1.Database{}).
		WithObjects(cnpg)
	for _, o := range dbObjs {
		builder = builder.WithObjects(o)
	}
	c := builder.Build()

	r := &DatabaseReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when ready, got %+v", res)
	}
	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionDatabaseReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != conditions.ReasonReady {
		t.Fatalf("expected Ready=True/Ready, got %+v", cond)
	}
}

func TestDatabaseReconciler_TwoClustersIsolated(t *testing.T) {
	scheme := newDBScheme(t)
	for _, name := range []string{testControlPlaneRed, testControlPlaneBlue} {
		cp := newControlPlane(name)
		secrets := allDBSecretObjects(cp)
		objs := make([]client.Object, 0, 1+len(secrets))
		objs = append(objs, cp)
		objs = append(objs, secrets...)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		r := &DatabaseReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
		if _, err := r.Reconcile(context.Background(), cp); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}

		got := &cnpgv1.Cluster{}
		if err := c.Get(context.Background(), types.NamespacedName{
			Namespace: "openchami-" + name,
			Name:      "openchami-" + name + "-postgres",
		}, got); err != nil {
			t.Fatalf("getting cnpg cluster for %s: %v", name, err)
		}
		if got.Namespace != "openchami-"+name {
			t.Errorf("expected namespace openchami-%s, got %s", name, got.Namespace)
		}
		// Every managed role should reference *this* cluster's per-service
		// Secret — never another cluster's Secret name.
		if got.Spec.Managed == nil {
			t.Fatalf("expected managed roles for %s", name)
		}
		for _, role := range got.Spec.Managed.Roles {
			if role.PasswordSecret == nil {
				t.Errorf("[%s] role %q missing PasswordSecret", name, role.Name)
				continue
			}
			wantPrefix := "openchami-" + name + "-"
			if got := role.PasswordSecret.Name; len(got) < len(wantPrefix) || got[:len(wantPrefix)] != wantPrefix {
				t.Errorf("[%s] role %q references foreign Secret %q",
					name, role.Name, role.PasswordSecret.Name)
			}
		}
	}
}

// healthyCNPGClusterWithReconciledRoles returns a CNPG Cluster CR
// whose status is what the operator expects to see when CNPG has
// fully reconciled every role in dbRoleSpecs into Postgres: phase
// Healthy *and* every spec role listed under
// ManagedRolesStatus.ByStatus[reconciled]. This is the common
// happy-path fixture for tests that exercise the post-role-sync
// behaviour (Database CR application, Ready=True).
func healthyCNPGClusterWithReconciledRoles(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *cnpgv1.Cluster {
	reconciledRoles := make([]string, 0, len(dbRoleSpecs))
	for _, spec := range dbRoleSpecs {
		reconciledRoles = append(reconciledRoles, spec.Role)
	}
	return &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cnpgClusterName(),
			Namespace: ControlPlaneNamespace(cp),
		},
		Status: cnpgv1.ClusterStatus{
			Phase: cnpgv1.PhaseHealthy,
			ManagedRolesStatus: cnpgv1.ManagedRoles{
				ByStatus: map[cnpgv1.RoleStatus][]string{
					cnpgv1.RoleStatusReconciled: reconciledRoles,
				},
			},
		},
	}
}

// TestDatabaseReconciler_GatesOnManagedRolesReconciled is the
// regression test for the dormant-RoleSynchronizer issue diagnosed
// 2026-05-14: CNPG can report phase=Healthy long before its
// instance-manager has actually created the managed roles in
// Postgres. If the operator races ahead and applies the Database
// CRs in that window, CNPG's database controller fails with
// `ERROR: role "<x>" does not exist` and the whole control plane
// hangs in Provisioning.
//
// The reconciler must instead requeue with a clear message naming
// the unreconciled roles, *not* apply any Database CR until every
// role is present in ManagedRolesStatus.ByStatus[reconciled].
func TestDatabaseReconciler_GatesOnManagedRolesReconciled(t *testing.T) {
	scheme := newDBScheme(t)
	cp := newControlPlane("alpha")
	secrets := allDBSecretObjects(cp)
	objs := make([]client.Object, 0, 1+len(secrets))
	objs = append(objs, cp)
	objs = append(objs, secrets...)

	// Cluster is Healthy but ManagedRolesStatus is empty — exactly
	// the dormant-synchronizer scenario observed on the live cluster.
	cnpg := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cnpgClusterName(),
			Namespace: ControlPlaneNamespace(cp),
		},
		Status: cnpgv1.ClusterStatus{Phase: cnpgv1.PhaseHealthy},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&cnpgv1.Cluster{}, &cnpgv1.Database{}).
		WithObjects(cnpg).
		Build()

	r := &DatabaseReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while managed roles unreconciled, got %+v", res)
	}

	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionDatabaseReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonProvisioning {
		t.Fatalf("expected Ready=False/Provisioning, got %+v", cond)
	}
	for _, spec := range dbRoleSpecs {
		if !strings.Contains(cond.Message, spec.Role) {
			t.Errorf("expected condition message to name role %q, got: %q", spec.Role, cond.Message)
		}
	}

	// Crucially, NO Database CR should have been applied yet — that's
	// the cascading failure the gate exists to prevent.
	for _, spec := range dbRoleSpecs {
		db := &cnpgv1.Database{}
		err := c.Get(context.Background(), types.NamespacedName{
			Namespace: ControlPlaneNamespace(cp),
			Name:      "openchami-alpha-" + spec.CRSuffix,
		}, db)
		if err == nil {
			t.Errorf("expected Database CR for %q to be absent until role is reconciled", spec.Role)
		}
	}
}

// TestBuildCNPGCluster_StampsManagedRolesHashAnnotation guards the
// poke-via-annotation half of the dormant-synchronizer fix: the
// content hash of the managed-role list is written as an annotation
// on the CNPG Cluster CR. When the role list changes the hash
// changes, the metadata delta fires the instance-manager's Cluster
// watch, and the RoleSynchronizer wakes up — even if the rest of
// the spec is byte-identical to what's already stored.
func TestBuildCNPGCluster_StampsManagedRolesHashAnnotation(t *testing.T) {
	cp := newControlPlane("alpha")
	r := &DatabaseReconciler{}
	got := r.buildCNPGCluster(cp)
	hash, ok := got.Annotations[managedRolesHashAnnotation]
	if !ok {
		t.Fatalf("expected annotation %q on CNPG Cluster, got %+v",
			managedRolesHashAnnotation, got.Annotations)
	}
	if len(hash) == 0 {
		t.Errorf("expected non-empty hash, got %q", hash)
	}
	// Stable across calls with the same inputs (deterministic hash).
	got2 := r.buildCNPGCluster(cp)
	if got2.Annotations[managedRolesHashAnnotation] != hash {
		t.Errorf("expected stable hash across calls, got %q then %q",
			hash, got2.Annotations[managedRolesHashAnnotation])
	}
}
