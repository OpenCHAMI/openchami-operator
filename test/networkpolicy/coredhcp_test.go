//go:build e2e

// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package networkpolicy

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/reconcilers"
)

const policyTestImage = "busybox:1.37.0"

// TestCoreDHCPHostNetworkIngress exercises the real CNI with production policy
// generation. It needs three nodes: one server node and two remote clients.
// It calls only the policy reconciler, so no operator deployment, CRDs, Vault,
// or service images are needed. Controller watch dispatch is covered by unit tests.
func TestCoreDHCPHostNetworkIngress(t *testing.T) {
	if os.Getenv("E2E_NETWORK_POLICY") != "true" {
		t.Skip("set E2E_NETWORK_POLICY=true on a disposable three-node cluster with an enforcing CNI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes); err != nil {
		t.Fatal(err)
	}
	if len(nodes.Items) < 3 {
		t.Fatal("three nodes are required to distinguish local-node bypass from remote policy enforcement")
	}
	sort.Slice(nodes.Items, func(i, j int) bool { return nodes.Items[i].Name < nodes.Items[j].Name })
	name := fmt.Sprintf("policy46-%d", time.Now().UnixNano())
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "openchami-" + name}}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		if err := c.Delete(cleanupCtx, ns); err != nil {
			t.Errorf("deleting test namespace: %v", err)
		}
	})
	cp := &openchamiv1alpha1.OpenCHAMIControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: openchamiv1alpha1.OpenCHAMIControlPlaneSpec{
			ClusterName: name,
			Platform: openchamiv1alpha1.PlatformSpec{
				Vault:         openchamiv1alpha1.VaultSpec{Address: "http://vault.vault-system.svc.cluster.local:8200"},
				ObjectStorage: openchamiv1alpha1.ObjectStorageSpec{Endpoint: "http://versitygw.s3-system.svc.cluster.local:10000"},
			},
			Services: openchamiv1alpha1.ServicesSpec{CoreDHCP: openchamiv1alpha1.CoreDHCPSpec{
				Enabled:      true,
				NodeSelector: map[string]string{"kubernetes.io/hostname": nodes.Items[1].Labels["kubernetes.io/hostname"]},
			}},
		},
	}
	if cp.Spec.Services.CoreDHCP.NodeSelector["kubernetes.io/hostname"] == "" {
		t.Fatal("client node must have kubernetes.io/hostname label")
	}
	r := &reconcilers.NetworkPoliciesReconciler{Client: c, Recorder: record.NewFakeRecorder(100)}
	applyPolicies := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, cp); err != nil {
			t.Fatal(err)
		}
	}
	applyPolicies()

	// Stand-ins listen on the real service ports and carry the real selectors.
	// Clients must be on other nodes; node-local ingress bypasses NetworkPolicy.
	destinations := make([]string, 0, 4)
	for service, port := range map[string]int32{"smd": 27779, "boot-service": 27778} {
		server := policyPod(ns.Name, service, nodes.Items[0].Name, service, false)
		server.Spec.Containers[0].Command = []string{"httpd", "-f", "-p", fmt.Sprint(port), "-h", "/tmp"}
		if err := c.Create(ctx, server); err != nil {
			t.Fatal(err)
		}
		waitForPolicyPod(t, ctx, c, server)
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: service, Namespace: ns.Name},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app.kubernetes.io/name": service},
				Ports:    []corev1.ServicePort{{Port: port, TargetPort: intstr.FromInt32(port)}},
			},
		}
		if err := c.Create(ctx, svc); err != nil {
			t.Fatal(err)
		}
		destinations = append(destinations, net.JoinHostPort(server.Status.PodIP, fmt.Sprint(port)),
			net.JoinHostPort(svc.Spec.ClusterIP, fmt.Sprint(port)))
	}
	allowed := policyPod(ns.Name, "allowed", nodes.Items[1].Name, "coredhcp", true)
	denied := policyPod(ns.Name, "denied", nodes.Items[2].Name, "coredhcp", true)
	ordinary := policyPod(ns.Name, "ordinary", nodes.Items[1].Name, "unlabelled-client", false)
	for _, pod := range []*corev1.Pod{allowed, denied, ordinary} {
		if err := c.Create(ctx, pod); err != nil {
			t.Fatal(err)
		}
		waitForPolicyPod(t, ctx, c, pod)
	}
	// An isolated ordinary pod would otherwise fail at egress, masking an
	// accidentally permissive destination policy. Give this client egress only.
	cmd := exec.CommandContext(ctx, "kubectl", "apply", "--server-side", "-f", "-")
	cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: ordinary-client-egress
  namespace: %s
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: unlabelled-client
  policyTypes: [Egress]
  egress: [{}]
`, ns.Name))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("allowing test-client egress: %v: %s", err, output)
	}

	for _, destination := range destinations {
		t.Logf("checking cross-node ingress to %s", destination)
		assertPolicyConnection(t, ctx, ns.Name, allowed.Name, destination, true)
		assertPolicyConnection(t, ctx, ns.Name, denied.Name, destination, false)
		assertPolicyConnection(t, ctx, ns.Name, ordinary.Name, destination, false)
	}
	// Revoke eligibility without moving the host-network client. Every check
	// uses a new TCP connection so conntrack cannot preserve an old allowance.
	cp.Spec.Services.CoreDHCP.NodeSelector = map[string]string{"kubernetes.io/hostname": "no-such-policy46-node"}
	applyPolicies()
	for _, destination := range destinations {
		assertPolicyConnection(t, ctx, ns.Name, allowed.Name, destination, false)
	}
}

func policyPod(namespace, name, node, service string, hostNetwork bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			Labels: map[string]string{"app.kubernetes.io/name": service},
		},
		Spec: corev1.PodSpec{
			NodeName: node, HostNetwork: hostNetwork,
			Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers:  []corev1.Container{{Name: "test", Image: policyTestImage, Command: []string{"sleep", "3600"}}},
		},
	}
	if hostNetwork {
		pod.Spec.DNSPolicy = corev1.DNSClusterFirstWithHostNet
	}
	return pod
}

func waitForPolicyPod(t *testing.T, ctx context.Context, c client.Client, pod *corev1.Pod) {
	t.Helper()
	if err := wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
			return false, err
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("waiting for %s: %v", pod.Name, err)
	}
}

func assertPolicyConnection(t *testing.T, ctx context.Context, namespace, pod, destination string, allowed bool) {
	t.Helper()
	consecutive := 0
	var lastOutput []byte
	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		// An HTTP 404 still proves ingress reached the stand-in server.
		cmd := exec.CommandContext(ctx, "kubectl", "exec", "-n", namespace, pod, "--",
			"wget", "-T", "2", "-O", "/dev/null", "http://"+destination+"/")
		lastOutput, lastErr = cmd.CombinedOutput()
		reachable := lastErr == nil || strings.Contains(string(lastOutput), "404 Not Found")
		// Only a network timeout is evidence of policy denial. Missing exec
		// permissions, broken containers and other errors must fail the test.
		blocked := lastErr != nil && strings.Contains(string(lastOutput), "timed out")
		if (allowed && reachable) || (!allowed && blocked) {
			consecutive++
		} else {
			consecutive = 0
		}
		return consecutive >= 3, nil
	})
	if err != nil {
		t.Fatalf("%s -> %s: allowed=%t: %v; last exec: %v: %s", pod, destination, allowed, err, lastErr, lastOutput)
	}
}
