// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/logging"
)

const (
	networkingAPIVersion = "networking.k8s.io/v1"
	kindNetworkPolicy    = "NetworkPolicy"

	// envoyGatewaySystemNS is the conventional Kubernetes namespace that hosts
	// the Envoy Gateway control-plane pods. Routes proxied by the gateway
	// originate from pods in this namespace, so the per-service policies must
	// admit ingress from it.
	envoyGatewaySystemNS = "envoy-gateway-system"

	// kubernetesMetadataNameLabel is the well-known label kubelet stamps onto
	// every Namespace; selectors target it instead of guessing custom labels.
	kubernetesMetadataNameLabel = "kubernetes.io/metadata.name"

	// cnpgClusterLabel is the per-pod label CNPG stamps onto every Postgres
	// instance Pod identifying which CNPG Cluster the pod belongs to.
	// NetworkPolicies use it to select Postgres pods by Cluster name.
	cnpgClusterLabel = "cnpg.io/cluster"

	// versityGWPort is the listening port for VersityGW's S3 endpoint. The
	// operator does not deploy VersityGW (invariant #1) but allows egress to it.
	versityGWPort int32 = 10000

	// postgresPort is the listening port of the CNPG primary service.
	postgresPort int32 = 5432

	// cnpgInstanceStatusPort is the port CNPG's instance manager listens
	// on inside each postgres pod for the controller to scrape pod status.
	// Without ingress on this port from cnpg-system, the Cluster phase
	// stays in "Instance Status Extraction Error: HTTP communication
	// issue" and DatabaseReady never reaches True.
	cnpgInstanceStatusPort int32 = 8000

	// vaultPort is the conventional listening port for the external Vault
	// instance. The actual hostname/IP comes from spec.platform.vault.address.
	vaultPort int32 = 8200

	// httpsExternalPort permits TLS egress to arbitrary external endpoints.
	// Used by tokensmith (OIDC discovery), magellan (BMC HTTPS), and the
	// network probe (general reachability checks).
	httpsExternalPort int32 = 443

	// dnsPort permits resolution against the cluster DNS service.
	dnsPort int32 = 53

	// Canonical NetworkPolicy names. Exported for the test package and admin
	// CLI describe output.
	policyDefaultDenyAll       = "default-deny-all"
	policyAllowDNSEgress       = "allow-dns-egress"
	policyAllowVaultEgress     = "allow-vault-egress"
	policyAllowVersityGWEgress = "allow-versitygw-egress"
	policyAllowLogsEgress      = "allow-logs-egress"
	policySMD                  = "smd-policy"
	policyTokensmith           = "tokensmith-policy"
	policyBootService          = "boot-service-policy"
	policyMetadataService      = "metadata-service-policy"
	policyCoreDHCP             = "coredhcp-policy"
	policyMagellan             = "magellan-policy"
	policyNetworkProbe         = "networkprobe-policy"
	policyFunicular            = "funicular-policy"
	policyPostgresIngress      = "postgres-ingress-policy"
)

// NetworkPoliciesReconciler ensures the per-cluster zero-trust NetworkPolicies
// exist in the cluster namespace.
//
// Every policy lives in the cluster's own namespace (invariant #2). The
// reconciler is a single pass — all 13 policies are independent of each other
// and idempotent under server-side apply (invariant #5).
type NetworkPoliciesReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

