// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	corev1 "k8s.io/api/core/v1"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

// imageDefaults is the per-service repository + curated "release"
// stream tag that ship baked into the operator binary. Bumped in
// lockstep with operator releases; documented in SERVICES.md.
type imageDefaults struct {
	Repository string
	ReleaseTag string
}

// imageTagLatest is the "moving" tag. Used as the bleedingEdge stream
// value and as the release-stream fallback for services that haven't
// yet cut a versioned upstream release.
const imageTagLatest = "latest"

// builtInImages is the single source of truth for default image
// repositories and curated release-stream tags. Keys are the canonical
// service short-names (ServiceSMD, etc.) so the resolver, webhook,
// and tests all index by the same identifier.
//
// When bumping a release tag: update the entry here, update
// SERVICES.md, and add an UPGRADE.md note if the change is not
// backward-compatible.
// builtInImages: tags verified against the OpenCHAMI org GitHub
// releases on 2026-05-14.
//
//   - smd, boot-service, metadata-service, tokensmith, magellan: curated
//     tag points at the most recent GitHub Release tag.
//   - coredhcp: no GitHub Releases yet but `:latest` exists on GHCR —
//     ReleaseTag empty means "fall back to :latest in the release
//     stream too" (see resolveStreamTag).
//   - legendary-funicular, openchami-operator (probe): no public image
//     published yet. ReleaseTag empty so we don't claim a curated tag;
//     the funicular reconciler already refuses to schedule without an
//     explicit ImageSpec override, and the network-probe DaemonSet
//     stays disabled by default.
//
// When a new release ships upstream, bump the value here and update
// SERVICES.md.
var builtInImages = map[string]imageDefaults{
	ServiceSMD:             {Repository: "ghcr.io/openchami/smd", ReleaseTag: "v2.20.3"},
	ServiceTokensmith:      {Repository: "ghcr.io/openchami/tokensmith", ReleaseTag: "v0.4.1"},
	ServiceBootService:     {Repository: "ghcr.io/openchami/boot-service", ReleaseTag: "v0.1.5"},
	ServiceMetadataService: {Repository: "ghcr.io/openchami/metadata-service", ReleaseTag: "v0.1.0"},
	ServiceCoreDHCP:        {Repository: "ghcr.io/openchami/coredhcp", ReleaseTag: ""},
	ServiceMagellan:        {Repository: "ghcr.io/openchami/magellan", ReleaseTag: "v0.5.1"},
	// network-probe runs the operator's own binary with `probe`
	// subcommand, so its default repository is the operator image.
	ServiceNetworkProbe: {Repository: "ghcr.io/openchami/openchami-operator", ReleaseTag: ""},
	ServiceFunicular:    {Repository: "ghcr.io/openchami/legendary-funicular", ReleaseTag: ""},
}

// ResolveImage returns the container image reference and pull policy
// the operator should use for the named service on this control plane.
//
// Precedence (high to low):
//  1. Per-service ImageSpec from the CR (Repository, Tag, PullPolicy).
//     A field is only used when non-empty — the override is merge-style,
//     not replace-style.
//  2. The stream-resolved tag combined with the built-in repository.
//  3. The built-in release tag as a defensive fallback when stream=pinned
//     but the pinned map is missing this service (the webhook should
//     refuse such CRs; this branch keeps the resolver total).
//
// PullPolicy:
//   - If overridden in the per-service ImageSpec, use it.
//   - Else, tag-aware default: "latest" -> Always, otherwise IfNotPresent.
//     This matches what kubelet would derive on its own but is set
//     explicitly so server-side apply produces a stable PodSpec and
//     tests can assert it.
//
// Returns ("", "") for an unknown service name. The deployment will
// fail validation, surfacing the typo loudly rather than silently
// substituting the wrong image.
func ResolveImage(cp *openchamiv1alpha1.OpenCHAMIControlPlane, service string) (image string, pullPolicy corev1.PullPolicy) {
	defaults, ok := builtInImages[service]
	if !ok {
		return "", ""
	}

	override := perServiceImageOverride(cp, service)

	repo := defaults.Repository
	if override != nil && override.Repository != "" {
		repo = override.Repository
	}

	var tag string
	switch {
	case override != nil && override.Tag != "":
		tag = override.Tag
	default:
		tag = resolveStreamTag(cp, service, defaults.ReleaseTag)
	}

	image = repo + ":" + tag

	if override != nil && override.PullPolicy != "" {
		return image, override.PullPolicy
	}
	return image, tagAwarePullPolicy(tag)
}

