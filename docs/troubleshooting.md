# Troubleshooting

Common reconcile failures and what to check. Newest at the top; the [bugs.md](../bugs.md) file has additional known issues.

## "Cluster stays in Provisioning indefinitely"

**Cause 1:** A required prereq CRD is missing.
```sh
kubectl get crd | grep -E 'cert-manager|cnpg|vault.hashicorp|gateway.networking|gateway.envoyproxy'
```
If any are missing, install them — see [external-dependencies](external-dependencies.md).

**Cause 2:** Vault is unreachable.
```sh
kubectl get openchamicluster <name> -o jsonpath='{.status.conditions[?(@.type=="VaultReady")]}' | jq
```
Look for `Reason: VaultUnreachable`. The Event has the runbook URL.

**Cause 3:** The operator pod isn't actually running.
```sh
kubectl get pods -n openchami-system
kubectl describe pod -n openchami-system -l app.kubernetes.io/name=openchami-operator
```

**Cause 4:** A DependsOn-earlier reconciler is stuck. Walk the conditions in reconcile order ([reconcilers.md](reconcilers.md)) — the first one that's `False` is the cause.

## "phase=Degraded"

This is recoverable by definition; the operator is requeueing. Check the most recent conditions:
```sh
kubectl get openchamicluster <name> -o yaml | yq '.status.conditions'
```
The condition with `Status: False` and a `Reason: *Unreachable` is the proximate cause. Open the runbook URL in its Event.

If `phase=Degraded` lasts more than 5 minutes without the underlying condition resolving, the issue is durable, not transient. Treat it as `Failed` for triage purposes.

## "phase=Failed"

The operator hit a fatal, unrecoverable condition. Look for:
```sh
kubectl describe openchamicluster <name> | grep -A 5 -i 'failed\|fatal'
```

Common causes:
- **CRD storage version mismatch.** The cluster was last reconciled by an operator at a version whose storage encoding the current operator can't read. Run `hack/migrate-storage-version.sh`.
- **Webhook unavailable during create.** The validating webhook rejected the apply. Check the most recent kubectl-apply error message.
- **Vault path collision.** Another `OpenCHAMICluster` already owns the prefix `openchami/{clusterName}/`. Pick a different `clusterName` or delete the conflicting cluster first.

## "Webhook timed out"

`kubectl apply` returns `Internal error occurred: failed calling webhook "vopenchamicluster.kb.io": ... context deadline exceeded`.

**Cause:** the webhook Service can't reach the operator. Common reasons:
- Operator pod not Running.
- cert-manager hasn't issued the webhook cert yet (look for the `Certificate` resource in the operator namespace).
- Service-to-pod network policy blocks the apiserver-to-operator path.

```sh
kubectl get certificates -n openchami-system
kubectl get pods -n openchami-system -l app.kubernetes.io/name=openchami-operator -o wide
kubectl get svc -n openchami-system openchami-operator-webhook-service
```

The operator's webhook tutorial in `config/webhook/` has the canonical wiring; if you've forked it, diff against upstream.

## "VaultStaticSecret never syncs"

Symptoms: the per-cluster Secret never appears in the cluster namespace; service Pods fail with "secret not found".

**Cause 1:** Vault Secrets Operator isn't installed.
```sh
kubectl get crd | grep vaultstaticsecret
```

**Cause 2:** The VSO `VaultConnection` or `VaultAuth` is misconfigured for this cluster. The operator emits these per cluster; check:
```sh
kubectl get vaultconnection -n openchami-<name>
kubectl get vaultauth -n openchami-<name>
kubectl describe vaultstaticsecret -n openchami-<name>
```

**Cause 3:** Vault doesn't have the secret yet. The Vault reconciler writes secrets *to* Vault; if the seed (e.g. `seed-vault.sh` in dev) hasn't run, the path is empty. Check `vault kv get openchami/<name>/...` from a Vault-CLI capable pod.

## "Operator log spams `decoded object generation differs`"

The operator is racing itself — a stale cached object is being patched. Usually self-resolves within a couple of reconciles.

If it persists for more than ~30 seconds, check:
- That you're not running two instances of the operator (the leader-election Lease should pin a single leader).
- That something else isn't writing to the same fields concurrently (a human kubectl edit, another controller).

