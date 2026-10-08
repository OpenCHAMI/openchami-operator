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

// TestConfigMapToCluster_EnqueuesReferencingControlPlanes verifies that an
// edit to a user-managed CoreDHCP ConfigMap enqueues exactly the control
// planes whose coreDHCP.configMapRef names it, in the same namespace.
func TestConfigMapToCluster_EnqueuesReferencingControlPlanes(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := openchamiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add openchami scheme: %v", err)
	}

	const (
		refName  = "site-coredhcp-config"
		matching = "venado"
	)
	mk := func(name, ns, ref string) *openchamiv1alpha1.OpenCHAMIControlPlane {
		cp := &openchamiv1alpha1.OpenCHAMIControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       openchamiv1alpha1.OpenCHAMIControlPlaneSpec{ClusterName: name},
		}
		if ref != "" {
			cp.Spec.Services.CoreDHCP.ConfigMapRef = &openchamiv1alpha1.CoreDHCPConfigMapRef{Name: ref}
		}
		return cp
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		mk("venado", "default", refName),   // match
		mk("frontier", "default", "other"), // different ConfigMap
		mk("aurora", "default", ""),        // generated config
		mk("summit", "site-b", refName),    // same name, other namespace
	).Build()
	r := &OpenCHAMIControlPlaneReconciler{Client: c, Scheme: scheme}

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: refName, Namespace: testNamespace}}
	got := r.configMapToCluster(context.Background(), cm)
	if len(got) != 1 || got[0].Name != matching || got[0].Namespace != testNamespace {
		t.Fatalf("expected single enqueue for %s/%s, got %+v", testNamespace, matching, got)
	}

	unrelated := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: testNamespace}}
	if got := r.configMapToCluster(context.Background(), unrelated); len(got) != 0 {
		t.Errorf("unrelated ConfigMap should not enqueue, got %+v", got)
	}
}
