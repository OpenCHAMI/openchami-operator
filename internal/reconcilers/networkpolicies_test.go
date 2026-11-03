// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/networkprobe"
)

// Shared fixture URLs for the in-cluster .svc.cluster.local fast-path used by
// most NetworkPolicy tests.
const (
	testInClusterVaultAddr     = "http://vault.vault-system.svc.cluster.local:8200"
	testInClusterVersityGWAddr = "http://versitygw.s3-system.svc.cluster.local:10000"
	testVaultSystemNS          = "vault-system"
	testVersityGWSystemNS      = "s3-system"
	testExternalVaultAddr      = "http://10.20.30.40:8200"
	testExternalVersityGWAddr  = "http://172.16.5.10:10000"
	testExternalVaultCIDR      = "10.20.30.40/32"
	testExternalVersityGWCIDR  = "172.16.5.10/32"
	testProbeSubnet            = "192.168.1.0/24"

	// testAPIServerIP is the address seeded into the default/kubernetes
	// Endpoints fixture so KubernetesAPIEgressPeers has something to discover.
	testAPIServerIP   = "10.96.0.1"
	testAPIServerCIDR = "10.96.0.1/32"

	// testAPIServerPort is the port seeded into the default/kubernetes
	// EndpointSlice fixture. It is deliberately 6443 (the k3s/RKE2 API-server
	// port) rather than 443 so the tests exercise the dynamic port-discovery
	// path from issue #31 — the CNPG egress rule must target the discovered
	// port, not a hardcoded 443.
	testAPIServerPort int32 = 6443
)

// kubernetesEndpoints returns an EndpointSlice for the default/kubernetes
// Service backing the API-server discovery path exercised by
// KubernetesAPIEgressPeers. It carries the well-known
// kubernetes.io/service-name=kubernetes label the helper selects on, and the
// discovered API-server port (testAPIServerPort).
func kubernetesEndpoints(ips ...string) *discoveryv1.EndpointSlice {
	eps := make([]discoveryv1.Endpoint, 0, len(ips))
	for _, ip := range ips {
		eps = append(eps, discoveryv1.Endpoint{Addresses: []string{ip}})
	}
	port := testAPIServerPort
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kubernetesServiceName,
			Namespace: kubernetesServiceNamespace,
			Labels:    map[string]string{discoveryv1.LabelServiceName: kubernetesServiceName},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   eps,
		Ports: []discoveryv1.EndpointPort{
			{Port: &port},
		},
	}
}

// newNetworkPolicyClient builds a fake client wired with only the
// DeducedTypeConverter. This works around a controller-runtime fake-client
// quirk: the default multiTypeConverter falls through to the deduced
// converter for the in-memory live NetworkPolicy (whose Go-side `.status`
// field is not declared in the apply-config schema), but uses the scheme
// converter for the unstructured patch object — producing two TypedValues
// from different schemas and failing with
//
//	"failed to merge config: expected objects with types from the same schema"
//
// when SSA reconciles them. Forcing the deduced converter for both sides
// keeps the schemas consistent.
//
// Production code never hits this path: real apiservers do not depend on the
// fake client's converter wiring.
func newNetworkPolicyClient(scheme *runtime.Scheme, cp *openchamiv1alpha1.OpenCHAMIControlPlane) client.WithWatch {
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cp, kubernetesEndpoints(testAPIServerIP)).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		Build()
}

// newInClusterNetworkPolicyCluster returns a cluster wired for in-cluster
// Vault and VersityGW so the namespaceSelector code path is exercised.
func newInClusterNetworkPolicyCluster(name string) *openchamiv1alpha1.OpenCHAMIControlPlane {
	c := newControlPlane(name)
	c.Spec.Platform.Vault.Address = testInClusterVaultAddr
	c.Spec.Platform.ObjectStorage.Endpoint = testInClusterVersityGWAddr
	return c
}

// expectedPolicyNames returns the canonical set of NetworkPolicy names this
// reconciler must apply. The list is centralised here so test assertions stay
// in lockstep with policy additions.
func expectedPolicyNames() []string {
	names := []string{
		policyDefaultDenyAll,
		policyAllowDNSEgress,
		policyAllowVaultEgress,
		policyAllowVersityGWEgress,
		policyAllowLogsEgress,
		policyAllowCNPGKubernetesAPIEgress,
		policySMD,
		policyTokensmith,
		policyBootService,
		policyMetadataService,
		policyCoreDHCP,
		policyMagellan,
		policyNetworkProbe,
		policyFunicular,
		policyPostgresIngress,
		policyPostgresEgress,
	}
	sort.Strings(names)
	return names
}

// listPolicies returns every NetworkPolicy in the cluster's namespace.
func listPolicies(t *testing.T, c client.Client, namespace string) *networkingv1.NetworkPolicyList {
	t.Helper()
	out := &networkingv1.NetworkPolicyList{}
	if err := c.List(context.Background(), out, client.InNamespace(namespace)); err != nil {
		t.Fatalf("listing network policies in %s: %v", namespace, err)
	}
	return out
}

func policyByName(list *networkingv1.NetworkPolicyList, name string) *networkingv1.NetworkPolicy {
	for i := range list.Items {
		if list.Items[i].Name == name {
			return &list.Items[i]
		}
	}
	return nil
}

func TestNetworkPoliciesReconciler_AppliesAllPolicies(t *testing.T) {
	scheme := newScheme(t)
	cp := newInClusterNetworkPolicyCluster("alpha")
	c := newNetworkPolicyClient(scheme, cp)

	r := &NetworkPoliciesReconciler{Client: c, Recorder: record.NewFakeRecorder(20)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	list := listPolicies(t, c, ControlPlaneNamespace(cp))
	assertAllExpectedPolicies(t, list)
	assertDefaultDenyAll(t, list)
	assertAllowDNSEgress(t, list)
	assertSMDPolicy(t, list)
	assertCNPGKubernetesAPIPolicy(t, list, cp.Spec.ClusterName)
	assertPostgresPolicies(t, list, cp.Spec.ClusterName)
}

func assertAllExpectedPolicies(t *testing.T, list *networkingv1.NetworkPolicyList) {
	t.Helper()
	if got, want := len(list.Items), len(expectedPolicyNames()); got != want {
		t.Fatalf("expected %d policies, got %d", want, got)
	}
	got := make([]string, 0, len(list.Items))
	for _, p := range list.Items {
		got = append(got, p.Name)
	}
	sort.Strings(got)
	want := expectedPolicyNames()
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("policy[%d]: want %q, got %q", i, want[i], got[i])
		}
	}
}

