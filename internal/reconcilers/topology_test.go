/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package reconcilers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openahamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/vault"
)

// fixedNow returns a deterministic clock used by topology tests so the
// generatedAt timestamp does not influence equality assertions on hash
// stability vs. spec mutation.
func fixedNow(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// newTopologyClient mirrors newNetworkPolicyClient: SSA against the fake
// client requires the deduced converter for ConfigMap apply patches.
func newTopologyClient(t *testing.T, cluster *openahamiv1alpha1.OpenCHAMICluster) client.Client {
	t.Helper()
	scheme := newScheme(t)
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		Build()
}

// fetchTopology reads the topology ConfigMap and parses topology.json.
func fetchTopology(t *testing.T, c client.Client, cluster *openahamiv1alpha1.OpenCHAMICluster) (*corev1.ConfigMap, TopologySpec) {
	t.Helper()
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{
		Namespace: ClusterNamespace(cluster),
		Name:      topologyConfigMapName(cluster),
	}
	if err := c.Get(context.Background(), key, cm); err != nil {
		t.Fatalf("getting topology configmap: %v", err)
	}
	raw, ok := cm.Data[topologyConfigMapKey]
	if !ok {
		t.Fatalf("topology configmap missing data key %q", topologyConfigMapKey)
	}
	var spec TopologySpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatalf("unmarshaling topology.json: %v", err)
	}
	return cm, spec
}

func TestTopologyReconciler_AppliesConfigMap(t *testing.T) {
	cluster := newCluster(testClusterAlpha)
	c := newTopologyClient(t, cluster)
	r := &TopologyReconciler{
		Client:   c,
		Recorder: record.NewFakeRecorder(10),
		nowFunc:  fixedNow(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)),
	}

	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cm, spec := fetchTopology(t, c, cluster)

	if cm.Labels[topologyLabelKey] != topologyLabelValue {
		t.Errorf("missing topology label: got %v", cm.Labels)
	}
	if cm.Labels[clusterLabelKey] != testClusterAlpha {
		t.Errorf("missing cluster label: got %v", cm.Labels)
	}

	if spec.ClusterName != testClusterAlpha {
		t.Errorf("clusterName: want alpha, got %q", spec.ClusterName)
	}
	if spec.Domain != testClusterAlpha+".test.local" {
		t.Errorf("domain: want alpha.test.local, got %q", spec.Domain)
	}

	ns := ClusterNamespace(cluster)
	wantSMD := fmt.Sprintf("http://smd.%s.svc.cluster.local:27779", ns)
	if spec.Services.SMD.Endpoint != wantSMD {
		t.Errorf("smd endpoint: want %q, got %q", wantSMD, spec.Services.SMD.Endpoint)
	}
	wantTok := fmt.Sprintf("http://tokensmith.%s.svc.cluster.local:8080", ns)
	if spec.Services.Tokensmith.Endpoint != wantTok {
		t.Errorf("tokensmith endpoint: want %q, got %q", wantTok, spec.Services.Tokensmith.Endpoint)
	}
	wantJWKS := wantTok + "/.well-known/jwks.json"
	if spec.Services.Tokensmith.JWKSURL != wantJWKS {
		t.Errorf("tokensmith jwksURL: want %q, got %q", wantJWKS, spec.Services.Tokensmith.JWKSURL)
	}
	wantBoot := fmt.Sprintf("http://boot-service.%s.svc.cluster.local:27778", ns)
	if spec.Services.BootService.Endpoint != wantBoot {
		t.Errorf("boot-service endpoint: want %q, got %q", wantBoot, spec.Services.BootService.Endpoint)
	}
	if spec.Services.BootService.S3Endpoint != "http://s3.test:9000" {
		t.Errorf("boot-service s3Endpoint: got %q", spec.Services.BootService.S3Endpoint)
	}
	if spec.Services.BootService.S3Bucket != "alpha-boot-images" {
		t.Errorf("boot-service s3Bucket: got %q", spec.Services.BootService.S3Bucket)
	}
	wantMeta := fmt.Sprintf("http://metadata-service.%s.svc.cluster.local:8081", ns)
	if spec.Services.MetadataService.Endpoint != wantMeta {
		t.Errorf("metadata-service endpoint: want %q, got %q", wantMeta, spec.Services.MetadataService.Endpoint)
	}

	paths := vault.Paths(testClusterAlpha)
	if spec.Platform.Vault.Address != "http://vault.test:8200" {
		t.Errorf("vault address: got %q", spec.Platform.Vault.Address)
	}
	if spec.Platform.Vault.KVMount != paths.KVMount {
		t.Errorf("vault kvMount: want %q, got %q", paths.KVMount, spec.Platform.Vault.KVMount)
	}
	if spec.Platform.Vault.PathPrefix != paths.SecretPrefix {
		t.Errorf("vault pathPrefix: want %q, got %q", paths.SecretPrefix, spec.Platform.Vault.PathPrefix)
	}

	wantRW := fmt.Sprintf("openchami-alpha-postgres-rw.%s.svc.cluster.local:5432", ns)
	if spec.Database.ReadWriteEndpoint != wantRW {
		t.Errorf("database rw: want %q, got %q", wantRW, spec.Database.ReadWriteEndpoint)
	}
	wantRO := fmt.Sprintf("openchami-alpha-postgres-ro.%s.svc.cluster.local:5432", ns)
	if spec.Database.ReadOnlyEndpoint != wantRO {
		t.Errorf("database ro: want %q, got %q", wantRO, spec.Database.ReadOnlyEndpoint)
	}

	if !strings.HasPrefix(spec.Version, topologyHashPrefix) {
		t.Errorf("version not sha256-prefixed: %q", spec.Version)
	}
	if cluster.Status.TopologyVersion != spec.Version {
		t.Errorf("status.topologyVersion: want %q, got %q", spec.Version, cluster.Status.TopologyVersion)
	}
	if spec.GeneratedAt != "2026-05-01T12:00:00Z" {
		t.Errorf("generatedAt: got %q", spec.GeneratedAt)
	}
}

