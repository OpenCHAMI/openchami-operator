// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	s3pkg "github.com/openchami/openchami-operator/internal/s3"
	vaultfake "github.com/openchami/openchami-operator/internal/vault/fake"
)

// faultS3CredsSecret returns a Secret matching what VSO would sync from the
// cluster's s3-credentials Vault path, so BucketReconciler progresses past
// the "wait for creds" stage.
func faultS3CredsSecret(cluster *openchamiv1alpha1.OpenCHAMICluster) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretName(cluster, SuffixS3Credentials),
			Namespace: ClusterNamespace(cluster),
		},
		Data: map[string][]byte{
			s3AccessKeyKey: []byte("AKIA-fault"),
			s3SecretKeyKey: []byte("secret-fault"),
		},
	}
}

// faultTransientS3Client returns an injected error on the first EnsureBucket
// call, then defers to an in-memory bucket map for subsequent calls. Used by
// F-03 to simulate a transient backend failure that recovers on retry.
//
// We cannot reuse internal/s3/fake.Client here because its Errors map fires on
// every call; F-03 needs call-count-aware behavior.
type faultTransientS3Client struct {
	mu       sync.Mutex
	calls    int
	firstErr error
	buckets  map[string]bool
}

func newFaultTransientS3Client(firstErr error) *faultTransientS3Client {
	return &faultTransientS3Client{
		firstErr: firstErr,
		buckets:  map[string]bool{},
	}
}

func (c *faultTransientS3Client) EnsureBucket(_ context.Context, bucket string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls == 1 {
		return c.firstErr
	}
	c.buckets[bucket] = true
	return nil
}

func (c *faultTransientS3Client) EnsureLifecycleRule(_ context.Context, _ string, _ int32) error {
	return nil
}

func (c *faultTransientS3Client) BucketExists(_ context.Context, bucket string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buckets[bucket], nil
}

func (c *faultTransientS3Client) DeleteBucket(_ context.Context, bucket string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.buckets, bucket)
	return nil
}

func (c *faultTransientS3Client) CallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *faultTransientS3Client) HasBucket(bucket string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buckets[bucket]
}

// Compile-time assertion that the fault client satisfies s3.Client.
var _ s3pkg.Client = (*faultTransientS3Client)(nil)

// faultDBScheme returns a runtime.Scheme that includes the CNPG Cluster type.
// Mirrors newDBScheme in database_test.go but kept private so we don't depend
// on a sibling helper that may move.
func faultDBScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := newScheme(t)
	if err := cnpgv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding cnpg scheme: %v", err)
	}
	return scheme
}

// faultDrainEvents reads all currently buffered events from rec and returns
// true if any of them matches the requested type and reason. It is non-blocking
// so it never deadlocks if the recorder buffered fewer events than expected.
//
// FakeRecorder formats events as "<type> <reason> <message>".
func faultDrainEvents(rec *record.FakeRecorder, wantType, wantReason string) bool {
	matched := false
	for {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, wantType) && strings.Contains(ev, wantReason) {
				matched = true
			}
		default:
			return matched
		}
	}
}

// -----------------------------------------------------------------------------
// F-01: Vault returns error on EnsureSecret => no crash;
//       VaultConfigured=False/Error; reconciler returns a wrapped error.
// -----------------------------------------------------------------------------

func TestF01_VaultEnsureSecretError(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster(testClusterRed)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	v := vaultfake.NewClient()
	// IsReachable and EnsureKVMount succeed; EnsureSecret is the first
	// secret-writing call and is the one we fault.
	v.Errors["EnsureSecret"] = errors.New("vault 500: storage backend unavailable")

	r := &VaultReconciler{
		Client:      c,
		Recorder:    record.NewFakeRecorder(10),
		VaultClient: v,
	}

	// Reconcile must not panic. It returns the wrapped error so the manager
	// will requeue on the standard rate limiter.
	res, err := r.Reconcile(context.Background(), cluster)
	if err == nil {
		t.Fatalf("expected wrapped error from EnsureSecret failure, got nil (res=%+v)", res)
	}
	if !strings.Contains(err.Error(), "vault 500") {
		t.Errorf("expected error message to surface underlying cause, got %q", err.Error())
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionVaultConfigured)
	if cond == nil {
		t.Fatalf("expected VaultConfigured condition to be set, got none")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("expected VaultConfigured=False, got Status=%s", cond.Status)
	}
	if cond.Reason != conditions.ReasonError {
		t.Errorf("expected Reason=Error, got %q", cond.Reason)
	}

	// EnsureKubernetesRole must NOT be reached after the secret-write failure.
	v.AssertNotCalled(t, "EnsureKubernetesRole")
}

// -----------------------------------------------------------------------------
// F-02: CNPG cluster phase never transitions to healthy =>
//       DatabaseReconciler returns RequeueAfter ~30s; downstream service
//       reconcilers see no DB and don't crash.
// -----------------------------------------------------------------------------

