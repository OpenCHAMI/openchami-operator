# Runbook conventions

Invariant 9: every operator Event includes a runbook URL.

## Schema

```
https://openchami.org/docs/ops/{reason-in-kebab-case}
```

`{reason-in-kebab-case}` is the lowercase, kebab-cased version of the reason constant in `internal/conditions/reasons.go`.

| Reason constant | Slug | URL |
|---|---|---|
| `ReasonVaultUnreachable` | `vault-unreachable` | https://openchami.org/docs/ops/vault-unreachable |
| `ReasonS3Unreachable` | `s3-unreachable` | https://openchami.org/docs/ops/s3-unreachable |
| `ReasonCoreDHCPNodeDrain` | `coredhcp-node-drain` | https://openchami.org/docs/ops/coredhcp-node-drain |
| `ReasonVersionPinned` | `version-pinned` | https://openchami.org/docs/ops/version-pinned |
| `ReasonCertExpiringSoon` | `cert-expiring-soon` | https://openchami.org/docs/ops/cert-expiring-soon |
| `ReasonNetworkProbeFailed` | `network-probe-failed` | https://openchami.org/docs/ops/network-probe-failed |
| `ReasonDatabaseBootstrapFailed` | `database-bootstrap-failed` | https://openchami.org/docs/ops/database-bootstrap-failed |

The full list lives in `internal/conditions/reasons.go`. `helpers.RunbookURL` does the slug conversion.

## Emitting an Event with a runbook URL

```go
helpers.RecordConditionEvent(
    r.Recorder,
    cluster,
    corev1.EventTypeWarning,
    conditions.ReasonVaultUnreachable,
    "vault dial: connection refused")
```

The runbook URL is appended to the message automatically. Output in `kubectl describe`:
```
Warning  VaultUnreachable  openchamicluster/foo  vault dial: connection refused (runbook: https://openchami.org/docs/ops/vault-unreachable)
```

**Never** call `r.Recorder.Event(...)` directly. The validate-invariants check rejects it.

## Adding a new reason

1. Add the constant to `internal/conditions/reasons.go`:
   ```go
   ReasonFooBar = "FooBar"
   ```
2. Add the kebab-case mapping if it isn't algorithmic (most aren't — `helpers.RunbookURL` lowercases and inserts hyphens before each capital).
3. Write the runbook page at `docs/ops/foo-bar.md` in the openchami.org documentation repository:
   - **Symptom:** What does the operator/cluster look like when this fires?
   - **Likely causes:** A short list, ordered by frequency.
   - **Diagnosis:** Commands to run.
   - **Remediation:** Steps to fix.
   - **Escalation:** When to page someone.
4. Use the constant from your sub-reconciler:
   ```go
   helpers.RecordConditionEvent(r.Recorder, cluster, corev1.EventTypeWarning,
       conditions.ReasonFooBar, "specific actionable detail here")
   ```
5. `make validate-invariants` will fail if the runbook page doesn't exist (when CI has access to the docs repo). Local dev skips this check.

## Rules of thumb

- **One reason per failure mode.** If two failures need different remediations, give them different reasons.
- **Never reuse a generic `Reason=Failed`.** Every reason is specific.
- **Keep the message actionable.** "vault dial: connection refused" is useful; "internal error" is not.
- **Don't put credentials, secrets, or stack traces in messages.** Events are world-readable to anyone with `get events` permission.
- **Lifetime of an Event is bounded** (default ~1 hour in many distributions). Don't rely on Events for long-term state — that's what Conditions are for.

## When to fire `EventTypeNormal` vs `EventTypeWarning`

- **Normal:** the reconciler made progress. "namespace created", "DaemonSet ready", "version pin matches".
- **Warning:** something needs attention but the operator is handling it. "vault unreachable, retrying", "cert expiring in 14 days".

Use `Warning` when the user might need to act soon. Use `Normal` for routine progress.

## Anti-patterns

- **Spammy success events.** A successful reconcile that already ran 30 seconds ago should not produce a new Event. Gate `Normal` events on "the condition transitioned to True" rather than "I just observed True".
- **Cryptic abbreviations.** `Reason=ENOSMD` saves three characters and costs ten minutes of a stranger's time. Spell it out.
- **Reason mismatched to runbook.** If the runbook says "rotate Vault token", the reason must be `VaultTokenRotationNeeded` and not `VaultUnreachable`.
