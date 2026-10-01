package reconcilers

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRBACReconciler_NetworkProbeBinding(t *testing.T) {
	cp := newControlPlane("alpha")
	client := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(cp).Build()
	reconciler := &RBACReconciler{Client: client}
	if _, err := reconciler.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile RBAC: %v", err)
	}

	name := "openchami-alpha-network-probe"
	binding := &rbacv1.ClusterRoleBinding{}
	if err := client.Get(context.Background(), types.NamespacedName{Name: name}, binding); err != nil {
		t.Fatalf("get probe binding: %v", err)
	}
	if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != name {
		t.Errorf("unexpected role reference: %+v", binding.RoleRef)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "ServiceAccount" ||
		binding.Subjects[0].Name != ServiceNetworkProbe || binding.Subjects[0].Namespace != ControlPlaneNamespace(cp) {
		t.Errorf("unexpected probe subjects: %+v", binding.Subjects)
	}
}
