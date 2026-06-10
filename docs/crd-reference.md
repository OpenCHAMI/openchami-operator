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
      address: https://vault.example.com:8200       # required
      authMethod: kubernetes | appRole              # default kubernetes
      appRoleSecretRef:                             # required if authMethod=appRole
        name: openchami-foo-approle                 # secret with role_id, secret_id keys
      caBundleSecretRef:                            # optional
        name: vault-ca                              # secret with ca.crt key
```

Vault is **external** (invariant 1). The operator never creates a Vault deployment.

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
      coredhcp: latest
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
      coredhcp: latest
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
    tokensmith:        { enabled: true,  replicas: 1, image: {...}, resources: {}, oidcProvider: ..., oidcIssuerURL: ... }
    bootService:       { enabled: true,  replicas: 2, image: {...}, resources: {} }
    metadataService:   { enabled: true,  replicas: 2, image: {...}, resources: {} }
    coreDHCP:
      enabled: true
      nodeSelector: {openchami.org/<clusterName>-provision-network-ready: "true"}   # auto-set by network-probe
      leaseRanges:
        - subnet: 10.10.0.0/16
          start: 10.10.0.50
          end: 10.10.0.250
      unknownLeaseDuration: 5m
      knownLeaseDuration: 24h
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
