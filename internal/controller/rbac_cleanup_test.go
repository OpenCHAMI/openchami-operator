// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

func TestNetworkProbeRBACCleanup(t *testing.T) {
	const (
		bindingFailure = "binding-failure"
		roleFailure    = "role-failure"
	)
	for _, mode := range []string{"present", "missing", bindingFailure, roleFailure} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, rbacv1.AddToScheme, openchamiv1alpha1.AddToScheme} {
				if err := add(scheme); err != nil {
					t.Fatal(err)
				}
			}
			cp := &openchamiv1alpha1.OpenCHAMIControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: testNamespace, Finalizers: []string{clusterFinalizer}},
				Spec:       openchamiv1alpha1.OpenCHAMIControlPlaneSpec{ClusterName: "alpha"},
			}
			const name = "openchami-alpha-network-probe"
			const otherName = "openchami-beta-network-probe"
			objects := []client.Object{cp,
				&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: otherName}},
				&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: otherName}},
			}
			if mode != "missing" {
				objects = append(objects,
					&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}},
					&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}},
				)
			}
			wantErr := errors.New("RBAC deletion denied")
			var deleted []string
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					switch obj.(type) {
					case *rbacv1.ClusterRoleBinding:
						deleted = append(deleted, "binding")
						if mode == bindingFailure {
							return wantErr
						}
					case *rbacv1.ClusterRole:
						deleted = append(deleted, "role")
						if mode == roleFailure {
							return wantErr
						}
					}
					return c.Delete(ctx, obj, opts...)
				},
			}).Build()
			r := &OpenCHAMIControlPlaneReconciler{Client: c}
			result, err := r.reconcileDelete(ctx, cp)
			updated := &openchamiv1alpha1.OpenCHAMIControlPlane{}
			if getErr := c.Get(ctx, client.ObjectKeyFromObject(cp), updated); getErr != nil {
				t.Fatal(getErr)
			}
			failed := mode == bindingFailure || mode == roleFailure
			if failed {
				if !errors.Is(err, wantErr) || result.RequeueAfter != 5*time.Second || len(updated.Finalizers) != 1 {
					t.Fatalf("failure must retain finalizer and retry: result=%+v err=%v finalizers=%v", result, err, updated.Finalizers)
				}
			} else {
				if err != nil || len(updated.Finalizers) != 0 {
					t.Fatalf("cleanup failed: err=%v finalizers=%v", err, updated.Finalizers)
				}
				for _, obj := range []client.Object{&rbacv1.ClusterRole{}, &rbacv1.ClusterRoleBinding{}} {
					if getErr := c.Get(ctx, types.NamespacedName{Name: name}, obj); !apierrors.IsNotFound(getErr) {
						t.Fatalf("RBAC resource remains after cleanup: %T err=%v", obj, getErr)
					}
				}
			}
			if len(deleted) == 0 || deleted[0] != "binding" || (mode == bindingFailure && len(deleted) != 1) {
				t.Fatalf("binding must be deleted before role: %v", deleted)
			}
			for _, obj := range []client.Object{&rbacv1.ClusterRole{}, &rbacv1.ClusterRoleBinding{}} {
				if getErr := c.Get(ctx, types.NamespacedName{Name: otherName}, obj); getErr != nil {
					t.Fatalf("another cluster's RBAC must survive: %v", getErr)
				}
			}
		})
	}
}
