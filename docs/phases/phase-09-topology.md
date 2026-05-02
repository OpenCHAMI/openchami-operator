# Phase 9 — Topology ConfigMap

**File:** `internal/reconcilers/topology.go`
**Schema:** `internal/reconcilers/topology_schema.go` (defined in Phase 0)

## ConfigMap
```
Name:      openchami-{clusterName}-topology
Namespace: openchami-{clusterName}
Labels:    openchami.org/topology=true
           openchami.org/cluster={clusterName}
```

## JSON schema (populate from cluster state)
```json
{
  "clusterName": "venado",
  "version": "sha256:...",
  "generatedAt": "2026-05-01T12:00:00Z",
  "domain": "venado.hpc.example.com",
  "services": {
    "smd":             {"endpoint": "http://smd.openchami-venado.svc.cluster.local:27779",
                        "externalPath": "/hsm", "ready": true},
    "tokensmith":      {"endpoint": "http://tokensmith.openchami-venado.svc.cluster.local:8080",
                        "jwksURL": "http://tokensmith.openchami-venado.svc.cluster.local:8080/.well-known/jwks.json",
                        "externalPath": "/oauth", "ready": true},
    "bootService":     {"endpoint": "http://boot-service.openchami-venado.svc.cluster.local:27778",
                        "s3Endpoint": "...", "s3Bucket": "venado-boot-images",
                        "externalPath": "/boot", "ready": true},
    "metadataService": {"endpoint": "http://metadata-service.openchami-venado.svc.cluster.local:8081",
                        "externalPath": "/cloud-init", "ready": true}
  },
  "platform": {
    "vault":         {"address": "...", "kvMount": "openchami", "pathPrefix": "openchami/venado"},
    "objectStorage": {"endpoint": "...", "bucket": "venado-boot-images"},
    "logging":       {"endpoint": "...", "bucket": "venado-logs", "parquetPrefix": "logs/"}
  },
  "database": {
    "readWriteEndpoint": "openchami-venado-postgres-rw.openchami-venado.svc.cluster.local:5432",
    "readOnlyEndpoint":  "openchami-venado-postgres-ro.openchami-venado.svc.cluster.local:5432"
  }
}
```

## Version hash
SHA-256 of JSON content excluding `version` and `generatedAt` fields.
Write to `status.topologyVersion`.

## Condition
`TopologyPublished=True` when ConfigMap exists and hash matches computed value.

## service.ready field
Populate from `status.services[name].ready`. Services that are not yet
deployed or have availableReplicas=0 get `"ready": false`.

```bash
tools/check-phase.sh 9
```