## "Cluster created but services have wrong image"

```sh
kubectl get deploy -n openchami-<name> -o jsonpath='{range .items[*]}{.metadata.name}{"  "}{.spec.template.spec.containers[*].image}{"\n"}{end}'
```

The image is the first of:
1. `cluster.spec.services.<name>.image.{repository,tag}` if set.
2. The default constant in `internal/reconcilers/<name>.go` (`defaultSMDImage`, etc.). See [SERVICES.md](../SERVICES.md).

If the image is wrong, the constant is wrong (a release shipped without updating it) or the spec override is shadowing the default.

## "kubectl describe shows old conditions even after fix"

Conditions persist until they're reset to `Unknown` or to a different `Reason`. The operator only writes a condition when its reconciler runs and decides on the new state. If you've fixed an issue but haven't seen `Status: True` flip back, force a re-reconcile:

```sh
kubectl annotate openchamicluster <name> openchami.org/touch=$(date +%s) --overwrite
```

(The annotation is arbitrary; any spec change triggers a reconcile.)

## "I get an Event with no runbook URL"

The reconciler called `r.Recorder.Event(...)` directly instead of `helpers.RecordConditionEvent(...)`. That's an invariant 9 violation. File the line in [bugs.md](../bugs.md) and fix it; `make validate-invariants` should have caught it but might have a gap.

## "envtest fails with `cannot find binary etcd`"

Run `make install-tools`. setup-envtest provisions the Kubernetes control-plane binaries into `testbin/`.

## "make e2e fails immediately on Vault dial"

`make dev-up` didn't finish or failed. Check the dev compose stack:
```sh
docker compose -f hack/local-dev/docker-compose.yaml ps
docker logs openchami-vault-dev | tail -30
```

If Vault is up but unreachable from inside the kind cluster, it's a docker-network issue — kind and the local-dev compose stack must share the docker bridge. `make dev-down && make dev-up` is the canonical reset.

## When to file a bug

If you've isolated a reproducible failure that:
- Happens against the canonical `test/fixtures/minimal-cluster.yaml`,
- Doesn't trace to a missing prereq or environmental issue,
- Hits the same condition reason on every retry,

then it belongs in [bugs.md](../bugs.md). Include: branch + SHA, exact reproduction steps, the failing condition with full message, the operator log line that fired the condition, and what you expected to happen.

## Production failure modes

Failure scenarios surfaced by audit on 2026-05-04 that `make e2e` does not cover end-to-end. Each entry documents the symptom, the operator's intended behaviour, and the recovery path.

### Vault token expires mid-reconcile

**Symptom:** Sub-reconciler returns `403 permission denied`; `VaultConfigured` flips to `False` with `Reason=Error` and a message containing "permission denied" or "token expired."

**Operator behaviour:** `internal/vault/client_vault.go` re-authenticates on the next reconcile pass when the configured auth method is AppRole or Kubernetes. The client does **not** transparently refresh mid-call — the sub-reconciler that hit the expired token returns an error, the controller requeues, and the next pass re-auths and retries. Expect one user-visible `Reconciling` blip per token TTL window (default 15 m).

**Recovery:** None required if AppRole credentials are still valid. If the AppRole secret_id has itself expired, rotate it: write a new secret_id into the cluster's referenced Secret (`spec.platform.vault.appRoleSecretRef.name`) and the next reconcile picks it up.

### Vault sealed (HTTP 503)

**Symptom:** `VaultConfigured=False`, `Reason=Unreachable`, message includes `503` or `vault is sealed`.

**Operator behaviour:** Honours invariant 1 — never crashes, always requeues. Sub-reconcilers that depend on Vault (`Database`, `Tokensmith`, `Boot`, `Metadata`, `Funicular`) do not run while `VaultConfigured=False`, so cluster Phase stays at `Provisioning` or `Degraded` (whichever it was last) until Vault is unsealed.

**Recovery:** Unseal Vault. The operator detects the change at the next requeue (default 30s).

### Vault Secrets Operator (VSO) CRDs missing

**Symptom:** `VaultConfigured=False`, message contains `no matches for kind "VaultStaticSecret"`.

