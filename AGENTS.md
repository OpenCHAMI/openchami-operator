# AGENTS.md — Sub-Agent Instructions

This file is read by sub-agents spawned during parallel phases.
The main CLAUDE.md governs overall project rules; this file governs
how parallel work is coordinated.

---

## Universal Rules for All Sub-Agents

1. Read `CLAUDE.md` before writing any code.
2. Read the relevant pre-written interface files before implementing:
   - `internal/reconcilers/reconciler.go` — subReconciler interface
   - `internal/vault/client.go` — Vault interface
   - `internal/s3/client.go` — S3 interface
   - `internal/logging/logger.go` — logging helpers
   - `internal/reconcilers/helpers.go` — RunbookURL, RecordConditionEvent, EffectiveNodeSelector
3. Every file you create must compile: `go build ./...` after each file.
4. Write the test alongside the implementation in the same sub-task.
5. Use server-side apply. Never `Create` then `Update`.
6. Every log line must use `logging.Enrich` or `logging.EnrichWithResource`.
7. Every Event must use `helpers.RecordConditionEvent`.
8. Never call `log.FromContext` directly.
9. Never call `recorder.Event` directly.

---

## Phase 5 — Core Service Reconcilers (4 parallel sub-agents)

Spawn four sub-agents simultaneously. Each is fully independent.
All four must complete before running `make test`.

### Sub-agent A: SMD
Files to create:
- `internal/reconcilers/smd.go`
- `internal/reconcilers/smd_test.go`

Implements: `subReconciler` interface for the SMD Deployment.
Read: `docs/phases/phase-05-services.md` section "SMD".
Condition to set: `ServicesReady` (partial — aggregate after all four complete).

### Sub-agent B: Tokensmith
Files to create:
- `internal/reconcilers/tokensmith.go`
- `internal/reconcilers/tokensmith_test.go`

Implements: `subReconciler` interface for the tokensmith Deployment + PVC.
Read: `docs/phases/phase-05-services.md` section "Tokensmith specifics".
Strategy must be `Recreate`. Replicas must always be 1.

### Sub-agent C: Boot Service
Files to create:
- `internal/reconcilers/boot_service.go`
- `internal/reconcilers/boot_service_test.go`

Implements: `subReconciler` interface for the boot-service Deployment.
Read: `docs/phases/phase-05-services.md` section "Boot Service".

### Sub-agent D: Metadata Service
Files to create:
- `internal/reconcilers/metadata_service.go`
- `internal/reconcilers/metadata_service_test.go`

Implements: `subReconciler` interface for the metadata-service Deployment.
Read: `docs/phases/phase-05-services.md` section "Metadata Service".

---

## Phase 8 — Network Policies (parallel within single agent)

All NetworkPolicy resources are independent. Generate all policy functions
in `internal/reconcilers/networkpolicies.go` in a single pass.
Do not split across multiple files.

Order within the file (for readability only — no functional dependency):
1. `defaultDenyAll`
2. `allowDNSEgress`
3. `allowVaultEgress`      — uses `vaultEgressPeer()` helper
4. `allowVersityGWEgress`
5. `allowLogsEgress`
6. `smdPolicy`
7. `tokensmithPolicy`
8. `bootServicePolicy`
9. `metadataServicePolicy`
10. `coreDHCPPolicy`
11. `magellanPolicy`
12. `networkProbePolicy`
13. `funicularPolicy`

The `vaultEgressPeer()` function is shared — implement it once at the top.

---

## Phase 14 — CLI Sub-commands (5 parallel sub-agents)

Spawn five sub-agents simultaneously after the cobra skeleton exists.

### Sub-agent A: `ochami-admin init`
File: `internal/admin/init.go`
Read: `docs/phases/phase-14-cli.md` section "init".

### Sub-agent B: `ochami-admin describe`
File: `internal/admin/describe.go`
Read: `docs/phases/phase-14-cli.md` section "describe".
Depends on: subReconciler `Describe()` method — read all reconciler interfaces first.

### Sub-agent C: `ochami-admin backup`
File: `internal/admin/backup.go`
Read: `docs/phases/phase-14-cli.md` section "backup".

### Sub-agent D: `ochami-admin restore`
File: `internal/admin/restore.go`
Read: `docs/phases/phase-14-cli.md` section "restore".

### Sub-agent E: `ochami-admin logs`
File: `internal/admin/logs.go`
Read: `docs/phases/phase-14-cli.md` section "logs".
Depends on: DuckDB Go bindings — confirm import path from go.mod.

---

## Phase 15 — E2E Tests (parallel test groups)

Spawn three sub-agents after dev environment is confirmed running.

### Sub-agent A: Lifecycle tests
Tests: E2E-01 through E2E-07
File: `test/e2e/lifecycle_test.go`

### Sub-agent B: Network and probe tests
Tests: E2E-03 through E2E-06
File: `test/e2e/network_test.go`

### Sub-agent C: Observability and log tests
Tests: E2E-11 through E2E-14
File: `test/e2e/observability_test.go`

Fault injection tests always run in the main unit test suite, not e2e.
File: `internal/reconcilers/fault_test.go`

---

## Coordination Rules

- Sub-agents working in parallel must not write to the same file.
- If two sub-agents need a shared type, it goes in `internal/reconcilers/helpers.go`
  and is written by the main agent before spawning sub-agents.
- Sub-agents report completion by stating the files created and the test output.
- Main agent runs `make test` after all sub-agents in a phase complete.
