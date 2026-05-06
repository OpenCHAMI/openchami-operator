# Architecture

The operator manages an `OpenCHAMICluster` custom resource. Each instance produces a self-contained, namespace-isolated OpenCHAMI deployment.

## Big picture

```
                 ┌──────────────────┐
                 │ OpenCHAMICluster │  user-authored CR
                 │      (CR)        │
                 └────────┬─────────┘
                          │ Reconcile
                          ▼
        ┌─────────────────────────────────────┐
        │ OpenCHAMIClusterReconciler          │
        │  (cmd/operator + internal/controller)│
        └─────────────────────────────────────┘
                          │
                          │ orchestrates a fixed-order chain of
                          ▼
   ┌──────────────────────────────────────────────────────────┐
   │ SubReconcilers (internal/reconcilers/*)                   │
   │  Namespace → RBAC → Vault → Bucket → Database →           │
   │  SMD → Tokensmith → BootService → MetadataService →       │
   │  NetworkProbe → CoreDHCP → Magellan → Certificates →      │
   │  Gateway → NetworkPolicies → Funicular → LogBucket →      │
   │  Topology → ServiceMonitor                                │
   └──────────────────────────────────────────────────────────┘
                          │
                          │ each writes Kubernetes resources +
                          │ patches one or more conditions
                          ▼
   ┌──────────────────────────────────────────────────────────┐
   │ openchami-{clusterName} namespace                         │
   │  ServiceAccount, RBAC, Secrets, VaultStaticSecrets,       │
   │  CNPG Cluster, Deployments, DaemonSets, CronJob,          │
   │  HTTPRoute, Certificate, NetworkPolicies (≥19),           │
   │  ConfigMap (topology), ServiceMonitor                     │
   └──────────────────────────────────────────────────────────┘
                          │
                          │ external (operator never deploys)
                          ▼
              Vault     VersityGW S3     Prometheus
```

## Reconcile loop (one tick)

`internal/controller/openchamicluster_controller.go::Reconcile`:

1. Fetch the `OpenCHAMICluster` (no-op on `IsNotFound`).
2. Branch on deletion: `cluster.DeletionTimestamp != nil` → `reconcileDelete`, else continue.
3. Add the `openchami.org/cluster-protection` finalizer if absent. Patch and return — the next reconcile picks up.
4. Snapshot the current `*OpenCHAMICluster` so we can patch the diff.
5. `cluster.Status.ObservedGeneration = cluster.Generation` and call `reconcileAll`.
6. `r.Status().Patch(ctx, cluster, client.MergeFrom(orig))` — the status patch is the very last thing, regardless of `reconcileErr`.

Step 6 is **invariant 6**. Status is always patched. If the reconcile errored *and* the patch errored, both errors are joined.

## `reconcileAll`

Runs the SubReconciler list in a fixed order. The list is:

```go
subs := []reconcilers.SubReconciler{
    &reconcilers.NamespaceReconciler{...},
    &reconcilers.RBACReconciler{...},
    &reconcilers.VaultReconciler{...},
    &reconcilers.BucketReconciler{...},
    &reconcilers.DatabaseReconciler{...},
    &reconcilers.SMDReconciler{...},
    &reconcilers.TokensmithReconciler{...},
    &reconcilers.BootServiceReconciler{...},
    &reconcilers.MetadataServiceReconciler{...},
    &reconcilers.NetworkProbeReconciler{...},
    &reconcilers.CoreDHCPReconciler{...},
    &reconcilers.MagellanReconciler{...},
    &reconcilers.CertificatesReconciler{...},
    &reconcilers.GatewayReconciler{...},
    &reconcilers.NetworkPoliciesReconciler{...},
    // … plus Funicular, LogBucket, Topology, ServiceMonitor
}
```

Each entry is invoked in turn. A reconciler returning a non-zero `ctrl.Result.RequeueAfter` causes the controller to requeue at that interval; a non-nil error causes immediate requeue with backoff.

