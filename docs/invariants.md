# Absolute invariants

Ten rules. Violating any one is a bug, regardless of what else works. Lifted directly from `CLAUDE.md` and translated for human reading. The original wording is the source of truth — when in doubt, read CLAUDE.md.

`make validate-invariants` greps the source for the most common violations. CI runs it before tests.

## 1. Vault and VersityGW are external

The operator **configures** Vault and VersityGW; it never **deploys** them.

- If Vault is unreachable, requeue with backoff. Set `VaultReady=False` with reason `VaultUnreachable`.
- If VersityGW is unreachable, requeue with backoff. Set `BucketReady=False` with reason `S3Unreachable`.
- **Never crash.** A panic on missing Vault is an invariant violation.

Code paths: `internal/vault/`, `internal/s3/`, `internal/reconcilers/vault.go`, `internal/reconcilers/bucket.go`.

## 2. Per-cluster namespace isolation

Every resource for `OpenCHAMIControlPlane foo` lives in `openchami-foo`.

- Nothing leaks across cluster boundaries.
- The Namespace reconciler is the **only** place that creates the namespace.
- Cross-cluster Secrets, ConfigMaps, etc. are forbidden.

Enforced by: the watch predicate in `internal/controller/openchamicontrolplane_controller.go`'s `SetupWithManager` plus per-resource `client.InNamespace(...)` filters.

## 3. Vault path isolation

All KV paths are prefixed `openchami/{clusterName}/`.

- The Vault reconciler validates uniqueness before any write.
- Two clusters cannot share a path.
- The validating webhook also enforces uniqueness at admission time.

Code paths: `internal/reconcilers/vault.go::validatePathUniqueness`, `api/v1alpha1/openchamicontrolplane_webhook.go::ValidateCreate`.

## 4. DHCP node exclusivity

When network probing is disabled, no two `OpenCHAMIControlPlane` instances may target the same Kubernetes node for CoreDHCP.

- The validating webhook enforces this at admission.
- When network probing is enabled, exclusivity is implicit (the probe labels are per-cluster).

Code path: `api/v1alpha1/openchamicontrolplane_webhook.go`. Look for `ValidateCoreDHCPExclusivity`.

## 5. Idempotent reconciliation

Every `Reconcile` function produces identical results when called twice.

- **Server-side apply everywhere.** `client.Apply` only.
- **Never** `client.Create` followed by `client.Update`. That sequence is non-idempotent.
- Mutations in `Reconcile` must be order-independent.

Enforced by: `make validate-invariants` greps for `client.Create` and warns. Acceptable uses (e.g. SubjectAccessReview) are explicitly allowlisted.

## 6. Status is always written last

Never return from `Reconcile` without patching `.status.conditions`.

- Use `r.Status().Patch(ctx, cluster, client.MergeFrom(orig))` — never `r.Status().Update(...)`.
- The status patch is the very last thing the controller does, regardless of `reconcileErr`.
- If both `reconcileErr` and the patch error, both are joined.

Code path: end of `OpenCHAMIControlPlaneReconciler.Reconcile` in `internal/controller/openchamicontrolplane_controller.go`.

## 7. No secrets in the CRD spec

The spec references secrets by name only. Never contains credential values.

- `VaultSpec.AppRoleSecretRef` and `CABundleSecretRef` are `*corev1.LocalObjectReference` — name-only.
- `ObjectStorageSpec` does not contain access keys; those live in Vault.
- No `password`, `token`, `apiKey` field anywhere in the spec.

Enforced by: code review + the OpenAPI schema review at every controller-gen run.

## 8. Structured logging on every line

Every `log.Info`/`log.Error` carries `cluster`, `reconciler`, and `resource` key-value pairs.

- Use `internal/logging/logger.go` helpers exclusively:
  - `logging.Enrich(ctx, cluster, reconciler) logr.Logger`
  - `logging.EnrichWithResource(ctx, cluster, reconciler, resource) logr.Logger`
- **Never** call `log.FromContext(ctx)` directly in sub-reconcilers.
- The top of every `Reconcile` must call `logging.Enrich`.

Enforced by: `make validate-invariants` greps for `log.FromContext`.

## 9. Every operator Event includes a runbook URL

Format: `https://openchami.org/docs/ops/{reason-in-kebab-case}`.

- Use `helpers.RecordConditionEvent` (in `internal/reconcilers/helpers.go`).
- **Never** call `recorder.Event(...)` directly in sub-reconcilers.
- The runbook URL is appended to the Event message automatically.

The list of reason→URL mappings is in `internal/conditions/`. Adding a new condition reason means committing a runbook page (see [runbook-conventions](runbook-conventions.md)).

Enforced by: `make validate-invariants` greps for direct `recorder.Event` calls.

## 10. The operator has no HPC domain knowledge

The operator does not know about:
- compute nodes, xnames, IPMI addresses
- boot parameters, DHCP lease assignments
- firmware states, scheduler queues

A deployment managed by this operator must be **indistinguishable to an HPC admin** from one running under quadlet or docker-compose.

### The quadlet test

Before adding any feature, ask:
> "Would this concept exist if the services were started as systemd units with no Kubernetes operator?"

- **Yes** → the feature belongs in the services. Don't add it here.
- **No** → it may belong in the operator.

Examples that have come up and been rejected:
- "An xname-to-IP map field on the CR" — belongs in SMD.
- "A boot script template" — belongs in boot-service.
- "A firmware version" — belongs in fru-tracker / magellan.

Examples that pass the quadlet test:
- "Where does Vault live?" — quadlet would need this too.
- "Which Kubernetes node hosts CoreDHCP?" — quadlet wouldn't ask, but Kubernetes scheduling does.
- "Should TLS be served by an Envoy Gateway or by the service directly?" — Kubernetes-only concern.

This is the line that divides the operator from the integration-sandbox: the sandbox tests *the services*, the operator tests *the deployment shape*.
