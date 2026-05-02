# Phase 4 — Database Provisioning

**File:** `internal/reconcilers/database.go`

## CloudNativePG Cluster resource
```
Name:      openchami-{clusterName}-postgres
Namespace: openchami-{clusterName}
Instances: spec.database.instances (default 3)
Storage:   spec.database.storageSize per instance (default 20Gi)

Bootstrap DB:   smd (owner: smd)
Post-init DB:   boot_service (owner: boot_service, via Job)

CNPG creates:
  openchami-{clusterName}-postgres-rw   (primary)
  openchami-{clusterName}-postgres-ro   (replicas)
```

Image: `ghcr.io/cloudnative-pg/postgresql:16.3`

Credentials: sourced from `openchami-{clusterName}-db-creds` Secret (VSO-synced).
Wait for this Secret before creating the CNPG Cluster.

## Requeue strategy
While CNPG cluster phase ≠ "Cluster in healthy state":
- Set `DatabaseReady=False, Reason=Provisioning`
- Return `ctrl.Result{RequeueAfter: 30 * time.Second}`

Set `DatabaseReady=True` once healthy.

## Post-init SQL Job
Runs once after `DatabaseReady=True` is first observed.
Name: `openchami-{clusterName}-db-init`
Image: `postgres:16-alpine`
Command: `CREATE DATABASE boot_service OWNER boot_service;` (wrapped in IF NOT EXISTS)
Only create if Job does not already exist.

## Describe()
Returns: CNPG Cluster object + post-init Job object.

## Tests
- CNPG Cluster created with correct fields
- `DatabaseReady=False` while cluster not healthy
- Post-init Job created after `DatabaseReady=True`
- Two clusters have separate CNPG clusters

```bash
tools/check-phase.sh 4
```
