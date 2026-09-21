# Publishing

Use immutable tags for anything shared beyond one workstation. The repository
publishes operator images to `ghcr.io/openchami/openchami-operator`.

## Validate before publishing

```sh
make install-tools
make generate manifests fmt vet lint test validate-invariants build
make docker-build IMG=ghcr.io/openchami/openchami-operator:local
```

`make generate manifests fmt` must leave no diff beyond intentional generated
changes. Run the kind-based suite for changes that affect admission or live
reconciliation:

```sh
make dev-up
make e2e
make dev-down
```

## Pull-request images

A same-repository pull request publishes two temporary images after the test
workflow succeeds:

- `ghcr.io/openchami/openchami-operator:pr-<number>`
- `ghcr.io/openchami/openchami-operator:pr-<number>-<head-sha>`

Use the SHA-qualified tag for reproducible testing. The moving PR-number tag is
convenient for repeated testing. Fork pull requests are built and smoke-tested
but cannot publish packages with the repository token. PR images intentionally
omit SBOM and provenance attestations.

## Releases

Maintainers publish a release by pushing a signed, reviewed `v*` tag. The
release workflow runs GoReleaser, creates the GitHub release and assets, and
pushes the corresponding GHCR image. Do not move or reuse a release tag.

Before tagging, verify that generated CRDs are present, the release notes match
the user-visible changes, and the service-image pins in `SERVICES.md` are the
intended versions. Consumers should deploy the release tag rather than
`latest`.

## Private registries

Build and push under a site-owned immutable tag, then set the deployment image
or installation overlay to that exact reference:

```sh
export IMG=registry.example.org/openchami/openchami-operator:v0.0.0-site.1
make docker-build IMG="$IMG"
docker push "$IMG"
```

Authentication, retention, signing, and mirroring policy belong to the target
registry; this repository does not embed registry credentials.