func TestF02_DatabaseNeverHealthy(t *testing.T) {
	scheme := faultDBScheme(t)
	cluster := newCluster(testClusterRed)

	// Pre-create the db-credentials secret so the reconciler progresses
	// past the wait-for-VSO stage.
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretName(cluster, SuffixDBCredentials),
			Namespace: ClusterNamespace(cluster),
		},
		Data: map[string][]byte{
			VaultKeySMDPassword:         []byte("smd-pw"),
			VaultKeyBootServicePassword: []byte("boot-pw"),
		},
	}

	// Pre-create the CNPG Cluster with a non-healthy phase that never
	// transitions; the reconciler should see this and requeue indefinitely.
	cnpg := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openchami-" + cluster.Spec.ClusterName + "-postgres",
			Namespace: ClusterNamespace(cluster),
		},
		Status: cnpgv1.ClusterStatus{Phase: "Setting up primary"},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster, creds).
		WithStatusSubresource(&cnpgv1.Cluster{}).
		WithObjects(cnpg).
		Build()

	dbR := &DatabaseReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := dbR.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("database reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("expected positive RequeueAfter while DB unhealthy, got %v", res.RequeueAfter)
	}
	// Match the documented ~30s requeue interval.
	if want := 30 * time.Second; res.RequeueAfter != want {
		t.Errorf("expected RequeueAfter=%v, got %v", want, res.RequeueAfter)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionDatabaseReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonProvisioning {
		t.Fatalf("expected DatabaseReady=False/Provisioning, got %+v", cond)
	}

	// Downstream service reconcilers must not crash when the DB is not yet
	// ready. boot-service reads its DB host from cluster.Spec, not from the
	// CNPG Cluster status, so it can still build resources — the contract
	// here is "no panic, no fatal error".
	cluster.Spec.Services.BootService.Enabled = true
	bsR := &BootServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := bsR.Reconcile(context.Background(), cluster); err != nil {
		t.Errorf("boot-service reconcile crashed while DB pending: %v", err)
	}
}

// -----------------------------------------------------------------------------
// F-03: S3 EnsureBucket transient error first call, succeeds second call =>
//       first call: BucketReady=False/Error; second call: True/Ready and the
//       bucket appears in the underlying store.
// -----------------------------------------------------------------------------

func TestF03_BucketTransientErrorThenSuccess(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster(testClusterRed)
	creds := faultS3CredsSecret(cluster)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, creds).Build()

	transientErr := errors.New("versitygw 503: try again")
	s3c := newFaultTransientS3Client(transientErr)

	r := &BucketReconciler{
		Client:   c,
		Recorder: record.NewFakeRecorder(10),
		S3Client: s3c,
	}

	// First reconcile: error path.
	if _, err := r.Reconcile(context.Background(), cluster); err == nil {
		t.Fatalf("expected error on first call, got nil")
	}
	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionBucketReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonError {
		t.Fatalf("expected BucketReady=False/Error after first call, got %+v", cond)
	}

	// Second reconcile: success path.
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("expected success on second call, got %v", err)
	}
	cond = apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionBucketReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != conditions.ReasonReady {
		t.Fatalf("expected BucketReady=True/Ready after retry, got %+v", cond)
	}

	bucket := BootBucketName(cluster)
	if !s3c.HasBucket(bucket) {
		t.Errorf("expected bucket %q to be recorded after successful retry", bucket)
	}
	if got := s3c.CallCount(); got != 2 {
		t.Errorf("expected exactly 2 EnsureBucket calls, got %d", got)
	}
}

// -----------------------------------------------------------------------------
// F-04: Tokensmith PVC pre-exists with a custom annotation =>
//       TokensmithReconciler does NOT delete/recreate the PVC; SSA leaves the
//       unmanaged annotation intact and the PVC's identity (UID) is preserved.
// -----------------------------------------------------------------------------

func TestF04_TokensmithPreservesExistingPVC(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster(testClusterAlpha)
	cluster.Spec.Services.Tokensmith.Enabled = true

	const preserveAnno = "e2e/test-preserved"
	const preserveValue = "true"

	preExisting := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tokensmithPVCName,
			Namespace: ClusterNamespace(cluster),
			UID:       types.UID("pre-existing-uid-12345"),
			Annotations: map[string]string{
				preserveAnno: preserveValue,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster, preExisting).
		Build()

	r := &TokensmithReconciler{
		Client:   c,
		Recorder: record.NewFakeRecorder(10),
	}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ClusterNamespace(cluster),
		Name:      tokensmithPVCName,
	}, got); err != nil {
		t.Fatalf("expected PVC to exist after reconcile: %v", err)
	}

	// Identity check: the operator must not have deleted and recreated the
	// PVC. The UID is the most direct way to verify this.
	if got.UID != preExisting.UID {
		t.Errorf("PVC UID changed (delete+recreate detected): got %q, want %q",
			got.UID, preExisting.UID)
	}

	// SSA must preserve fields the operator does not manage.
	if v, ok := got.Annotations[preserveAnno]; !ok || v != preserveValue {
		t.Errorf("expected SSA to preserve annotation %q=%q on PVC, got annotations=%+v",
			preserveAnno, preserveValue, got.Annotations)
	}

	// And the operator's own annotation should still be set (proving SSA
	// did run, not that the test was a no-op). We assert presence + non-empty
	// rather than the exact value to avoid duplicating the literal already
	// asserted in tokensmith_test.go.
	if v, ok := got.Annotations["helm.sh/resource-policy"]; !ok || v == "" {
		t.Errorf("expected operator-managed helm.sh/resource-policy annotation to be set, got %+v",
			got.Annotations)
	}
}

