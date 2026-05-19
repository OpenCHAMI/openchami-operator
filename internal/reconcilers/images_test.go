// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

func TestResolveImage_ReleaseStreamUsesBuiltInTags(t *testing.T) {
	cp := newControlPlane("alpha")
	cp.Spec.Images.Stream = openchamiv1alpha1.ImageStreamRelease

	for service, defaults := range builtInImages {
		image, pull := ResolveImage(cp, service)

		// An empty ReleaseTag in builtInImages means "no curated tag
		// yet — fall back to :latest" (current state for coredhcp,
		// funicular, network-probe). The resolver follows the
		// kubelet-style tag-aware pull-policy default, so :latest
		// pairs with PullAlways.
		wantTag := defaults.ReleaseTag
		wantPull := corev1.PullIfNotPresent
		if wantTag == "" {
			wantTag = imageTagLatest
			wantPull = corev1.PullAlways
		}
		want := defaults.Repository + ":" + wantTag
		if image != want {
			t.Errorf("[%s] expected image %q, got %q", service, want, image)
		}
		if pull != wantPull {
			t.Errorf("[%s] expected pullPolicy %q, got %q", service, wantPull, pull)
		}
	}
}

func TestResolveImage_EmptyStreamDefaultsToRelease(t *testing.T) {
	cp := newControlPlane("alpha")
	// Stream left empty — represents an object that predates the field or
	// hits the resolver before the kubebuilder default fires.
	cp.Spec.Images.Stream = ""

	image, _ := ResolveImage(cp, ServiceSMD)
	want := builtInImages[ServiceSMD].Repository + ":" + builtInImages[ServiceSMD].ReleaseTag
	if image != want {
		t.Errorf("expected default-to-release behaviour, got %q", image)
	}
}

func TestResolveImage_BleedingEdgeStreamUsesLatestAndAlways(t *testing.T) {
	cp := newControlPlane("alpha")
	cp.Spec.Images.Stream = openchamiv1alpha1.ImageStreamBleedingEdge

	for service, defaults := range builtInImages {
		image, pull := ResolveImage(cp, service)
		want := defaults.Repository + ":latest"
		if image != want {
			t.Errorf("[%s] expected %q, got %q", service, want, image)
		}
		if pull != corev1.PullAlways {
			t.Errorf("[%s] expected PullAlways for :latest, got %q", service, pull)
		}
	}
}

func TestResolveImage_PinnedStreamUsesPinnedMap(t *testing.T) {
	cp := newControlPlane("alpha")
	cp.Spec.Images.Stream = openchamiv1alpha1.ImageStreamPinned
	cp.Spec.Images.Pinned = map[string]string{
		ServiceSMD:         "v9.9.9",
		ServiceTokensmith:  "v1.0.0-rc1",
		ServiceBootService: "v0.5.0",
	}

	smdImage, smdPull := ResolveImage(cp, ServiceSMD)
	if smdImage != "ghcr.io/openchami/smd:v9.9.9" {
		t.Errorf("expected pinned smd tag, got %q", smdImage)
	}
	if smdPull != corev1.PullIfNotPresent {
		t.Errorf("expected IfNotPresent for pinned tag, got %q", smdPull)
	}

	tsImage, _ := ResolveImage(cp, ServiceTokensmith)
	if tsImage != "ghcr.io/openchami/tokensmith:v1.0.0-rc1" {
		t.Errorf("expected pinned tokensmith tag, got %q", tsImage)
	}
}

// TestResolveImage_PinnedStreamMissingEntryFallsBack documents the
// defensive runtime behaviour: when the webhook hasn't caught a gap
// in Spec.Images.Pinned, the resolver returns the curated release
// tag instead of producing a broken `repo:` image reference.
func TestResolveImage_PinnedStreamMissingEntryFallsBack(t *testing.T) {
	cp := newControlPlane("alpha")
	cp.Spec.Images.Stream = openchamiv1alpha1.ImageStreamPinned
	cp.Spec.Images.Pinned = map[string]string{
		ServiceSMD: "v9.9.9",
		// boot-service intentionally missing.
	}
	image, _ := ResolveImage(cp, ServiceBootService)
	wantTag := builtInImages[ServiceBootService].ReleaseTag
	want := "ghcr.io/openchami/boot-service:" + wantTag
	if image != want {
		t.Errorf("expected fallback to release tag %q, got %q", want, image)
	}
}

