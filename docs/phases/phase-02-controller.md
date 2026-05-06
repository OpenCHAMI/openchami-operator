# Phase 2 — Core Reconciler Skeleton

**Goal:** Main reconcile loop, namespace, RBAC, finalizer, version pin check.
No service-specific logic yet.

## Controller struct
```go
type OpenCHAMIClusterReconciler struct {
    client.Client
    Scheme        *runtime.Scheme
    Recorder      record.EventRecorder
    VaultClient   vault.Client   // injected in main.go
    S3Client      s3.Client      // injected in main.go
    DefaultImages version.ImageConfig
    DryRun        bool           // from OPENCHAMI_DRY_RUN env var
}
```

## Reconcile loop structure
```go
func (r *...) Reconcile(ctx, req) (ctrl.Result, error) {
    // 1. Fetch CR — return if not found
    // 2. Handle deletion (reconcileDelete)
    // 3. Ensure finalizer "openchami.org/cluster-protection"
    // 4. Set ObservedGeneration
    // 5. Call reconcileAll
    // 6. Always patch status (even on error)
}
```

## Version pin check (first thing in reconcileAll)
```go
if cluster.Spec.OperatorChannel == "pinned" &&
    cluster.Spec.PinnedVersion != version.Version {
    // Set ConditionReconcileActive=False, Reason=VersionPinned
    // Call helpers.RecordConditionEvent(...)
    // Return ctrl.Result{} — no requeue
}
```

## Sub-reconciler order in reconcileAll
namespace → rbac → vault → bucket → logbucket → database →
smd → tokensmith → bootService → metadataService →
networkprobe → coredhcp → magellan → funicular →
gateway → certificates → networkpolicies → topology

After all succeed: `cluster.Status.ManagedByVersion = version.Version`

## Namespace sub-reconciler
Creates `openchami-{clusterName}` with labels:
- `kubernetes.io/metadata.name=openchami-{clusterName}`
- `openchami.org/cluster={clusterName}`
- `pod-security.kubernetes.io/enforce=privileged` (with `warn=restricted`, `audit=restricted`). `privileged` is required because three workloads in the namespace require capabilities that PSA `baseline` rejects: `coredhcp` (hostNetwork + hostPort 67), `funicular-collector` (hostPath `/var/log/pods`), and the network-probe DaemonSet (hostNetwork). `baseline` forbids all three; there is no intermediate PSA level. The `warn`/`audit` levels surface any service-tier pod (smd, tokensmith, boot-service, metadata-service) that drifts away from a restricted-fit profile. For per-pod policy inside a privileged namespace, layer on Kyverno or OPA Gatekeeper.

**No OwnerReference** — namespace must outlive the CR.

## RBAC sub-reconciler
ServiceAccounts in cluster namespace (all with `automountServiceAccountToken: false`):
`smd`, `tokensmith`, `boot-service`, `metadata-service`, `coredhcp`,
`magellan`, `network-probe`, `funicular-collector`, `operator-config-reader`

Role + RoleBinding for `operator-config-reader`:
- get/list ConfigMap named `openchami-{clusterName}-topology`

ClusterRole `openchami-{clusterName}-network-probe`:
- get + patch on nodes (cluster-scoped for label writes)

## Deletion (reconcileDelete)
1. Delete namespace (cascades resources)
2. If annotation `openchami.org/cleanup-vault=true`: delete Vault paths
3. If annotation `openchami.org/cleanup-s3=true`: delete bucket contents
4. Delete ClusterRole `openchami-{clusterName}-network-probe`
5. Remove finalizer

## Watches
```go
ctrl.NewControllerManagedBy(mgr).
    For(&v1alpha1.OpenCHAMICluster{}).
    Owns(&appsv1.Deployment{}).
    Owns(&appsv1.DaemonSet{}).
    Owns(&batchv1.CronJob{}).
    Owns(&cnpgv1.Cluster{}).
    Owns(&corev1.ConfigMap{}).
    Owns(&vsov1.VaultStaticSecret{}).
    Watches(&gatewayv1.Gateway{}, handler.EnqueueRequestForOwner(...)).
    Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.nodeToCluster)).
    WithOptions(controller.Options{MaxConcurrentReconciles: 5}).
    Complete(r)
```

`nodeToCluster` maps a node to all OpenCHAMIClusters that have probe labels
matching `openchami.org/{clusterName}/*-network-ready` on that node.

## DryRun mode
When `r.DryRun == true`, each sub-reconciler should call `Describe()` and
log the result instead of applying. Implement this in reconcileAll by
checking `r.DryRun` before calling `rec.Reconcile()`.

## Validation
```bash
tools/check-phase.sh 2
```