// Reconcile applies every per-cluster NetworkPolicy and reports
// ConditionNetworkPoliciesReady. Failures are reported as Ready=False with
// Reason=Error and an Event linking to the standard runbook.
func (r *NetworkPoliciesReconciler) Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "networkpolicies")

	policies, err := r.buildPolicies(cp, true)
	if err != nil {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionNetworkPoliciesReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonError,
			Message:            fmt.Sprintf("building network policies: %v", err),
			ObservedGeneration: cp.Generation,
		})
		RecordConditionEvent(r.Recorder, cp, corev1.EventTypeWarning,
			conditions.ReasonError,
			fmt.Sprintf("could not build network policies: %v", err))
		return ctrl.Result{}, fmt.Errorf("building network policies: %w", err)
	}

	for i := range policies {
		p := &policies[i]
		pLog := logging.EnrichWithResource(log, kindNetworkPolicy, p.Name)
		pLog.Info("applying network policy")
		if err := r.Client.Patch(ctx, p, client.Apply, //nolint:staticcheck // SSA via Patch
			client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
			apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditions.ConditionNetworkPoliciesReady,
				Status:             metav1.ConditionFalse,
				Reason:             conditions.ReasonError,
				Message:            fmt.Sprintf("applying %s: %v", p.Name, err),
				ObservedGeneration: cp.Generation,
			})
			RecordConditionEvent(r.Recorder, cp, corev1.EventTypeWarning,
				conditions.ReasonError,
				fmt.Sprintf("applying network policy %s failed", p.Name))
			return ctrl.Result{}, fmt.Errorf("applying network policy %s: %w", p.Name, err)
		}
	}

	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionNetworkPoliciesReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            fmt.Sprintf("%d network policies applied", len(policies)),
		ObservedGeneration: cp.Generation,
	})
	return ctrl.Result{}, nil
}

// Describe returns the Kubernetes objects this reconciler would apply.
//
// Describe uses the syntax-only Vault/VersityGW egress helpers and never
// performs DNS — it honours the SubReconciler contract that Describe "must
// not contact any external service". For external hostnames the resulting
// allow-vault-egress / allow-versitygw-egress / allow-logs-egress policies
// reference a sentinel ipBlock 0.0.0.0/0 in place of the resolved /32 peer.
// Reconcile() resolves the peer for real before applying.
func (r *NetworkPoliciesReconciler) Describe(cp *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
	policies, err := r.buildPolicies(cp, false)
	if err != nil {
		return []client.Object{}, fmt.Errorf("building network policies: %w", err)
	}
	out := make([]client.Object, 0, len(policies))
	for i := range policies {
		out = append(out, &policies[i])
	}
	return out, nil
}

// buildPolicies materialises the full set of per-cluster NetworkPolicy objects.
// Returns an error only when egress peer resolution (Vault or VersityGW) fails;
// all policies that do not depend on external resolution are still constructed.
//
// resolveDNS controls how external (non-.svc.cluster.local) Vault/VersityGW
// hostnames become peers. When true, hostnames are resolved with net.LookupHost
// to produce a /32 ipBlock — this is the Reconcile path. When false, the
// syntax-only helpers substitute a placeholder ipBlock without touching DNS —
// this is the Describe path.
func (r *NetworkPoliciesReconciler) buildPolicies(cp *openchamiv1alpha1.OpenCHAMIControlPlane, resolveDNS bool) ([]networkingv1.NetworkPolicy, error) {
	ns := ControlPlaneNamespace(cp)

	vaultPeerFn := VaultEgressPeerSyntax
	versityPeerFn := VersityGWEgressPeerSyntax
	if resolveDNS {
		vaultPeerFn = VaultEgressPeer
		versityPeerFn = VersityGWEgressPeer
	}

	vaultPeer, err := vaultPeerFn(cp.Spec.Platform.Vault.Address)
	if err != nil {
		return nil, fmt.Errorf("resolving vault egress peer: %w", err)
	}
	versityPeer, err := versityPeerFn(cp.Spec.Platform.ObjectStorage.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("resolving versitygw egress peer: %w", err)
	}

	return []networkingv1.NetworkPolicy{
		r.defaultDenyAll(ns),
		r.allowDNSEgress(ns),
		r.allowVaultEgress(ns, vaultPeer),
		r.allowVersityGWEgress(ns, versityPeer),
		r.allowLogsEgress(ns, versityPeer),
		r.smdPolicy(ns),
		r.tokensmithPolicy(ns, vaultPeer),
		r.bootServicePolicy(ns, versityPeer),
		r.metadataServicePolicy(ns),
		r.coreDHCPPolicy(ns),
		r.magellanPolicy(ns),
		r.networkProbePolicy(ns),
		r.funicularPolicy(ns, versityPeer),
		r.postgresIngressPolicy(ns),
	}, nil
}

