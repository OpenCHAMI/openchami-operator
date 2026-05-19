# Phase 10 — Admission Webhooks

**File:** `api/v1alpha1/openchamicontrolplane_webhook.go`

## Defaulting (implement Default())
```go
// Set these if zero/empty:
spec.platform.objectStorage.bucket     = clusterName + "-boot-images"
spec.logging.logBucket                 = clusterName + "-logs"
spec.networking.gatewayClass           = "envoy"
spec.networking.tls.issuer             = "vault-pki-issuer"
spec.database.instances                = 3
spec.networkProbe.intervalSeconds      = 300
spec.services.*.replicas               = 2 (except tokensmith = 1)
spec.services.coreDHCP.unknownLeaseDuration = "5m"
spec.services.coreDHCP.knownLeaseDuration   = "1h"
spec.services.magellan.schedule        = "*/30 * * * *"
spec.services.magellan.concurrencyPolicy = Forbid
spec.logging.retentionDays             = 90
spec.logging.flushIntervalSeconds      = 60
```

## Validation (implement ValidateCreate())

**Always validate:**
1. `spec.platform.vault.address` starts with `https://`
2. `authMethod=appRole` requires `appRoleSecretRef` non-nil
3. `oidcProvider=external` requires `oidcIssuerURL` non-empty
4. `spec.clusterName` is unique across all OpenCHAMIControlPlane resources (list all, compare)

**When `networkProbe.enabled=false`:**
5. `coreDHCP.nodeSelector` must be non-empty (required for scheduling)
6. `coreDHCP.nodeSelector` must contain at least one key whose value
   contains the clusterName (discriminating label requirement)
7. No other OpenCHAMIControlPlane has an identical `coreDHCP.nodeSelector` (list all, compare)

**When `networkProbe.enabled=true`:**
8. If `coreDHCP.enabled=true`: `networkProbe.provisionNetwork` must be non-nil
9. If `magellan.enabled=true`: `networkProbe.bmcNetwork` must be non-nil
10. If `coreDHCP.nodeSelector` is set: emit a warning (it will be ignored)

## ValidateUpdate()
Re-run create validations for mutable fields, plus:
- `spec.clusterName` is immutable → `field.Forbidden`

## Conversion hub
`api/v1alpha1/openchamicontrolplane_conversion.go` should already have `Hub()`.
Verify it's present and compiles.

```bash
tools/check-phase.sh 10
```