func TestResolveImage_PerServiceImageOverrideWins(t *testing.T) {
	cp := newControlPlane("alpha")
	cp.Spec.Images.Stream = openchamiv1alpha1.ImageStreamRelease
	cp.Spec.Services.SMD.Image = &openchamiv1alpha1.ImageSpec{
		Repository: "registry.internal/mirror/smd",
		Tag:        "v1.2.3-rc1",
		PullPolicy: corev1.PullAlways,
	}

	image, pull := ResolveImage(cp, ServiceSMD)
	if image != "registry.internal/mirror/smd:v1.2.3-rc1" {
		t.Errorf("expected per-service override, got %q", image)
	}
	if pull != corev1.PullAlways {
		t.Errorf("expected user-specified PullAlways, got %q", pull)
	}
}

// TestResolveImage_PartialOverrideMergesWithStream guards the
// merge-style behaviour: setting only the repository (e.g. to point
// at a private mirror) keeps the stream-resolved tag, and vice versa.
func TestResolveImage_PartialOverrideMergesWithStream(t *testing.T) {
	t.Run("repository-only override keeps release tag", func(t *testing.T) {
		cp := newControlPlane("alpha")
		cp.Spec.Images.Stream = openchamiv1alpha1.ImageStreamRelease
		cp.Spec.Services.SMD.Image = &openchamiv1alpha1.ImageSpec{
			Repository: "registry.internal/mirror/smd",
		}
		image, _ := ResolveImage(cp, ServiceSMD)
		want := "registry.internal/mirror/smd:" + builtInImages[ServiceSMD].ReleaseTag
		if image != want {
			t.Errorf("expected %q, got %q", want, image)
		}
	})

	t.Run("tag-only override keeps default repository", func(t *testing.T) {
		cp := newControlPlane("alpha")
		cp.Spec.Images.Stream = openchamiv1alpha1.ImageStreamBleedingEdge
		cp.Spec.Services.SMD.Image = &openchamiv1alpha1.ImageSpec{
			Tag: "v2.0.0-hotfix",
		}
		image, pull := ResolveImage(cp, ServiceSMD)
		if image != "ghcr.io/openchami/smd:v2.0.0-hotfix" {
			t.Errorf("expected tag override to win over stream :latest, got %q", image)
		}
		// Tag is no longer imageTagLatest, so the tag-aware default flips
		// to IfNotPresent even though the stream would have been Always.
		if pull != corev1.PullIfNotPresent {
			t.Errorf("expected IfNotPresent for non-latest override, got %q", pull)
		}
	})
}

func TestResolveImage_UnknownServiceReturnsEmpty(t *testing.T) {
	cp := newControlPlane("alpha")
	image, pull := ResolveImage(cp, "not-a-real-service")
	if image != "" || pull != "" {
		t.Errorf("expected empty resolution for unknown service, got (%q, %q)", image, pull)
	}
}

func TestEnabledServiceNamesForPinning(t *testing.T) {
	cp := newControlPlane("alpha")
	cp.Spec.Services.SMD.Enabled = true
	cp.Spec.Services.Tokensmith.Enabled = true
	cp.Spec.Services.BootService.Enabled = false
	cp.Spec.NetworkProbe.Enabled = true
	cp.Spec.Logging.Enabled = false

	names := EnabledServiceNamesForPinning(cp)
	wantPresent := map[string]bool{
		ServiceSMD:          true,
		ServiceTokensmith:   true,
		ServiceNetworkProbe: true,
	}
	wantAbsent := map[string]bool{
		ServiceBootService: true,
		ServiceFunicular:   true,
	}

	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for n := range wantPresent {
		if !got[n] {
			t.Errorf("expected %q in enabled list, got %v", n, names)
		}
	}
	for n := range wantAbsent {
		if got[n] {
			t.Errorf("did not expect %q in enabled list (disabled), got %v", n, names)
		}
	}
}