func TestTopologyReconciler_HashIsDeterministic(t *testing.T) {
	cluster := newCluster(testClusterAlpha)
	c := newTopologyClient(t, cluster)

	// Two reconciles with two different wall-clock times; the version hash
	// excludes generatedAt and version itself, so the digest must match.
	r1 := &TopologyReconciler{
		Client:   c,
		Recorder: record.NewFakeRecorder(10),
		nowFunc:  fixedNow(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)),
	}
	if _, err := r1.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	first := cluster.Status.TopologyVersion

	r2 := &TopologyReconciler{
		Client:   c,
		Recorder: record.NewFakeRecorder(10),
		nowFunc:  fixedNow(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
	if _, err := r2.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	second := cluster.Status.TopologyVersion

	if first != second {
		t.Errorf("topology hash changed despite identical inputs: %q vs %q", first, second)
	}

	// Confirm the rendered generatedAt actually differed across reconciles.
	_, spec := fetchTopology(t, c, cluster)
	if spec.GeneratedAt != "2027-01-01T00:00:00Z" {
		t.Errorf("expected second-run generatedAt, got %q", spec.GeneratedAt)
	}
}

func TestTopologyReconciler_HashChangesOnSpecChange(t *testing.T) {
	cluster := newCluster(testClusterAlpha)
	c := newTopologyClient(t, cluster)
	r := &TopologyReconciler{
		Client:   c,
		Recorder: record.NewFakeRecorder(10),
		nowFunc:  fixedNow(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)),
	}

	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	first := cluster.Status.TopologyVersion

	cluster.Spec.Domain = "alpha-changed.test.local"
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	second := cluster.Status.TopologyVersion

	if first == second {
		t.Errorf("topology hash unchanged after Domain mutation: %q", first)
	}
}

