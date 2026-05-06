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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

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
	cluster := &openchamiv1alpha1.OpenCHAMICluster{
		ObjectMeta: metav1.ObjectMeta{Name: "venado", Namespace: "default"},
		Spec: openchamiv1alpha1.OpenCHAMIClusterSpec{
			ClusterName:  "venado",
			NetworkProbe: probeOn,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	r := &OpenCHAMIClusterReconciler{Client: c, Scheme: scheme}

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
			labels: map[string]string{"openchami.org/venado-provision-network-ready": "true"},
			want:   1,
		},
		{
			name:   "bmc label present — enqueue (regression)",
			labels: map[string]string{"openchami.org/venado-bmc-network-ready": "true"},
			want:   1,
		},
		{
			name: "both labels present — exactly one enqueue (no double-fire)",
			labels: map[string]string{
				"openchami.org/venado-provision-network-ready": "true",
				"openchami.org/venado-bmc-network-ready":       "true",
			},
			want: 1,
		},
		{
			name:   "wrong cluster's label — no enqueue",
			labels: map[string]string{"openchami.org/frontier-provision-network-ready": "true"},
			want:   0,
		},
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
	cluster := &openchamiv1alpha1.OpenCHAMICluster{
		ObjectMeta: metav1.ObjectMeta{Name: "frontier", Namespace: "default"},
		Spec: openchamiv1alpha1.OpenCHAMIClusterSpec{
			ClusterName:  "frontier",
			NetworkProbe: openchamiv1alpha1.NetworkProbeSpec{Enabled: false},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	r := &OpenCHAMIClusterReconciler{Client: c, Scheme: scheme}

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "n1",
		Labels: map[string]string{"openchami.org/frontier-provision-network-ready": "true"},
	}}
	got := r.nodeToCluster(context.Background(), node)
	if len(got) != 0 {
		t.Fatalf("probe-disabled cluster should not enqueue; got %d enqueue(s)", len(got))
	}
}