func assertDefaultDenyAll(t *testing.T, list *networkingv1.NetworkPolicyList) {
	t.Helper()
	deny := policyByName(list, policyDefaultDenyAll)
	if deny == nil {
		t.Fatalf("missing %s", policyDefaultDenyAll)
	}
	if len(deny.Spec.PodSelector.MatchLabels) != 0 || len(deny.Spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("expected empty PodSelector for default-deny-all, got %+v", deny.Spec.PodSelector)
	}
	if len(deny.Spec.Ingress) != 0 || len(deny.Spec.Egress) != 0 {
		t.Errorf("expected no rules in default-deny-all, got ingress=%d egress=%d",
			len(deny.Spec.Ingress), len(deny.Spec.Egress))
	}
	wantTypes := map[networkingv1.PolicyType]bool{
		networkingv1.PolicyTypeIngress: false,
		networkingv1.PolicyTypeEgress:  false,
	}
	for _, pt := range deny.Spec.PolicyTypes {
		wantTypes[pt] = true
	}
	if !wantTypes[networkingv1.PolicyTypeIngress] || !wantTypes[networkingv1.PolicyTypeEgress] {
		t.Errorf("expected both Ingress+Egress in PolicyTypes, got %+v", deny.Spec.PolicyTypes)
	}
}

func assertAllowDNSEgress(t *testing.T, list *networkingv1.NetworkPolicyList) {
	t.Helper()
	dns := policyByName(list, policyAllowDNSEgress)
	if dns == nil {
		t.Fatalf("missing %s", policyAllowDNSEgress)
	}
	if len(dns.Spec.Egress) != 1 {
		t.Fatalf("expected 1 egress rule, got %d", len(dns.Spec.Egress))
	}
	sawUDP, sawTCP := false, false
	for _, p := range dns.Spec.Egress[0].Ports {
		if p.Port == nil || p.Port.IntVal != 53 || p.Protocol == nil {
			continue
		}
		switch *p.Protocol {
		case corev1.ProtocolUDP:
			sawUDP = true
		case corev1.ProtocolTCP:
			sawTCP = true
		}
	}
	if !sawUDP || !sawTCP {
		t.Errorf("expected DNS port 53 UDP+TCP, got %+v", dns.Spec.Egress[0].Ports)
	}
}

func assertSMDPolicy(t *testing.T, list *networkingv1.NetworkPolicyList) {
	t.Helper()
	smd := policyByName(list, policySMD)
	if smd == nil {
		t.Fatalf("missing %s", policySMD)
	}
	if len(smd.Spec.Ingress) != 1 {
		t.Fatalf("expected 1 ingress rule on smd-policy, got %d", len(smd.Spec.Ingress))
	}
	wantPeers := map[string]bool{
		ServiceBootService:     false,
		ServiceMetadataService: false,
		ServiceMagellan:        false,
	}
	sawEnvoyNS := false
	for _, peer := range smd.Spec.Ingress[0].From {
		if peer.PodSelector != nil {
			if name, ok := peer.PodSelector.MatchLabels[labelAppName]; ok {
				if _, want := wantPeers[name]; want {
					wantPeers[name] = true
				}
			}
		}
		if peer.NamespaceSelector != nil &&
			peer.NamespaceSelector.MatchLabels[kubernetesMetadataNameLabel] == envoyGatewaySystemNS {
			sawEnvoyNS = true
		}
	}
	for name, ok := range wantPeers {
		if !ok {
			t.Errorf("smd-policy missing ingress peer %q", name)
		}
	}
	if !sawEnvoyNS {
		t.Errorf("smd-policy missing ingress from envoy-gateway-system namespace")
	}

	if len(smd.Spec.Egress) != 2 {
		t.Fatalf("expected 2 egress rules on smd-policy, got %d", len(smd.Spec.Egress))
	}
	sawPostgres, sawTokensmith := false, false
	for _, eg := range smd.Spec.Egress {
		for _, p := range eg.Ports {
			if p.Port == nil {
				continue
			}
			switch p.Port.IntVal {
			case postgresPort:
				sawPostgres = true
			case tokensmithPort:
				sawTokensmith = true
			}
		}
	}
	if !sawPostgres {
		t.Errorf("smd-policy missing postgres egress port %d", postgresPort)
	}
	if !sawTokensmith {
		t.Errorf("smd-policy missing tokensmith egress port %d", tokensmithPort)
	}
}

func assertCNPGKubernetesAPIPolicy(t *testing.T, list *networkingv1.NetworkPolicyList, clusterName string) {
	t.Helper()
	policy := policyByName(list, policyAllowCNPGKubernetesAPIEgress)
	if policy == nil {
		t.Fatalf("missing %s", policyAllowCNPGKubernetesAPIEgress)
	}

	// Verify pod selector targets CNPG postgres pods
	cnpgClusterName := "openchami-" + clusterName + "-postgres"
	if got := policy.Spec.PodSelector.MatchLabels[cnpgClusterLabel]; got != cnpgClusterName {
		t.Errorf("expected pod selector %s=%s, got %s=%s",
			cnpgClusterLabel, cnpgClusterName, cnpgClusterLabel, got)
	}

	// Verify egress rule exists
	if len(policy.Spec.Egress) != 1 {
		t.Fatalf("expected 1 egress rule, got %d", len(policy.Spec.Egress))
	}

	egress := policy.Spec.Egress[0]

	// Verify the discovered API-server endpoint IP is targeted as an ipBlock,
	// not a namespaceSelector (which does not match the post-DNAT API IP).
	if len(egress.To) != 1 {
		t.Fatalf("expected 1 peer in egress rule, got %d", len(egress.To))
	}
	peer := egress.To[0]
	if peer.IPBlock == nil {
		t.Fatalf("expected ipBlock peer, got %+v", peer)
	}
	if peer.IPBlock.CIDR != testAPIServerCIDR {
		t.Errorf("expected ipBlock CIDR %q, got %q", testAPIServerCIDR, peer.IPBlock.CIDR)
	}
	if peer.NamespaceSelector != nil {
		t.Errorf("expected no namespaceSelector, got %+v", peer.NamespaceSelector)
	}

	// Verify the discovered API-server port is allowed (not a hardcoded 443).
	if len(egress.Ports) != 1 {
		t.Fatalf("expected 1 port, got %d", len(egress.Ports))
	}
	port := egress.Ports[0]
	if port.Port == nil || port.Port.IntVal != testAPIServerPort {
		t.Errorf("expected port %d, got %+v", testAPIServerPort, port.Port)
	}
	if port.Protocol == nil || *port.Protocol != corev1.ProtocolTCP {
		t.Errorf("expected TCP protocol, got %+v", port.Protocol)
	}
}

