/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package reconcilers

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openchami/openchami-operator/internal/conditions"
)

func TestFunicularReconciler_DisabledTriviallyReady(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.Logging.Enabled = false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	r := &FunicularReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when logging disabled, got %+v", res)
	}

	ds := &appsv1.DaemonSet{}
	getErr := c.Get(context.Background(), types.NamespacedName{
		Namespace: ClusterNamespace(cluster),
		Name:      ServiceFunicular,
	}, ds)
	if !apierrors.IsNotFound(getErr) {
		t.Errorf("expected funicular DaemonSet absent when logging disabled, got err=%v", getErr)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionLogCollectorReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != conditions.ReasonReady {
		t.Fatalf("expected LogCollectorReady=True/Ready when disabled, got %+v", cond)
	}
}

func assertFunicularPodSpec(t *testing.T, ds *appsv1.DaemonSet) {
	t.Helper()
	spec := ds.Spec.Template.Spec
	if spec.ServiceAccountName != ServiceFunicular {
		t.Errorf("expected SA=%q, got %q", ServiceFunicular, spec.ServiceAccountName)
	}
	if spec.NodeSelector != nil {
		t.Errorf("expected no nodeSelector (collector runs everywhere), got %+v", spec.NodeSelector)
	}
	if len(spec.Tolerations) != 1 || spec.Tolerations[0].Operator != corev1.TolerationOpExists {
		t.Errorf("expected single Exists toleration, got %+v", spec.Tolerations)
	}
}

func assertFunicularLogVolume(t *testing.T, ds *appsv1.DaemonSet) {
	t.Helper()
	var foundLogVol bool
	for _, v := range ds.Spec.Template.Spec.Volumes {
		if v.Name != funicularLogVolume {
			continue
		}
		foundLogVol = true
		if v.HostPath == nil || v.HostPath.Path != funicularHostLogPath {
			t.Errorf("expected hostPath %q, got %+v", funicularHostLogPath, v.HostPath)
		}
	}
	if !foundLogVol {
		t.Errorf("expected hostPath volume %q on DaemonSet", funicularLogVolume)
	}
}

func assertFunicularLogMount(t *testing.T, container corev1.Container) {
	t.Helper()
	var sawReadOnlyLogMount bool
	for _, m := range container.VolumeMounts {
		if m.Name != funicularLogVolume {
			continue
		}
		sawReadOnlyLogMount = true
		if !m.ReadOnly {
			t.Errorf("expected /var/log/pods mount to be read-only")
		}
		if m.MountPath != funicularHostLogPath {
			t.Errorf("expected mount path %q, got %q", funicularHostLogPath, m.MountPath)
		}
	}
	if !sawReadOnlyLogMount {
		t.Errorf("expected container to mount %q", funicularLogVolume)
	}
}

func assertFunicularEnvValues(t *testing.T, envs map[string]corev1.EnvVar) {
	t.Helper()
	cases := map[string]string{
		"FUNICULAR_CLUSTER_NAME":     "alpha",
		"FUNICULAR_NAMESPACE":        "openchami-alpha",
		"FUNICULAR_S3_ENDPOINT":      testS3Endpoint,
		"FUNICULAR_S3_BUCKET":        "alpha-logs",
		"FUNICULAR_FLUSH_INTERVAL":   "60",
		"FUNICULAR_INCLUDE_SERVICES": "smd,boot-service",
	}
	for name, want := range cases {
		got, ok := envs[name]
		if !ok {
			t.Errorf("missing env %q", name)
			continue
		}
		if got.Value != want {
			t.Errorf("env %q: expected %q, got %q", name, want, got.Value)
		}
	}
}

func assertFunicularSecretRef(t *testing.T, envs map[string]corev1.EnvVar, wantSecret, name, key string) {
	t.Helper()
	ev, ok := envs[name]
	if !ok {
		t.Errorf("missing env %q", name)
		return
	}
	if ev.ValueFrom == nil || ev.ValueFrom.SecretKeyRef == nil {
		t.Errorf("env %q: expected SecretKeyRef, got %+v", name, ev)
		return
	}
	if ev.ValueFrom.SecretKeyRef.Name != wantSecret {
		t.Errorf("env %q: expected SecretKeyRef.Name=%q, got %q",
			name, wantSecret, ev.ValueFrom.SecretKeyRef.Name)
	}
	if ev.ValueFrom.SecretKeyRef.Key != key {
		t.Errorf("env %q: expected SecretKeyRef.Key=%q, got %q",
			name, key, ev.ValueFrom.SecretKeyRef.Key)
	}
}

