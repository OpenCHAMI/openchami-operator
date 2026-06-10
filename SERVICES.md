# SERVICES.md
<!-- Default service image tags shipped with this operator release. -->
<!-- Update this file on every release alongside UPGRADE.md. -->

# Default service images (unreleased)

This file documents the default container images baked into the operator binary at build time. These defaults are used when deploying OpenCHAMI services unless overridden at runtime via the `OpenCHAMIControlPlane` CRD.

## Current defaults

| Service | Repository | Release Tag | Source |
|---|---|---|---|
| SMD | `ghcr.io/openchami/smd` | `v2.20.3` | `internal/reconcilers/images.go` |
| Tokensmith | `ghcr.io/openchami/tokensmith` | `v0.4.1` | `internal/reconcilers/images.go` |
| Boot service | `ghcr.io/openchami/boot-service` | `v0.1.5` | `internal/reconcilers/images.go` |
| Metadata service | `ghcr.io/openchami/metadata-service` | `v0.1.0` | `internal/reconcilers/images.go` |
| CoreDHCP | `ghcr.io/openchami/coredhcp` | `latest` | `internal/reconcilers/images.go` |
| Magellan | `ghcr.io/openchami/magellan` | `v0.5.1` | `internal/reconcilers/images.go` |
| Funicular collector | `ghcr.io/openchami/legendary-funicular` | `latest` | `internal/reconcilers/images.go` |
| Logq compactor | `ghcr.io/openchami/openchami-logq-compactor` | `pr-1` | `internal/reconcilers/images.go` |
| Logq query | `ghcr.io/openchami/openchami-logq-query` | `pr-1` | `internal/reconcilers/images.go` |
| Network probe | `ghcr.io/openchami/openchami-operator` | `latest` | `internal/reconcilers/images.go` |

**Note:** Services showing `latest` as the release tag have not yet cut a versioned upstream release. The operator falls back to `:latest` for these services in the `release` stream.

---

## How container versions are controlled

Container image versions are determined by a three-layer system:

### 1. Build-time defaults (this file)

The operator binary ships with curated image tags defined in `internal/reconcilers/images.go` in the `builtInImages` map. These defaults are:

- **Reproducible** — locked to the operator version; same operator binary = same service images.
- **Curated** — tested together as a release set.
- **Bumped on operator releases** — updated when the operator itself is released.

**To change build-time defaults:**

Edit `internal/reconcilers/images.go` and update the `builtInImages` map:

```go
var builtInImages = map[string]imageDefaults{
    ServiceSMD:             {Repository: "ghcr.io/openchami/smd", ReleaseTag: "v2.20.3"},
    ServiceTokensmith:      {Repository: "ghcr.io/openchami/tokensmith", ReleaseTag: "v0.4.1"},
    // ... etc
}
```

Then rebuild the operator:

```sh
make build          # Local binaries
make docker-build   # Container image
```

### 2. Image streams (runtime selection)

The `OpenCHAMIControlPlane` CRD provides three image streams that control which tags are used at runtime:

#### `release` (default)

Uses the curated tags baked into the operator binary (see table above). This is the **recommended production setting**.

```yaml
spec:
  images:
    stream: release   # or omit — release is the default
```

**Effect:** SMD uses `v2.20.3`, tokensmith uses `v0.4.1`, etc.

#### `bleedingEdge`

Uses `:latest` for every service. Pods pull fresh images on every restart (`PullPolicy: Always` is applied automatically).

```yaml
spec:
  images:
    stream: bleedingEdge
```

**Effect:** All services use `:latest`. **Never use in production.**

#### `pinned`

Reads tags from `spec.images.pinned`. You must provide an entry for every enabled service.

```yaml
spec:
  images:
    stream: pinned
    pinned:
      smd: v2.19.0
      tokensmith: v0.4.0
      bootService: v0.1.4
      metadataService: v0.1.0
      coredhcp: latest
      magellan: v0.5.0
      funicular: latest
      networkProbe: v1.0.0
```

**Effect:** Each service uses the explicitly-specified tag. The validating webhook rejects CRs with missing entries.

