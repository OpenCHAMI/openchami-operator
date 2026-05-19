# Contributing

How to add a sub-reconciler, a CRD field, a webhook check, or a runbook reason — without breaking the invariants.

## Read first

- [`CLAUDE.md`](../CLAUDE.md) — the source of truth for invariants and project rules.
- [`AGENTS.md`](../AGENTS.md) — sub-agent collaboration rules. Read before spawning parallel work.
- [invariants.md](invariants.md) — human-readable form of the 10 absolute rules.

If your change would violate any invariant, stop and either rework or open a discussion.

## Adding a sub-reconciler

1. **Decide where it sits in the reconcile order.** The order is in `internal/controller/openchamicontrolplane_controller.go::reconcileAll`. Insert your reconciler at the position that respects its dependencies.
2. **Pick a condition type.** Add it to `internal/conditions/types.go` (`ConditionFooReady`).
3. **Pick reason constants.** Add them to `internal/conditions/reasons.go`. Each one needs a runbook page; see [runbook-conventions](runbook-conventions.md).
4. **Create `internal/reconcilers/foo.go`:**
   ```go
   type FooReconciler struct {
       Client   client.Client
       Recorder record.EventRecorder
       // ... extra deps (vault, s3, etc.) as needed
   }

   func (r *FooReconciler) Reconcile(ctx context.Context, cluster *v1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
       log := logging.Enrich(ctx, cluster, "foo")  // INVARIANT 8

       // Skip if disabled
       if !cluster.Spec.Services.Foo.Enabled {
           log.Info("foo disabled, skipping")
           return ctrl.Result{}, nil
       }

       // Apply resources via server-side apply (INVARIANT 5)
       obj := r.buildFoo(cluster)
       if err := r.Client.Patch(ctx, obj, client.Apply, client.FieldOwner("openchami-operator"), client.ForceOwnership); err != nil {
           apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
               Type:    conditions.ConditionFooReady,
               Status:  metav1.ConditionFalse,
               Reason:  conditions.ReasonFooApplyFailed,
               Message: err.Error(),
               ObservedGeneration: cluster.Generation,
           })
           helpers.RecordConditionEvent(r.Recorder, cluster, corev1.EventTypeWarning,
               conditions.ReasonFooApplyFailed, err.Error())   // INVARIANT 9
           return ctrl.Result{}, fmt.Errorf("applying Foo: %w", err)
       }

       // Read status
       // ... aggregate readiness ...

       apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
           Type:    conditions.ConditionFooReady,
           Status:  metav1.ConditionTrue,
           Reason:  conditions.ReasonFooReady,
           Message: "Foo is ready",
           ObservedGeneration: cluster.Generation,
       })
       return ctrl.Result{}, nil
   }

   func (r *FooReconciler) Describe(cluster *v1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
       // No I/O. Just build the would-be objects.
       return []client.Object{r.buildFoo(cluster)}, nil
   }
   ```
5. **Wire it into `reconcileAll`.** Add the new reconciler to the `subs` slice at the right position.
6. **Write tests in `internal/reconcilers/foo_test.go`.** Cover: enabled, disabled, apply error, status patch on success, status patch on failure.
7. **Update [reconcilers.md](reconcilers.md)** with the new section.
8. **Run the gauntlet:**
   ```sh
   make generate manifests fmt vet lint test validate-invariants
   ```
   All four must pass.

## Adding a CRD field

1. **Edit `api/v1alpha1/openchamicontrolplane_types.go`.** Add the field with `+kubebuilder:` markers for validation, defaulting, and required-ness.
2. **Run `make generate manifests`.** This regenerates `zz_generated.deepcopy.go` and `config/crd/bases/...yaml`. Commit both.
3. **Update defaulting in `api/v1alpha1/openchamicontrolplane_webhook.go::Default`** if the field needs a default value. (Prefer `+kubebuilder:default=` markers when possible — they live with the type.)
4. **Update validation in the same file's `ValidateCreate`/`ValidateUpdate`** if the field needs cross-field checks (the markers handle most simple cases).
5. **Add unit tests in `api/v1alpha1/openchamicontrolplane_webhook_test.go`.** Cover defaulting, validation accept, validation reject.
6. **Update consuming reconcilers.** Read the field where it's needed.
7. **Update [crd-reference.md](crd-reference.md).** Add the field, its type, its default.
8. **If the field requires a CRD schema migration:**
   - You almost never need a new API version for additive changes — add the field as `omitempty` and v1alpha1 keeps working.
   - For breaking changes, see [upgrade-and-versioning](upgrade-and-versioning.md).

## Adding a webhook check

1. **Decide if it's a defaulting or validating concern.**
   - Defaulting fills gaps; validating rejects invalid configurations.
2. **Edit `api/v1alpha1/openchamicontrolplane_webhook.go`.** Add the check to `Default()` or `ValidateCreate()`/`ValidateUpdate()`.
3. **Add unit tests** in `openchamicontrolplane_webhook_test.go`. Cover the full grid: missing field, valid field, invalid field, immutable-update attempt.
4. **Update [webhooks.md](webhooks.md).**
5. **Run the gauntlet.**

If the check enforces an invariant, also extend `make validate-invariants` if it's something a grep can catch.

## Adding a runbook reason

See [runbook-conventions](runbook-conventions.md). One-line summary: add the constant, write the runbook page in the openchami.org docs site, use it via `helpers.RecordConditionEvent`. **Never** call `recorder.Event` directly.

## Adding a default service image

1. **Edit the constant in `internal/reconcilers/<service>.go`** (e.g. `defaultSMDImage`).
2. **Update [SERVICES.md](../SERVICES.md)** in the same PR.
3. **Update [UPGRADE.md](../UPGRADE.md)** if this is a release commit.

`latest` is OK during development; release PRs must pin a tag. CI should reject `latest` in default constants for tagged releases.

## Don't

- **Don't ever call `client.Create` followed by `client.Update`.** Use `client.Apply`. (Invariant 5.)
- **Don't ever return from `Reconcile` without patching status.** (Invariant 6.)
- **Don't ever call `log.FromContext` directly.** Use `logging.Enrich`. (Invariant 8.)
- **Don't ever call `recorder.Event` directly.** Use `helpers.RecordConditionEvent`. (Invariant 9.)
- **Don't add HPC domain knowledge.** (Invariant 10.) Refresh on the [quadlet test](invariants.md#10-the-operator-has-no-hpc-domain-knowledge).
- **Don't introduce a new sub-reconciler that crosses the cluster boundary.** Per-cluster namespace isolation is non-negotiable. (Invariant 2.)

## PR template

A good PR includes:
1. **What changed.** One-paragraph description.
2. **Why.** Link to the issue, the user request, or the design discussion.
3. **CRD/API impact.** Yes/no. If yes, which version(s)?
4. **Migration impact.** Yes/no. If yes, what should operators do?
5. **Test coverage.** Which of unit / envtest / e2e / [integration-sandbox](https://github.com/OpenCHAMI/integration-sandbox) runs cover this?
6. **Phase reference (if applicable).** "Implements `docs/phases/phase-NN-*.md` step X."
