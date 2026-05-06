// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"testing"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

// newServiceMonitorClient mirrors the network-policy / topology pattern:
// the controller-runtime fake client requires the deduced type converter
// for SSA against ServiceMonitor (status field on the live object is not
// declared in the apply-config schema).
func newServiceMonitorClient(t *testing.T, cluster *openchamiv1alpha1.OpenCHAMICluster, extra ...client.Object) client.Client {
	t.Helper()
	scheme := newScheme(t)
	objs := append([]client.Object{cluster}, extra...)
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		Build()
}

func TestServiceMonitorReconciler_DisabledNoOp(t *testing.T) {
	cluster := newCluster("alpha")
	// Pre-create a ServiceMonitor so the test asserts the disabled path
	// does not delete user-managed objects either.
	preexisting := &monitoringv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openchami-alpha-services",
			Namespace: ClusterNamespace(cluster),
		},
		Spec: monitoringv1.ServiceMonitorSpec{
			Selector:  metav1.LabelSelector{MatchLabels: map[string]string{"foo": "bar"}},
			Endpoints: []monitoringv1.Endpoint{{Port: "metrics"}},
		},
	}
	c := newServiceMonitorClient(t, cluster, preexisting)

	r := &ServiceMonitorReconciler{Client: c, Recorder: record.NewFakeRecorder(4)}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &monitoringv1.ServiceMonitor{}
	err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ClusterNamespace(cluster),
		Name:      "openchami-alpha-services",
	}, got)
	if err != nil {
		t.Fatalf("expected pre-existing ServiceMonitor to remain, got error: %v", err)
	}
	// And no extra ones were created.
	list := &monitoringv1.ServiceMonitorList{}
	if err := c.List(context.Background(), list, client.InNamespace(ClusterNamespace(cluster))); err != nil {
		t.Fatalf("listing service monitors: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected exactly 1 ServiceMonitor (the pre-existing one), got %d", len(list.Items))
	}
}

func TestServiceMonitorReconciler_EnabledApplied(t *testing.T) {
	cluster := newCluster("beta")
	cluster.Spec.Observability.PrometheusOperator = true
	c := newServiceMonitorClient(t, cluster)

	r := &ServiceMonitorReconciler{Client: c, Recorder: record.NewFakeRecorder(4)}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	sm := &monitoringv1.ServiceMonitor{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ClusterNamespace(cluster),
		Name:      "openchami-beta-services",
	}, sm); err != nil {
		t.Fatalf("expected ServiceMonitor to exist, got %v", err)
	}

	if got := sm.Spec.Selector.MatchLabels[labelAppPartOf]; got != labelAppPartOfValue {
		t.Errorf("expected selector part-of=%q, got %q", labelAppPartOfValue, got)
	}
	if got := sm.Spec.Selector.MatchLabels[labelAppInst]; got != "openchami-beta" {
		t.Errorf("expected selector instance=openchami-beta, got %q", got)
	}
	if len(sm.Spec.Endpoints) != 1 {
		t.Fatalf("expected exactly 1 endpoint, got %d", len(sm.Spec.Endpoints))
	}
	ep := sm.Spec.Endpoints[0]
	if ep.Port != serviceMonitorPort {
		t.Errorf("endpoint port: want %q, got %q", serviceMonitorPort, ep.Port)
	}
	if ep.Path != serviceMonitorPath {
		t.Errorf("endpoint path: want %q, got %q", serviceMonitorPath, ep.Path)
	}
	if string(ep.Interval) != serviceMonitorInterval {
		t.Errorf("endpoint interval: want %q, got %q", serviceMonitorInterval, ep.Interval)
	}
}

func TestServiceMonitorReconciler_TwoClustersIsolated(t *testing.T) {
	for _, name := range []string{testClusterRed, testClusterBlue} {
		cluster := newCluster(name)
		cluster.Spec.Observability.PrometheusOperator = true
		c := newServiceMonitorClient(t, cluster)

		r := &ServiceMonitorReconciler{Client: c, Recorder: record.NewFakeRecorder(4)}
		if _, err := r.Reconcile(context.Background(), cluster); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}

		sm := &monitoringv1.ServiceMonitor{}
		if err := c.Get(context.Background(), types.NamespacedName{
			Namespace: ClusterNamespace(cluster),
			Name:      "openchami-" + name + "-services",
		}, sm); err != nil {
			t.Fatalf("getting %s ServiceMonitor: %v", name, err)
		}
		if got := sm.Spec.Selector.MatchLabels[labelAppInst]; got != "openchami-"+name {
			t.Errorf("cluster %s: expected instance label openchami-%s, got %q", name, name, got)
		}
		if got := sm.Namespace; got != "openchami-"+name {
			t.Errorf("cluster %s: expected namespace openchami-%s, got %q", name, name, got)
		}

		// No leakage: the other cluster's ServiceMonitor must not
		// exist in this namespace.
		other := testClusterBlue
		if name == testClusterBlue {
			other = testClusterRed
		}
		stray := &monitoringv1.ServiceMonitor{}
		err := c.Get(context.Background(), types.NamespacedName{
			Namespace: ClusterNamespace(cluster),
			Name:      "openchami-" + other + "-services",
		}, stray)
		if err == nil || !apierrors.IsNotFound(err) {
			t.Errorf("cluster %s: did not expect %s ServiceMonitor to exist, got err=%v", name, other, err)
		}
	}
}
