# Phase 12 — Legendary Funicular Log Infrastructure

**Scope:** Infrastructure only. Log schema, NDJSON field definitions, and query
patterns are defined by `github.com/OpenCHAMI/legendary-funicular`.
Before implementing, read the legendary-funicular README to confirm:
- Correct env var names the collector expects
- Expected bucket path layout

## Log bucket — `internal/reconcilers/logbucket.go`

```
Bucket:  helpers.LogBucketName(cluster) (default: {clusterName}-logs)
Credentials: openchami-{clusterName}-s3-logs-creds (VSO-synced)

Steps:
1. Wait for VSO to sync log credentials Secret (requeue 10s if absent)
2. s3Client.EnsureBucket(logBucketName)
3. s3Client.EnsureLifecycleRule(logBucketName, spec.logging.retentionDays)
4. Set LogBucketReady=True; write status.logBucket

Describe(): returns S3 bucket parameters (no Kubernetes object; log as text)
```

## Funicular collector DaemonSet — `internal/reconcilers/funicular.go`

If `spec.logging.enabled=false`: skip. Set `LogCollectorReady=True` trivially.

```
DaemonSet:   funicular-collector
Namespace:   openchami-{clusterName}
Runs on:     all nodes (no nodeSelector)
SA:          funicular-collector

Security: CommonSecurityContext() — no elevated privileges needed.

Volumes:
  hostPath /var/log/pods → /var/log/pods (readOnly: true)
  emptyDir memory        → /tmp

Env vars (confirm names against legendary-funicular README):
  FUNICULAR_CLUSTER_NAME       = spec.clusterName
  FUNICULAR_NAMESPACE          = openchami-{clusterName}
  FUNICULAR_S3_ENDPOINT        = spec.platform.objectStorage.endpoint
  FUNICULAR_S3_BUCKET          = helpers.LogBucketName(cluster)
  FUNICULAR_FLUSH_INTERVAL     = spec.logging.flushIntervalSeconds
  FUNICULAR_ACCESS_KEY         secretKeyRef: openchami-{name}-s3-logs-creds, key: access_key
  FUNICULAR_SECRET_KEY         secretKeyRef: openchami-{name}-s3-logs-creds, key: secret_key
  FUNICULAR_INCLUDE_SERVICES   = strings.Join(spec.logging.includeServices, ",") or ""
```

Condition: `LogCollectorReady=True` when DaemonSet NumberReady>0.

## Logq Compactor CronJob — `internal/reconcilers/logq_compactor.go`

If `spec.logging.compactorEnabled=false`: skip. Set `LogCompactorReady=True` trivially.

```
CronJob:     logq-compactor
Namespace:   openchami-{clusterName}
Schedule:    spec.logging.compactorSchedule (default: "0 2 * * *")
SA:          logq-compactor

Security: CommonSecurityContext() — no elevated privileges needed.

Volumes:
  emptyDir memory → /tmp

Env vars:
  LOGQ_CLUSTER_NAME       = spec.clusterName
  LOGQ_NAMESPACE          = openchami-{clusterName}
  LOGQ_S3_ENDPOINT        = spec.platform.objectStorage.endpoint
  LOGQ_S3_BUCKET          = helpers.LogBucketName(cluster)
  LOGQ_S3_ACCESS_KEY      secretKeyRef: openchami-{name}-s3-logs-creds, key: access_key
  LOGQ_S3_SECRET_KEY      secretKeyRef: openchami-{name}-s3-logs-creds, key: secret_key
```

Condition: `LogCompactorReady=True` when CronJob is applied.

## Logq Query Deployment — `internal/reconcilers/logq_query.go`

If `spec.logging.queryEnabled=false`: skip.

```
Deployment:  logq-query
Namespace:   openchami-{clusterName}
Replicas:    spec.logging.queryReplicas (default: 1)
SA:          logq-query

Security: CommonSecurityContext() — no elevated privileges needed.

Volumes:
  emptyDir memory → /tmp

Env vars:
  LOGQ_CLUSTER_NAME       = spec.clusterName
  LOGQ_NAMESPACE          = openchami-{clusterName}
  LOGQ_S3_ENDPOINT        = spec.platform.objectStorage.endpoint
  LOGQ_S3_BUCKET          = helpers.LogBucketName(cluster)
  LOGQ_HTTP_PORT          = spec.logging.queryPort (default: 8080)
  LOGQ_S3_ACCESS_KEY      secretKeyRef: openchami-{name}-s3-logs-creds, key: access_key
  LOGQ_S3_SECRET_KEY      secretKeyRef: openchami-{name}-s3-logs-creds, key: secret_key

Service:
  Type: ClusterIP
  Port: spec.logging.queryPort (default: 8080)
  
Health checks:
  LivenessProbe:  HTTP GET /health
  ReadinessProbe: HTTP GET /health
```

Condition: `LogQueryReady=True` when Deployment availableReplicas >= 1.

```bash
tools/check-phase.sh 12
```
