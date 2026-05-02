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
   ```
   kubectl patch openchamicluster <name> --type=merge \
     -p '{"spec":{"operatorChannel":"pinned","pinnedVersion":"<current>"}}'
   ```
2. Deploy new operator
3. Validate staging clusters: `kubectl get openchamicluster -A`
4. Unpin production clusters one at a time
5. Verify `status.managedByVersion` matches new version

## Storage version migration
After installing a new operator that ships a new CRD storage version, run:
```
hack/migrate-storage-version.sh
```
The script no-op patches every OpenCHAMICluster, forcing the API server to
re-encode it at the current storage version. Safe to re-run.
