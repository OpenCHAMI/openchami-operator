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
| `smd-policy` | smd | boot-service, metadata-service, coredhcp, magellan, envoy-gateway-system NS | postgres-rw:5432, tokensmith:8080 |
| `tokensmith-policy` | tokensmith | all in NS, envoy-gateway-system NS | vault:8200, :443 |
| `boot-service-policy` | boot-service | coredhcp, envoy-gateway-system NS | smd:27779, postgres-rw:5432, versitygw:10000, tokensmith:8080 |
| `metadata-service-policy` | metadata-service | envoy-gateway-system NS | smd:27779, tokensmith:8080 |
| `coredhcp-policy` | coredhcp | — | smd:27779, boot-service:27778 |
| `magellan-policy` | magellan | — | smd:27779, :443 |
| `networkprobe-policy` | network-probe | — | :443 (ValidateHost reachability) |
| `funicular-policy` | funicular-collector | — | versitygw:10000 |

## Implementation pattern
```go
func (r *NetworkPoliciesReconciler) Reconcile(ctx, cluster) (ctrl.Result, error) {
    log := logging.Enrich(ctx, cluster, "networkpolicies")
    ns := helpers.ClusterNamespace(cluster)
    policies := []networkingv1.NetworkPolicy{
        r.defaultDenyAll(ns),
        r.allowDNSEgress(ns),
        // ... all 13 policies
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
