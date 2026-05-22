# Production install

End-to-end walkthrough for installing the OpenCHAMI operator into a fresh
(non-dev) Kubernetes cluster. Pairs with
[`test/fixtures/production-controlplane.yaml.example`](../test/fixtures/production-controlplane.yaml.example)
as the worked CR example.

Local-dev install — `make dev-up` + `make dev-run` — is covered separately
in [quickstart.md](quickstart.md). This document is for the case where you
own a real kube cluster and a real Vault.

---

## Audience and scope

You are an HPC site or platform operator. You have:

- A working Kubernetes cluster (1.29+, see compatibility matrix in
  [external-dependencies.md](external-dependencies.md)).
- `kubectl` and `helm` installed locally; cluster-admin context.
- An external Vault (1.13+) with admin credentials.
- An external S3-compatible endpoint (VersityGW, real S3, MinIO).
- Authority to install ClusterRoles and ClusterRoleBindings.

Out of scope: deploying Kubernetes, deploying Vault, deploying VersityGW,
managing site DNS, configuring TLS-terminating ingress in front of the
Envoy Gateway. The operator never deploys any of those (invariant 1 in
[invariants.md](invariants.md)).

---

## 1. Verify prerequisites

```sh
kubectl version --short
kubectl cluster-info
kubectl auth can-i create clusterrolebindings
```

Required: `clusterrolebindings`/`clusterroles`/`customresourcedefinitions`
must all return `yes`. The install creates these.

---

## 2. Install prerequisite controllers and CRDs

Install in this order. Each adds CRDs the operator depends on; missing
ones surface as `False` conditions on the `OpenCHAMIControlPlane`.

### 2.1 gateway-api standard CRDs (≥ 1.4)

The operator emits `BackendTLSPolicy` against the `gateway.networking.k8s.io/v1`
API, which was promoted from `v1alpha3` in gateway-api 1.4. The Envoy
Gateway helm chart does **not** install the v1 CRDs on its own.

```sh
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml
```

If you skip this, the operator falls back gracefully — it reports
`GatewayReady=False/MissingCRD` and defers publishing the JWT-gated routes,
so the rest of the cluster stays usable. The dev cluster used this graceful
degrade path until 2026-05-21; install the CRDs to get the JWT-gated
routes back online.

### 2.2 cert-manager

```sh
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
kubectl wait --for=condition=Available --timeout=120s -n cert-manager deployment/cert-manager-webhook
```

The operator emits `Certificate` resources but does **not** create any
`Issuer` or `ClusterIssuer` — that is intentional. See §3 below for issuer
choice.

### 2.3 CloudNativePG

```sh
kubectl apply --server-side -f https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.29/releases/cnpg-1.29.0.yaml
```

The operator emits `postgresql.cnpg.io/Cluster` resources. CNPG itself
manages StatefulSets, primary/replica failover, and backups.

### 2.4 Vault Secrets Operator (VSO)

```sh
helm repo add hashicorp https://helm.releases.hashicorp.com
helm upgrade --install vault-secrets-operator hashicorp/vault-secrets-operator \
  --namespace vault-secrets-operator-system --create-namespace \
  --set defaultVaultConnection.enabled=false
```

The operator emits one `VaultConnection`, one `VaultAuth`, and one
`VaultStaticSecret` per cluster — VSO is what actually syncs Vault paths
into Kubernetes Secrets that pods mount.

### 2.5 Envoy Gateway + GatewayClass

```sh
helm upgrade --install envoy-gateway oci://docker.io/envoyproxy/gateway-helm \
  --version v1.5.1 \
  --namespace envoy-gateway-system --create-namespace
```

The chart installs the Envoy Gateway controller. You must also create a
`GatewayClass` named `envoy` (or whatever you set
`spec.networking.gatewayClass` to). For a single-class single-cluster
install:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: envoy
spec:
  controllerName: gateway.envoyproxy.io/gatewayclass-controller