func TestFunicularReconciler_AppliesDaemonSet(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.Logging.Enabled = true
	cluster.Spec.Logging.RetentionDays = 90
	cluster.Spec.Logging.FlushIntervalSeconds = 60
	cluster.Spec.Logging.IncludeServices = []string{"smd", "boot-service"}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	r := &FunicularReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}

	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// No NumberReady on the freshly-applied (no-status) DaemonSet, so requeue.
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while DaemonSet has 0 ready pods, got %+v", res)
	}

	ds := &appsv1.DaemonSet{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ClusterNamespace(cluster),
		Name:      ServiceFunicular,
	}, ds); err != nil {
		t.Fatalf("getting funicular DaemonSet: %v", err)
	}

	assertFunicularPodSpec(t, ds)
	assertFunicularLogVolume(t, ds)

	if len(ds.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(ds.Spec.Template.Spec.Containers))
	}
	container := ds.Spec.Template.Spec.Containers[0]
	if container.Image != defaultFunicularImage {
		t.Errorf("expected image=%q, got %q", defaultFunicularImage, container.Image)
	}
	assertFunicularLogMount(t, container)

	envs := map[string]corev1.EnvVar{}
	for _, e := range container.Env {
		envs[e.Name] = e
	}
	assertFunicularEnvValues(t, envs)

	wantSecret := SecretName(cluster, SuffixLogCredentials)
	assertFunicularSecretRef(t, envs, wantSecret, "FUNICULAR_ACCESS_KEY", s3AccessKeyKey)
	assertFunicularSecretRef(t, envs, wantSecret, "FUNICULAR_SECRET_KEY", s3SecretKeyKey)

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionLogCollectorReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonProvisioning {
		t.Fatalf("expected LogCollectorReady=False/Provisioning before pods ready, got %+v", cond)
	}
}

func TestFunicularReconciler_ReadyWhenNumberReady(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.Logging.Enabled = true
	cluster.Spec.Logging.RetentionDays = 30
	cluster.Spec.Logging.FlushIntervalSeconds = 30

	existingDS := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceFunicular,
			Namespace: ClusterNamespace(cluster),
		},
		Status: appsv1.DaemonSetStatus{NumberReady: 1},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&appsv1.DaemonSet{}).
		WithObjects(existingDS).
		Build()

	r := &FunicularReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when DaemonSet already ready, got %+v", res)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionLogCollectorReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != conditions.ReasonReady {
		t.Fatalf("expected LogCollectorReady=True/Ready, got %+v", cond)
	}
	st, ok := cluster.Status.Services[ServiceFunicular]
	if !ok {
		t.Fatalf("expected Status.Services[%q] to be set", ServiceFunicular)
	}
	if !st.Ready {
		t.Errorf("expected Status.Services[%q].Ready=true, got %+v", ServiceFunicular, st)
	}
}

func TestFunicularReconciler_RequeuesUntilReady(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	cluster.Spec.Logging.Enabled = true
	cluster.Spec.Logging.RetentionDays = 30
	cluster.Spec.Logging.FlushIntervalSeconds = 60

	existingDS := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceFunicular,
			Namespace: ClusterNamespace(cluster),
		},
		Status: appsv1.DaemonSetStatus{NumberReady: 0},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&appsv1.DaemonSet{}).
		WithObjects(existingDS).
		Build()

	r := &FunicularReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue when NumberReady=0, got %+v", res)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionLogCollectorReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonProvisioning {
		t.Fatalf("expected LogCollectorReady=False/Provisioning, got %+v", cond)
	}
}

func TestFunicularReconciler_TwoClustersIsolated(t *testing.T) {
	scheme := newScheme(t)
	rec := record.NewFakeRecorder(20)

	for _, name := range []string{testClusterRed, testClusterBlue} {
		cluster := newCluster(name)
		cluster.Spec.Logging.Enabled = true
		cluster.Spec.Logging.RetentionDays = 7
		cluster.Spec.Logging.FlushIntervalSeconds = 15

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
		r := &FunicularReconciler{Client: c, Recorder: rec}
		if _, err := r.Reconcile(context.Background(), cluster); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}

		ds := &appsv1.DaemonSet{}
		if err := c.Get(context.Background(), types.NamespacedName{
			Namespace: ClusterNamespace(cluster),
			Name:      ServiceFunicular,
		}, ds); err != nil {
			t.Fatalf("getting funicular DaemonSet for %s: %v", name, err)
		}
		if ds.Namespace != "openchami-"+name {
			t.Errorf("expected namespace openchami-%s, got %s", name, ds.Namespace)
		}

		envs := map[string]string{}
		for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
			envs[e.Name] = e.Value
		}
		if got := envs["FUNICULAR_CLUSTER_NAME"]; got != name {
			t.Errorf("expected FUNICULAR_CLUSTER_NAME=%q, got %q", name, got)
		}
		if got, want := envs["FUNICULAR_S3_BUCKET"], name+"-logs"; got != want {
			t.Errorf("expected FUNICULAR_S3_BUCKET=%q, got %q", want, got)
		}
		if got := envs["FUNICULAR_NAMESPACE"]; got != "openchami-"+name {
			t.Errorf("expected FUNICULAR_NAMESPACE=openchami-%s, got %q", name, got)
		}
	}
}