func TestTopologyReconciler_ServiceReadyReflectsStatus(t *testing.T) {
	cluster := newCluster(testClusterAlpha)
	cluster.Status.Services = map[string]openahamiv1alpha1.ServiceStatus{
		ServiceSMD: {Ready: true, Endpoint: "ignored"},
	}
	c := newTopologyClient(t, cluster)
	r := &TopologyReconciler{
		Client:   c,
		Recorder: record.NewFakeRecorder(10),
		nowFunc:  fixedNow(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)),
	}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	_, spec := fetchTopology(t, c, cluster)
	if !spec.Services.SMD.Ready {
		t.Errorf("smd: expected ready=true, got false")
	}
	// Tokensmith was never reported by an upstream reconciler; topology must
	// still emit the entry with ready=false.
	if spec.Services.Tokensmith.Ready {
		t.Errorf("tokensmith: expected ready=false (absent from status), got true")
	}
}

func TestTopologyReconciler_ConditionSet(t *testing.T) {
	cluster := newCluster(testClusterAlpha)
	c := newTopologyClient(t, cluster)
	r := &TopologyReconciler{
		Client:   c,
		Recorder: record.NewFakeRecorder(10),
		nowFunc:  fixedNow(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)),
	}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionTopologyPublished)
	if cond == nil {
		t.Fatalf("missing TopologyPublished condition")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("status: want True, got %q", cond.Status)
	}
	if cond.Reason != conditions.ReasonReady {
		t.Errorf("reason: want %q, got %q", conditions.ReasonReady, cond.Reason)
	}
	if cond.ObservedGeneration != cluster.Generation {
		t.Errorf("observedGeneration: want %d, got %d", cluster.Generation, cond.ObservedGeneration)
	}
}

func TestTopologyReconciler_TwoClustersIsolated(t *testing.T) {
	scheme := newScheme(t)
	rec := record.NewFakeRecorder(20)
	now := fixedNow(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))

	results := map[string]TopologySpec{}
	for _, name := range []string{testClusterRed, testClusterBlue} {
		cluster := newCluster(name)
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(cluster).
			WithTypeConverters(managedfields.NewDeducedTypeConverter()).
			Build()
		r := &TopologyReconciler{Client: c, Recorder: rec, nowFunc: now}
		if _, err := r.Reconcile(context.Background(), cluster); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}

		// Confirm the ConfigMap exists in the cluster's own namespace.
		cm := &corev1.ConfigMap{}
		key := types.NamespacedName{
			Namespace: ClusterNamespace(cluster),
			Name:      topologyConfigMapName(cluster),
		}
		if err := c.Get(context.Background(), key, cm); err != nil {
			t.Fatalf("getting %s topology configmap: %v", name, err)
		}

		var spec TopologySpec
		if err := json.Unmarshal([]byte(cm.Data[topologyConfigMapKey]), &spec); err != nil {
			t.Fatalf("unmarshaling %s topology.json: %v", name, err)
		}
		results[name] = spec

		// Sanity-check namespace isolation: nothing leaked into the wrong
		// namespace.
		other := testClusterBlue
		if name == testClusterBlue {
			other = testClusterRed
		}
		stray := &corev1.ConfigMap{}
		strayKey := types.NamespacedName{
			Namespace: "openchami-" + other,
			Name:      "openchami-" + other + "-topology",
		}
		if err := c.Get(context.Background(), strayKey, stray); err == nil {
			t.Errorf("%s reconcile leaked configmap into namespace %s", name, strayKey.Namespace)
		}
	}

	red := results[testClusterRed]
	blue := results[testClusterBlue]
	if red.ClusterName == blue.ClusterName {
		t.Errorf("two clusters share clusterName")
	}
	if red.ClusterName != testClusterRed || blue.ClusterName != testClusterBlue {
		t.Errorf("cluster names crossed over: red=%q blue=%q", red.ClusterName, blue.ClusterName)
	}
	if !strings.Contains(red.Services.SMD.Endpoint, "openchami-"+testClusterRed) {
		t.Errorf("red SMD endpoint missing red namespace: %q", red.Services.SMD.Endpoint)
	}
	if strings.Contains(red.Services.SMD.Endpoint, testClusterBlue) {
		t.Errorf("red SMD endpoint contains blue cluster name: %q", red.Services.SMD.Endpoint)
	}
}