// assertPostgresPolicies verifies both the ingress and egress halves of the
// CNPG pod-to-pod data plane. The egress half is the fix for issue #14: the
// default-deny-all policy blocks a replica join pod from dialing the primary,
// producing the "dial error: timeout" in the bug report.
func assertPostgresPolicies(t *testing.T, list *networkingv1.NetworkPolicyList, clusterName string) {
	t.Helper()
	cnpgCluster := "openchami-" + clusterName + "-postgres"

	// Ingress: CNPG pods must accept 5432 from SMD, boot-service and their
	// own cluster peers (replication), plus :8000 from cnpg-system.
	ingress := policyByName(list, policyPostgresIngress)
	if ingress == nil {
		t.Fatalf("missing %s", policyPostgresIngress)
	}
	if got := ingress.Spec.PodSelector.MatchLabels[cnpgClusterLabel]; got != cnpgCluster {
		t.Errorf("ingress pod selector: want %s=%s, got %q", cnpgClusterLabel, cnpgCluster, got)
	}
	sawCNPGIngressPeer := false
	for _, rule := range ingress.Spec.Ingress {
		for _, peer := range rule.From {
			if peer.PodSelector != nil &&
				peer.PodSelector.MatchLabels[cnpgClusterLabel] == cnpgCluster {
				sawCNPGIngressPeer = true
			}
		}
	}
	if !sawCNPGIngressPeer {
		t.Errorf("%s missing cnpg cluster-peer ingress (replication)", policyPostgresIngress)
	}

	// Egress: CNPG pods must be allowed to dial their own cluster peers on
	// 5432 (join → -rw Service, and streaming replication).
	egress := policyByName(list, policyPostgresEgress)
	if egress == nil {
		t.Fatalf("missing %s", policyPostgresEgress)
	}
	if got := egress.Spec.PodSelector.MatchLabels[cnpgClusterLabel]; got != cnpgCluster {
		t.Errorf("egress pod selector: want %s=%s, got %q", cnpgClusterLabel, cnpgCluster, got)
	}
	if len(egress.Spec.PolicyTypes) != 1 || egress.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Errorf("%s: want PolicyTypes [Egress], got %v", policyPostgresEgress, egress.Spec.PolicyTypes)
	}
	if len(egress.Spec.Egress) != 1 {
		t.Fatalf("%s: want 1 egress rule, got %d", policyPostgresEgress, len(egress.Spec.Egress))
	}
	rule := egress.Spec.Egress[0]
	if len(rule.To) != 1 || rule.To[0].PodSelector == nil ||
		rule.To[0].PodSelector.MatchLabels[cnpgClusterLabel] != cnpgCluster {
		t.Errorf("%s: want egress to peer %s=%s, got %+v",
			policyPostgresEgress, cnpgClusterLabel, cnpgCluster, rule.To)
	}
	sawPort := false
	for _, p := range rule.Ports {
		if p.Port != nil && p.Port.IntVal == postgresPort &&
			p.Protocol != nil && *p.Protocol == corev1.ProtocolTCP {
			sawPort = true
		}
	}
	if !sawPort {
		t.Errorf("%s: missing TCP %d egress port", policyPostgresEgress, postgresPort)
	}
}