// -----------------------------------------------------------------------------
// F-05: 10 concurrent reconciles for distinct clusters via t.Parallel() =>
//       go test -race reports no data races. Each sub-test gets its own
//       reconciler, fake client, and fake VaultClient so any shared
//       mutable state inside the reconciler implementation would surface.
// -----------------------------------------------------------------------------

func TestF05_ConcurrentReconciles_NoRaces(t *testing.T) {
	t.Parallel()
	for i := range 10 {
		name := fmt.Sprintf("red-%d", i)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			scheme := newScheme(t)
			cluster := newCluster(name)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
			v := vaultfake.NewClient()
			r := &VaultReconciler{
				Client:      c,
				Recorder:    record.NewFakeRecorder(10),
				VaultClient: v,
			}
			if _, err := r.Reconcile(context.Background(), cluster); err != nil {
				t.Fatalf("reconcile %s: %v", name, err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// F-06: NetworkProbeReconciler with all nodes returning false =>
//       NetworkProbeReady=False/NoEligibleNodes + Warning Event recorded.
// -----------------------------------------------------------------------------

func TestF06_NetworkProbeNoEligibleNodes(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster(testClusterAlpha)
	cluster.Spec.NetworkProbe = openchamiv1alpha1.NetworkProbeSpec{
		Enabled:          true,
		ProvisionNetwork: &openchamiv1alpha1.NetworkProbeTarget{Subnet: testProvisionSubnet},
		BMCNetwork:       &openchamiv1alpha1.NetworkProbeTarget{Subnet: testBMCSubnet},
	}

	// Pre-create a probe DaemonSet showing pods are running, plus a couple
	// of nodes lacking any probe-applied labels — i.e. the probe ran and
	// found nothing.
	dsName := "openchami-" + cluster.Spec.ClusterName + "-network-probe"
	existingDS := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dsName,
			Namespace: ClusterNamespace(cluster),
		},
		Status: appsv1.DaemonSetStatus{NumberReady: 2},
	}
	// Two unlabeled nodes are sufficient to prove "the probe ran but no
	// node matches". We avoid the literal "node-b" used by sibling tests
	// to keep goconst quiet.
	nodeA := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testNodeAName}}
	nodeC := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "fault-node-c"}}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&appsv1.DaemonSet{}).
		WithObjects(existingDS, nodeA, nodeC).
		Build()

	rec := record.NewFakeRecorder(10)
	r := &NetworkProbeReconciler{Client: c, Recorder: rec}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("expected positive RequeueAfter when no eligible nodes, got %v", res.RequeueAfter)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionNetworkProbeReady)
	if cond == nil {
		t.Fatalf("expected NetworkProbeReady condition to be set, got none")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("expected NetworkProbeReady=False, got Status=%s", cond.Status)
	}
	if cond.Reason != conditions.ReasonNoEligibleNodes {
		t.Errorf("expected Reason=NoEligibleNodes, got %q", cond.Reason)
	}

	// Confirm a Warning Event was emitted with the documented reason.
	if !faultDrainEvents(rec, corev1.EventTypeWarning, conditions.ReasonNoEligibleNodes) {
		t.Errorf("expected at least one Warning event with reason=%s",
			conditions.ReasonNoEligibleNodes)
	}

	// Status should reflect the empty probe result.
	if cluster.Status.NetworkProbe == nil {
		t.Fatalf("expected status.networkProbe to be populated, got nil")
	}
	if len(cluster.Status.NetworkProbe.NodesWithProvisionAccess) != 0 {
		t.Errorf("expected empty NodesWithProvisionAccess, got %+v",
			cluster.Status.NetworkProbe.NodesWithProvisionAccess)
	}
	if len(cluster.Status.NetworkProbe.NodesWithBMCAccess) != 0 {
		t.Errorf("expected empty NodesWithBMCAccess, got %+v",
			cluster.Status.NetworkProbe.NodesWithBMCAccess)
	}
	if cluster.Status.NetworkProbe.ProbeReady {
		t.Errorf("expected probeReady=false, got true")
	}
}