// perServiceImageOverride returns the ImageSpec the user wrote (or nil
// if absent) for a given service. The mapping is hard-coded because
// each service has its own field on the spec; there is no reflection
// here, by design, so a typo at a call site fails to compile.
func perServiceImageOverride(cp *openchamiv1alpha1.OpenCHAMIControlPlane, service string) *openchamiv1alpha1.ImageSpec {
	s := &cp.Spec.Services
	switch service {
	case ServiceSMD:
		return s.SMD.Image
	case ServiceTokensmith:
		return s.Tokensmith.Image
	case ServiceBootService:
		return s.BootService.Image
	case ServiceMetadataService:
		return s.MetadataService.Image
	case ServiceCoreDHCP:
		return s.CoreDHCP.Image
	case ServiceMagellan:
		return s.Magellan.Image
	case ServiceNetworkProbe:
		return cp.Spec.NetworkProbe.Image
	case ServiceFunicular:
		return cp.Spec.Logging.Image
	}
	return nil
}

// resolveStreamTag picks the tag according to Spec.Images.Stream.
// An empty stream defaults to "release" (matches the kubebuilder
// default; this branch is the safety net for objects predating the
// field).
//
// An empty releaseDefault means the upstream hasn't cut a versioned
// release yet — fall back to `latest` so the release stream still
// produces a pullable image (currently true for coredhcp; for
// funicular and the probe the result is still an invalid reference,
// but those services are gated elsewhere so we never get there).
func resolveStreamTag(cp *openchamiv1alpha1.OpenCHAMIControlPlane, service, releaseDefault string) string {
	stream := cp.Spec.Images.Stream
	if stream == "" {
		stream = openchamiv1alpha1.ImageStreamRelease
	}
	switch stream {
	case openchamiv1alpha1.ImageStreamBleedingEdge:
		return imageTagLatest
	case openchamiv1alpha1.ImageStreamPinned:
		if cp.Spec.Images.Pinned != nil {
			if t, ok := cp.Spec.Images.Pinned[service]; ok && t != "" {
				return t
			}
		}
		// Webhook should have caught this, but if a CR slips through
		// (e.g. webhook disabled in dev) fall back to the curated tag
		// rather than producing an invalid image reference.
		if releaseDefault != "" {
			return releaseDefault
		}
		return imageTagLatest
	default: // ImageStreamRelease
		if releaseDefault != "" {
			return releaseDefault
		}
		return imageTagLatest
	}
}

// tagAwarePullPolicy returns the conventional Kubernetes pull policy
// for the given tag. Mirrors kubelet's own default so explicit and
// implicit behaviour agree.
func tagAwarePullPolicy(tag string) corev1.PullPolicy {
	if tag == imageTagLatest {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}

// EnabledServiceNamesForPinning returns the canonical names of every
// service this control plane has enabled, restricted to the set the
// operator manages images for. Used by the validating webhook when
// Stream=pinned to surface the exact missing entries.
func EnabledServiceNamesForPinning(cp *openchamiv1alpha1.OpenCHAMIControlPlane) []string {
	var names []string
	if cp.Spec.Services.SMD.Enabled {
		names = append(names, ServiceSMD)
	}
	if cp.Spec.Services.Tokensmith.Enabled {
		names = append(names, ServiceTokensmith)
	}
	if cp.Spec.Services.BootService.Enabled {
		names = append(names, ServiceBootService)
	}
	if cp.Spec.Services.MetadataService.Enabled {
		names = append(names, ServiceMetadataService)
	}
	if cp.Spec.Services.CoreDHCP.Enabled {
		names = append(names, ServiceCoreDHCP)
	}
	if cp.Spec.Services.Magellan.Enabled {
		names = append(names, ServiceMagellan)
	}
	if cp.Spec.NetworkProbe.Enabled {
		names = append(names, ServiceNetworkProbe)
	}
	if cp.Spec.Logging.Enabled {
		names = append(names, ServiceFunicular)
	}
	return names
}
