# Phase 15 — End-to-End Tests

**PARALLEL PHASE — see AGENTS.md for sub-agent assignments.**
Build tag: `//go:build e2e`
Requires: `make dev-up` before running.

## Suite setup — `test/e2e/suite_test.go`
```go
var _ = BeforeSuite(func() {
    // Verify prerequisites are installed
    // Install CRD: kubectl apply -f config/crd/bases/
    // Start operator: cmd/operator/main.go in goroutine
})
```

## Lifecycle tests — `test/e2e/lifecycle_test.go`

| ID | Test |
|---|---|
| E2E-01 | Single cluster: deploy → Ready condition → delete → namespace gone |
| E2E-02 | Dual cluster: venado + frontier reach Ready independently |
| E2E-07 | DHCP nodeSelector conflict rejected by webhook (probe disabled) |
| E2E-08 | Vault unreachable: VaultConfigured=False → fix address → recovers |
| E2E-09 | Version pin: ReconcileActive=False; ready after unpin |
| E2E-10 | Upgrade simulation: new VERSION ldflags; pinned cluster untouched |

## Network tests — `test/e2e/network_test.go`

| ID | Test |
|---|---|
| E2E-03 | Probe DaemonSet runs; labels appear on kind node within 2 intervals |
| E2E-04 | CoreDHCP waits for provision-network-ready=true before scheduling |
| E2E-05 | Magellan waits for bmc-network-ready=true before scheduling |
| E2E-06 | Misconfigured subnet → NetworkProbeReady=False + Warning Event |

## Observability tests — `test/e2e/observability_test.go`

| ID | Test |
|---|---|
| E2E-11 | Certificate Warning Event fires when cert has <48h remaining (cert duration=2m) |
| E2E-12 | Log bucket exists with lifecycle rule in localstack |
| E2E-13 | Funicular DaemonSet running; Parquet files appear after 2× flush interval |
| E2E-14 | ochami-admin backup completes; objects appear in localstack |

## Fault injection unit tests — `internal/reconcilers/fault_test.go`
These run in the standard `make test` suite (no e2e tag).

| ID | Fault | Expected |
|---|---|---|
| F-01 | Vault 500 on EnsureSecret | No crash; VaultConfigured=False; requeue |
| F-02 | CNPG phase never healthy (mock) | RequeueAfter 30s; no service deploy |
| F-03 | S3 CreateBucket transient error | BucketReady=False; requeue; retry succeeds |
| F-04 | tokensmith PVC pre-exists | No delete/recreate |
| F-05 | 10 concurrent clusters, MaxConcurrentReconciles=5 | No data races |
| F-06 | All probe nodes return false | NetworkProbeReady=False + Warning Event |

Run fault tests with: `go test -race ./internal/reconcilers/...`

```bash
tools/check-phase.sh 15
```