// TestNetworkPoliciesReconciler_PostgresEgress is a focused regression test
// for issue #14 — the CNPG replica join timeout caused by the missing egress
// allowance.
func TestNetworkPoliciesReconciler_PostgresEgress(t *testing.T) {
	scheme := newScheme(t)
	cp := newInClusterNetworkPolicyCluster("alpha")
	c := newNetworkPolicyClient(scheme, cp)

	r := &NetworkPoliciesReconciler{Client: c, Recorder: record.NewFakeRecorder(20)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	list := listPolicies(t, c, ControlPlaneNamespace(cp))
	assertPostgresPolicies(t, list, cp.Spec.ClusterName)
}

func TestNetworkPoliciesReconciler_VaultEgressInCluster(t *testing.T) {
	scheme := newScheme(t)
	cp := newInClusterNetworkPolicyCluster("alpha")
	c := newNetworkPolicyClient(scheme, cp)

	r := &NetworkPoliciesReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	list := listPolicies(t, c, ControlPlaneNamespace(cp))
	policy := policyByName(list, policyAllowVaultEgress)
	if policy == nil {
		t.Fatalf("missing %s", policyAllowVaultEgress)
	}
	if len(policy.Spec.Egress) != 1 || len(policy.Spec.Egress[0].To) != 1 {
		t.Fatalf("unexpected egress shape: %+v", policy.Spec.Egress)
	}
	peer := policy.Spec.Egress[0].To[0]
	if peer.NamespaceSelector == nil {
		t.Fatalf("expected namespaceSelector for in-cluster Vault, got %+v", peer)
	}
	if peer.NamespaceSelector.MatchLabels[kubernetesMetadataNameLabel] != testVaultSystemNS {
		t.Errorf("expected %s namespace, got %+v", testVaultSystemNS, peer.NamespaceSelector.MatchLabels)
	}
}

func TestNetworkPoliciesReconciler_VaultEgressExternal(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Platform.Vault.Address = testExternalVaultAddr
	cp.Spec.Platform.ObjectStorage.Endpoint = testInClusterVersityGWAddr
	c := newNetworkPolicyClient(scheme, cp)

	r := &NetworkPoliciesReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	list := listPolicies(t, c, ControlPlaneNamespace(cp))
	policy := policyByName(list, policyAllowVaultEgress)
	if policy == nil {
		t.Fatalf("missing %s", policyAllowVaultEgress)
	}
	peer := policy.Spec.Egress[0].To[0]
	if peer.IPBlock == nil {
		t.Fatalf("expected ipBlock for external Vault, got %+v", peer)
	}
	if peer.IPBlock.CIDR != testExternalVaultCIDR {
		t.Errorf("expected ipBlock CIDR %s, got %q", testExternalVaultCIDR, peer.IPBlock.CIDR)
	}
}

func TestNetworkPoliciesReconciler_VersityGWEgressInCluster(t *testing.T) {
	scheme := newScheme(t)
	cp := newInClusterNetworkPolicyCluster("alpha")
	c := newNetworkPolicyClient(scheme, cp)

	r := &NetworkPoliciesReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	list := listPolicies(t, c, ControlPlaneNamespace(cp))
	policy := policyByName(list, policyAllowVersityGWEgress)
	if policy == nil {
		t.Fatalf("missing %s", policyAllowVersityGWEgress)
	}
	peer := policy.Spec.Egress[0].To[0]
	if peer.NamespaceSelector == nil {
		t.Fatalf("expected namespaceSelector for in-cluster VersityGW, got %+v", peer)
	}
	if peer.NamespaceSelector.MatchLabels[kubernetesMetadataNameLabel] != testVersityGWSystemNS {
		t.Errorf("expected %s namespace, got %+v", testVersityGWSystemNS, peer.NamespaceSelector.MatchLabels)
	}
}

func TestNetworkPoliciesReconciler_VersityGWEgressExternal(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Platform.Vault.Address = testInClusterVaultAddr
	cp.Spec.Platform.ObjectStorage.Endpoint = testExternalVersityGWAddr
	c := newNetworkPolicyClient(scheme, cp)

	r := &NetworkPoliciesReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	list := listPolicies(t, c, ControlPlaneNamespace(cp))
	policy := policyByName(list, policyAllowVersityGWEgress)
	if policy == nil {
		t.Fatalf("missing %s", policyAllowVersityGWEgress)
	}
	peer := policy.Spec.Egress[0].To[0]
	if peer.IPBlock == nil {
		t.Fatalf("expected ipBlock for external VersityGW, got %+v", peer)
	}
	if peer.IPBlock.CIDR != testExternalVersityGWCIDR {
		t.Errorf("expected ipBlock CIDR %s, got %q", testExternalVersityGWCIDR, peer.IPBlock.CIDR)
	}
}

func TestNetworkPoliciesReconciler_CNPGKubernetesAPIEgress(t *testing.T) {
	scheme := newScheme(t)
	cp := newInClusterNetworkPolicyCluster("alpha")
	c := newNetworkPolicyClient(scheme, cp)

	r := &NetworkPoliciesReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	list := listPolicies(t, c, ControlPlaneNamespace(cp))
	policy := policyByName(list, policyAllowCNPGKubernetesAPIEgress)
	if policy == nil {
		t.Fatalf("missing %s", policyAllowCNPGKubernetesAPIEgress)
	}

	// Verify the policy only applies to CNPG postgres pods
	cnpgClusterName := "openchami-alpha-postgres"
	if got := policy.Spec.PodSelector.MatchLabels[cnpgClusterLabel]; got != cnpgClusterName {
		t.Errorf("expected pod selector %s=%s, got %s=%s",
			cnpgClusterLabel, cnpgClusterName, cnpgClusterLabel, got)
	}

	// Verify egress targets the discovered API-server endpoint IP as an
	// ipBlock (not a namespaceSelector, which would not match the API IP).
	if len(policy.Spec.Egress) != 1 {
		t.Fatalf("expected 1 egress rule, got %d", len(policy.Spec.Egress))
	}
	egress := policy.Spec.Egress[0]
	if len(egress.To) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(egress.To))
	}
	peer := egress.To[0]
	if peer.IPBlock == nil {
		t.Fatalf("expected ipBlock peer, got %+v", peer)
	}
	if peer.IPBlock.CIDR != testAPIServerCIDR {
		t.Errorf("expected ipBlock CIDR %q, got %q", testAPIServerCIDR, peer.IPBlock.CIDR)
	}
	if peer.NamespaceSelector != nil {
		t.Errorf("expected no namespaceSelector, got %+v", peer.NamespaceSelector)
	}

	// Verify the discovered API-server port TCP (issue #31: must be the
	// port from the EndpointSlice, not a hardcoded 443).
	if len(egress.Ports) != 1 {
		t.Fatalf("expected 1 port, got %d", len(egress.Ports))
	}
	port := egress.Ports[0]
	if port.Port == nil || port.Port.IntVal != testAPIServerPort {
		t.Errorf("expected port %d, got %+v", testAPIServerPort, port.Port)
	}
	if port.Protocol == nil || *port.Protocol != corev1.ProtocolTCP {
		t.Errorf("expected TCP protocol, got %+v", port.Protocol)
	}
}

func TestNetworkPoliciesReconciler_Idempotent(t *testing.T) {
	scheme := newScheme(t)
	cp := newInClusterNetworkPolicyCluster("alpha")
	c := newNetworkPolicyClient(scheme, cp)

	r := &NetworkPoliciesReconciler{Client: c, Recorder: record.NewFakeRecorder(20)}
	for i := range 2 {
		if _, err := r.Reconcile(context.Background(), cp); err != nil {
			t.Fatalf("reconcile pass %d: %v", i, err)
		}
	}

	list := listPolicies(t, c, ControlPlaneNamespace(cp))
	if len(list.Items) != len(expectedPolicyNames()) {
		t.Errorf("expected %d policies after idempotent passes, got %d",
			len(expectedPolicyNames()), len(list.Items))
	}
}

