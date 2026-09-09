# Phase 8 — Network Policies

**Single file, single pass.** All policies are independent.
See AGENTS.md for implementation order guidance.

**File:** `internal/reconcilers/networkpolicies.go`

## vaultEgressPeer() helper
This is the only shared function. Implement first.
It is pre-written in `internal/reconcilers/helpers.go` — use it directly.

## Policy table

| Policy | Pod selector | Ingress from | Egress to |
|---|---|---|---|
| `default-deny-all` | all | — | — |
| `allow-dns-egress` | all | — | :53 UDP+TCP |
| `allow-vault-egress` | all | — | Vault :8200 (vaultEgressPeer) |
| `allow-versitygw-egress` | boot-service | — | VersityGW :10000 |
| `allow-logs-egress` | funicular-collector | — | VersityGW :10000 |
| `allow-cnpg-kubernetes-api-egress` | cnpg.io/cluster=* | — | default namespace :443 |
| `smd-policy` | smd | boot-service, metadata-service, coredhcp, magellan, envoy-gateway-system NS | postgres-rw:5432, tokensmith:8080 |
| `tokensmith-policy` | tokensmith | all in NS, envoy-gateway-system NS | vault:8200, :443 |
| `boot-service-policy` | boot-service | coredhcp, envoy-gateway-system NS | smd:27779, postgres-rw:5432, versitygw:10000, tokensmith:8080 |
| `metadata-service-policy` | metadata-service | envoy-gateway-system NS | smd:27779, tokensmith:8080 |
| `coredhcp-policy` | coredhcp | — | smd:27779, boot-service:27778 |
| `magellan-policy` | magellan | — | smd:27779, :443 |
| `networkprobe-policy` | network-probe | — | :443 (ValidateHost reachability) |
| `funicular-policy` | funicular-collector | — | versitygw:10000 |
| `postgres-ingress-policy` | cnpg.io/cluster=* | smd, boot-service, cnpg cluster peers, cnpg-system NS | — |
| `postgres-egress-policy` | cnpg.io/cluster=* | — | cnpg cluster peers :5432 |

## Implementation pattern
```go
func (r *NetworkPoliciesReconciler) Reconcile(ctx, cluster) (ctrl.Result, error) {
    log := logging.Enrich(ctx, cluster, "networkpolicies")
    ns := helpers.ClusterNamespace(cluster)
    policies := []networkingv1.NetworkPolicy{
        r.defaultDenyAll(ns),
        r.allowDNSEgress(ns),
        // ... all 14 policies
    }
    for _, policy := range policies {
        if err := r.Client.Apply(ctx, &policy, ...); err != nil {
            return ctrl.Result{}, fmt.Errorf("applying policy %s: %w", policy.Name, err)
        }
    }
    return ctrl.Result{}, nil
}
```

## envoy-gateway-system namespace selector
```go
envoyNSSelector := &networkingv1.NetworkPolicyPeer{
    NamespaceSelector: &metav1.LabelSelector{
        MatchLabels: map[string]string{
            "kubernetes.io/metadata.name": "envoy-gateway-system",
        },
    },
}
```

```bash
tools/check-phase.sh 8
```

## Third-party (operator-managed) workloads and the default-deny trap

`default-deny-all` selects **every** pod in the namespace, including pods the
operator does *not* template itself but that another operator creates inside
our namespace — today that is CloudNativePG (CNPG). These pods are the source
of a recurring class of bug (see issue #14), so any future integration that
drops third-party pods into the control-plane namespace must account for the
following:

1. **They do not carry our `app.kubernetes.io/name=<service>` label.**
   All the per-service policies key their PodSelector on that label, so a
   third-party pod matches *none* of them and is left with only
   `default-deny-all` plus the namespace-wide `allow-dns-egress` /
   `allow-vault-egress`. It can resolve DNS but cannot open any other
   connection — inbound *or* outbound.

2. **Both directions must be granted explicitly.** NetworkPolicy is enforced
   on the destination for ingress and on the source for egress. Allowing SMD
   *egress* to postgres does nothing if the postgres pod has no matching
   *ingress* rule, and — the trap in #14 — a postgres *ingress* rule does
   nothing for a replica that cannot *egress* to dial the `-rw` Service.
   Pod-to-pod internal traffic (replication, clustering, leader election)
   therefore needs a policy pair selecting the third-party pods on **their
   own** vendor label.

   For CNPG the vendor label is `cnpg.io/cluster=<cluster>`; the pair is
   `postgres-ingress-policy` + `postgres-egress-policy`. Model any future
   clustered third-party workload the same way: one ingress policy and one
   egress policy, both `PodSelector`ed on the vendor's per-instance label,
   with the peer being that same selector for intra-cluster traffic.

3. **External destinations behind a Service (e.g. the Kubernetes API) need an
   ipBlock, not a namespaceSelector.** A `namespaceSelector` matches the
   backing pods, but traffic to a ClusterIP is enforced on the *post-DNAT*
   destination IP, which the ClusterIP does not match. Discover the concrete
   endpoint IP(s) at reconcile time and emit `/32` ipBlock peers (see the
   CNPG→Kubernetes-API egress helper). In-namespace pod-to-pod traffic does
   not have this problem because it is not DNATed through a ClusterIP in the
   same way; a `podSelector` peer is correct there.

Checklist for onboarding a new operator-managed third-party workload:

- [ ] Identify the vendor's per-instance pod label.
- [ ] Add an ingress policy admitting its OpenCHAMI consumers **and** its own
      peers (if it clusters) on the relevant port(s).
- [ ] Add an egress policy for any outbound traffic it initiates: peer
      (clustering) traffic uses a `podSelector`; Service/ClusterIP or external
      destinations use discovered `/32` ipBlock peers.
- [ ] Add the policy names to `expectedPolicyNames()` and cover both
      directions in a test.
