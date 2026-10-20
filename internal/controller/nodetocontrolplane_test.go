// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

const testProbeRevision = "new"

// TestNodeToCluster_TriggersOnEitherProbeLabel is the regression test for the
// 2026-05-04 watch-handler bug: nodeToCluster only checked the provision
// label, so a node that flipped only its bmc-network-ready value (e.g. a
// BMC-network-only node) never enqueued a reconcile. The fixed handler must
// match either probe-ready label.
func TestNodeToCluster_TriggersOnEitherProbeLabel(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := openchamiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add openchami scheme: %v", err)
	}

	probeOn := openchamiv1alpha1.NetworkProbeSpec{Enabled: true}
	cp := &openchamiv1alpha1.OpenCHAMIControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "venado", Namespace: "default"},
		Spec: openchamiv1alpha1.OpenCHAMIControlPlaneSpec{
			ClusterName:  "venado",
			NetworkProbe: probeOn,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	r := &OpenCHAMIControlPlaneReconciler{Client: c, Scheme: scheme}

	cases := []struct {
		name   string
		labels map[string]string
		want   int
	}{
		{
			name:   "no probe labels — no enqueue",
			labels: map[string]string{"kubernetes.io/hostname": "n1"},
			want:   0,
		},
		{
			name:   "provision label present — enqueue",
			labels: map[string]string{"openchami.org/venado-provision-network-ready": annotationOptInTrue},
			want:   1,
		},
		{
			name:   "bmc label present — enqueue (regression)",
			labels: map[string]string{"openchami.org/venado-bmc-network-ready": annotationOptInTrue},
			want:   1,
		},
		{
			name: "both labels present — exactly one enqueue (no double-fire)",
			labels: map[string]string{
				"openchami.org/venado-provision-network-ready": annotationOptInTrue,
				"openchami.org/venado-bmc-network-ready":       annotationOptInTrue,
			},
			want: 1,
		},
		{
			name:   "wrong cluster's label — no enqueue",
			labels: map[string]string{"openchami.org/frontier-provision-network-ready": annotationOptInTrue},
			want:   0,
		},
		{name: "provision revision changed", labels: map[string]string{"openchami.org/venado-provision-probe-revision": testProbeRevision}, want: 1},
		{name: "BMC revision changed", labels: map[string]string{"openchami.org/venado-bmc-probe-revision": testProbeRevision}, want: 1},
		{name: "other cluster revision", labels: map[string]string{"openchami.org/frontier-bmc-probe-revision": testProbeRevision}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: tc.labels}}
			got := r.nodeToCluster(context.Background(), node)
			if len(got) != tc.want {
				t.Errorf("got %d enqueue(s), want %d (labels=%v)", len(got), tc.want, tc.labels)
			}
		})
	}
}

// TestNodeToCluster_IgnoresClustersWithProbeDisabled confirms that a probe
// label on a node does not enqueue clusters that have networkProbe disabled.
// Without this guard, a node whose label happens to match a disabled cluster's
// name would cause spurious reconciles.
func TestNodeToCluster_IgnoresClustersWithProbeDisabled(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := openchamiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add openchami scheme: %v", err)
	}
	cp := &openchamiv1alpha1.OpenCHAMIControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "frontier", Namespace: "default"},
		Spec: openchamiv1alpha1.OpenCHAMIControlPlaneSpec{
			ClusterName:  "frontier",
			NetworkProbe: openchamiv1alpha1.NetworkProbeSpec{Enabled: false},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	r := &OpenCHAMIControlPlaneReconciler{Client: c, Scheme: scheme}

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "n1",
		Labels: map[string]string{"openchami.org/frontier-provision-network-ready": annotationOptInTrue},
	}}
	got := r.nodeToCluster(context.Background(), node)
	if len(got) != 0 {
		t.Fatalf("probe-disabled cluster should not enqueue; got %d enqueue(s)", len(got))
	}
}

const (
	manualPlacementCluster = "manual-placement"
	manualPlacementLabel   = "dhcp"
	manualPlacementZone    = "zone"
)

func TestNodeToCluster_ManualPlacementEvents(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := openchamiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cp := &openchamiv1alpha1.OpenCHAMIControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: manualPlacementCluster, Namespace: testNamespace},
		Spec: openchamiv1alpha1.OpenCHAMIControlPlaneSpec{
			ClusterName: manualPlacementCluster,
			Services: openchamiv1alpha1.ServicesSpec{CoreDHCP: openchamiv1alpha1.CoreDHCPSpec{
				Enabled: true, NodeSelector: map[string]string{manualPlacementLabel: manualPlacementCluster, manualPlacementZone: "a"},
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	r := &OpenCHAMIControlPlaneReconciler{Client: c, Scheme: scheme}
	h := handler.EnqueueRequestsFromMapFunc(r.nodeToCluster)
	eligible := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{manualPlacementLabel: manualPlacementCluster, manualPlacementZone: "a"}}}
	unrelated := eligible.DeepCopy()
	unrelated.Labels = map[string]string{manualPlacementLabel: "other", manualPlacementZone: "a"}
	removed := eligible.DeepCopy()
	removed.Labels = nil
	changedAddress := eligible.DeepCopy()
	changedAddress.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.3"}}
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		old  *corev1.Node
		new  *corev1.Node
		want int
	}{
		{name: "create eligible", new: eligible, want: 1},
		{name: "create unrelated", new: unrelated, want: 0},
		{name: "address change", old: eligible, new: changedAddress, want: 1},
		{name: "labels removed", old: eligible, new: removed, want: 1},
		{name: "labels changed to another cluster", old: eligible, new: unrelated, want: 1},
		{name: "gained eligibility", old: unrelated, new: eligible, want: 1},
		{name: "delete eligible", old: eligible, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
			defer q.ShutDown()
			switch {
			case tc.old == nil:
				h.Create(ctx, event.CreateEvent{Object: tc.new}, q)
			case tc.new == nil:
				h.Delete(ctx, event.DeleteEvent{Object: tc.old}, q)
			default:
				h.Update(ctx, event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new}, q)
			}
			if q.Len() != tc.want {
				t.Fatalf("enqueued %d requests, want %d", q.Len(), tc.want)
			}
			if tc.want > 0 {
				req, _ := q.Get()
				defer q.Done(req)
				if req.Name != cp.Name || req.Namespace != cp.Namespace {
					t.Fatalf("unexpected cluster enqueued: %+v", req)
				}
			}
		})
	}
}
