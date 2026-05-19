# SERVICES.md
<!-- Default service image tags shipped with this operator release. -->
<!-- Update this file on every release alongside UPGRADE.md. -->

# Default service images (unreleased)

| Service | Image | Source constant |
|---|---|---|
| SMD | `ghcr.io/openchami/smd:latest` | `defaultSMDImage` (internal/reconcilers/smd.go) |
| Tokensmith | `ghcr.io/openchami/tokensmith:latest` | `defaultTokensmithImage` (internal/reconcilers/tokensmith.go) |
| Boot service | `ghcr.io/openchami/boot-service:latest` | `defaultBootServiceImage` (internal/reconcilers/boot_service.go) |
| Metadata service | `ghcr.io/openchami/metadata-service:latest` | `defaultMetadataServiceImage` (internal/reconcilers/metadata_service.go) |
| CoreDHCP | `ghcr.io/openchami/coredhcp:latest` | `defaultCoreDHCPImage` (internal/reconcilers/coredhcp.go) |
| Magellan | `ghcr.io/openchami/magellan:latest` | `defaultMagellanImage` (internal/reconcilers/magellan.go) |
| Funicular collector | `ghcr.io/openchami/funicular-collector:latest` | `defaultFunicularImage` (internal/reconcilers/funicular.go) |
| Network probe | `ghcr.io/openchami/openchami-operator:latest` | hard-coded in internal/reconcilers/networkprobe.go |

Per-cluster overrides: each service spec accepts an `image` block with
`repository` and `tag`. Override at the `OpenCHAMIControlPlane` resource level.

## Pinning to a specific tag at release time

When cutting a release, update each constant above to a pinned tag (e.g.
`ghcr.io/openchami/smd:v1.4.2`) — `latest` is intended only for development.