// objectMeta builds the common ObjectMeta for every NetworkPolicy in this
// reconciler. Labels mirror the convention used by sibling reconcilers so
// `kubectl get networkpolicy -l openchami.org/managed-by=operator` works.
func policyMeta(ns, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: ns,
		Labels: map[string]string{
			labelAppName:   name,
			labelManagedBy: managedByValue,
		},
	}
}

// policyTypeMeta returns the canonical TypeMeta for a NetworkPolicy.
func policyTypeMeta() metav1.TypeMeta {
	return metav1.TypeMeta{APIVersion: networkingAPIVersion, Kind: kindNetworkPolicy}
}

// podMatch is a small helper that returns a pod selector matching pods labelled
// app.kubernetes.io/name=<service>.
func podMatch(service string) metav1.LabelSelector {
	return metav1.LabelSelector{MatchLabels: map[string]string{labelAppName: service}}
}

// envoyGatewayNSPeer returns the peer that matches every pod inside the
// envoy-gateway-system namespace. Per-service ingress rules use this to admit
// traffic from the gateway control plane without naming any specific pod.
func envoyGatewayNSPeer() networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{kubernetesMetadataNameLabel: envoyGatewaySystemNS},
		},
	}
}

// portRule returns a NetworkPolicyPort for a TCP port.
func portTCP(port int32) networkingv1.NetworkPolicyPort {
	proto := corev1.ProtocolTCP
	p := intstr.FromInt32(port)
	return networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &p}
}

// portUDP returns a NetworkPolicyPort for a UDP port.
func portUDP(port int32) networkingv1.NetworkPolicyPort {
	proto := corev1.ProtocolUDP
	p := intstr.FromInt32(port)
	return networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &p}
}

// defaultDenyAll denies all ingress and egress for every pod in the namespace.
// Subsequent policies layer specific allow rules on top.
func (r *NetworkPoliciesReconciler) defaultDenyAll(ns string) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		TypeMeta:   policyTypeMeta(),
		ObjectMeta: policyMeta(ns, policyDefaultDenyAll),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
		},
	}
}

// allowDNSEgress permits all pods in the namespace to query cluster DNS.
func (r *NetworkPoliciesReconciler) allowDNSEgress(ns string) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		TypeMeta:   policyTypeMeta(),
		ObjectMeta: policyMeta(ns, policyAllowDNSEgress),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				Ports: []networkingv1.NetworkPolicyPort{
					portUDP(dnsPort),
					portTCP(dnsPort),
				},
			}},
		},
	}
}

// allowVaultEgress permits all pods in the namespace to reach the configured
// Vault address. The peer is computed once via VaultEgressPeer.
func (r *NetworkPoliciesReconciler) allowVaultEgress(ns string, peer networkingv1.NetworkPolicyPeer) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		TypeMeta:   policyTypeMeta(),
		ObjectMeta: policyMeta(ns, policyAllowVaultEgress),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To:    []networkingv1.NetworkPolicyPeer{peer},
				Ports: []networkingv1.NetworkPolicyPort{portTCP(vaultPort)},
			}},
		},
	}
}

// allowVersityGWEgress permits the boot-service to reach the VersityGW S3
// endpoint.
func (r *NetworkPoliciesReconciler) allowVersityGWEgress(ns string, peer networkingv1.NetworkPolicyPeer) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		TypeMeta:   policyTypeMeta(),
		ObjectMeta: policyMeta(ns, policyAllowVersityGWEgress),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: podMatch(ServiceBootService),
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To:    []networkingv1.NetworkPolicyPeer{peer},
				Ports: []networkingv1.NetworkPolicyPort{portTCP(versityGWPort)},
			}},
		},
	}
}