func TestNetworkPoliciesReconciler_NetworkProbeEgress(t *testing.T) {
	target := func(host string, port int32) *openchamiv1alpha1.NetworkProbeTarget {
		return &openchamiv1alpha1.NetworkProbeTarget{Subnet: testProbeSubnet, ValidateHost: host, ValidatePort: port}
	}
	tests := []struct {
		name           string
		provision, bmc *openchamiv1alpha1.NetworkProbeTarget
		ports          []int32
	}{
		{name: "no targets"},
		{name: "subnet only", provision: target("", 0), bmc: target("", 0)},
		{name: "provision SSH", provision: target("192.168.1.1", 22), ports: []int32{22}},
		{name: "BMC hostname", bmc: target("bmc.test.invalid", 623), ports: []int32{623}},
		{name: "both targets", provision: target("192.168.1.1", 22), bmc: target("bmc.test.invalid", 623), ports: []int32{22, 623}},
		{name: "duplicate ports", provision: target("192.168.1.1", 22), bmc: target("bmc.test.invalid", 22), ports: []int32{22}},
		{name: "HTTPS validation", provision: target("192.168.1.1", 443), ports: []int32{443}},
		{name: "API port validation", provision: target("192.168.1.1", testAPIServerPort), ports: []int32{testAPIServerPort}},
		{name: "port without host", provision: target("", 22)},
		{name: "host without port", provision: target("192.168.1.1", 0)},
		{name: "invalid ports", provision: target("192.168.1.1", -1), bmc: target("bmc.test.invalid", 65536)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := newInClusterNetworkPolicyCluster("alpha")
			cp.Spec.NetworkProbe.Enabled = true
			cp.Spec.NetworkProbe.ProvisionNetwork = tt.provision
			cp.Spec.NetworkProbe.BMCNetwork = tt.bmc
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).
				WithObjects(cp, kubernetesEndpoints(testAPIServerIP, "192.168.150.10")).
				WithTypeConverters(managedfields.NewDeducedTypeConverter()).Build()
			r := &NetworkPoliciesReconciler{Client: c, Recorder: record.NewFakeRecorder(20)}
			if _, err := r.Reconcile(context.Background(), cp); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			policy := policyByName(listPolicies(t, c, ControlPlaneNamespace(cp)), policyNetworkProbe)
			assertNetworkProbeEgress(t, policy, []string{testAPIServerCIDR, "192.168.150.10/32"}, testAPIServerPort, tt.ports)
		})
	}
}

func assertNetworkProbeEgress(t *testing.T, policy *networkingv1.NetworkPolicy, apiCIDRs []string, apiPort int32, validationPorts []int32) {
	t.Helper()
	if policy == nil {
		t.Fatal("missing networkprobe-policy")
	}
	if !reflect.DeepEqual(policy.Spec.PodSelector, podMatch(ServiceNetworkProbe)) ||
		!reflect.DeepEqual(policy.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}) {
		t.Fatalf("unexpected probe selector or policy types: %+v", policy.Spec)
	}
	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(apiCIDRs))
	for _, cidr := range apiCIDRs {
		peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
	}
	want := []networkingv1.NetworkPolicyEgressRule{{To: peers, Ports: []networkingv1.NetworkPolicyPort{portTCP(apiPort)}}}
	if len(validationPorts) > 0 {
		ports := make([]networkingv1.NetworkPolicyPort, 0, len(validationPorts))
		for _, port := range validationPorts {
			ports = append(ports, portTCP(port))
		}
		want = append(want, networkingv1.NetworkPolicyEgressRule{Ports: ports})
	}
	if !reflect.DeepEqual(policy.Spec.Egress, want) {
		t.Fatalf("probe egress = %+v, want %+v", policy.Spec.Egress, want)
	}
}

func TestNetworkPoliciesReconciler_NetworkProbeEgressUpdates(t *testing.T) {
	cp := newInClusterNetworkPolicyCluster("alpha")
	cp.Spec.NetworkProbe.Enabled = true
	cp.Spec.NetworkProbe.ProvisionNetwork = &openchamiv1alpha1.NetworkProbeTarget{
		Subnet: testProbeSubnet, ValidateHost: "192.168.1.1", ValidatePort: 22,
	}
	c := newNetworkPolicyClient(newScheme(t), cp)
	r := &NetworkPoliciesReconciler{Client: c, Recorder: record.NewFakeRecorder(20)}
	for _, port := range []int32{22, 8443, 0} {
		cp.Spec.NetworkProbe.ProvisionNetwork.ValidatePort = port
		if port == 0 {
			cp.Spec.NetworkProbe.ProvisionNetwork.ValidateHost = ""
		}
		// Repeat each configuration to exercise SSA updates and idempotency.
		for range 2 {
			if _, err := r.Reconcile(context.Background(), cp); err != nil {
				t.Fatalf("reconcile validation port %d: %v", port, err)
			}
			var ports []int32
			if port != 0 {
				ports = []int32{port}
			}
			policy := policyByName(listPolicies(t, c, ControlPlaneNamespace(cp)), policyNetworkProbe)
			assertNetworkProbeEgress(t, policy, []string{testAPIServerCIDR}, testAPIServerPort, ports)
		}
	}
}

func TestNetworkPoliciesReconciler_NetworkProbeDescribeOffline(t *testing.T) {
	cp := newInClusterNetworkPolicyCluster("alpha")
	cp.Spec.NetworkProbe.Enabled = true
	cp.Spec.NetworkProbe.ProvisionNetwork = &openchamiv1alpha1.NetworkProbeTarget{
		Subnet: testProbeSubnet, ValidateHost: "probe.test.invalid", ValidatePort: 22,
	}
	r := &NetworkPoliciesReconciler{}
	objs, err := r.Describe(cp)
	if err != nil {
		t.Fatalf("offline describe: %v", err)
	}
	policy := findDescribedPolicy(t, objs, policyNetworkProbe)
	assertNetworkProbeEgress(t, policy, []string{SyntaxOnlyExternalCIDR}, defaultKubernetesAPIPort, []int32{22})
}

