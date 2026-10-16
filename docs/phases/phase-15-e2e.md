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
| E2E-08 | Ready → pause test Vault → VaultConfigured=False/Unreachable and sustained Degraded → resume Vault → Ready without restarting the operator |
| E2E-09 | Version pin: ReconcileActive=False; ready after unpin |
| E2E-10 | Upgrade simulation: new VERSION ldflags; pinned cluster untouched |

## Network tests — `test/e2e/network_test.go`

| ID | Test |
|---|---|
| E2E-03 | Probe DaemonSet runs; labels appear on kind node within 2 intervals |
| E2E-04 | CoreDHCP waits for provision-network-ready=true before scheduling |
| E2E-05 | Unavailable Magellan reports NotConfigured and creates no CronJob |
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

E2E-08 pauses the dedicated `openchami-vault-dev` Docker container from
`make dev-up` and registers cleanup to resume it even if the test fails. The
lifecycle group runs serially so this outage cannot overlap other specs.
The operator's `VAULT_ADDR` must point at this test Vault. To use another
dedicated Docker Vault, set `E2E_VAULT_CONTAINER` to its container name and
`E2E_VAULT_ADDR` to its service address. When `E2E_VAULT_ADDR` is overridden,
E2E-08 skips unless `E2E_VAULT_CONTAINER` is also set. Do not target a shared or
production Vault. Patching the CR's Vault address does not redirect the
operator's startup-configured reachability client.

```bash
tools/check-phase.sh 15
```