```

The dev environment ships an equivalent at
`hack/local-dev/envoy-gatewayclass.yaml`.

### 2.6 (Optional) Prometheus Operator

If you want `ServiceMonitor` emission, install kube-prometheus-stack or
any distribution that ships `monitoring.coreos.com/v1`. Otherwise leave
`spec.observability.prometheusOperator: false`.

---

## 3. Configure cert-manager Issuers

cert-manager doesn't ship a default `Issuer`/`ClusterIssuer`. The operator
references whatever issuer name you put in `spec.networking.tls.issuer`.
Common production choices:

- **`ClusterIssuer` backed by Let's Encrypt** (ACME, HTTP-01 or DNS-01) for
  public FQDNs.
- **`ClusterIssuer` backed by Vault PKI** for an internal CA you already
  trust — install the cert-manager Vault external issuer plugin.
- **`ClusterIssuer` backed by a CA Secret** if you have a long-lived
  internal CA cert+key you can stage as a Secret.

The operator does not care which — it only references the issuer by name.
Test by creating a hand-rolled Certificate against the issuer and confirming
cert-manager mints a Secret before you wire it into the `OpenCHAMIControlPlane`.

---

## 4. Plan namespaces and node layout

The operator creates one Kubernetes namespace per cluster:
`openchami-<clusterName>`. Inside it live PostgreSQL, all services,
DaemonSets, NetworkPolicies, the Gateway, and HTTPRoutes.

The operator itself lives in `openchami-operator-system` (set by
`config/default/kustomization.yaml`).

If you run **multiple `OpenCHAMIControlPlane` resources** on the same
cluster, invariant 4 (DHCP node exclusivity) applies — no two
`coreDHCP.enabled: true` clusters may target the same node, because they
all want host UDP ports 67/68. The validating webhook enforces this when
`networkProbe.enabled: false`. With probes on, each cluster gets its own
label set (`openchami.org/<clusterName>-{provision,bmc}-network-ready`)
and operates over disjoint label-selected node sets.

---

## 5. Deploy the operator

### 5.1 Standard kustomize deploy

```sh
make deploy IMG=ghcr.io/openchami/openchami-operator:v1.0.0
```

Equivalent kubectl-only form:

```sh
kubectl apply --server-side --force-conflicts -k config/default
kubectl -n openchami-operator-system set image \
  deploy/openchami-operator-controller-manager \
  manager=ghcr.io/openchami/openchami-operator:v1.0.0
kubectl -n openchami-operator-system rollout status \
  deploy/openchami-operator-controller-manager --timeout=120s
```

This installs:

- The `OpenCHAMIControlPlane` CRD.
- ClusterRoles: `manager-role` (the operator's permissions),
  `metrics-auth-role`, `metrics-reader-role`, plus `*-admin-role`,
  `*-editor-role`, `*-viewer-role` for human/team scoping.
- The webhook ValidatingWebhookConfiguration and a `Certificate` for the
  webhook server.
- The operator `Deployment` and `ServiceAccount` in
  `openchami-operator-system`.

### 5.2 RBAC scoping notes

The operator's `manager-role` is a **cluster-scoped** ClusterRole because:

- It creates and manages per-cluster `Namespace` objects (cluster-scoped).
- It applies CRs in multiple namespaces (one per `OpenCHAMIControlPlane`).
- It reads `Node` objects to compute network-probe state.

It cannot meaningfully be reduced to a per-namespace `Role` without
breaking the multi-cluster model. If you need to limit blast radius:

- Run the operator in its own namespace with strict NetworkPolicies.
- Use the generated `openchamicontrolplane_{admin,editor,viewer}_role.yaml`
  ClusterRoles to scope human/team access to `OpenCHAMIControlPlane`
  resources without granting cluster-wide write power.
- Audit `config/rbac/role.yaml` for the exact permission set. Every entry
  is sourced from a `+kubebuilder:rbac:` marker in
  `internal/controller/openchamicontrolplane_controller.go`; if you see a
  permission you don't recognize, grep for the marker to find the
  reconciler that needs it.

To verify the live operator's permissions:

```sh
kubectl auth can-i --as=system:serviceaccount:openchami-operator-system:openchami-operator-controller-manager \
  create poddisruptionbudgets