func TestNetworkPoliciesReconciler_TwoClustersIsolated(t *testing.T) {
	scheme := newScheme(t)
	rec := record.NewFakeRecorder(40)

	for _, name := range []string{testControlPlaneRed, testControlPlaneBlue} {
		cp := newInClusterNetworkPolicyCluster(name)
		c := newNetworkPolicyClient(scheme, cp)
		r := &NetworkPoliciesReconciler{Client: c, Recorder: rec}
		if _, err := r.Reconcile(context.Background(), cp); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}

		ns := ControlPlaneNamespace(cp)
		list := listPolicies(t, c, ns)
		if len(list.Items) != len(expectedPolicyNames()) {
			t.Errorf("cluster %s: expected %d policies in %s, got %d",
				name, len(expectedPolicyNames()), ns, len(list.Items))
		}
		for _, p := range list.Items {
			if p.Namespace != ns {
				t.Errorf("cluster %s: policy %q in unexpected namespace %q",
					name, p.Name, p.Namespace)
			}
		}

		// Cross-namespace check: nothing should exist in the *other* cluster's
		// namespace within this client (each cluster uses its own client).
		other := testControlPlaneBlue
		if name == testControlPlaneBlue {
			other = testControlPlaneRed
		}
		otherList := listPolicies(t, c, "openchami-"+other)
		if len(otherList.Items) != 0 {
			t.Errorf("cluster %s: leaked %d policies into namespace openchami-%s",
				name, len(otherList.Items), other)
		}
	}
}

// TestNetworkPoliciesReconciler_DescribeNoDNS_ExternalVault asserts that
// calling Describe against a cluster whose Vault address is a non-resolvable
// external hostname (test.invalid is reserved by RFC 6761) does not return an
// error. Live DNS would surface as a "no such host" failure on most systems;
// success here means VaultEgressPeerSyntax was used and no LookupHost call
// was made.
func TestNetworkPoliciesReconciler_DescribeNoDNS_ExternalVault(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Platform.Vault.Address = "https://test.invalid:8200"
	cp.Spec.Platform.ObjectStorage.Endpoint = testInClusterVersityGWAddr
	c := newNetworkPolicyClient(scheme, cp)

	r := &NetworkPoliciesReconciler{Client: c, Recorder: record.NewFakeRecorder(5)}
	objs, err := r.Describe(cp)
	if err != nil {
		t.Fatalf("describe with unresolvable vault host: %v", err)
	}
	if len(objs) != len(expectedPolicyNames()) {
		t.Fatalf("expected %d objects, got %d", len(expectedPolicyNames()), len(objs))
	}

	policy := findDescribedPolicy(t, objs, policyAllowVaultEgress)
	peer := policy.Spec.Egress[0].To[0]
	if peer.IPBlock == nil {
		t.Fatalf("expected ipBlock placeholder, got %+v", peer)
	}
	if peer.IPBlock.CIDR != SyntaxOnlyExternalCIDR {
		t.Errorf("expected sentinel CIDR %q, got %q", SyntaxOnlyExternalCIDR, peer.IPBlock.CIDR)
	}
}

// TestNetworkPoliciesReconciler_DescribeNoDNS_ExternalVersityGW mirrors the
// Vault test for VersityGW; same rationale.
func TestNetworkPoliciesReconciler_DescribeNoDNS_ExternalVersityGW(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Platform.Vault.Address = testInClusterVaultAddr
	cp.Spec.Platform.ObjectStorage.Endpoint = "https://test.invalid:10000"
	c := newNetworkPolicyClient(scheme, cp)

	r := &NetworkPoliciesReconciler{Client: c, Recorder: record.NewFakeRecorder(5)}
	objs, err := r.Describe(cp)
	if err != nil {
		t.Fatalf("describe with unresolvable versitygw host: %v", err)
	}

	policy := findDescribedPolicy(t, objs, policyAllowVersityGWEgress)
	peer := policy.Spec.Egress[0].To[0]
	if peer.IPBlock == nil {
		t.Fatalf("expected ipBlock placeholder, got %+v", peer)
	}
	if peer.IPBlock.CIDR != SyntaxOnlyExternalCIDR {
		t.Errorf("expected sentinel CIDR %q, got %q", SyntaxOnlyExternalCIDR, peer.IPBlock.CIDR)
	}
}

// findDescribedPolicy locates a NetworkPolicy by name in a Describe slice.
func findDescribedPolicy(t *testing.T, objs []client.Object, name string) *networkingv1.NetworkPolicy {
	t.Helper()
	for _, o := range objs {
		np, ok := o.(*networkingv1.NetworkPolicy)
		if !ok {
			continue
		}
		if np.Name == name {
			return np
		}
	}
	t.Fatalf("describe output missing policy %q", name)
	return nil
}

func TestNetworkPoliciesReconciler_ConditionSet(t *testing.T) {
	scheme := newScheme(t)
	cp := newInClusterNetworkPolicyCluster("alpha")
	c := newNetworkPolicyClient(scheme, cp)

	r := &NetworkPoliciesReconciler{Client: c, Recorder: record.NewFakeRecorder(20)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionNetworkPoliciesReady)
	if cond == nil {
		t.Fatalf("expected ConditionNetworkPoliciesReady to be set")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("expected status True, got %q", cond.Status)
	}
	if cond.Reason != conditions.ReasonReady {
		t.Errorf("expected reason %q, got %q", conditions.ReasonReady, cond.Reason)
	}
}

// TestKubernetesAPIEgressPeers_Discovers verifies that every address backing
// the default/kubernetes Endpoints becomes a /32 ipBlock peer, including HA
// control planes with multiple endpoint addresses.
func TestKubernetesAPIEgressPeers_Discovers(t *testing.T) {
	scheme := newScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(kubernetesEndpoints("10.96.0.1", "10.96.0.2")).
		Build()

	peers, err := KubernetesAPIEgressPeers(context.Background(), c)
	if err != nil {
		t.Fatalf("KubernetesAPIEgressPeers: %v", err)
	}
	want := map[string]bool{"10.96.0.1/32": false, "10.96.0.2/32": false}
	if len(peers.Peers) != len(want) {
		t.Fatalf("expected %d peers, got %d", len(want), len(peers.Peers))
	}
	for _, p := range peers.Peers {
		if p.IPBlock == nil {
			t.Fatalf("expected ipBlock peer, got %+v", p)
		}
		if _, ok := want[p.IPBlock.CIDR]; !ok {
			t.Errorf("unexpected CIDR %q", p.IPBlock.CIDR)
			continue
		}
		want[p.IPBlock.CIDR] = true
	}
	for cidr, seen := range want {
		if !seen {
			t.Errorf("missing expected CIDR %q", cidr)
		}
	}
	// The discovered port must come from the EndpointSlice, not a default.
	if len(peers.Ports) != 1 || peers.Ports[0] != testAPIServerPort {
		t.Errorf("expected discovered port %d, got %+v", testAPIServerPort, peers.Ports)
	}
}

