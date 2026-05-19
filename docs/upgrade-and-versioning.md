# Upgrade and versioning

Two parallel concerns:
1. The operator's own version moving forward (channel + pin).
2. The CRD storage version moving forward when the API surface evolves.

## Version model

The operator embeds its own semver via `internal/version/version.go`. Every reconcile compares it against `cluster.Spec.PinnedVersion` when `cluster.Spec.OperatorChannel == "pinned"`.

```yaml
spec:
  operatorChannel: stable | pinned   # default: stable
  pinnedVersion: "1.4.2"             # required when operatorChannel=pinned
```

| Channel | Behavior |
|---|---|
| `stable` | The operator always reconciles, regardless of version. This is the default. Use it for clusters you want to track upgrades automatically. |
| `pinned` | The operator reconciles **only** if its semver matches `pinnedVersion`. Mismatched operators set `ReconcileActive=False` with reason `VersionPinned` and emit an Event. The cluster is "frozen at this version" until the pin is updated. |

`status.managedByVersion` records the version that **last successfully reconciled**. It can lag `pinnedVersion` if the matching operator hasn't run yet.

## Upgrading the operator (rolling, no breakage)

1. **Pin production clusters before upgrading.**
   ```sh
   kubectl patch openchamicontrolplane <name> --type=merge \
     -p '{"spec":{"operatorChannel":"pinned","pinnedVersion":"<current>"}}'
   ```
2. **Deploy the new operator** (helm upgrade, image bump, whatever your distribution uses). The new operator will see pinned clusters as `pinnedVersion != myVersion` and skip them.
3. **Validate staging clusters first.** Unpin one staging cluster:
   ```sh
   kubectl patch openchamicontrolplane staging-foo --type=merge \
     -p '{"spec":{"operatorChannel":"stable"}}'
   ```
   Watch `status.managedByVersion` advance to the new version. Verify `status.phase=Ready` and that no reconciler reports a regression.
4. **Unpin production clusters one at a time.** Same patch as step 3, on each in turn. Wait for `status.managedByVersion` to advance and `phase=Ready` between clusters.
5. **Verify the rollout.** `kubectl get openchamicontrolplane -A -o jsonpath='{.items[*].status.managedByVersion}' | tr ' ' '\n' | sort | uniq -c` — every count should be the new version.

This is the procedure documented in `UPGRADE.md`. It's lifted there so each release can amend it without forking docs.

## CRD storage version migration

Different from the operator version: the CRD has its own version axis. Today there's only `v1alpha1`. When `v1beta1` lands:

1. **Apply the new CRD.** controller-gen serves both versions; `v1beta1` is marked `storage: true`.
2. **Run the storage-version migration:**
   ```sh
   hack/migrate-storage-version.sh
   ```
   This script no-op patches every `OpenCHAMIControlPlane` in every namespace, forcing the API server to re-encode it at the current storage version. Safe to re-run.
3. **Once every CR is at the new storage version**, the next operator release can drop `v1alpha1` from `served` (it stays as a conversion source).

The storage-version migration is **mandatory** before dropping the old version from `served`. Skipping it leaves cached objects unreadable to the new API server.

The conversion webhook itself is a stub today; it becomes substantive when `v1beta1` lands. See [webhooks](webhooks.md) for the codebase layout.

## Default service images

Per-release default service image tags are pinned in `SERVICES.md` at the repository root. **Each release** updates this file alongside `UPGRADE.md`.

```
| Service        | Image                                   | Source constant                                          |
|----------------|-----------------------------------------|----------------------------------------------------------|
| SMD            | ghcr.io/openchami/smd:v1.4.2            | defaultSMDImage (internal/reconcilers/smd.go)            |
| Tokensmith     | ghcr.io/openchami/tokensmith:v1.4.2     | defaultTokensmithImage (...)                             |
| ...            | ...                                     | ...                                                      |
```

Per-cluster overrides via `spec.services.<name>.image.{repository,tag}` always win.

`latest` is intended only for development. Cutting a release means walking each constant and replacing `:latest` with a pinned tag, then committing the change to `SERVICES.md` in the same PR.

## What you do NOT need to do at upgrade time

- **You do not need to migrate Vault paths.** Path prefixes are derived from `clusterName` and never change for a given cluster.
- **You do not need to drain workloads.** Reconciliation is idempotent (invariant 5); the operator can replace itself underneath running services.
- **You do not need to backup the CRD.** Kubernetes etcd is the source of truth; back it up at the cluster level.
- **You do not need to rotate certificates.** cert-manager handles certificate lifecycle independently of operator version.

## When the channel doesn't match

If you accidentally deploy operator `1.5.0` against a cluster pinned to `1.4.2`, the symptom is:

```
Conditions:
  Type:    ReconcileActive
  Status:  False
  Reason:  VersionPinned
  Message: operator version 1.5.0 does not match pinned version 1.4.2
```

There's no damage — the operator simply skipped the cluster. To resolve:
- Roll back the operator to `1.4.2`, **or**
- Bump `pinnedVersion` to `1.5.0` (after validating in staging).
