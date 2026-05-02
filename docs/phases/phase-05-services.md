# Phase 5 — Core Service Reconcilers

**PARALLEL PHASE — see AGENTS.md for sub-agent assignments.**
Spawn 4 sub-agents simultaneously. Run `make test` after all 4 complete.

## Common properties (all Deployments)

```
Namespace:   openchami-{clusterName}
Strategy:    RollingUpdate, maxUnavailable=0, maxSurge=1
Apply:       server-side apply, field manager "openchami-operator"

Security context (use helpers.CommonSecurityContext()):
  runAsNonRoot, runAsUser=65534, readOnlyRootFilesystem=true
  allowPrivilegeEscalation=false, seccompProfile=RuntimeDefault
  capabilities.drop=[ALL]

Pod security context (use helpers.CommonPodSecurityContext()):
  runAsNonRoot, runAsUser=65534, runAsGroup=65534, fsGroup=65534

Volumes: tmp emptyDir memory (use helpers.TmpVolume())

Probes:
  startupProbe:   failureThreshold=30, periodSeconds=5
  livenessProbe:  periodSeconds=20, failureThreshold=3
  readinessProbe: periodSeconds=10, failureThreshold=3

topologySpreadConstraints:
  maxSkew=1, topologyKey=kubernetes.io/hostname

PDB: minAvailable=1 (created alongside each Deployment)
```

## Env var bootstrap pattern
Each Deployment has BOTH direct env vars (bootstrap) AND a mounted
topology ConfigMap volume (steady-state). Both are injected; services
use whichever is available at startup.

## SMD (Sub-agent A)
Port: 27779
Health path: `/hsm/v2/service/ready`
Key env vars:
```
SMD_DBHOST      = openchami-{name}-postgres-rw.openchami-{name}.svc.cluster.local
SMD_DBPORT      = 5432
SMD_DBNAME      = smd
SMD_DBUSER      = smd
SMD_DBPASS      = secretKeyRef: openchami-{name}-db-creds, key: SMD_DB_PASSWORD
SMD_JWKS_URL    = http://tokensmith.openchami-{name}.svc.cluster.local:8080/.well-known/jwks.json
```

## Tokensmith specifics (Sub-agent B)
Port: 8080
Health path: `/health`
**Strategy: Recreate** (PVC is ReadWriteOnce — Rolling would deadlock)
**Replicas: always 1** (key material consistency)
PVC: `tokensmith-data`, 1Gi, ReadWriteOnce
     Annotation: `"helm.sh/resource-policy": "keep"` (never delete on CR deletion)
Key env vars:
```
OIDC_CLIENT_SECRET  = secretKeyRef: openchami-{name}-tokensmith-oidc, key: client_secret
```
When oidcProvider=vault, also inject the Vault OIDC issuer URL derived from
spec.platform.vault.address + `/v1/identity/oidc/provider/default`.
When oidcProvider=external, inject spec.services.tokensmith.oidcIssuerURL.

## Boot Service (Sub-agent C)
Port: 27778
Health path: `/boot/v1/service/status`
Key env vars:
```
BOOT_SERVICE_DBHOST         = openchami-{name}-postgres-rw...
BOOT_SERVICE_DBNAME         = boot_service
BOOT_SERVICE_DBPASS         = secretKeyRef: openchami-{name}-db-creds, key: BOOT_SERVICE_DB_PASSWORD
BOOT_SERVICE_SMD_URL        = http://smd.openchami-{name}.svc.cluster.local:27779
BOOT_SERVICE_JWKS_URL       = http://tokensmith.openchami-{name}.svc.cluster.local:8080/.well-known/jwks.json
BOOT_SERVICE_S3_ENDPOINT    = spec.platform.objectStorage.endpoint
BOOT_SERVICE_S3_BUCKET      = helpers.BootBucketName(cluster)
BOOT_SERVICE_S3_ACCESS_KEY  = secretKeyRef: openchami-{name}-s3-creds, key: access_key
BOOT_SERVICE_S3_SECRET_KEY  = secretKeyRef: openchami-{name}-s3-creds, key: secret_key
```

## Metadata Service (Sub-agent D)
Port: 8081
Health path: `/cloud-init/health`
Key env vars:
```
METADATA_CLUSTER_NAME  = spec.clusterName
METADATA_SMD_URL       = http://smd.openchami-{name}.svc.cluster.local:27779
METADATA_JWKS_URL      = http://tokensmith.openchami-{name}.svc.cluster.local:8080/.well-known/jwks.json
```

## ServicesReady condition
Set True only when ALL enabled Deployments have availableReplicas >= 1.
Record Warning Event for each service that transitions Ready→NotReady.

```bash
tools/check-phase.sh 5
```
