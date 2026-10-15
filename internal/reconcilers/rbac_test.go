// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestRBACReconciler_NetworkProbeBinding(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	r := &RBACReconciler{Client: c}
	for _, name := range []string{testAlphaName, "beta"} {
		cp := newControlPlane(name)
		cp.Namespace = "cr-namespace"
		cp.Spec.NetworkProbe.Enabled = name == testAlphaName
		var first *rbacv1.ClusterRoleBinding
		for range 2 {
			if _, err := r.Reconcile(ctx, cp); err != nil {
				t.Fatal(err)
			}
			binding := &rbacv1.ClusterRoleBinding{}
			key := types.NamespacedName{Name: "openchami-" + name + "-network-probe"}
			if err := c.Get(ctx, key, binding); err != nil {
				t.Fatal(err)
			}
			wantRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: key.Name}
			wantSubjects := []rbacv1.Subject{{Kind: "ServiceAccount", Name: ServiceNetworkProbe, Namespace: "openchami-" + name}}
			if binding.Namespace != "" || len(binding.OwnerReferences) != 0 || binding.RoleRef != wantRef || !reflect.DeepEqual(binding.Subjects, wantSubjects) {
				t.Fatalf("incorrect cluster-scoped binding: %+v", binding)
			}
			if first != nil && (!reflect.DeepEqual(first.Subjects, binding.Subjects) || first.RoleRef != binding.RoleRef) {
				t.Fatal("repeated reconciliation changed binding permissions")
			}
			first = binding.DeepCopy()
			role := &rbacv1.ClusterRole{}
			if err := c.Get(ctx, key, role); err != nil {
				t.Fatal(err)
			}
			wantRules := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "patch"}}}
			if !reflect.DeepEqual(role.Rules, wantRules) {
				t.Fatalf("unexpected probe permissions: %+v", role.Rules)
			}
		}
		objects, err := (&RBACReconciler{}).Describe(cp)
		if err != nil {
			t.Fatal(err)
		}
		binding, ok := objects[len(objects)-1].(*rbacv1.ClusterRoleBinding)
		role, roleOK := objects[len(objects)-2].(*rbacv1.ClusterRole)
		if !ok || !roleOK || role.Name != binding.RoleRef.Name || binding.Name != first.Name || binding.RoleRef != first.RoleRef || !reflect.DeepEqual(binding.Subjects, first.Subjects) {
			t.Fatal("Describe must include the matching binding immediately after its ClusterRole")
		}
	}
	list := &rbacv1.ClusterRoleBindingList{}
	if err := c.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("expected two isolated bindings, got %d", len(list.Items))
	}
}

func TestRBACReconciler_NetworkProbeBindingApplyFailure(t *testing.T) {
	wantErr := errors.New("binding apply denied")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if binding, ok := obj.(*rbacv1.ClusterRoleBinding); ok {
				if patch.Type() != types.ApplyPatchType {
					t.Fatal("binding must use server-side apply")
				}
				role := &rbacv1.ClusterRole{}
				if err := c.Get(ctx, types.NamespacedName{Name: binding.RoleRef.Name}, role); err != nil {
					t.Fatalf("role must be applied before its binding: %v", err)
				}
				return wantErr
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	r := &RBACReconciler{Client: c}
	_, err := r.Reconcile(context.Background(), newControlPlane(testAlphaName))
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "reconciling network-probe clusterrolebinding") {
		t.Fatalf("expected wrapped binding apply failure, got %v", err)
	}
}