// allowLogsEgress permits the funicular collector to reach VersityGW for log
// upload.
func (r *NetworkPoliciesReconciler) allowLogsEgress(ns string, peer networkingv1.NetworkPolicyPeer) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		TypeMeta:   policyTypeMeta(),
		ObjectMeta: policyMeta(ns, policyAllowLogsEgress),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: podMatch(ServiceFunicular),
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To:    []networkingv1.NetworkPolicyPeer{peer},
				Ports: []networkingv1.NetworkPolicyPort{portTCP(versityGWPort)},
			}},
		},
	}
}

// smdPolicy admits ingress to SMD from peer services and the gateway, and
// permits egress to postgres-rw and tokensmith.
func (r *NetworkPoliciesReconciler) smdPolicy(ns string) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		TypeMeta:   policyTypeMeta(),
		ObjectMeta: policyMeta(ns, policySMD),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: podMatch(ServiceSMD),
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{
					{PodSelector: ptrPodMatch(ServiceBootService)},
					{PodSelector: ptrPodMatch(ServiceMetadataService)},
					{PodSelector: ptrPodMatch(ServiceCoreDHCP)},
					{PodSelector: ptrPodMatch(ServiceMagellan)},
					envoyGatewayNSPeer(),
				},
				Ports: []networkingv1.NetworkPolicyPort{portTCP(smdPort)},
			}},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To:    []networkingv1.NetworkPolicyPeer{postgresPeer(ns)},
					Ports: []networkingv1.NetworkPolicyPort{portTCP(postgresPort)},
				},
				{
					To: []networkingv1.NetworkPolicyPeer{
						{PodSelector: ptrPodMatch(ServiceTokensmith)},
					},
					Ports: []networkingv1.NetworkPolicyPort{portTCP(tokensmithPort)},
				},
			},
		},
	}
}

// tokensmithPolicy admits ingress from any pod in the namespace and the
// gateway control plane, and permits egress to Vault and arbitrary HTTPS
// endpoints (for OIDC discovery).
func (r *NetworkPoliciesReconciler) tokensmithPolicy(ns string, vaultPeer networkingv1.NetworkPolicyPeer) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		TypeMeta:   policyTypeMeta(),
		ObjectMeta: policyMeta(ns, policyTokensmith),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: podMatch(ServiceTokensmith),
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{
					{PodSelector: &metav1.LabelSelector{}},
					envoyGatewayNSPeer(),
				},
				Ports: []networkingv1.NetworkPolicyPort{portTCP(tokensmithPort)},
			}},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To:    []networkingv1.NetworkPolicyPeer{vaultPeer},
					Ports: []networkingv1.NetworkPolicyPort{portTCP(vaultPort)},
				},
				{
					Ports: []networkingv1.NetworkPolicyPort{portTCP(httpsExternalPort)},
				},
			},
		},
	}
}

// bootServicePolicy admits ingress from coredhcp and the gateway, and permits
// egress to SMD, postgres, VersityGW, and tokensmith.
func (r *NetworkPoliciesReconciler) bootServicePolicy(ns string, versityPeer networkingv1.NetworkPolicyPeer) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		TypeMeta:   policyTypeMeta(),
		ObjectMeta: policyMeta(ns, policyBootService),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: podMatch(ServiceBootService),
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{
					{PodSelector: ptrPodMatch(ServiceCoreDHCP)},
					envoyGatewayNSPeer(),
				},
				Ports: []networkingv1.NetworkPolicyPort{portTCP(bootServicePort)},
			}},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{
						{PodSelector: ptrPodMatch(ServiceSMD)},
					},
					Ports: []networkingv1.NetworkPolicyPort{portTCP(smdPort)},
				},
				{
					To:    []networkingv1.NetworkPolicyPeer{postgresPeer(ns)},
					Ports: []networkingv1.NetworkPolicyPort{portTCP(postgresPort)},
				},
				{
					To:    []networkingv1.NetworkPolicyPeer{versityPeer},
					Ports: []networkingv1.NetworkPolicyPort{portTCP(versityGWPort)},
				},
				{
					To: []networkingv1.NetworkPolicyPeer{
						{PodSelector: ptrPodMatch(ServiceTokensmith)},
					},
					Ports: []networkingv1.NetworkPolicyPort{portTCP(tokensmithPort)},
				},
			},
		},
	}
}

