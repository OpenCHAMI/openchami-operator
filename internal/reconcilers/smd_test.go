// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSMDReconciler_DisabledSkips(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.Services.SMD.Enabled = false
	cluster.Spec.Services.SMD.Replicas = 1
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	r := &SMDReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when disabled, got %+v", res)
	}

	dep := &appsv1.Deployment{}
	getErr := c.Get(context.Background(), types.NamespacedName{
		Namespace: ClusterNamespace(cluster),
		Name:      ServiceSMD,
	}, dep)
	if !apierrors.IsNotFound(getErr) {
		t.Errorf("expected smd Deployment to be absent when disabled, got err=%v", getErr)
	}
	if _, ok := cluster.Status.Services[ServiceSMD]; ok {
		t.Errorf("expected no status entry when disabled")
	}
}

func TestSMDReconciler_AppliesDeploymentServicePDB(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.Services.SMD.Enabled = true
	cluster.Spec.Services.SMD.Replicas = 2
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	r := &SMDReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ns := ClusterNamespace(cluster)

	// Deployment.
	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: ServiceSMD}, dep); err != nil {
		t.Fatalf("getting smd deployment: %v", err)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
		t.Errorf("expected replicas=2, got %v", dep.Spec.Replicas)
	}
	if dep.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		t.Errorf("expected RollingUpdate strategy, got %v", dep.Spec.Strategy.Type)
	}
	if dep.Spec.Strategy.RollingUpdate == nil ||
		dep.Spec.Strategy.RollingUpdate.MaxUnavailable.IntValue() != 0 ||
		dep.Spec.Strategy.RollingUpdate.MaxSurge.IntValue() != 1 {
		t.Errorf("unexpected rollingUpdate config: %+v", dep.Spec.Strategy.RollingUpdate)
	}
	if dep.Spec.Template.Spec.ServiceAccountName != ServiceSMD {
		t.Errorf("expected SA=%q, got %q", ServiceSMD, dep.Spec.Template.Spec.ServiceAccountName)
	}
	if len(dep.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(dep.Spec.Template.Spec.Containers))
	}
	container := dep.Spec.Template.Spec.Containers[0]
	if container.Image != defaultSMDImage {
		t.Errorf("expected default image %q, got %q", defaultSMDImage, container.Image)
	}
	if len(container.Ports) != 1 || container.Ports[0].ContainerPort != smdPort {
		t.Errorf("expected single port %d, got %+v", smdPort, container.Ports)
	}

	// SMD_DBPASS env var must reference the right secret/key.
	var foundDBPass bool
	for _, e := range container.Env {
		if e.Name == "SMD_DBPASS" {
			foundDBPass = true
			if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
				t.Fatalf("SMD_DBPASS missing secretKeyRef")
			}
			wantSecret := SecretName(cluster, SuffixDBCredentials)
			if e.ValueFrom.SecretKeyRef.Name != wantSecret {
				t.Errorf("expected SMD_DBPASS secret=%q, got %q", wantSecret, e.ValueFrom.SecretKeyRef.Name)
			}
			if e.ValueFrom.SecretKeyRef.Key != VaultKeySMDPassword {
				t.Errorf("expected SMD_DBPASS key=%q, got %q", VaultKeySMDPassword, e.ValueFrom.SecretKeyRef.Key)
			}
		}
	}
	if !foundDBPass {
		t.Errorf("expected SMD_DBPASS env var, got %+v", container.Env)
	}

	// Service.
	svc := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: ServiceSMD}, svc); err != nil {
		t.Fatalf("getting smd service: %v", err)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != smdPort {
		t.Errorf("expected service port %d, got %+v", smdPort, svc.Spec.Ports)
	}

	// PDB.
	pdb := &policyv1.PodDisruptionBudget{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: ServiceSMD}, pdb); err != nil {
		t.Fatalf("getting smd pdb: %v", err)
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Errorf("expected pdb minAvailable=1, got %+v", pdb.Spec.MinAvailable)
	}
}

func TestSMDReconciler_RequeuesUntilAvailable(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.Services.SMD.Enabled = true
	cluster.Spec.Services.SMD.Replicas = 1
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	r := &SMDReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while smd not yet available, got %+v", res)
	}
	st, ok := cluster.Status.Services[ServiceSMD]
	if !ok {
		t.Fatalf("expected status.services[smd] to be set")
	}
	if st.Ready {
		t.Errorf("expected status.services[smd].ready=false, got %+v", st)
	}
}

func TestSMDReconciler_ReadyWhenAvailable(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.Services.SMD.Enabled = true
	cluster.Spec.Services.SMD.Replicas = 1

	// Pre-create the Deployment with status.AvailableReplicas=1 so the
	// reconciler's read-back observes a ready deployment.
	existing := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceSMD,
			Namespace: ClusterNamespace(cluster),
		},
		Status: appsv1.DeploymentStatus{AvailableReplicas: 1},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&appsv1.Deployment{}).
		WithObjects(existing).
		Build()

	r := &SMDReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when smd available, got %+v", res)
	}
	st, ok := cluster.Status.Services[ServiceSMD]
	if !ok {
		t.Fatalf("expected status.services[smd] to be set")
	}
	if !st.Ready {
		t.Errorf("expected status.services[smd].ready=true, got %+v", st)
	}
}

func TestSMDReconciler_TwoClustersIsolated(t *testing.T) {
	scheme := newScheme(t)
	for _, name := range []string{testClusterRed, testClusterBlue} {
		cluster := newCluster(name)
		cluster.Spec.Services.SMD.Enabled = true
		cluster.Spec.Services.SMD.Replicas = 1
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

		r := &SMDReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
		if _, err := r.Reconcile(context.Background(), cluster); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}

		dep := &appsv1.Deployment{}
		if err := c.Get(context.Background(), types.NamespacedName{
			Namespace: "openchami-" + name,
			Name:      ServiceSMD,
		}, dep); err != nil {
			t.Fatalf("getting smd deployment for %s: %v", name, err)
		}
		if dep.Namespace != "openchami-"+name {
			t.Errorf("expected namespace openchami-%s, got %s", name, dep.Namespace)
		}
	}
}