// TestKubernetesAPIEgressPeers_MissingEndpointsErrors verifies the strict
// (prod) path aborts when the API endpoints cannot be read.
func TestKubernetesAPIEgressPeers_MissingEndpointsErrors(t *testing.T) {
	t.Setenv("OPENCHAMI_BEST_EFFORT_DNS", "")
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	if _, err := KubernetesAPIEgressPeers(context.Background(), c); err == nil {
		t.Fatalf("expected error when default/kubernetes endpoints are absent")
	}
}

// TestKubernetesAPIEgressPeers_BestEffortFallback verifies the off-cluster
// dev-run mode degrades to the syntax-only sentinel peer instead of failing.
func TestKubernetesAPIEgressPeers_BestEffortFallback(t *testing.T) {
	t.Setenv("OPENCHAMI_BEST_EFFORT_DNS", "true")
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	peers, err := KubernetesAPIEgressPeers(context.Background(), c)
	if err != nil {
		t.Fatalf("best-effort KubernetesAPIEgressPeers: %v", err)
	}
	if len(peers.Peers) != 1 || peers.Peers[0].IPBlock == nil || peers.Peers[0].IPBlock.CIDR != SyntaxOnlyExternalCIDR {
		t.Fatalf("expected sentinel peer %q, got %+v", SyntaxOnlyExternalCIDR, peers.Peers)
	}
}

// TestKubernetesAPIEgressPeersSyntax verifies the Describe-path helper never
// reads the API and always returns the sentinel peer.
func TestKubernetesAPIEgressPeersSyntax(t *testing.T) {
	peers := KubernetesAPIEgressPeersSyntax()
	if len(peers.Peers) != 1 || peers.Peers[0].IPBlock == nil || peers.Peers[0].IPBlock.CIDR != SyntaxOnlyExternalCIDR {
		t.Fatalf("expected sentinel peer %q, got %+v", SyntaxOnlyExternalCIDR, peers.Peers)
	}
}

const (
	testCoreDHCPNodeCIDR        = "10.0.0.2/32"
	testCoreDHCPUpdatedNodeCIDR = "10.0.0.3/32"
)

// These are node addresses, not provision-subnet addresses: CoreDHCP's
// host-network connections to pod-network services use the node's route.
func coreDHCPPolicyNode(name string, labels map[string]string, ips ...string) *corev1.Node {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	for _, ip := range ips {
		node.Status.Addresses = append(node.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ip})
	}
	return node
}

func assertCoreDHCPIngress(t *testing.T, policies *networkingv1.NetworkPolicyList, want []string) {
	t.Helper()
	for name, port := range map[string]int32{policySMD: smdPort, policyBootService: bootServicePort} {
		policy := policyByName(policies, name)
		if policy == nil {
			t.Fatalf("missing %s", name)
		}
		var got []string
		for _, rule := range policy.Spec.Ingress {
			if len(rule.From) == 0 {
				t.Fatalf("%s has unrestricted ingress", name)
			}
			if !reflect.DeepEqual(rule.Ports, []networkingv1.NetworkPolicyPort{portTCP(port)}) {
				t.Fatalf("%s has unexpected ingress ports: %+v", name, rule.Ports)
			}
			for _, peer := range rule.From {
				if peer.PodSelector == nil && peer.NamespaceSelector == nil && peer.IPBlock == nil {
					t.Fatalf("%s has unrestricted peer", name)
				}
				if peer.PodSelector != nil && peer.PodSelector.MatchLabels[labelAppName] == ServiceCoreDHCP {
					t.Fatalf("%s still admits CoreDHCP by pod label", name)
				}
				if peer.IPBlock != nil {
					if peer.PodSelector != nil || peer.NamespaceSelector != nil || len(peer.IPBlock.Except) != 0 {
						t.Fatalf("%s has unexpected node peer: %+v", name, peer)
					}
					got = append(got, peer.IPBlock.CIDR)
				}
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s node CIDRs = %v, want %v", name, got, want)
		}
	}
}

func TestNetworkPoliciesReconciler_CoreDHCPIngress(t *testing.T) {
	for _, probe := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "probe"}[probe], func(t *testing.T) {
			cp := newInClusterNetworkPolicyCluster(testAlphaName)
			cp.Spec.Services.CoreDHCP.Enabled = true
			cp.Spec.Services.CoreDHCP.NodeSelector = map[string]string{testNodeRoleDHCP: testAlphaName, "zone": "a"}
			cp.Spec.NetworkProbe.Enabled = probe
			selector := EffectiveNodeSelector(cp, probeTypeProvision)
			eligible := coreDHCPPolicyNode("eligible", selector, "fd00::2", "10.0.0.2", "10.0.0.2")
			eligible.Status.Addresses = append(eligible.Status.Addresses,
				corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
				corev1.NodeAddress{Type: corev1.NodeHostName, Address: "eligible"})
			staleLabels := maps.Clone(selector)
			staleKey := "zone"
			if probe {
				staleKey = networkprobe.RevisionLabel(cp.Spec.ClusterName, probeTypeProvision)
			}
			staleLabels[staleKey] = "stale"
			c := newNetworkPolicyClient(newScheme(t), cp)
			for _, node := range []*corev1.Node{
				eligible,
				coreDHCPPolicyNode("duplicate", selector, "::ffff:10.0.0.2"),
				coreDHCPPolicyNode("other", map[string]string{testNodeRoleDHCP: "other"}, "10.0.0.3"),
				coreDHCPPolicyNode("stale", staleLabels, "10.0.0.4"),
			} {
				if err := c.Create(context.Background(), node); err != nil {
					t.Fatal(err)
				}
			}
			r := &NetworkPoliciesReconciler{Client: c, Recorder: record.NewFakeRecorder(20)}
			if _, err := r.Reconcile(context.Background(), cp); err != nil {
				t.Fatal(err)
			}
			assertCoreDHCPIngress(t, listPolicies(t, c, ControlPlaneNamespace(cp)), []string{testCoreDHCPNodeCIDR, "fd00::2/128"})
		})
	}
}

