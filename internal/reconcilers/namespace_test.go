// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestNamespaceReconciler_PSALabelsPermitHostNetworkWorkloads pins the PSA
// label set the operator writes onto each cluster namespace.
//
// Regression context (2026-05-04): the namespace previously enforced
// `restricted`, then briefly `baseline`. Both reject the three host-namespace
// workloads the operator schedules into the same namespace:
//
//   - coredhcp DaemonSet (hostNetwork=true, hostPort 67)
//   - funicular-collector DaemonSet (hostPath /var/log/pods)
//   - network-probe DaemonSet (hostNetwork=true)
//
// `baseline` forbids host namespaces, hostPath, and host ports outright; only
// `privileged` lets them schedule. warn/audit at restricted preserve audit
// visibility for service-tier pods that should fit a stricter floor. This
// test fails if anyone tightens enforce back to baseline/restricted without
// first splitting the host workloads into a separate namespace.
func TestNamespaceReconciler_PSALabelsPermitHostNetworkWorkloads(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	r := &NamespaceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ns := &corev1.Namespace{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "openchami-alpha"}, ns); err != nil {
		t.Fatalf("getting namespace: %v", err)
	}

	cases := []struct{ key, want string }{
		{"pod-security.kubernetes.io/enforce", "privileged"},
		{"pod-security.kubernetes.io/warn", "restricted"},
		{"pod-security.kubernetes.io/audit", "restricted"},
		{"openchami.org/cluster", "alpha"},
		{"kubernetes.io/metadata.name", "openchami-alpha"},
	}
	for _, tc := range cases {
		if got := ns.Labels[tc.key]; got != tc.want {
			t.Errorf("namespace label %q = %q, want %q", tc.key, got, tc.want)
		}
	}
}
