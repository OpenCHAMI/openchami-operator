# OpenCHAMIControlPlane CRD reference

Group/version: `openchami.openchami.org/v1alpha1`. Kind: `OpenCHAMIControlPlane`. Source of truth: `api/v1alpha1/openchamicontrolplane_types.go`. Authoritative auto-generated form: `config/crd/bases/openchami.openchami.org_openchamicontrolplanes.yaml`. This file is the human-readable summary.

## Spec

### Required top-level fields

| Field | Type | Notes |
|---|---|---|
| `clusterName` | string | Pattern `^[a-z][a-z0-9-]*$`. Immutable after creation. Determines the namespace `openchami-{clusterName}` and Vault path prefix `openchami/{clusterName}/`. |
| `domain` | string | FQDN. Used as the SNI / route host on the Envoy gateway. |
| `platform.vault` | object | See [VaultSpec](#vaultspec). |
| `platform.objectStorage` | object | See [ObjectStorageSpec](#objectstoragespec). |

### Optional top-level fields

| Field | Type | Default | Notes |
|---|---|---|---|
| `images` | object | `stream=release` | See [ImagesSpec](#imagesspec). Controls container image tag selection. |
| `networkProbe` | object | disabled | See [NetworkProbeSpec](#networkprobespec). |
| `services` | object | each subfield enabled | See [ServicesSpec](#servicesspec). |
| `networking` | object | gatewayClass=`envoy`, issuer auto | See [NetworkingSpec](#networkingspec). |
| `database` | object | 1 instance, 10Gi | See [DatabaseSpec](#databasespec). |
| `logging` | object | enabled, 30 days | See [LoggingSpec](#loggingspec). |
| `observability.prometheusOperator` | bool | false | When true, emits `ServiceMonitor` resources. |
| `operatorChannel` | enum | `stable` | `stable` or `pinned`. See [upgrade-and-versioning](upgrade-and-versioning.md). |
| `pinnedVersion` | string | "" | Required when `operatorChannel=pinned`. Operator skips reconcile if its own version doesn't match. |

### VaultSpec

```yaml
spec:
  platform:
    vault:
      address: https://vault.example.com:8200       # required (operator dial address)
      oidcIssuer: https://vault.example.org          # optional; canonical OIDC issuer
      authMethod: kubernetes | appRole              # default kubernetes
      appRoleSecretRef:                             # required if authMethod=appRole
        name: openchami-foo-approle                 # secret with role_id, secret_id keys
      caBundleSecretRef:                            # optional
        name: vault-ca                              # secret with ca.crt key
```

Vault is **external** (invariant 1). The operator never creates a Vault deployment.

`address` is the URL the operator uses to communicate with the Vault API — often an
in-cluster `.svc` address. `oidcIssuer` is the canonical `scheme://host[:port]` (no
path) advertised as the issuer of the shared `openchami` Vault OIDC provider; it is
what OIDC clients — including TokenSmith — validate the `iss` claim against and must
be able to reach. It need not be publicly routable, only stable and reachable by
those clients. It must be `scheme://host[:port]` with no userinfo, path, query, or
fragment, and follows the same TLS policy as `address`: `https` for any host, `http`
only for cluster-internal hosts (loopback, single-label/`.svc` DNS, RFC1918/link-local
IPs) — public issuers must use `https`. When omitted, `oidcIssuer` falls back to
`address`. All `OpenCHAMIControlPlane` resources sharing one Vault must agree on this
issuer; a disagreement is reported as a configuration conflict rather than silently
overwriting the shared provider.

### ObjectStorageSpec

```yaml
spec:
  platform:
    objectStorage:
      endpoint: https://versitygw.example.com:7070  # required
      bucket: foo-boot-images                       # default: "{clusterName}-boot-images"
      tlsInsecure: false                            # default false; dev/test only
```

VersityGW is **external** (invariant 1). The operator never creates a gateway.

### ImagesSpec

Controls how the operator selects container image tags for all managed services. Three strategies (streams) are available:

```yaml
spec:
  images:
    stream: release              # default; uses curated tags from SERVICES.md
```

```yaml
spec:
  images:
    stream: bleedingEdge         # uses :latest for all services (dev only)
```

```yaml
spec:
  images:
    stream: pinned               # requires explicit tag for every enabled service
    pinned:
      smd: v2.20.3
      tokensmith: v0.4.1
      bootService: v0.1.5
      metadataService: v0.1.0
      coredhcp: v0.7.1
      magellan: v0.5.1
      funicular: latest
      networkProbe: v1.0.0
```

#### Image stream reference

| Stream | Behavior | Pull Policy | Use Case |
|---|---|---|---|
| `release` | Uses curated tags baked into the operator binary (see [SERVICES.md](../SERVICES.md)). Reproducible across reconciles. | `IfNotPresent` for versioned tags, `Always` for `:latest` | **Production (recommended)** |
| `bleedingEdge` | Uses `:latest` for every service. Pulls fresh images on every pod restart. | `Always` | Development and testing only |
| `pinned` | Uses tags from `spec.images.pinned` map. Validating webhook rejects CRs with missing entries for enabled services. | `IfNotPresent` for versioned tags, `Always` for `:latest` | Lock a cluster to a specific tested service set |

#### Precedence

Image selection follows this priority (highest to lowest):

1. **Per-service override** — `spec.services.<name>.image.tag` (see [ServicesSpec](#servicesspec))
2. **Image stream** — `spec.images.stream` + `spec.images.pinned`
3. **Build-time defaults** — from `internal/reconcilers/images.go`

#### Examples

##### Example 1: Production cluster using curated defaults

```yaml
spec:
  images:
    stream: release   # or omit — release is the default
  services:
    smd:
      enabled: true   # uses ghcr.io/openchami/smd:v2.20.3 (from SERVICES.md)
```

##### Example 2: Pin entire cluster to a tested service set

```yaml
spec:
  images:
    stream: pinned
    pinned:
      smd: v2.19.0              # one version behind current release
      tokensmith: v0.4.1
      bootService: v0.1.5
      metadataService: v0.1.0
      coredhcp: v0.7.1
      magellan: v0.5.1
  services:
    smd:
      enabled: true             # uses ghcr.io/openchami/smd:v2.19.0
```

##### Example 3: Override a single service for testing

```yaml
spec:
  images:
    stream: release             # most services use curated defaults
  services:
    smd:
      enabled: true
      image:
        tag: v2.21.0-rc1        # override: test a pre-release SMD
        pullPolicy: Always
```

##### Example 4: Development cluster with bleeding-edge builds

```yaml
spec:
  images:
    stream: bleedingEdge        # all services use :latest
```

**Warning:** `bleedingEdge` is non-deterministic. Never use in production.

#### Service names for pinned map

When using `stream: pinned`, the map keys must match the operator's canonical service names:

| Service | Key in `pinned` map |
|---|---|
| SMD | `smd` |
| Tokensmith | `tokensmith` |
| Boot service | `bootService` |
| Metadata service | `metadataService` |
| CoreDHCP | `coredhcp` |
| Magellan | `magellan` |
| Funicular (log collector) | `funicular` |
| Network probe | `networkProbe` |

The validating webhook enforces that every **enabled** service has a corresponding entry. Disabled services do not require an entry.

#### Related documentation

- [SERVICES.md](../SERVICES.md) — current build-time defaults and how to update them
- [Upgrade and versioning](upgrade-and-versioning.md) — operator upgrade policy

### NetworkProbeSpec

```yaml
spec:
  networkProbe:
    enabled: true                       # default false
    intervalSeconds: 30                 # default 30
    provisionNetwork:
      subnet: 10.10.0.0/16              # required when enabled
      validateHost: 10.10.0.1           # optional reachability target
      validatePort: 67                  # optional
      validateTimeout: 5s               # optional
    bmcNetwork:
      subnet: 10.20.0.0/16
      validateHost: 10.20.0.1
      validatePort: 443
```

When enabled, the operator deploys a DaemonSet that probes each node and labels it `openchami.org/<clusterName>-provision-network-ready=true` / `openchami.org/<clusterName>-bmc-network-ready=true`. CoreDHCP and Magellan use those labels for node selection. (Kubernetes label keys allow at most one `/`, so the cluster name is joined with a hyphen rather than a second slash.)

When disabled, you must set `spec.services.coreDHCP.nodeSelector` and `spec.services.magellan.nodeSelector` explicitly. The validating webhook enforces invariant 4 (no two clusters target the same node for CoreDHCP).

### ServicesSpec

```yaml
spec:
  services:
    smd:               { enabled: true,  replicas: 1, image: {repository: ..., tag: ..., pullPolicy: IfNotPresent}, resources: {} }
    tokensmith:
      enabled: true
      replicas: 1
      image: {...}
      resources: {}
      oidcProvider: vault
      oidcIssuerURL: ""
      oidcIntrospectionEndpoint: ""
      cliOIDC:
        redirectURIs: ["http://127.0.0.1:8250/callback"]
        assignments: ["allow_all"]
    bootService:       { enabled: true,  replicas: 2, image: {...}, resources: {}, httpBootScript: true }
    metadataService:   { enabled: true,  replicas: 2, image: {...}, resources: {} }
    coreDHCP:
      enabled: true
      nodeSelector: {openchami.org/<clusterName>-provision-network-ready: "true"}   # auto-set by network-probe
      leaseRanges:
        - subnet: 10.10.0.0/16
          start: 10.10.0.50
          end: 10.10.0.250
      unknownLeaseDuration: 5m       # generated config only
      knownLeaseDuration: 1h         # generated config only
      # configMapRef:                # alternative to leaseRanges/*LeaseDuration — see below
      #   name: site-coredhcp-config
      #   key: config.yml
      image: {...}
      resources: {}
      tolerations: []
    magellan:
      enabled: true
      nodeSelector: {openchami.org/<clusterName>-bmc-network-ready: "true"}
      schedule: "*/15 * * * *"        # cron
      bmcSubnet: 10.20.0.0/16
      concurrencyPolicy: Forbid
      image: {...}
      resources: {}
```

The default image for each service is determined by the image stream (see [ImagesSpec](#imagesspec)). The `release` stream (default) uses curated tags from [SERVICES.md](../SERVICES.md).

For tokensmith, `oidcIntrospectionEndpoint` is optional and provider-neutral.
Set it when the upstream issuer discovery document omits both
`token_introspection_endpoint` and `introspection_endpoint`; the operator
passes the value through as `TOKENSMITH_OIDC_INTROSPECTION_ENDPOINT` and does
not derive provider-specific paths.

When `oidcProvider` is `vault`, `cliOIDC` configures a separate public Vault
client named `openchami-<cluster>-cli`. Its default loopback redirect is suitable
for a native CLI and its default `allow_all` assignment is Vault's built-in
assignment. Replace the assignment list to restrict authorization. Public
clients use PKCE and receive no secret; CLI users must never receive the
confidential `openchami-<cluster>-tokensmith` client secret. See
[Local Vault OIDC CLI testing](vault-oidc-local-testing.md).

#### Per-service overrides

Every service spec embeds `ServiceDefaults`, which provides four uniform knobs:

| Field | Default | Effect |
|---|---|---|
| `enabled` | `true` | When `false`, the operator does not create any in-cluster objects for the service. |
| `replicas` | `2` (1 for tokensmith) | Replica count for `Deployment`-backed services. Ignored for the DaemonSet (CoreDHCP) and CronJob (Magellan). |
| `image` | from [ImagesSpec](#imagesspec) | Override `repository`, `tag`, and `pullPolicy`. Takes precedence over the image stream. |
| `resources` | none | Standard `corev1.ResourceRequirements` (requests + limits). |
| `externalEndpoint` | unset | Declares the service is provided **externally** at the given http(s) URL. The operator skips deploying it and wires *consumers* to this URL instead of the in-cluster Service DNS. |

`externalEndpoint` is supported only on the four HTTP-facing services — `smd`, `tokensmith`, `bootService`, `metadataService`. It is rejected on `coreDHCP` (DHCP is layer-2/3) and `magellan` (a CronJob, not a service).

##### Validation rules (admission webhook)

- Setting `externalEndpoint` requires `enabled: false`. Both being true is rejected.
- The URL must parse as `http://…` or `https://…` with a non-empty host.
- The URL is taken verbatim — no path or trailing slash is appended.

##### Example: site uses external SMD and external metadata-service, runs Kea instead of CoreDHCP

```yaml
spec:
  services:
    smd:
      enabled: false
      externalEndpoint: "https://smd.platform.example.com"
    metadataService:
      enabled: false
      externalEndpoint: "https://metadata.platform.example.com"
    coreDHCP:
      enabled: false   # site provides DHCP via Kea/dnsmasq/ISC; operator stays out
    tokensmith:
      enabled: true    # operator-managed
    bootService:
      enabled: true    # operator-managed
```

When `externalEndpoint` is set, the operator also skips the corresponding `HTTPRoute` and `SecurityPolicy` so the in-cluster gateway doesn't try to back-end onto a Service that doesn't exist. Sites running an external instance are responsible for terminating and routing to it themselves.

#### CoreDHCP configuration

The CoreDHCP DaemonSet (image `ghcr.io/openchami/coresmd`, which is CoreDHCP
built with the `coresmd` and `bootloop` plugins) reads a single config file
at `/etc/coredhcp/config.yml`. The operator fills it from one of two sources:

| Mode | Set | Who owns the config | Suitable for |
|---|---|---|---|
| Generated (default) | `leaseRanges` (+ optional lease durations) | operator | dev / test only |
| User-provided | `configMapRef` | you | production |

> **Warning: the generated config does not PXE-boot nodes from OpenCHAMI.**
> It is a minimal stock-CoreDHCP config — `range` plugin over the first
> `leaseRanges` entry, `server_id`/`router` assumed to be the subnet's
> `.1`, DNS hard-coded to `1.1.1.1 8.8.8.8`, listening on every interface.
> It does **not** enable `coresmd` (SMD-backed leases, iPXE boot script,
> TFTP) or `bootloop`. Production deployments must supply a config with
> `configMapRef`.

`configMapRef` and the generated-config fields (`leaseRanges`,
`unknownLeaseDuration`, `knownLeaseDuration`) are **mutually exclusive**; the
validating webhook rejects a spec that sets both rather than silently
ignoring one.

##### Using `configMapRef`

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: site-coredhcp-config
  namespace: default            # same namespace as the OpenCHAMIControlPlane
data:
  config.yml: |
    server4:
      listen:
        - "%eno1"               # provision-network interface on the DHCP node
      plugins:
        - server_id: 172.16.0.254
        - dns: 172.16.0.254
        - router: 172.16.0.1
        - netmask: 255.255.255.0
        - coresmd: |
            svc_base_uri=https://demo.openchami.example
            ipxe_uri=http://demo.openchami.example/boot/v1/bootscript
            ca_cert=/root_ca/root_ca.crt
            cache_valid=30s
            lease_time=1h
            single_port=true
        - bootloop: |
            lease_file=/tmp/coredhcp.db
            script_path=default
            lease_time=5m
            ipv4_start=172.16.0.200
            ipv4_end=172.16.0.250
---
apiVersion: openchami.openchami.org/v1alpha1
kind: OpenCHAMIControlPlane
metadata:
  name: demo
  namespace: default
spec:
  domain: demo.openchami.example
  services:
    coreDHCP:
      enabled: true
      configMapRef:
        name: site-coredhcp-config
        key: config.yml         # optional, default config.yml
```

How it behaves:

- The referenced ConfigMap must live in the **same namespace as the
  `OpenCHAMIControlPlane`** (not `openchami-<cluster>`, which the operator
  creates). The operator copies the key verbatim into its own
  `openchami-<cluster>/coredhcp-config` ConfigMap (annotated
  `openchami.org/coredhcp-config-source: <ns>/<name>:<key>`) and mounts that.
  The operator never writes to your ConfigMap.
- The operator watches the referenced ConfigMap. An edit is mirrored and
  rolls the DaemonSet automatically: the pod template carries
  `openchami.org/coredhcp-config-hash`, and coredhcp only reads its config at
  startup.
- If the ConfigMap or key is missing or empty, `DHCPReady=False` with reason
  `ConfigMapNotFound` and a Warning Event; an already-running DaemonSet keeps
  serving its last config. The operator does not validate the contents —
  a bad config shows up as a crash-looping `coredhcp` pod.
- The operator still owns the DaemonSet: image, resources, tolerations,
  node selection, security context, mounts, and ports.

##### What the DaemonSet provides to your config

| Item | Value | Use in config |
|---|---|---|
| Networking | `hostNetwork: true`, `dnsPolicy: ClusterFirstWithHostNet` | `listen:` refers to the **node's** interfaces |
| Ports | UDP 67 (DHCP), UDP 69 (TFTP) as hostPorts | coresmd's built-in TFTP server defaults to 69 (`tftp_port`) |
| Writable path | `/tmp` (memory-backed emptyDir; root filesystem is read-only) | lease databases, e.g. `bootloop` `lease_file=/tmp/coredhcp.db`. **Lost on pod restart** |
| CA bundle | `/root_ca/root_ca.crt` — the `ca.crt` key of the gateway TLS Secret (`spec.networking.tls.secretName`) | coresmd `ca_cert=/root_ca/root_ca.crt` |
| TFTP root | `/tftpboot` (iPXE binaries bundled in the coresmd image) | coresmd `tftp_dir` default |

##### Reaching SMD and boot-service from coresmd

coresmd runs on the host network, so in-cluster pod-selector
NetworkPolicies do not admit it to SMD directly. Point it at the gateway
instead:

- `svc_base_uri=https://<spec.domain>` — coresmd only issues
  `GET /hsm/v2/Inventory/EthernetInterfaces` and `GET /hsm/v2/State/Components`,
  both on SMD's default unauthenticated public route list
  (`spec.services.smd.publicRoutes`, enabled by default). If you replace
  `publicRoutes.paths`, keep both.
- `ipxe_uri=http://<spec.domain>/boot/v1/bootscript` — on boot-service's
  default public route list (`spec.services.bootService.publicRoutes`) and,
  by default, served over plain HTTP. See iPXE and TLS below.
- `<spec.domain>` must resolve, from the DHCP node via cluster DNS, to the
  Envoy Gateway's address.

**CA trust.** `/root_ca/root_ca.crt` is the gateway TLS Secret's `ca.crt`.
cert-manager populates that key for CA and Vault issuers, but **not** for
ACME (Let's Encrypt) issuers. The volume is optional, so a missing key does
not block the pod from starting, but the file will not exist.

coresmd (v0.7.1) treats `ca_cert` as effectively required: it reads the
file unconditionally at startup (an unset or missing path makes the plugin
fail to load), and when set it trusts **only** that file, not the system
store. So:

- CA / Vault issuer: use `ca_cert=/root_ca/root_ca.crt`.
- ACME or other publicly trusted issuer: point `ca_cert` at the image's
  system bundle instead (for the wolfi-based coresmd image,
  `/etc/ssl/certs/ca-certificates.crt` — verify against the image you run).
- Anything else: bake the CA into a derived image and override
  `spec.services.coreDHCP.image`.

**iPXE and TLS.** The iPXE binaries bundled in the coresmd image do not trust
a private CA. The gateway's HTTP listener redirects every request to HTTPS
**except** `GET /boot/v1/bootscript`, which it serves directly over HTTP
(`spec.services.bootService.httpBootScript`, default `true`). Use an
`http://` `ipxe_uri` and iPXE never needs to validate the gateway
certificate. The plaintext route exists only while `/boot/v1/bootscript` is
also on the HTTPS public list (`publicRoutes`). No other boot-service path,
including `/admin/boot`, is reachable over HTTP.

If the gateway certificate chains to a CA iPXE already trusts (or you build
iPXE with your CA embedded), you can use
`ipxe_uri=https://<spec.domain>/boot/v1/bootscript` and set
`httpBootScript: false` to keep the HTTP listener redirect-only. Kernel,
initrd, and rootfs URLs come from the boot configuration and are not
affected.

### NetworkingSpec

```yaml
spec:
  networking:
    gatewayClass: envoy                   # default: envoy
    tls:
      issuer: letsencrypt-staging         # cert-manager Issuer name; auto if empty
      secretName: foo-tls                 # default: "{clusterName}-tls"
```

### DatabaseSpec

```yaml
spec:
  database:
    instances: 3                          # default 1
    storageSize: 50Gi                     # default 10Gi (resource.Quantity)
    storageClass: fast-ssd                # default: cluster default StorageClass
    backupEnabled: true                   # default false; requires objectStorage configured
```

### LoggingSpec

```yaml
spec:
  logging:
    enabled: true
    logBucket: foo-logs                   # default: "{clusterName}-logs"
    retentionDays: 30                     # default 30
    flushIntervalSeconds: 60              # default 60
    includeServices: [smd, tokensmith]    # default: all enabled services
```

The operator deploys the **funicular** log-collector DaemonSet when `enabled=true`. See [legendary-funicular](https://github.com/openchami/legendary-funicular) for the data plane.

## Status

Status is patched **last** on every reconcile (invariant 6). The operator never returns from `Reconcile` without patching `.status.conditions`.

| Field | Type | Notes |
|---|---|---|
| `phase` | enum | `Provisioning`, `Ready`, `Degraded`, `Deleting`, `Failed`. Computed from conditions by `internal/status/Reporter`. |
| `conditions[]` | metav1.Condition | One per reconciler. `type/status/reason/message/observedGeneration`. |
| `services[name]` | map | Per-service `ServiceStatus{ready, endpoint, message}`. |
| `namespace` | string | The created namespace name. |
| `vaultPathPrefix` | string | The KV-v2 path prefix in use (`openchami/{clusterName}/`). |
| `networkProbe.nodesWithProvisionAccess[]` | []string | Nodes that pass the provision-network probe. |
| `networkProbe.nodesWithBMCAccess[]` | []string | Nodes that pass the BMC-network probe. |
| `networkProbe.probeReady` | bool | True when DaemonSet is running and at least one node passes each configured probe. |
| `topologyVersion` | string | SHA-256 of the topology ConfigMap content. Bumps on every meaningful spec change. |
| `managedByVersion` | string | Operator semver that last reconciled this cluster. Used by [upgrade-and-versioning](upgrade-and-versioning.md). |
| `certExpiryTime` | RFC3339 string | The earliest expiry across all gateway certificates. |
| `logBucket` | string | The active log bucket (after defaulting). |
| `observedGeneration` | int64 | Standard Kubernetes pattern. |

### Phases

| Phase | Trigger |
|---|---|
| `Provisioning` | At least one reconciler has not yet reported `Ready=True`. |
| `Ready` | All required reconcilers report `Ready=True`. |
| `Degraded` | At least one previously-Ready reconciler now reports `Ready=False` and recovery is in progress. |
| `Deleting` | `metadata.deletionTimestamp` is set; the finalizer is doing cleanup. |
| `Failed` | A reconciler reported a fatal, unrecoverable condition (rare; most failures fall into `Degraded` and requeue). |

The aggregator is in `internal/status/`. Tests assert phase computation in `internal/status/*_test.go`.

## Webhooks

See [webhooks.md](webhooks.md) for defaulting, validation, and conversion behavior.

## Examples

Three fixtures ship with the repo:

- `test/fixtures/minimal-controlplane.yaml` — single cluster `testcluster`, networkProbe disabled, all services enabled with defaults, 1 DB replica.
- `test/fixtures/full-controlplane.yaml` — `venado-prod` shape with network probing on, 3 DB replicas + backup enabled.
- `test/fixtures/dual-controlplane.yaml` — two clusters (`venado`, `frontier`) for concurrency testing.
- `test/fixtures/production-controlplane.yaml.example` — heavily-annotated production-shaped example; see [install-production.md](install-production.md).

Apply any of them with `kubectl apply -f` and watch with `kubectl get openchamicontrolplane -A -w`.