# expect: yes
```

---

## 6. Provision Vault

The operator needs:

- A KV-v2 mount at `openchami/` (or any path you reference via the
  `openchami/<clusterName>/...` prefix).
- An AppRole role per cluster, e.g. `openchami-<clusterName>-services`.
- A Vault policy that scopes the role to that cluster's KV path and any
  PKI roles you've configured.

### 6.1 Vault paths the operator and services read

```
openchami/<clusterName>/db/smd                # username, password
openchami/<clusterName>/db/boot-service       # username, password
openchami/<clusterName>/s3/versitygw          # access_key, secret_key (boot bucket)
openchami/<clusterName>/s3/logs               # access_key, secret_key (log bucket)
openchami/<clusterName>/oidc/tokensmith-client # client_secret
```

The vault reconciler calls `EnsureSecret(..., overwrite=false)` against
each path (`internal/vault/client_vault.go:142`). The effect:

- **If the path is empty:** the operator writes a random 32-byte hex string
  for each key. This is the correct behaviour for the database paths (CNPG
  creates the Postgres role with the generated password).
- **If the path already has a secret:** the operator leaves it alone.

For S3 and OIDC, the operator's random values are placeholders only —
they will not match any real IAM principal or OIDC client registration.
Pre-stage real credentials at those paths **before** applying the CR:

```sh
vault kv put openchami/venado/s3/versitygw \
  access_key="<real-key>" secret_key="<real-secret>"
vault kv put openchami/venado/s3/logs \
  access_key="<real-key>" secret_key="<real-secret>"
vault kv put openchami/venado/oidc/tokensmith-client \
  client_secret="<real-secret>"
```

### 6.2 Provision script

`hack/local-dev/seed-vault.sh` is the canonical reference for the shape.
For production, run the equivalent against your real Vault — same `vault
kv put`, `vault policy write`, `vault write auth/approle/role/...` calls.
Do **not** check secret values into Git; pull them from your secret
manager or rotate them out of band.

A minimal production version of the script:

```sh
CLUSTER=venado
PREFIX="openchami/$CLUSTER"

# KV v2 (idempotent)
vault secrets enable -path=openchami kv-v2 || true

# Policy
vault policy write "openchami-$CLUSTER-services" - <<POLICY
path "$PREFIX/data/*"           { capabilities = ["read"] }
path "pki/issue/openchami-services" { capabilities = ["create", "update"] }
POLICY

# AppRole
vault auth enable approle || true
vault write "auth/approle/role/openchami-$CLUSTER-services" \
  token_policies="openchami-$CLUSTER-services" \
  token_ttl=15m token_max_ttl=1h

# Stage the bootstrap credentials (rotate post-Ready)
vault kv put "$PREFIX/s3/versitygw" \
  access_key=...  secret_key=...
vault kv put "$PREFIX/oidc/tokensmith-client" \
  client_secret=...
```

Token TTLs at 15m/1h are dev defaults — production should match your
existing Vault token-rotation policy.

---

## 7. Provision S3 bucket access

Two buckets are needed when logging is enabled:

- **Boot bucket** (`spec.platform.objectStorage.bucket`, default
  `<clusterName>-boot-images`) — read+write for the operator and the boot
  service.
- **Log bucket** (`spec.logging.logBucket`, default `<clusterName>-logs`)
  — write for the funicular collector, read for `ochami-admin logs`.

Either pre-create the buckets with a bucket-policy that allows the
AccessKey staged at `openchami/<cluster>/s3/...`, or grant the AccessKey
bucket-creation permission and let the operator's bucket reconciler
create them itself.

Bucket-policy shape (S3-compatible):

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Sid": "OpenCHAMIBootBucketAccess",
    "Effect": "Allow",
    "Principal": {"AWS": "arn:aws:iam:::user/openchami-venado"},
    "Action": ["s3:ListBucket", "s3:GetObject", "s3:PutObject", "s3:DeleteObject"],
    "Resource": ["arn:aws:s3:::venado-boot-images", "arn:aws:s3:::venado-boot-images/*"]
  }]
}
```

VersityGW honours this shape; adapt principals to your gateway's identity
model.

---

## 8. Stage the per-cluster AppRole Secret