func TestNetworkPoliciesReconciler_CoreDHCPRevokesIngress(t *testing.T) {
	ctx := context.Background()
	cp := newInClusterNetworkPolicyCluster(testAlphaName)
	cp.Spec.Services.CoreDHCP.Enabled = true
	cp.Spec.Services.CoreDHCP.NodeSelector = map[string]string{testNodeRoleDHCP: testAlphaName}
	node := coreDHCPPolicyNode("n1", cp.Spec.Services.CoreDHCP.NodeSelector, "10.0.0.2")
	c := newNetworkPolicyClient(newScheme(t), cp)
	if err := c.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	r := &NetworkPoliciesReconciler{Client: c, Recorder: record.NewFakeRecorder(20)}
	reconcile := func(want []string) {
		t.Helper()
		if _, err := r.Reconcile(ctx, cp); err != nil {
			t.Fatal(err)
		}
		assertCoreDHCPIngress(t, listPolicies(t, c, ControlPlaneNamespace(cp)), want)
	}
	reconcile([]string{testCoreDHCPNodeCIDR})
	reconcile([]string{testCoreDHCPNodeCIDR}) // idempotent apply
	node.Status.Addresses[0].Address = "10.0.0.3"
	if err := c.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	reconcile([]string{testCoreDHCPUpdatedNodeCIDR})
	node.Labels = nil
	if err := c.Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	reconcile(nil)
	node.Labels = cp.Spec.Services.CoreDHCP.NodeSelector
	if err := c.Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	reconcile([]string{testCoreDHCPUpdatedNodeCIDR})
	cp.Spec.Services.CoreDHCP.Enabled = false
	reconcile(nil)
	cp.Spec.Services.CoreDHCP.Enabled = true
	reconcile([]string{testCoreDHCPUpdatedNodeCIDR})
	if err := c.Delete(ctx, node); err != nil {
		t.Fatal(err)
	}
	reconcile(nil)
}

func TestNetworkPoliciesReconciler_CoreDHCPProbeRevisionRevokesIngress(t *testing.T) {
	cp := newInClusterNetworkPolicyCluster(testAlphaName)
	cp.Spec.NetworkProbe.Enabled = true
	cp.Spec.Services.CoreDHCP.Enabled = true
	c := newNetworkPolicyClient(newScheme(t), cp)
	if err := c.Create(context.Background(), coreDHCPPolicyNode("n1", EffectiveNodeSelector(cp, probeTypeProvision), "10.0.0.2")); err != nil {
		t.Fatal(err)
	}
	r := &NetworkPoliciesReconciler{Client: c, Recorder: record.NewFakeRecorder(20)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatal(err)
	}
	assertCoreDHCPIngress(t, listPolicies(t, c, ControlPlaneNamespace(cp)), []string{testCoreDHCPNodeCIDR})
	cp.Spec.NetworkProbe.ProvisionNetwork = &openchamiv1alpha1.NetworkProbeTarget{Subnet: "192.0.2.0/24"}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatal(err)
	}
	assertCoreDHCPIngress(t, listPolicies(t, c, ControlPlaneNamespace(cp)), nil)
}

func TestNetworkPoliciesReconciler_CoreDHCPDiscoveryErrors(t *testing.T) {
	for _, address := range []string{"missing", "invalid", "0.0.0.0", "127.0.0.1", "::", "fe80::1%eth0"} {
		t.Run(address, func(t *testing.T) {
			cp := newInClusterNetworkPolicyCluster(testAlphaName)
			cp.Spec.Services.CoreDHCP.Enabled = true
			node := coreDHCPPolicyNode("n1", nil, address)
			if address == "missing" {
				node.Status.Addresses = nil
			}
			c := newNetworkPolicyClient(newScheme(t), cp)
			if err := c.Create(context.Background(), node); err != nil {
				t.Fatal(err)
			}
			assertCoreDHCPDiscoveryError(t, cp, c)
		})
	}
	t.Run("node list fails", func(t *testing.T) {
		cp := newInClusterNetworkPolicyCluster(testAlphaName)
		cp.Spec.Services.CoreDHCP.Enabled = true
		c := interceptor.NewClient(newNetworkPolicyClient(newScheme(t), cp), interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.NodeList); ok {
					return errors.New("node discovery unavailable")
				}
				return c.List(ctx, list, opts...)
			},
		})
		assertCoreDHCPDiscoveryError(t, cp, c)
	})
}

func assertCoreDHCPDiscoveryError(t *testing.T, cp *openchamiv1alpha1.OpenCHAMIControlPlane, c client.Client) {
	t.Helper()
	recorder := record.NewFakeRecorder(20)
	r := &NetworkPoliciesReconciler{Client: c, Recorder: recorder}
	if _, err := r.Reconcile(context.Background(), cp); err == nil {
		t.Fatal("expected node discovery error")
	}
	condition := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionNetworkPoliciesReady)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != conditions.ReasonError {
		t.Fatalf("expected NetworkPoliciesReady=False/Error, got %+v", condition)
	}
	if len(listPolicies(t, c, ControlPlaneNamespace(cp)).Items) != 0 {
		t.Fatal("discovery failure must not apply partial or unrestricted policies")
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, RunbookURL(conditions.ReasonError)) {
			t.Fatalf("event missing runbook URL: %s", event)
		}
	default:
		t.Fatal("expected warning event")
	}
}

func TestNetworkPoliciesReconciler_CoreDHCPDescribeOffline(t *testing.T) {
	cp := newInClusterNetworkPolicyCluster(testAlphaName)
	cp.Spec.Services.CoreDHCP.Enabled = true
	objects, err := (&NetworkPoliciesReconciler{}).Describe(cp)
	if err != nil {
		t.Fatal(err)
	}
	policies := &networkingv1.NetworkPolicyList{}
	for _, object := range objects {
		policies.Items = append(policies.Items, *object.(*networkingv1.NetworkPolicy))
	}
	assertCoreDHCPIngress(t, policies, nil)
}
