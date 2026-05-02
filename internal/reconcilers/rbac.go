/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package reconcilers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openahamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/logging"
)

const (
	fieldManager     = "openchami-operator"
	configReaderName = "operator-config-reader"
	rbacAPIVersion   = "rbac.authorization.k8s.io/v1"
)

// serviceAccountNames lists every SA the operator creates in the cluster namespace.
var serviceAccountNames = []string{
	"smd",
	"tokensmith",
	"boot-service",
	"metadata-service",
	"coredhcp",
	"magellan",
	"network-probe",
	"funicular-collector",
	configReaderName,
}

// RBACReconciler ensures ServiceAccounts, Roles, and ClusterRoles exist.
type RBACReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

func (r *RBACReconciler) Reconcile(ctx context.Context, cluster *openahamiv1alpha1.OpenCHAMICluster) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cluster, "rbac")
	ns := ClusterNamespace(cluster)

	log.Info("reconciling service accounts")
	for _, name := range serviceAccountNames {
		sa := r.buildServiceAccount(ns, name)
		if err := r.Client.Patch(ctx, sa, client.Apply, //nolint:staticcheck // SSA via Patch is the supported pattern; new client.Apply API requires runtime.ApplyConfiguration
			client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling service account %s: %w", name, err)
		}
	}

	log.Info("reconciling operator-config-reader role")
	role := r.buildConfigReaderRole(cluster)
	if err := r.Client.Patch(ctx, role, client.Apply, //nolint:staticcheck // SSA via Patch is the supported pattern
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling config-reader role: %w", err)
	}

	rb := r.buildConfigReaderRoleBinding(cluster)
	if err := r.Client.Patch(ctx, rb, client.Apply, //nolint:staticcheck // SSA via Patch is the supported pattern
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling config-reader rolebinding: %w", err)
	}

	log.Info("reconciling network-probe cluster role")
	cr := r.buildNetworkProbeClusterRole(cluster)
	if err := r.Client.Patch(ctx, cr, client.Apply, //nolint:staticcheck // SSA via Patch is the supported pattern
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling network-probe clusterrole: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *RBACReconciler) Describe(cluster *openahamiv1alpha1.OpenCHAMICluster) ([]client.Object, error) {
	ns := ClusterNamespace(cluster)
	objs := make([]client.Object, 0, len(serviceAccountNames)+3)
	for _, name := range serviceAccountNames {
		objs = append(objs, r.buildServiceAccount(ns, name))
	}
	objs = append(objs,
		r.buildConfigReaderRole(cluster),
		r.buildConfigReaderRoleBinding(cluster),
		r.buildNetworkProbeClusterRole(cluster),
	)
	return objs, nil
}

func (r *RBACReconciler) buildServiceAccount(ns, name string) *corev1.ServiceAccount {
	f := false
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"openchami.org/managed-by": "operator"},
		},
		AutomountServiceAccountToken: &f,
	}
}

func (r *RBACReconciler) buildConfigReaderRole(cluster *openahamiv1alpha1.OpenCHAMICluster) *rbacv1.Role {
	ns := ClusterNamespace(cluster)
	return &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      configReaderName,
			Namespace: ns,
		},
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"configmaps"},
			ResourceNames: []string{"openchami-" + cluster.Spec.ClusterName + "-topology"},
			Verbs:         []string{"get", "list"},
		}},
	}
}

func (r *RBACReconciler) buildConfigReaderRoleBinding(cluster *openahamiv1alpha1.OpenCHAMICluster) *rbacv1.RoleBinding {
	ns := ClusterNamespace(cluster)
	return &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      configReaderName,
			Namespace: ns,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      configReaderName,
			Namespace: ns,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     configReaderName,
		},
	}
}

func (r *RBACReconciler) buildNetworkProbeClusterRole(cluster *openahamiv1alpha1.OpenCHAMICluster) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacAPIVersion, Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "openchami-" + cluster.Spec.ClusterName + "-network-probe",
		},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"nodes"},
			Verbs:     []string{"get", "patch"},
		}},
	}
}