**Use case:** Pin an entire cluster to a specific, tested service set independent of operator upgrades.

### 3. Per-service image overrides

Each service accepts an `image` block that overrides repository, tag, and pull policy:

```yaml
spec:
  services:
    smd:
      image:
        repository: ghcr.io/myorg/custom-smd
        tag: v3.0.0-beta
        pullPolicy: Always
```

**Precedence:** Per-service overrides always win, regardless of stream setting.

**Use case:** Test a pre-release version of a single service without changing the rest.

---

## Precedence summary

From highest to lowest priority:

1. **Per-service `image` override** (e.g., `spec.services.smd.image.tag`)
2. **Image stream** (`spec.images.stream` + `spec.images.pinned`)
3. **Build-time defaults** (from `internal/reconcilers/images.go`)

---

## Updating defaults for a new operator release

When cutting a new operator release:

1. **Check upstream releases** — visit each service's GitHub Releases page and identify the latest stable tag.
2. **Update `internal/reconcilers/images.go`** — bump `ReleaseTag` values in the `builtInImages` map.
3. **Update this file** — sync the table above with the new values.
4. **Update `UPGRADE.md`** — note any breaking changes in service versions.
5. **Rebuild and tag the operator** — `make docker-build IMG=ghcr.io/openchami/openchami-operator:vX.Y.Z`.

Example commit:

```
chore: bump service defaults for v1.2.0 release

- SMD: v2.19.0 → v2.20.3
- Magellan: v0.5.0 → v0.5.1
- All other services unchanged

Tested against integration-sandbox e2e suite.
```

---

## Developer workflow: testing unreleased service changes

If you're developing a service and need to test it with the operator:

### Option A: Override at the CR level (recommended)

Build and push your dev image, then override in the test fixture:

```yaml
# test/fixtures/dev-cluster.yaml
spec:
  services:
    smd:
      image:
        repository: ghcr.io/myusername/smd
        tag: feature-branch-sha
        pullPolicy: Always
```

Apply and reconcile:

```sh
kubectl apply -f test/fixtures/dev-cluster.yaml
```

### Option B: Use `bleedingEdge` stream + tag as `:latest`

Push your dev build as `:latest` to your registry:

```sh
docker build -t ghcr.io/myusername/smd:latest .
docker push ghcr.io/myusername/smd:latest
```

Then override the repository:

```yaml
spec:
  images:
    stream: bleedingEdge
  services:
    smd:
      image:
        repository: ghcr.io/myusername/smd
```

### Option C: Rebuild the operator with modified defaults (not recommended)

Edit `internal/reconcilers/images.go`, rebuild the operator, load into kind, and redeploy. This approach is slower and makes it easy to forget you've changed the defaults.

---

## FAQ

### Q: Why are some services stuck on `:latest`?

A: Those services haven't cut a versioned GitHub Release yet. Once they do, update the `ReleaseTag` field in `builtInImages` and this file.

### Q: Can I use a private registry?

A: Yes. Override `repository` for each service:

```yaml
spec:
  services:
    smd:
      image:
        repository: myregistry.example.com/openchami/smd
        tag: v2.20.3
```

Or set `imagePullSecrets` on the namespace (the operator does not manage pull secrets; that's left to external tooling or manual setup).

### Q: How do I know which versions are compatible?

A: The curated `release` stream defaults are tested together. If you mix versions manually, consult each service's `UPGRADE.md` or compatibility matrix.

### Q: What happens if I set `stream: pinned` but forget a service?

A: The validating webhook rejects the CR with an error listing the missing entries.

### Q: Can I pin the operator itself to a version?

A: Yes, via `spec.operatorChannel: pinned` and `spec.pinnedVersion`. See [docs/upgrade-and-versioning.md](docs/upgrade-and-versioning.md).

---

## Related documentation

- [CRD reference](docs/crd-reference.md) — full `OpenCHAMIControlPlane` spec.
- [Upgrade and versioning](docs/upgrade-and-versioning.md) — operator upgrade policy.
- [UPGRADE.md](UPGRADE.md) — release-to-release breaking changes.
- [Quickstart](docs/quickstart.md) — local dev cluster setup.
