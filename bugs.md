# openchami-operator — bugs

Surfaced by the [OpenCHAMI integration sandbox](https://github.com/OpenCHAMI/integration-sandbox) planning + smoke pass on 2026-05-03,
and follow-up `make dev-up` shakedowns on 2026-05-04. Each entry below
is annotated with the date it was fixed. Kept as a record so a reviewer
can audit the changes against the original symptoms.

## `hack/local-dev/seed-vault.sh` writes Vault paths the operator never reads  *(fixed 2026-05-22)*
- **Where:** `hack/local-dev/seed-vault.sh:38`; `internal/vault/paths.go:60-61`.
- **Observation:** The dev seed script writes credentials to `openchami/<cluster>/db/credentials` (singular path with four keys: `username`, `password`, `SMD_DB_PASSWORD`, `BOOT_SERVICE_DB_PASSWORD`). The vault reconciler reads from `openchami/<cluster>/db/smd` and `openchami/<cluster>/db/boot-service` — two separate per-role paths defined by `VaultPaths.DBSMDCredentials` and `VaultPaths.DBBootServiceCredentials`. The seed paths are silently ignored. The operator papers over the absence by generating random passwords itself (`internal/reconcilers/vault.go:218` calls `randomHex(32)` and `EnsureSecret(..., overwrite=false)`), so the dev cluster still reaches Ready — but the seed script's pre-baked passwords (`test-smd-password-...`) are dead code, and any admin who follows the seed-script shape as a production template will face the same silent skip.
- **Fix:** Rewrote the seed block to mirror `internal/vault/paths.go`'s schema: one `vault kv put` per role path (`$PREFIX/db/smd` and `$PREFIX/db/boot-service`), each carrying `username` + `password` keys matching the `VaultKeyDBUsername` / `VaultKeyDBPassword` constants the vault reconciler reads. Verified post-fix that the operator reuses the seeded passwords instead of generating random ones (path is non-empty → `EnsureSecret(..., false)` short-circuits). The seed script and reconciler now share a single source of truth for path naming, so a future rename surfaces as a test-time conflict instead of silent drift.
- **Severity:** medium — dev convenience only. The operator's auto-generation of random credentials masked the bug; no production deployments were affected because production admins stage their own credentials per `docs/install-production.md §6`.
- **Found:** 2026-05-22 during the docs-audit pass.



## `make dev-up` does not install OpenCHAMI CRDs or start the operator  *(fixed 2026-05-04)*
- **Where:** `Makefile` `dev-up` target; `docs/quickstart.md`; `docs/dev-loop.md`.
- **Observation:** After `make dev-up` the cluster has prereqs (cert-manager, CNPG, VSO, Envoy Gateway) and Vault is seeded — but the `OpenCHAMIControlPlane` CRD is missing and no operator is running. The docs implied the cluster was ready; in practice `kubectl apply -f test/fixtures/minimal-cluster.yaml` either silently stored the CR with empty `.status` or failed with `no matches for kind`.
- **Fix:** Added the standard kubebuilder targets `install` / `uninstall` / `deploy` / `undeploy` / `dev-deploy` (kustomize against `config/crd` and `config/default`). `dev-up` now calls `make install` after the prereqs land. `dev-run` depends on `install`. `quickstart.md` and `dev-loop.md` rewritten with the two paths spelled out (Path A: `dev-run` off-cluster; Path B: `dev-deploy` in-cluster).
- **Severity:** high (blocks the documented quickstart)
- **Found:** 2026-05-04

## `make dev-run` exits immediately on missing webhook TLS cert  *(fixed 2026-05-04)*
- **Where:** `Makefile` `dev-run` target.
- **Observation:** Off-cluster startup tries to start the webhook server and fails: `open /tmp/k8s-webhook-server/serving-certs/tls.crt: no such file or directory`. cert-manager only provisions those certs for the in-cluster Deployment.
- **Fix:** `dev-run` now sets `ENABLE_WEBHOOKS=false`. The existing `if os.Getenv("ENABLE_WEBHOOKS") != "false"` gate in `cmd/operator/main.go:211` already supported this; the Makefile simply didn't wire it. Webhook coverage in dev still works via `make dev-deploy` (in-cluster, with cert-manager doing its job).
- **Severity:** high (blocks `dev-run`)
- **Found:** 2026-05-04

## Vault client has no `token` auth method  *(fixed 2026-05-04)*
- **Where:** `internal/vault/client_vault.go` `authenticate`; `cmd/operator/main.go` `buildVaultClient`.
- **Observation:** `internal/vault/client_vault.go` only handled `kubernetes` and `appRole`, returning `unsupported auth method "token"` for any other value. But the canonical dev-mode Vault path is `vault server -dev` + the root token (`dev-root-token` per `hack/local-dev/seed-vault.sh`). Without a token method, `make dev-run` could not authenticate to the dev Vault, so `VaultConfigured` reported `False/Error` and every dependent reconciler stalled.
- **Fix:** Added a `token` AuthMethod that calls `c.api.SetToken(c.cfg.Token)` directly (no login round-trip — a token is already an authenticated session). Documented as **dev-only** in the `Config.AuthMethod` doc comment. `buildVaultClient` reads `VAULT_TOKEN` when `VAULT_AUTH_METHOD=token`. `dev-run` now sets `VAULT_ADDR=http://127.0.0.1:8200`, `VAULT_TOKEN=dev-root-token`, `VAULT_AUTH_METHOD=token`.
- **Severity:** high (blocks any local dev loop that uses Vault)
- **Found:** 2026-05-04

## OIDC issuer URL passed to Vault contains a path component  *(fixed 2026-05-04)*
- **Where:** `internal/reconcilers/vault.go:139`.
- **Observation:** The reconciler built `https://<cluster.Spec.Domain>/oidc/<clusterName>` and passed it to `EnsureOIDCConfig` → Vault `PUT identity/oidc/config`. Vault rejects this with `400 invalid issuer, which must include only a scheme, host, and optional port (e.g. https://example.com:8200)`. Vault hosts the issuer at `<base>/v1/identity/oidc/<namespace>` itself; the operator must pass only the base, not the cluster-scoped path.
- **Fix:** The reconciler now passes `https://<cluster.Spec.Domain>` (scheme + host only). The cluster-name partition is carried by the OIDC key `openchami-<clusterName>` already created inside `EnsureOIDCConfig` — it never belonged in the issuer URL. A regression test `TestVaultReconciler_OIDCIssuerHasNoPath` pins the new format.
- **Severity:** high (blocked `VaultConfigured=True` for any cluster with `oidcProvider=vault`, which is the default)
- **Found:** 2026-05-04 during dev-up shakedown

## No production S3 client implementation  *(fixed 2026-05-04)*
- **Where:** `internal/s3/`; `cmd/operator/main.go`.
- **Observation:** The repository shipped only the `s3.Client` interface and a `s3/fake` implementation. There was no constructor that built a real S3 client from env vars (compare `buildVaultClient`). `cmd/operator/main.go` never set the `OpenCHAMIControlPlaneReconciler.S3Client` field, so the `BucketReconciler` and `LogBucketReconciler` always saw `nil` and reported `Error: s3 client not configured` in dev. In-cluster the same was true — bucket provisioning had never run for real.
- **Fix:** Added `internal/s3/s3client.go` backed by `aws-sdk-go-v2` with a `NewClient(ctx, Config)` constructor implementing all four interface methods (`EnsureBucket`, `EnsureLifecycleRule`, `BucketExists`, `DeleteBucket` with paginated empty-then-delete). Path-style addressing and explicit `BaseEndpoint` make it work against VersityGW, LocalStack, and MinIO. Wired `buildS3Client()` in `main.go` reading `AWS_ENDPOINT_URL` (gateway), `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION` (defaults to `us-east-1`), and `AWS_S3_TLS_INSECURE` (dev-only). `make dev-run` now points the bucket reconcilers at LocalStack on `127.0.0.1:4566` with the `test/test` keys. Constructor validation is unit-tested; live SDK behaviour is covered by the existing integration-sandbox.
- **Severity:** medium (no path to provisioning real buckets; LocalStack-backed dev couldn't validate the bucket reconcilers)
- **Found:** 2026-05-04 during dev-up shakedown



## Cluster namespace's PSA `enforce=restricted` rejects every host-namespace DaemonSet  *(fixed 2026-05-04)*
- **Where:** `internal/reconcilers/namespace.go:75`; `docs/phases/phase-02-controller.md:53`.
- **Observation:** The NamespaceReconciler stamped each cluster namespace with `pod-security.kubernetes.io/enforce: restricted`. The operator then schedules three DaemonSets into the same namespace that the `restricted` (and even the next-looser `baseline`) PSA profile rejects: `coredhcp` (hostNetwork=true, hostPort 67 — DHCP is fundamentally a host-net protocol), `funicular-collector` (hostPath `/var/log/pods` — log collection), and `network-probe` (hostNetwork=true — link-layer probing). The DS controller emitted `FailedCreate: pods "coredhcp-XYZ" is forbidden: violates PodSecurity "restricted:latest"` for every pod attempt, so `DHCPReady` and `LogCollectorReady` were stuck in `Provisioning` indefinitely. `make dev-run` couldn't surface this because off-cluster reconciles use cluster-admin, which doesn't bypass PSA but in our setup never tried to schedule the DaemonSets at all (the operator only *applies* the DaemonSet object; the DS controller is the one that creates pods, and that controller runs in the cluster).
- **Fix:** Switched the namespace label set to `enforce=privileged`, `warn=restricted`, `audit=restricted`. There is no PSA level between baseline and privileged that permits hostNetwork/hostPath/hostPort, so privileged is the only PSA option that permits all three host-namespace DaemonSets without splitting the cluster namespace in two. The `warn`/`audit` levels preserve admission visibility — service-tier pods (smd, tokensmith, boot-service, metadata-service) that fit `restricted` still raise audit events if they ever drift. For per-pod policy inside a privileged namespace, layer Kyverno or OPA Gatekeeper. Added regression test `TestNamespaceReconciler_PSALabelsPermitHostNetworkWorkloads` to pin the label set.
- **Severity:** high (blocked the entire host-network tier of the operator's reconciliation; conditions never reached True for in-cluster deployments)
- **Found:** 2026-05-04 during in-cluster shakedown — another fault `make dev-run` is structurally incapable of surfacing.

## CoreDHCP DaemonSet container expects config file at `/etc/coredhcp/config.{yml,yaml}` but operator never mounts one  *(fixed 2026-05-04)*
- **Where:** `internal/reconcilers/coredhcp.go` DaemonSet builder; new `internal/reconcilers/coredhcp_config.go`.
- **Observation:** With the PSA fix in place, the coredhcp pod scheduled, started, then immediately failed: `level=fatal msg="Failed to load configuration: Config File 'config' Not Found in '[/ /coredhcp /.coredhcp /etc/coredhcp]'"`. The reconciler shipped a DaemonSet that defined `CLUSTER_NAME`/`LEASE_RANGES_JSON`/`UNKNOWN_LEASE_DURATION`/`KNOWN_LEASE_DURATION` env vars on the assumption that the upstream `ghcr.io/openchami/coredhcp` entrypoint would render a config from them — but no such entrypoint exists.
- **Fix:** The reconciler now renders a real `coredhcp` `server4:` config (one listener, plugin chain) from `cluster.spec.services.coreDHCP.leaseRanges` into `<cluster-ns>/coredhcp-config` (key `config.yml`) and mounts it read-only at `/etc/coredhcp/` — already on coredhcp's auto-discovery search path, so no `--config` flag is needed (and a flag would not work anyway: the upstream image's ENTRYPOINT is `tini --`, which would try to exec the flag as a binary). The lease DB lives at `/tmp/coredhcp-leases.txt` because `/tmp` is the only writable mount on the pod. Multi-subnet config is not yet implemented (coredhcp's `server4` block has one plugin chain); the operator emits a warning comment in the rendered YAML when more than one leaseRange is configured. Two new tests pin the format: `TestRenderCoreDHCPConfig` and `TestRenderCoreDHCPConfig_NotesIgnoredAdditionalRanges`. Verified end-to-end: pod is `1/1 Running`, log line `Listen 0.0.0.0:67`; CR condition `DHCPReady=True/Ready: coredhcp DaemonSet ready (numberReady=1)`.
- **Severity:** high (DHCPReady can never reach True on any deployment that uses the default in-tree CoreDHCP image)
- **Found:** 2026-05-04 during in-cluster shakedown after the PSA fix unblocked DS scheduling.

## Funicular-collector image `ghcr.io/openchami/funicular-collector:latest` does not exist on ghcr.io  *(fixed 2026-05-04)*
- **Where:** `internal/reconcilers/funicular.go`; `api/v1alpha1/openchamicontrolplane_types.go` `LoggingSpec`; `test/fixtures/minimal-cluster.yaml`.
- **Observation:** With the PSA fix in place, kubelet attempted to pull `ghcr.io/openchami/funicular-collector:latest` and got `403 Forbidden` from the ghcr.io anonymous-token endpoint — repository does not exist. The reconciler also offered no per-cluster image override, so deployments could not point at any working alternative without code changes. `LogCollectorReady` was stuck in `Provisioning` with a misleading "waiting for pods" message that hid the real cause.
- **Fix:** Three coordinated changes. (1) Added `Image *ImageSpec` field to `LoggingSpec` so each cluster can supply a working collector image (fluent-bit, vector, otel-collector, or a real funicular build once published). (2) Flipped the kubebuilder default of `LoggingSpec.Enabled` from `true` to `false` so unconfigured clusters don't try to schedule a DaemonSet they can't satisfy — the reconciler short-circuits to `LogCollectorReady=True/Ready: "logging disabled"`. (3) When `Enabled=true` but `Image` is unset, the reconciler reports `LogCollectorReady=False/Reason=ImageNotConfigured` with an actionable message ("spec.logging.image is required when spec.logging.enabled=true"), instead of silently scheduling a DaemonSet that lands in ImagePullBackOff. The `minimal-cluster.yaml` fixture is updated with `logging.enabled: false` and a comment showing the override pattern. New regression test `TestFunicularReconciler_ImageNotConfigured` pins the gate. Verified live: previously misleading `LogCollectorReady=False/Provisioning: waiting for pods` is now `LogCollectorReady=False/ImageNotConfigured: spec.logging.image is required ...`.
- **Severity:** medium (LogCollectorReady stuck Provisioning, but the rest of the cluster reconciles around it; not load-bearing on other conditions)
- **Found:** 2026-05-04 during in-cluster shakedown after the PSA fix unblocked DS scheduling.

## Webhook update validation rejects in-place dev fixture patches  *(fixed 2026-05-04)*
- **Where:** `api/v1alpha1/openchamicontrolplane_webhook.go` `validate` (vault-address scheme check) and `nodeSelectorHasClusterDiscriminator`.
- **Observation:** When the in-cluster operator's webhook was active, `kubectl patch openchamicontrolplane ...` was rejected with `spec.platform.vault.address: Invalid value: "http://localhost:8200": vault address must use https:// scheme` and `spec.services.coreDHCP.nodeSelector: at least one nodeSelector value must contain the clusterName "testcluster"`. The first was over-strict — the dev fixture deliberately uses `http://localhost:8200` because Vault dev-mode runs without TLS. The second inspected the *value* (`"true"`) of nodeSelector entries instead of the *key* (`openchami.org/testcluster-provision-network-ready`), so the canonical convention failed validation. Net effect: any update to a CR (including `kubectl annotate` for unrelated reconcile-poke purposes) failed until both fields were retroactively changed, even when those fields were unchanged from the CREATE-time accepted state.
- **Fix:** (1) New `isAllowedVaultAddress` helper — `https://` is always accepted; `http://` is accepted only when the host portion is `localhost`, `127.0.0.1`, or `::1`; everything else continues to be rejected. Production endpoints (any non-loopback host) still require `https://`. (2) `nodeSelectorHasClusterDiscriminator` now inspects both keys *and* values of the selector map for a clusterName substring — covers both the canonical key form (`openchami.org/<clusterName>-<probe>-network-ready: "true"`) and any pre-existing value-based deployments (`cluster: <clusterName>`). Three new tests pin the matrix: `TestValidateCreate_VaultAddressAllowsLoopbackHTTP`, `TestValidateCreate_VaultAddressRejectsNonLoopbackHTTP`, `TestValidateCreate_NodeSelectorKeyDiscriminator`.
- **Severity:** medium (blocked CR iteration in dev once webhooks were live; not a data-integrity bug)
- **Found:** 2026-05-04 during in-cluster shakedown.

## In-cluster operator missing RBAC for resources it applies  *(fixed 2026-05-04)*
- **Where:** `internal/controller/openchamicontrolplane_controller.go` (`+kubebuilder:rbac` markers).
- **Observation:** `make dev-deploy` rolled the operator out, but every reconcile failed with `poddisruptionbudgets.policy "smd" is forbidden: User "system:serviceaccount:openchami-operator-system:openchami-operator-controller-manager" cannot patch resource "poddisruptionbudgets"`. `make dev-run` could never catch this because off-cluster the operator runs under the developer's cluster-admin kubeconfig, which short-circuits all RBAC checks. A full audit of `client.Patch(ctx, …, client.Apply)` call sites surfaced five missing groups: `policy/poddisruptionbudgets`, `""/persistentvolumeclaims` (tokensmith PVC), `batch/jobs` (database init), `postgresql.cnpg.io/clusters`, and `secrets.hashicorp.com/{vaultconnections,vaultauths,vaultstaticsecrets}`.
- **Fix:** Added the missing `+kubebuilder:rbac` markers; ran `make manifests`; reapplied `config/default`. `kubectl auth can-i` confirms the new permissions land on the operator's ServiceAccount.
- **Severity:** high (blocks every services-tier reconcile when running in-cluster, which is the production deployment mode)
- **Found:** 2026-05-04 during in-cluster shakedown — a fault `make dev-run` is structurally incapable of surfacing.

## `make deploy` silently ships placeholder image when `kustomize` CLI missing  *(fixed 2026-05-04)*
- **Where:** `Makefile` `deploy` target; `config/manager/manager.yaml`.
- **Observation:** The kubebuilder scaffold leaves `image: controller:latest` in `config/manager/manager.yaml` as a placeholder, intended to be rewritten before apply by `kustomize edit set image controller=$(IMG)`. The previous `deploy` target shelled out to that command and fell through to a silent `echo` if the `kustomize` CLI was not installed. Net effect: `make dev-deploy` reported success but the in-cluster Pod was `ImagePullBackOff` for `controller:latest` (an image that does not exist on Docker Hub). Compounding the issue, `imagePullPolicy` defaulted to `Always` (derived from the `:latest` tag), so even after fixing the image kubelet refused to use the kind-loaded copy.
- **Fix:** `deploy` now applies via `kubectl apply -k` (kubectl's built-in kustomize, no external CLI required) followed by `kubectl set image` to rewrite the manager image and `kubectl rollout status` to fail fast. `config/manager/manager.yaml` pins `imagePullPolicy: IfNotPresent` so kind-loaded images are honoured. Both pieces are needed: the rewrite covers the image, the policy covers the cache.
- **Severity:** high (blocked `make dev-deploy` end-to-end despite the test suite being green)
- **Found:** 2026-05-04 during in-cluster shakedown

## In-cluster operator never receives Vault/S3 connection env vars  *(fixed 2026-05-04)*
- **Where:** `Makefile` `dev-deploy` target.
- **Observation:** `make dev-run` exports `VAULT_ADDR=http://127.0.0.1:8200` and `AWS_ENDPOINT_URL=http://127.0.0.1:4566`, but `dev-deploy` shipped the kubebuilder default Pod spec with no env vars. After RBAC was fixed, both clients still came up nil — `buildVaultClient`/`buildS3Client` early-return when their endpoint env is unset, so the reconciler reported `VaultConfigured=False/Error: vault client not configured`. Even setting the env on the live Deployment wouldn't have helped: `127.0.0.1:8200` from inside a Pod is the Pod's own loopback, not the host's.
- **Fix:** `dev-deploy` now (a) `docker network connect kind` the compose containers `openchami-vault-dev` and `openchami-localstack-dev` (idempotent, swallows the already-attached error), and (b) `kubectl set env` the Deployment with `VAULT_ADDR=http://openchami-vault-dev:8200` / `AWS_ENDPOINT_URL=http://openchami-localstack-dev:4566` so kube-DNS resolves them through the cross-network attachment. Production deployments override these via normal kustomize overlay or external-secrets injection.
- **Severity:** high (in-cluster operator could never reach Vault or S3 even with correct RBAC)
- **Found:** 2026-05-04 during in-cluster shakedown

## Network-probe label key has two `/` (invalid Kubernetes label syntax)  *(fixed 2026-05-04)*
- **Where:** `internal/reconcilers/networkprobe.go:50`, `cmd/probe/main.go:173`, `internal/controller/openchamicontrolplane_controller.go:335`, `test/e2e/network_test.go:279`, plus the literal labels in `hack/local-dev/kind-config.yaml`, `test/fixtures/{minimal,dual}-cluster.yaml`, `docs/phases/phase-{06,16}-*.md`.
- **Observation:** `probeNetworkReadyLabelFmt = "openchami.org/%s/%s-network-ready"` produced keys like `openchami.org/testcluster/provision-network-ready`. Kubernetes label keys allow at most one `/` (`<prefix>/<qualified-name>`); kubelet rejected the kind-config labels with `failed to validate kubelet flags: invalid node labels: ... a qualified name must consist of alphanumeric characters, '-', '_' or '.'` on every restart, blocking `make dev-up` because the kind control-plane never became Ready. The API server would have rejected the same keys had the network-probe DaemonSet ever tried to apply them.
- **Fix:** Format changed to `openchami.org/%s-%s-network-ready` (cluster + probe joined into the qualified-name segment with a hyphen). All four code paths plus all YAML/doc literals updated. Verified end-to-end: `make dev-up` brings up cert-manager, CNPG, VSO, and Envoy Gateway and the node carries the new keys (`openchami.org/testcluster-provision-network-ready=true`, etc.).
- **Severity:** high (blocks `make dev-up`)
- **Found:** 2026-05-04

## `make dev-up` swallows kind-create errors and never exports kubeconfig  *(fixed 2026-05-04)*
- **Where:** `Makefile` `dev-up` target.
- **Observation:** `kind create cluster ... 2>/dev/null || echo "Cluster already exists"` masked failures and, on the "exists" branch, never ran `kind export kubeconfig`. A half-broken cluster (e.g. one whose kubelet was crash-looping due to the label bug above) reported success and downstream `kubectl apply` calls hit the default `localhost:8080` because no context was set.
- **Fix:** Replaced the silent `||` with an explicit `kind get clusters | grep -qx`; both branches end in `kind export kubeconfig`. Added a 30-iteration `kubectl get --raw=/livez` poll so dev-up fails fast if the API server is wedged.
- **Severity:** medium (turned the label bug into a confusing kubectl-localhost-8080 error)
- **Found:** 2026-05-04

## CNPG manifest URL is dead  *(fixed 2026-05-04)*
- **Where:** `Makefile` `dev-install-prereqs` target.
- **Observation:** `https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/main/releases/cnpg-latest.yaml` returns 404; CNPG no longer publishes a `latest`-aliased manifest at that path.
- **Fix:** Pin to a `CNPG_VERSION ?= 1.29.0` Makefile variable (matches the API surface vendored in `go.mod`) and apply `https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-$(basename $(CNPG_VERSION))/releases/cnpg-$(CNPG_VERSION).yaml` with `--server-side`.
- **Severity:** medium
- **Found:** 2026-05-04

## Envoy Gateway helm-repo URL doesn't exist  *(fixed 2026-05-04)*
- **Where:** `Makefile` `dev-install-prereqs` target.
- **Observation:** `helm repo add envoy-gateway https://charts.envoyproxy.io` fails silently (`2>/dev/null || true`); the chart is published as an OCI image at `oci://docker.io/envoyproxy/gateway-helm`, not as a classic helm repo.
- **Fix:** Pin to `ENVOY_GATEWAY_VERSION ?= v1.5.1` and pull from OCI: `helm upgrade --install envoy-gateway oci://docker.io/envoyproxy/gateway-helm --version $(ENVOY_GATEWAY_VERSION)`.
- **Severity:** medium
- **Found:** 2026-05-04

---

## Original sandbox-pass entries (all fixed 2026-05-04)

## Operator Dockerfile build path is wrong  *(fixed 2026-05-04)*
- **Where:** `Dockerfile:22`
- **Observation:** `RUN ... go build -a -o manager cmd/main.go`. The actual entry point lives at `cmd/operator/main.go`. `make docker-build` will fail.
- **Fix:** Dockerfile now builds `./cmd/operator` (package import path) so the manager binary resolves correctly regardless of file layout under that directory.
- **Severity:** high
- **Found:** 2026-05-03 (sandbox planning session)

## CoreDHCP default image references the wrong repo  *(fixed 2026-05-04)*
- **Where:** `internal/version/images.go:34`, `internal/reconcilers/coredhcp.go:34,152`, `SERVICES.md:13`
- **Observation:** `defaultCoreDHCPImage = "ghcr.io/openchami/coresmd:latest"`. CoreDHCP and CoreSMD are different projects; this pulls the wrong image. Both the constant and the per-spec-tag fallback used the typo.
- **Fix:** All three code paths (env-default in `images.go`, package constant in `coredhcp.go`, partial-override branch that mounted `tag` onto a hard-coded repo) now reference `ghcr.io/openchami/coredhcp`. `SERVICES.md` and `docs/reconcilers.md` updated; the troubleshooting note pointing at this bug removed.
- **Severity:** high
- **Found:** 2026-05-03

## Stale `go.mod.bak` in the repo root  *(fixed 2026-05-04)*
- **Where:** `go.mod.bak`
- **Observation:** Backup file from an earlier phase, no longer relevant; clutters the tree.
- **Fix:** Deleted. `.gitignore` now excludes `go.mod.bak` and `go.sum.bak` so future tooling can't reintroduce them. `REUSE.toml` annotation list shortened to drop the stale path.
- **Severity:** low
- **Found:** 2026-05-03

## Generic kubebuilder placeholder still in e2e suite  *(fixed 2026-05-04)*
- **Where:** `test/e2e/e2e_test.go` (around line 349)
- **Observation:** Scaffold comment with no implementation. Either the assertions belong here or the comment should be removed.
- **Fix:** Deleted the placeholder block. Real coverage continues to live in `lifecycle_test.go`, `network_test.go`, and `observability_test.go`.
- **Severity:** low
- **Found:** 2026-05-03

## Phase-15 observability TODOs leave assertions stubbed  *(fixed 2026-05-04)*
- **Where:** `test/e2e/observability_test.go` (parquet check around line 406, backup-bucket check around line 476)
- **Observation:** `TODO(phase-15): assert Parquet objects appear in log bucket` and `TODO(phase-15): assert backup artifacts`. The test passes today without verifying the lifecycle.
- **Fix:** Both assertions are now implemented but gated by env vars (`E2E_PARQUET_PRESENCE=1`, `E2E_BACKUP_PRESENCE=1`). When unset, the test prints a `GinkgoWriter` notice that the assertion is skipped — green no longer silently implies coverage. Re-enabling either is a one-line CI flip once upstream supports a deterministic flush / automated upload.
- **Severity:** medium
- **Found:** 2026-05-03

## Lifecycle recovery test assumes Vault address is reachable from fixture  *(fixed 2026-05-04)*
- **Where:** `test/e2e/lifecycle_test.go:55` (now around line 350)
- **Observation:** `TODO(e2e): the recovery half assumes Vault is reachable at lifecycle fixture address`.
- **Fix:** The dev address is now reached through `lifecycleDevVaultAddr()` which resolves from `E2E_VAULT_ADDR` first, falling back to the kind dev default. All call sites use the helper. The TODO comment is replaced with a note pointing reviewers at the env var.
- **Severity:** low
- **Found:** 2026-05-03

## kind-config hardcodes node labels for `testcluster` only  *(fixed 2026-05-04)*
- **Where:** `hack/local-dev/kind-config.yaml:14`
- **Observation:** Labels are pinned to the `testcluster` name. Running `dual-cluster.yaml` (venado/frontier) under the same kind config means those clusters never see the `bmc`/`provision` labels their reconcilers expect.
- **Fix:** kind-config now pre-labels the control-plane node for every cluster name used by the test fixtures: `testcluster`, `venado`, `frontier`, `venado-prod`. Comment block names the source fixtures so adding a new fixture is a localised diff.
- **Severity:** medium
- **Found:** 2026-05-03

## Reconciler import alias has the H/A transposed  *(fixed 2026-05-04)*
- **Where:** `internal/reconcilers/*.go` (39 files), `internal/controller/*`, etc.
- **Observation:** Imports `openahamiv1alpha1` instead of `openchamiv1alpha1`. Compiles because it's just an alias, but it's misleading.
- **Fix:** Mass-renamed `openahamiv1alpha1 → openchamiv1alpha1` across all affected files. `go build`, `go vet`, and per-package test compilation still succeed.
- **Severity:** low (style)
- **Found:** 2026-05-03
