# Observability

Three layers, each with strict conventions: conditions, events, metrics. Plus a fourth concern, structured logs.

## Conditions

Conditions are the canonical signal for "is this thing healthy?". Operators (and humans) read `status.conditions[]` to answer it.

### Convention

Each sub-reconciler owns exactly one or two condition types. Format follows `metav1.Condition`:

```yaml
- type: NamespaceReady          # CamelCase
  status: "True"                # "True" | "False" | "Unknown"
  reason: NamespaceCreated      # CamelCase, finite vocabulary in internal/conditions/
  message: "namespace openchami-foo created"
  observedGeneration: 7
  lastTransitionTime: "2026-05-03T11:23:45Z"
```

### Standard condition types

| Type | Owner |
|---|---|
| `ReconcileActive` | `internal/controller/openchamicontrolplane_controller.go` (set false when pinned-version mismatch) |
| `NamespaceReady` | `internal/reconcilers/namespace.go` |
| `RBACReady` | `internal/reconcilers/rbac.go` |
| `VaultReady` | `internal/reconcilers/vault.go` |
| `BucketReady` | `internal/reconcilers/bucket.go` |
| `DatabaseReady` | `internal/reconcilers/database.go` |
| `ServicesReady` | aggregated from SMD/Tokensmith/Boot/Metadata reconcilers |
| `NetworkProbeReady` | `internal/reconcilers/networkprobe.go` |
| `DHCPReady` | `internal/reconcilers/coredhcp.go` |
| `MagellanReady` | `internal/reconcilers/magellan.go` |
| `CertificatesReady` | `internal/reconcilers/certificates.go` |
| `GatewayReady` | `internal/reconcilers/gateway.go` |
| `NetworkPoliciesReady` | `internal/reconcilers/networkpolicies.go` |
| `LoggingReady` | `internal/reconcilers/funicular.go` + `logbucket.go` |
| `ObservabilityReady` | `internal/reconcilers/servicemonitor.go` |

### Reasons

Reasons are a closed vocabulary in `internal/conditions/`. Adding a reason means:
1. Add a constant (`ReasonFooBar`) in `internal/conditions/reasons.go`.
2. Map it to a runbook URL via `helpers.RunbookURL` (kebab-case slug).
3. Write the runbook page at `docs/ops/{slug}.md` in the openchami.org docs site.

The `validate-invariants` check fails if a reason ships without a runbook page.

## Phase aggregation

`internal/status/Reporter` reads conditions and computes `status.phase`. The mapping is:

- All required conditions True → `Ready`.
- Any required condition False with `Reason=*Unreachable` → `Degraded` (recovery is in progress; requeue is scheduled).
- Any required condition False with `Reason=*Failed` → `Failed`.
- DeletionTimestamp set → `Deleting`.
- Otherwise → `Provisioning`.

The list of required conditions is in `internal/conditions/required.go`. Optional conditions (e.g. `ObservabilityReady` when `prometheusOperator=false`) are omitted automatically.

## Events

Events are the human-readable trail of what just happened. They show up in `kubectl describe openchamicontrolplane …` and in the Kubernetes events stream.

### Convention (invariant 9)

```go
helpers.RecordConditionEvent(
    r.Recorder,
    cluster,
    corev1.EventTypeWarning,         // or Normal
    conditions.ReasonVaultUnreachable,
    "vault dial: connection refused")
```

`RecordConditionEvent` automatically:
- Looks up the runbook URL for the reason (`https://openchami.org/docs/ops/vault-unreachable`).
- Appends it to the message.
- Emits the Event with `r.Recorder`.

**Never** call `r.Recorder.Event(...)` directly. `make validate-invariants` rejects it.

## Metrics

Prometheus metrics are emitted from `internal/status/metrics.go`. The metric set:

| Metric | Type | Labels |
|---|---|---|
| `openchami_cluster_phase` | Gauge | `cluster`, `phase` (one-hot) |
| `openchami_cluster_condition` | Gauge | `cluster`, `type`, `reason`, `status` |
| `openchami_cluster_reconcile_duration_seconds` | Histogram | `cluster`, `reconciler` |
| `openchami_cluster_reconcile_total` | Counter | `cluster`, `result` (`success` | `error`) |
| `openchami_cluster_cert_expiry_seconds` | Gauge | `cluster`, `secret` (Unix timestamp) |
| `openchami_cluster_managed_by_version` | Gauge | `cluster`, `version` (one-hot) |

Scraped via the controller's `/metrics` endpoint (default `:8080`). The `ServiceMonitor` reconciler emits a Prometheus-Operator scrape config when `spec.observability.prometheusOperator=true`.

## Structured logs (invariant 8)

Every log line carries `cluster`, `reconciler`, and (where applicable) `resource` keys.

```go
log := logging.Enrich(ctx, cluster, "vault")
log.Info("seeding KV path", "path", "openchami/foo/db/credentials")
```

Output (`--zap-encoder=json`):
```json
{"level":"info","ts":"…","logger":"controllers.openchamicontrolplane","msg":"seeding KV path","cluster":"foo","reconciler":"vault","path":"openchami/foo/db/credentials"}
```

For per-resource lines, prefer `EnrichWithResource`:
```go
dsLog := logging.EnrichWithResource(ctx, cluster, "coredhcp", "DaemonSet/openchami-foo/coredhcp")
dsLog.Info("applying coredhcp DaemonSet")
```

**Never** call `log.FromContext(ctx)` directly in sub-reconcilers. `make validate-invariants` rejects it.

## How to debug a stuck cluster

1. `kubectl get openchamicontrolplane <name> -o yaml | yq '.status'` — what's the phase, what's the latest condition, what's the message?
2. `kubectl describe openchamicontrolplane <name>` — recent Events, with their runbook URLs.
3. Open the runbook URL.
4. If Events don't tell the story: check the operator log for the `cluster=<name>` lines (`kubectl logs -n <op-ns> deploy/openchami-operator-controller-manager | grep cluster=foo`).
5. For deeper traces, run the operator locally with `make dev-run` and `--zap-log-level=debug`.

## Common log query recipes

```bash
# All errors for a specific cluster
kubectl logs -n openchami-system deploy/openchami-operator | jq 'select(.cluster == "foo" and .level == "error")'

# Slow reconciles (any sub-reconciler taking > 5 s)
# (only available when reconcile_duration_seconds Histogram is queried in Prometheus)

# Recent restarts of the controller manager
kubectl get events -n openchami-system --field-selector reason=BackOff
```