// metadataServicePolicy admits ingress from the gateway and permits egress to
// SMD and tokensmith.
func (r *NetworkPoliciesReconciler) metadataServicePolicy(ns string) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		TypeMeta:   policyTypeMeta(),
		ObjectMeta: policyMeta(ns, policyMetadataService),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: podMatch(ServiceMetadataService),
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{
					envoyGatewayNSPeer(),
				},
				Ports: []networkingv1.NetworkPolicyPort{portTCP(metadataServicePort)},
			}},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{
						{PodSelector: ptrPodMatch(ServiceSMD)},
					},
					Ports: []networkingv1.NetworkPolicyPort{portTCP(smdPort)},
				},
				{
					To: []networkingv1.NetworkPolicyPeer{
						{PodSelector: ptrPodMatch(ServiceTokensmith)},
					},
					Ports: []networkingv1.NetworkPolicyPort{portTCP(tokensmithPort)},
				},
			},
		},
	}
}

// coreDHCPPolicy permits the CoreDHCP DaemonSet to reach SMD and boot-service.
// CoreDHCP runs with hostNetwork so ingress from arbitrary DHCP clients is
// not enforceable here; that traffic is governed by the host firewall.
func (r *NetworkPoliciesReconciler) coreDHCPPolicy(ns string) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		TypeMeta:   policyTypeMeta(),
		ObjectMeta: policyMeta(ns, policyCoreDHCP),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: podMatch(ServiceCoreDHCP),
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{
						{PodSelector: ptrPodMatch(ServiceSMD)},
					},
					Ports: []networkingv1.NetworkPolicyPort{portTCP(smdPort)},
				},
				{
					To: []networkingv1.NetworkPolicyPeer{
						{PodSelector: ptrPodMatch(ServiceBootService)},
					},
					Ports: []networkingv1.NetworkPolicyPort{portTCP(bootServicePort)},
				},
			},
		},
	}
}

// magellanPolicy permits the Magellan CronJob to reach SMD and arbitrary HTTPS
// endpoints (BMC Redfish). BMC subnets vary per site so we cannot constrain
// to a specific CIDR here.
func (r *NetworkPoliciesReconciler) magellanPolicy(ns string) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		TypeMeta:   policyTypeMeta(),
		ObjectMeta: policyMeta(ns, policyMagellan),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: podMatch(ServiceMagellan),
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{
						{PodSelector: ptrPodMatch(ServiceSMD)},
					},
					Ports: []networkingv1.NetworkPolicyPort{portTCP(smdPort)},
				},
				{
					Ports: []networkingv1.NetworkPolicyPort{portTCP(httpsExternalPort)},
				},
			},
		},
	}
}

// networkProbePolicy permits the network-probe DaemonSet to reach arbitrary
// HTTPS endpoints (cluster admins configure ValidateHosts to point anywhere).
func (r *NetworkPoliciesReconciler) networkProbePolicy(ns string) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		TypeMeta:   policyTypeMeta(),
		ObjectMeta: policyMeta(ns, policyNetworkProbe),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: podMatch(ServiceNetworkProbe),
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				Ports: []networkingv1.NetworkPolicyPort{portTCP(httpsExternalPort)},
			}},
		},
	}
}

