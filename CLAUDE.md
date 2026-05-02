# OpenCHAMI Operator — CLAUDE.md

Read this file completely before writing any code.
Read the phase file for each phase before starting that phase.
After every phase: `make generate manifests fmt vet lint test`
All four must pass before proceeding.

---

## Project Identity

| Item | Value |
|---|---|
| Go module | `github.com/openchami/openchami-operator` |
| Operator image | `ghcr.io/openchami/openchami-operator` |
| Admin CLI | `ochami-admin` |
| CRD group/version | `openchami.org/v1alpha1` |
| Primary resource | `OpenCHAMICluster` |
| Kubebuilder | v4 |
| Go | 1.23+ |
| Kubernetes target | 1.29+ |
| Log lake | legendary-funicular (VersityGW + NDJSON + Parquet + DuckDB) |

---

## Absolute Invariants

Violating any of these is a bug regardless of what else works.

**1. Vault and VersityGW are external.**
Configure them; never deploy them. Unreachable → requeue with backoff. Never crash.

**2. Per-cluster namespace isolation.**
Every resource for `OpenCHAMICluster` named `foo` lives in `openchami-foo`.
Nothing leaks across namespace boundaries.

**3. Vault path isolation.**
All KV paths prefixed `openchami/{clusterName}/`.
Validate uniqueness before any write. Two clusters cannot share a path.

**4. DHCP node exclusivity.**
When network probing is disabled, no two `OpenCHAMICluster` instances may
target the same Kubernetes node for CoreDHCP. Webhook enforces this.

**5. Idempotent reconciliation.**
Every reconcile function produces identical results when called twice.
Server-side apply everywhere. Never create then update.

**6. Status is always written last.**
Never return from reconcile without patching `.status.conditions`.
Use `status.Patch`, never `status.Update`.

**7. No secrets in the CRD spec.**
The spec references secrets by name only. Never contains credential values.

**8. Structured logging on every line.**
Every `log.Info`/`log.Error` must carry `cluster`, `reconciler`, and `resource`
key-value pairs. Use `internal/logging/logger.go` helpers exclusively.
Never call `log.FromContext` directly in sub-reconcilers.

**9. Every operator Event includes a runbook URL.**
Format: `https://openchami.org/docs/ops/{reason-in-kebab-case}`.
Use `helpers.RecordConditionEvent`. Never call `recorder.Event` directly.

**10. The operator has no HPC domain knowledge.**
It does not know about compute nodes, xnames, IPMI addresses, boot parameters,
DHCP lease assignments, firmware states, or scheduler queues.
A deployment managed by this operator must be indistinguishable — to an HPC
admin — from one running under quadlet, docker-compose, or any other means.

**The quadlet test:** Before adding any feature, ask: "Would this concept
exist if the services were started as systemd units with no Kubernetes operator?"
If yes: it belongs in the services. If no: it may belong here.

---

## Repository Layout

```
openchami-operator/
├── CLAUDE.md                    ← this file
├── AGENTS.md                    ← sub-agent parallel instructions
├── README.md
├── UPGRADE.md                   ← generated per release
├── SERVICES.md                  ← image versions per release
├── Makefile
├── go.mod / go.sum
├── .golangci.yml
├── docs/phases/                 ← one file per phase
├── tools/
│   ├── check-phase.sh           ← run after each phase to validate
│   └── validate-invariants.sh   ← static checks on invariant compliance
├── hack/local-dev/              ← kind + Vault dev + localstack
├── test/fixtures/               ← pre-written CR YAML for tests
├── api/v1alpha1/
├── internal/
│   ├── conditions/conditions.go ← PRE-WRITTEN: condition type constants
│   ├── logging/logger.go        ← PRE-WRITTEN: structured log helpers
│   ├── reconcilers/
│   │   ├── reconciler.go        ← PRE-WRITTEN: subReconciler interface
│   │   ├── helpers.go           ← PRE-WRITTEN: RunbookURL, EffectiveNodeSelector
│   │   └── topology_schema.go   ← define schema here; owned by this operator
│   ├── vault/
│   │   ├── client.go            ← PRE-WRITTEN: Client interface
│   │   ├── paths.go             ← PRE-WRITTEN: VaultPaths + Paths()
│   │   └── fake/client.go       ← implement in Phase 3
│   ├── s3/
│   │   ├── client.go            ← PRE-WRITTEN: S3Client interface
│   │   └── fake/client.go       ← implement in Phase 3
│   ├── version/
│   │   ├── version.go           ← PRE-WRITTEN: version vars
│   │   └── images.go            ← PRE-WRITTEN: ImageConfig
│   ├── status/reporter.go
│   └── controller/
├── cmd/operator/main.go
└── cmd/ochami-admin/main.go
```

