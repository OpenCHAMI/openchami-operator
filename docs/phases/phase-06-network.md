# Phase 6 — Network Services

**Implement in this order:** networkprobe → coredhcp → magellan
CoreDHCP and Magellan depend on probe label scheme being defined first.

## 6.1 Network Probe DaemonSet — `internal/reconcilers/networkprobe.go`

If `spec.networkProbe.enabled=false`: skip entirely. Set `NetworkProbeReady=True`
(since manual nodeSelectors are used instead — this condition becomes trivially
satisfied from the probe's perspective).

```
DaemonSet:   openchami-{name}-network-probe
Namespace:   openchami-{name}
Runs on:     ALL nodes (no nodeSelector)
Priority:    system-node-critical

Security: CommonSecurityContext() applies.
          No NET_BIND_SERVICE needed.
          Netlink route inspection and TCP connects are unprivileged.

Env vars:
  PROBE_CLUSTER_NAME       = spec.clusterName
  PROBE_INTERVAL_SECONDS   = spec.networkProbe.intervalSeconds
  PROBE_PROVISION_SUBNET   = spec.networkProbe.provisionNetwork.subnet
  PROBE_PROVISION_HOST     = spec.networkProbe.provisionNetwork.validateHost
  PROBE_PROVISION_PORT     = spec.networkProbe.provisionNetwork.validatePort
  PROBE_PROVISION_TIMEOUT  = spec.networkProbe.provisionNetwork.validateTimeout
  PROBE_BMC_SUBNET         = spec.networkProbe.bmcNetwork.subnet
  PROBE_BMC_HOST           = spec.networkProbe.bmcNetwork.validateHost
  PROBE_BMC_PORT           = spec.networkProbe.bmcNetwork.validatePort
  PROBE_BMC_TIMEOUT        = spec.networkProbe.bmcNetwork.validateTimeout
  NODE_NAME                = fieldRef: spec.nodeName
```

**Probe binary logic** (`cmd/probe/main.go` — implement here):
```go
func probeNetwork(target) bool {
    // 1. Check routing table via github.com/vishvananda/netlink
    //    RouteGet(target IP in subnet) — if error, return false
    // 2. If ValidateHost set: net.DialTimeout("tcp", host:port, timeout)
    //    If error, return false
    return true
}

// Apply both true AND false labels explicitly:
// openchami.org/{clusterName}/provision-network-ready = "true"|"false"
// openchami.org/{clusterName}/bmc-network-ready       = "true"|"false"
// Use nodeClient.CoreV1().Nodes().Patch(ctx, nodeName, MergePatchType, ...)
```

**Node watch → status update:**
When reconcile is triggered by a node label change, read all nodes,
collect those with `=true` labels, update:
- `status.networkProbe.nodesWithProvisionAccess`
- `status.networkProbe.nodesWithBMCAccess`

**Conditions:**
- `NetworkProbeReady=True`: DaemonSet NumberReady>0 AND at least one node passes each configured probe
- `NetworkProbeReady=False, Reason=NoEligibleNodes`: DaemonSet running but zero nodes pass
  → Record Warning Event with runbook URL

## 6.2 CoreDHCP DaemonSet — `internal/reconcilers/coredhcp.go`

If probing enabled AND `NetworkProbeReady=False`:
  Set `DHCPReady=False, Reason=WaitingForNetworkProbe`
  Return `ctrl.Result{RequeueAfter: 30s}`

```
hostNetwork:   true
dnsPolicy:     ClusterFirstWithHostNet
priorityClass: system-node-critical
nodeSelector:  helpers.EffectiveNodeSelector(cluster, "provision")

Security: capabilities.add=[NET_BIND_SERVICE] (required for port 67)
          capabilities.drop=[ALL others]
          allowPrivilegeEscalation=false
          readOnlyRootFilesystem=true
```

Pre-stop hook (required):
```go
Lifecycle: &corev1.Lifecycle{
    PreStop: &corev1.LifecycleHandler{
        Exec: &corev1.ExecAction{
            Command: []string{"/bin/sh", "-c",
                `echo "WARN: CoreDHCP stopping on $(hostname). ` +
                `Provision network DHCP may be interrupted. ` +
                `Runbook: https://openchami.org/docs/ops/coredhcp-node-drain" >&2`},
        },
    },
}
```

Node cordon watcher: when a node with CoreDHCP running gets
`spec.unschedulable=true`, record Warning Event on cluster: `DHCPNodeCordoned`.

Bootstrap token Job: after tokensmith ready, mint token into Secret
`coredhcp-smd-token`. Only if Secret does not already exist.

Condition: `DHCPReady=True` when NumberReady>0. Populate `status.coreDHCPNodes`.

## 6.3 Magellan CronJob — `internal/reconcilers/magellan.go`

If probing enabled AND `NetworkProbeReady=False`:
  Set `MagellanReady=False, Reason=WaitingForNetworkProbe`, requeue 30s.

Node affinity via `spec.jobTemplate.spec.template.spec.nodeSelector`:
  `helpers.EffectiveNodeSelector(cluster, "bmc")`

The Magellan sub-reconciler's ONLY job:
- Deploy the CronJob
- Set `MagellanReady=True` when CronJob exists

Do NOT parse Magellan output. Do NOT populate any HPC inventory fields.
What Magellan finds is SMD's business.

```bash
tools/check-phase.sh 6
```