// funicularPolicy permits the funicular collector DaemonSet to reach VersityGW
// for log batch upload.
func (r *NetworkPoliciesReconciler) funicularPolicy(ns string, peer networkingv1.NetworkPolicyPeer) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		TypeMeta:   policyTypeMeta(),
		ObjectMeta: policyMeta(ns, policyFunicular),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: podMatch(ServiceFunicular),
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To:    []networkingv1.NetworkPolicyPeer{peer},
				Ports: []networkingv1.NetworkPolicyPort{portTCP(versityGWPort)},
			}},
		},
	}
}

// ptrPodMatch returns a pointer to a podSelector matching pods labelled
// app.kubernetes.io/name=<service>. Convenience helper because
// NetworkPolicyPeer.PodSelector is *metav1.LabelSelector.
func ptrPodMatch(service string) *metav1.LabelSelector {
	s := podMatch(service)
	return &s
}

// postgresIngressPolicy admits ingress on the CNPG primary pods from the
// services that legitimately consume the database (SMD, boot-service, and
// the operator-managed db-init Job). Without this the default-deny-all
// policy in the namespace blocks all ingress to the CNPG pods, so the
// "egress allowed" rules in smd-policy and boot-service-policy don't help —
// the destination side is what enforces the block.
//
// CNPG itself does not ship a default ingress policy for its pods (it
// expects the platform owner to control that), so the operator owns the
// CNPG-side rule too.
func (r *NetworkPoliciesReconciler) postgresIngressPolicy(ns string) networkingv1.NetworkPolicy {
	cnpgClusterName := ns + "-postgres"
	return networkingv1.NetworkPolicy{
		TypeMeta:   policyTypeMeta(),
		ObjectMeta: policyMeta(ns, policyPostgresIngress),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{cnpgClusterLabel: cnpgClusterName},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					// Service-pod data plane: SMD, boot-service, and
					// instance-to-instance CNPG replication on port 5432.
					// The previous "db-init" peer (for the operator's
					// post-init shell-script Job) was removed when
					// CNPG's managed.roles + Database CRDs took over
					// role and database creation declaratively.
					From: []networkingv1.NetworkPolicyPeer{
						{PodSelector: ptrPodMatch(ServiceSMD)},
						{PodSelector: ptrPodMatch(ServiceBootService)},
						{PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{cnpgClusterLabel: cnpgClusterName},
						}},
					},
					Ports: []networkingv1.NetworkPolicyPort{portTCP(postgresPort)},
				},
				{
					// CNPG controller scraping the per-pod instance
					// status endpoint on :8000. Without this, Cluster
					// phase reports "HTTP communication issue" and
					// DatabaseReady never reaches True.
					From: []networkingv1.NetworkPolicyPeer{
						{NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"kubernetes.io/metadata.name": "cnpg-system"},
						}},
					},
					Ports: []networkingv1.NetworkPolicyPort{portTCP(cnpgInstanceStatusPort)},
				},
			},
		},
	}
}

// postgresPeer returns the peer that matches the CNPG primary pods for a
// cluster. CNPG labels every pod it manages with `cnpg.io/cluster=<cluster>`.
// The cluster name mirrors the one chosen in DatabaseReconciler.
func postgresPeer(ns string) networkingv1.NetworkPolicyPeer {
	// Mirror the DatabaseReconciler naming: openchami-{clusterName}-postgres.
	// `ns` already encodes the clusterName as `openchami-{clusterName}`, so
	// the cluster name is `ns + "-postgres"` (drop the operator prefix and add
	// the suffix). We avoid stripping/parsing because the suffix is the only
	// CNPG-side coupling.
	cnpgClusterName := ns + "-postgres"
	return networkingv1.NetworkPolicyPeer{
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{cnpgClusterLabel: cnpgClusterName},
		},
	}
}