VSO authenticates to Vault using the AppRole `secret_id` stored as a
Kubernetes Secret in the **per-cluster namespace** (not the namespace
where the CR lives). The namespace must exist before VSO's first auth.

```sh
kubectl create namespace openchami-venado

ROLE_ID=$(vault read  -field=role_id   auth/approle/role/openchami-venado-services/role-id)
SECRET_ID=$(vault write -f -field=secret_id auth/approle/role/openchami-venado-services/secret-id)

kubectl -n openchami-venado create secret generic venado-vault-approle \
  --from-literal=id="$SECRET_ID" \
  --from-literal=role_id="$ROLE_ID"
```

Keys: `id` holds the secret_id (VSO's required key name); `role_id` is
kept for human inspection — VSO discovers `role_id` from the operator-owned
`VaultAuth` resource, not from this Secret.

Rotation: re-run the `vault write ... secret-id` step and update the
Secret in place; VSO picks up the change on its next refresh.

---

## 9. Apply the OpenCHAMIControlPlane

Copy
[`test/fixtures/production-controlplane.yaml.example`](../test/fixtures/production-controlplane.yaml.example),
edit the `CHANGEME` markers (cluster name, domain, Vault address, S3
endpoint, network CIDRs, issuer name, AppRole Secret name), then:

```sh
kubectl apply -f my-cluster.yaml
kubectl get openchamicontrolplane -A -w
```

Within a few minutes the phase walks `Provisioning → Ready`.

---

## 10. Verify readiness

```sh
kubectl get openchamicontrolplane <name> -o jsonpath='{range .status.conditions[*]}{.type}={.status} [{.reason}] {.message}{"\n"}{end}'
```

All conditions should report `Status=True`. If any are `False`, the
`Reason` and `Message` point at the next thing to check. Common
first-time-install false-Falses and their fixes are in
[troubleshooting.md](troubleshooting.md).

Smoke check the gateway. The operator exposes the tokensmith JWKS endpoint
through Envoy (`internal/reconcilers/gateway.go:112`); fetching it confirms
the whole TLS-termination → HTTPRoute → backend chain works end-to-end:

```sh
GATEWAY_IP=$(kubectl -n openchami-<name> get gateway openchami-gateway -o jsonpath='{.status.addresses[0].value}')
curl -k -H "Host: <domain>" "https://$GATEWAY_IP/.well-known/jwks.json" | jq
```

A 200 with a JWK set (`{"keys": [...]}`) means tokensmith is reachable
through Envoy with TLS terminated. From this point the cluster is ready
for compute nodes to PXE / iPXE / cloud-init against.

---

## 11. What's next

- **Logging.** Flip `spec.logging.enabled: true` and set
  `spec.logging.image` to a working collector image (fluent-bit, vector,
  otel-collector configured for the funicular schema). Confirm Parquet
  files land in the log bucket.
- **Observability.** Set `spec.observability.prometheusOperator: true` if
  you have kube-prometheus-stack. The operator emits one `ServiceMonitor`
  per service.
- **Backup/restore.** `ochami-admin backup --cluster-name <name>` produces
  a snapshot artifact; see [cli.md](cli.md) for the full procedure.
- **Upgrades.** Pin to a tag in `spec.operatorChannel: pinned` +
  `spec.pinnedVersion` during controlled upgrade windows; see
  [upgrade-and-versioning.md](upgrade-and-versioning.md).
- **Multi-cluster.** Apply additional `OpenCHAMIControlPlane` resources;
  each gets its own `openchami-<clusterName>` namespace and Vault prefix.
  The webhook enforces DHCP node exclusivity (invariant 4).

---

## Reference

- [`test/fixtures/production-controlplane.yaml.example`](../test/fixtures/production-controlplane.yaml.example) — the worked CR example.
- [external-dependencies.md](external-dependencies.md) — the prerequisite catalog with version pins.
- [troubleshooting.md](troubleshooting.md) — common failure modes during first install.
- [cli.md](cli.md) — `ochami-admin` reference (init / describe / backup / restore / logs).
- [crd-reference.md](crd-reference.md) — every field on `OpenCHAMIControlPlane`.
- [invariants.md](invariants.md) — the 10 absolute rules.
