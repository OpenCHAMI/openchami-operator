// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
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

	// testAPIServerIP is the address seeded into the default/kubernetes
	// Endpoints fixture so KubernetesAPIEgressPeers has something to discover.
	testAPIServerIP   = "10.96.0.1"
	testAPIServerCIDR = "10.96.0.1/32"
)

// kubernetesEndpoints returns a default/kubernetes Endpoints object backing the
// API-server discovery path exercised by KubernetesAPIEgressPeers.
func kubernetesEndpoints(ips ...string) *corev1.Endpoints {
	addrs := make([]corev1.EndpointAddress, 0, len(ips))
	for _, ip := range ips {
		addrs = append(addrs, corev1.EndpointAddress{IP: ip})
	}
	return &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
		Subsets:    []corev1.EndpointSubset{{Addresses: addrs}},
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
func newNetworkPolicyClient(scheme *runtime.Scheme, cp *openchamiv1alpha1.OpenCHAMIControlPlane) client.Client {
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
		ServiceCoreDHCP:        false,
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

	// Verify port 443 is allowed
	if len(egress.Ports) != 1 {
		t.Fatalf("expected 1 port, got %d", len(egress.Ports))
	}
	port := egress.Ports[0]
	if port.Port == nil || port.Port.IntVal != kubernetesAPIPort {
		t.Errorf("expected port %d, got %+v", kubernetesAPIPort, port.Port)
	}
	if port.Protocol == nil || *port.Protocol != corev1.ProtocolTCP {
		t.Errorf("expected TCP protocol, got %+v", port.Protocol)
	}
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

	// Verify port 443 TCP
	if len(egress.Ports) != 1 {
		t.Fatalf("expected 1 port, got %d", len(egress.Ports))
	}
	port := egress.Ports[0]
	if port.Port == nil || port.Port.IntVal != 443 {
		t.Errorf("expected port 443, got %+v", port.Port)
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
	if len(peers) != len(want) {
		t.Fatalf("expected %d peers, got %d", len(want), len(peers))
	}
	for _, p := range peers {
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
	if len(peers) != 1 || peers[0].IPBlock == nil || peers[0].IPBlock.CIDR != SyntaxOnlyExternalCIDR {
		t.Fatalf("expected sentinel peer %q, got %+v", SyntaxOnlyExternalCIDR, peers)
	}
}

// TestKubernetesAPIEgressPeersSyntax verifies the Describe-path helper never
// reads the API and always returns the sentinel peer.
func TestKubernetesAPIEgressPeersSyntax(t *testing.T) {
	peers := KubernetesAPIEgressPeersSyntax()
	if len(peers) != 1 || peers[0].IPBlock == nil || peers[0].IPBlock.CIDR != SyntaxOnlyExternalCIDR {
		t.Fatalf("expected sentinel peer %q, got %+v", SyntaxOnlyExternalCIDR, peers)
	}
}