The order is **not** arbitrary. Namespace must precede everything that owns a resource. Vault and Bucket must precede Database (CNPG bootstraps with Vault-sourced credentials). SMD must precede services that talk to it. CoreDHCP and Magellan depend on NetworkProbe labels. See [reconcilers](reconcilers.md) for each dependency.

If `cluster.Spec.OperatorChannel == "pinned"` and `cluster.Spec.PinnedVersion != version.Version`, `reconcileAll` short-circuits with the `ReasonVersionPinned` condition and emits an Event. See [upgrade-and-versioning](upgrade-and-versioning.md).

## SubReconciler interface

```go
type SubReconciler interface {
    Reconcile(ctx context.Context, cluster *v1alpha1.OpenCHAMICluster) (ctrl.Result, error)
    Describe(cluster *v1alpha1.OpenCHAMICluster) ([]client.Object, error)
}
```

Implementation rules (enforced by `make validate-invariants`):

- **Idempotent.** Calling `Reconcile` twice has no additional effect.
- **Server-side apply only.** Use `client.Apply`; never `client.Create` followed by `client.Update` (invariant 5).
- **Logs use `logging.Enrich`.** Top of every `Reconcile` (invariant 8).
- **Conditions, never raw status fields.** Use `apimeta.SetStatusCondition`. The aggregator computes `phase` from conditions.
- **Events use `helpers.RecordConditionEvent`.** Never `recorder.Event` directly. Each Event includes a runbook URL (invariant 9).
- **Errors wrap.** `fmt.Errorf("reconciling X: %w", err)`.
- **`Describe` is pure.** No I/O, no API calls. Used by `ochami-admin describe`.

`make validate-invariants` greps the source for violations. CI runs it before tests.

## Per-cluster namespace isolation

Every resource for `OpenCHAMICluster foo` lives in `openchami-foo`. Nothing leaks across cluster boundaries (invariant 2). The Namespace reconciler is the first sub-reconciler and is the only place that creates the namespace.

## Vault path isolation

All KV paths are prefixed `openchami/{clusterName}/`. The Vault reconciler validates uniqueness before any write (invariant 3). Two clusters cannot share a path; the validation webhook also enforces uniqueness at admission time.

## What the operator does NOT know

The operator has no HPC domain knowledge (invariant 10). It does not know about compute nodes, xnames, IPMI addresses, boot parameters, DHCP lease assignments, firmware states, or scheduler queues. **The quadlet test:** before adding any feature, ask "would this concept exist if the services were started as systemd units with no Kubernetes operator?" If yes, it belongs in the services. If no, it may belong in the operator.

This is what divides the operator from the integration-sandbox: the sandbox tests *the services*, the operator tests *the deployment shape*. See [relationship-to-integration-sandbox](relationship-to-integration-sandbox.md).

## Source map

| Path | Purpose |
|---|---|
| `cmd/operator/` | manager binary entrypoint |
| `cmd/ochami-admin/` | CLI: `init`, `describe`, `backup`, `restore`, `logs` |
| `cmd/probe/` | network-probe DaemonSet binary |
| `api/v1alpha1/` | CRD types, webhooks, conversions |
| `internal/controller/` | top-level controller + reconcile loop |
| `internal/reconcilers/` | one file per concern (Namespace, Vault, SMD, ...) |
| `internal/conditions/` | condition Type/Reason constants |
| `internal/logging/` | log enrichment helpers (invariant 8) |
| `internal/status/` | post-reconcile aggregator + Prometheus metrics |
| `internal/vault/` | Vault client interface + AppRole auth |
| `internal/s3/` | S3 client interface |
| `internal/version/` | operator semver and image tag config |
| `config/` | controller-gen output: CRD, RBAC, webhook manifests |
| `hack/local-dev/` | docker-compose, kind config, Vault seed |
| `test/fixtures/` | minimal-cluster.yaml, dual-cluster.yaml, full-cluster.yaml |
| `test/e2e/` | Ginkgo end-to-end tests |
| `docs/` | this tree |
