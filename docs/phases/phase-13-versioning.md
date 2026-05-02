# Phase 13 — Versioning Infrastructure

## Version pin (already wired in Phase 2)
Verify the check at the top of `reconcileAll` exists and has a unit test.

## Unit test: version pin
```go
// internal/controller/openchamicluster_controller_test.go
It("should not reconcile when cluster is pinned to a different version", func() {
    cluster := &v1alpha1.OpenCHAMICluster{
        Spec: v1alpha1.OpenCHAMIClusterSpec{
            OperatorChannel: "pinned",
            PinnedVersion:   "0.1.0",
            // ...minimal valid spec...
        },
    }
    // version.Version = "0.2.0" in tests
    // Assert: no Deployments created, ReconcileActive=False condition set
})
```

## Storage version migration
Verify `hack/migrate-storage-version.sh` exists, is executable, and runs
without error against a kind cluster (it's a no-op if no CRs exist).

## UPGRADE.md template
Create `UPGRADE.md` with the template structure. Mark it as auto-generated:
```markdown
# UPGRADE.md
<!-- This file is updated on each release. See SERVICES.md for image versions. -->

# Upgrading to openchami-operator (unreleased)

## CRD changes
None.

## Service image updates
None.

## Breaking changes
None.

## Upgrade procedure
1. Pin production clusters before upgrading:
   kubectl patch openchamicluster <name> --type=merge \
     -p '{"spec":{"operatorChannel":"pinned","pinnedVersion":"<current>"}}'
2. Deploy new operator
3. Validate staging clusters: kubectl get openchamicluster -A
4. Unpin production clusters one at a time
5. Verify status.managedByVersion matches new version
```

## SERVICES.md
Create `SERVICES.md` listing current default image tags/digests.
Update this file on every release.

```bash
tools/check-phase.sh 13
```