Files marked PRE-WRITTEN exist already. Read them before implementing
anything that depends on them. Do not modify their interfaces without
updating all callers.

---

## Phase Index

Read the phase file before starting. Implement in strict order.

| Phase | File | Goal | Parallel? |
|---|---|---|---|
| 0 | docs/phases/phase-00-bootstrap.md | Scaffold, dev env, CLI skeleton | No |
| 1 | docs/phases/phase-01-crd-types.md | All CRD types, status, conditions | No |
| 2 | docs/phases/phase-02-controller.md | Core reconcile loop, namespace, RBAC | No |
| 3 | docs/phases/phase-03-vault.md | Vault, fakes, S3 bucket | No |
| 4 | docs/phases/phase-04-database.md | CloudNativePG cluster | No |
| 5 | docs/phases/phase-05-services.md | SMD, tokensmith, boot-service, metadata-service | **Yes — 4 parallel sub-agents** |
| 6 | docs/phases/phase-06-network.md | Network probe, CoreDHCP, Magellan | No (ordered dependency) |
| 7 | docs/phases/phase-07-gateway.md | Envoy Gateway, certs, expiry tracking | No |
| 8 | docs/phases/phase-08-networkpolicies.md | Zero-trust NetworkPolicies | **Yes — all policies parallel** |
| 9 | docs/phases/phase-09-topology.md | Topology ConfigMap | No |
| 10 | docs/phases/phase-10-webhooks.md | Defaulting, validation, conversion hub | No |
| 11 | docs/phases/phase-11-observability.md | Status, Prometheus metrics, ServiceMonitor | No |
| 12 | docs/phases/phase-12-funicular.md | Log bucket, collector DaemonSet | No |
| 13 | docs/phases/phase-13-versioning.md | Version pin, migration, UPGRADE.md | No |
| 14 | docs/phases/phase-14-cli.md | ochami-admin init/describe/backup/restore/logs | **Yes — 5 parallel sub-agents** |
| 15 | docs/phases/phase-15-e2e.md | E2E + fault injection tests | **Yes — test groups parallel** |

---

## Critical Conventions (apply everywhere)

```go
// Logging — always use helpers
log := logging.Enrich(ctx, cluster, "reconciler-name")
log = logging.EnrichWithResource(log, "Deployment", "smd")

// Events — always use helper
helpers.RecordConditionEvent(r.Recorder, cluster,
    corev1.EventTypeWarning, "ReasonPascalCase", "human message")

// Node selector — always use helper for CoreDHCP/Magellan
nodeSelector := helpers.EffectiveNodeSelector(cluster, "provision") // or "bmc"

// Errors — always wrap
return ctrl.Result{}, fmt.Errorf("reconciling vault: %w", err)

// Status — always patch, never update
r.Status().Patch(ctx, cluster, patch)
```

---

## External CRD Prerequisites

Check at startup; warn but don't fail if missing:
```
clusters.postgresql.cnpg.io
vaultstaticsecretes.secrets.hashicorp.com
vaultconnections.secrets.hashicorp.com
vaultauths.secrets.hashicorp.com
gateways.gateway.networking.k8s.io
httproutes.gateway.networking.k8s.io
securitypolicies.gateway.envoyproxy.io
backendtrafficpolicies.gateway.envoyproxy.io
certificates.cert-manager.io
```

---

## Validation Gate

After every phase, run: `tools/check-phase.sh <phase-number>`
This script verifies the phase-specific criteria automatically.
Do not proceed until it exits 0.
