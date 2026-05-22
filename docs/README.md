# openchami-operator docs

The complete documentation tree for the operator.

**Installing for the first time?**
- [Quickstart](quickstart.md) — local dev cluster in under 15 minutes.
- [Install (production)](install-production.md) — end-to-end walkthrough for a fresh non-dev Kubernetes cluster.
- [ochami-admin CLI](cli.md) — user-facing reference for `init` / `describe` / `backup` / `restore` / `logs`.

**Reading the codebase top-to-bottom:**
1. [Quickstart](quickstart.md) — local dev cluster, applying a CR, watching it reconcile.
2. [Architecture](architecture.md) — controller, sub-reconciler pattern, reconcile order.
3. [Sub-reconcilers](reconcilers.md) — one section per sub-reconciler: what it owns, conditions it sets, dependencies.
4. [CRD reference](crd-reference.md) — every `OpenCHAMIControlPlane` spec and status field.
5. [Invariants](invariants.md) — the 10 absolute rules. Violating any is a bug, regardless of what else works.
6. [External dependencies](external-dependencies.md) — what the operator expects to find in the cluster.
7. [Install (production)](install-production.md) — end-to-end install walkthrough for a real cluster.
8. [ochami-admin CLI](cli.md) — companion CLI reference.
9. [Webhooks](webhooks.md) — defaulting, validation, conversion.
10. [Observability](observability.md) — conditions, events, metrics, log shape.
11. [Runbook conventions](runbook-conventions.md) — every event includes a runbook URL; here's the schema.
12. [Upgrade and versioning](upgrade-and-versioning.md) — `operatorChannel`, pinning, storage-version migration.
13. [Testing](testing.md) — envtest, e2e, the integration sandbox.
14. [Dev loop](dev-loop.md) — every make target, kind config, dry-run mode.
15. [Contributing](contributing.md) — adding a sub-reconciler / CRD field / webhook check.
16. [Troubleshooting](troubleshooting.md) — common reconcile failure modes with fixes.
17. [Relationship to integration-sandbox](relationship-to-integration-sandbox.md) — what each suite does and doesn't cover.

Reference (skim, don't read):
- [Phase docs](phases/) — original implementation roadmap. Each phase is "done" or "in progress"; this is historical context, not a task list.
- [Type stubs](types.md) — Go-form CRD types with kubebuilder markers (auto-extracted).
- [SERVICES.md](../SERVICES.md) — pinned default service images for the current release.
- [UPGRADE.md](../UPGRADE.md) — release-to-release upgrade notes.
- [bugs.md](../bugs.md) — known issues, severity-tagged.
- [CLAUDE.md](../CLAUDE.md), [AGENTS.md](../AGENTS.md) — agent-specific guidance.