**Operator behaviour:** `cmd/operator/main.go` checks for VSO CRDs at startup and warns but does not fail. Sub-reconcilers that try to apply VSS resources will return errors; conditions reflect that.

**Recovery:** Install VSO. The operator does not deploy it (invariant 1 — Vault and VSO are external).

### cert-manager missing

**Symptom:** `CertificatesValid` condition stays `Unknown`; `Gateway` does not get a TLS Secret materialised.

**Operator behaviour:** The certificates reconciler skips when cert-manager CRDs aren't present. The cluster reaches `Ready` only if `spec.networking.tls.issuer` is empty (i.e., TLS is opt-out for that cluster) or if a Secret is pre-staged.

**Recovery:** Install cert-manager, then either delete the existing `Certificate` resource (cert-manager will reconcile) or wait for the next operator reconcile pass.

### Webhook unavailable on first apply

**Symptom:** `kubectl apply -f cluster.yaml` returns `Internal error occurred: failed calling webhook ... no endpoints available for service`.

**Operator behaviour:** This happens during operator rollout — the operator pod is starting and has not yet completed cert-manager-managed webhook certificate issuance. The webhook will be available within ~30s of operator pod readiness.

**Recovery:** Retry the apply after the operator pod logs `starting webhook server`. CI pipelines should poll for `kubectl get deployment openchami-operator -o jsonpath='{.status.readyReplicas}'` ≥ 1 before applying cluster CRs.

### Database post-init job fails

**Symptom:** `DatabaseReady=False`, `Reason=Error`, message references the failed Job. A Warning Event is emitted.

**Operator behaviour:** Once a Job lands in `Status.Failed > 0`, the operator does **not** delete it — Kubernetes only retries Jobs up to `backoffLimit`. The operator surfaces the failure via the condition and Event but leaves the Job in place for forensics.

**Recovery:**
```sh
kubectl logs -n openchami-<cluster> job/openchami-<cluster>-db-init
kubectl delete -n openchami-<cluster> job/openchami-<cluster>-db-init
```
Re-deletion + reconcile recreates the Job. (If this fails repeatedly with the same logs, the cluster's database credentials Secret may be stale — refresh from Vault.)

### CNPG primary failover during reconcile

**Symptom:** `DatabaseReady` flips between `True` and `False/Provisioning` for ~30s while CNPG promotes a replica.

**Operator behaviour:** Each reconcile reads `cnpg.Status.Phase`; only `PhaseHealthy` flips Ready=True. Phase `PhaseSwitchover` and `PhaseFailover` map to `Provisioning`. There is no operator action required.

**Recovery:** None — the failover is transparent to OpenCHAMI services that read the same Service endpoint, and Ready=True returns once CNPG settles.

### N-1 service version skew during upgrade

**Symptom:** A subset of services run image version N+1 while their peers and the operator's defaults are still on N. Cluster Phase stays `Degraded` even though all pods are Ready.

**Operator behaviour:** No automatic gate — the operator applies images per-cluster from `spec.services.<name>.image` (preferred) or the default constants. There is no compatibility matrix.

**Recovery:** Either pin the operator with `spec.operatorChannel: pinned` + `spec.pinnedVersion` (so upgrades are explicit) or upgrade all services in lockstep. See [upgrade-and-versioning.md](upgrade-and-versioning.md).

### Per-cluster namespace deletion finalizer order

**Symptom:** Namespace `openchami-<cluster>` stays in `Terminating` after `kubectl delete openchamicluster <cluster>`.

**Operator behaviour:** The cluster's namespace is owned by the operator. Resources inside (Deployments, Services, NetworkPolicies, ServiceMonitor, Gateway, HTTPRoute, VaultStaticSecret, CNPG Cluster) are GC'd via owner references. CNPG Cluster deletion can take 60–120s due to its own teardown.

**Recovery:** Wait. If a finalizer is wedged, find the offending resource:
```sh
kubectl api-resources --verbs=list -n openchami-<cluster> -o name | xargs -n1 -I{} kubectl get {} -n openchami-<cluster> --ignore-not-found
```
Manual finalizer removal is a last resort; prefer fixing the upstream controller.
