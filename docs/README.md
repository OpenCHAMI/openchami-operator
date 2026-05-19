# openchami-operator docs

The complete documentation tree for the operator.

Read in order if you've never touched the codebase:
1. [Quickstart](quickstart.md) — local dev cluster, applying a CR, watching it reconcile.
2. [Architecture](architecture.md) — controller, sub-reconciler pattern, reconcile order.
3. [Sub-reconcilers](reconcilers.md) — one section per sub-reconciler: what it owns, conditions it sets, dependencies.
4. [CRD reference](crd-reference.md) — every `OpenCHAMIControlPlane` spec and status field.
5. [Invariants](invariants.md) — the 10 absolute rules. Violating any is a bug, regardless of what else works.
6. [External dependencies](external-dependencies.md) — what the operator expects to find in the cluster.
7. [Webhooks](webhooks.md) — defaulting, validation, conversion.
8. [Observability](observability.md) — conditions, events, metrics, log shape.
9. [Runbook conventions](runbook-conventions.md) — every event includes a runbook URL; here's the schema.
10. [Upgrade and versioning](upgrade-and-versioning.md) — `operatorChannel`, pinning, storage-version migration.
11. [Testing](testing.md) — envtest, e2e, the integration sandbox.
12. [Dev loop](dev-loop.md) — every make target, kind config, dry-run mode.
13. [Contributing](contributing.md) — adding a sub-reconciler / CRD field / webhook check.
14. [Troubleshooting](troubleshooting.md) — common reconcile failure modes with fixes.
15. [Relationship to integration-sandbox](relationship-to-integration-sandbox.md) — what each suite does and doesn't cover.

Reference (skim, don't read):
- [Phase docs](phases/) — original implementation roadmap. Each phase is "done" or "in progress"; this is historical context, not a task list.
- [Type stubs](types.md) — Go-form CRD types with kubebuilder markers (auto-extracted).
- [SERVICES.md](../SERVICES.md) — pinned default service images for the current release.
- [UPGRADE.md](../UPGRADE.md) — release-to-release upgrade notes.
- [bugs.md](../bugs.md) — known issues, severity-tagged.
- [CLAUDE.md](../CLAUDE.md), [AGENTS.md](../AGENTS.md) — agent-specific guidance.
