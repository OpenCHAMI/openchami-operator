// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package version

import "os"

// ImageConfig holds default container image references for all operator-managed services.
// Override any field at operator startup via environment variables.
// Override per-cluster via spec.services.{service}.image in the CR.
type ImageConfig struct {
	SMD             string
	Tokensmith      string
	BootService     string
	MetadataService string
	CoreDHCP        string
	Magellan        string
	// NetworkProbe defaults to the operator image itself with subcommand "probe".
	NetworkProbe string
	Funicular    string
}

// DefaultImages returns the ImageConfig populated from environment variables,
// falling back to latest tags. In production, the operator Helm chart overrides
// these with digest-pinned references.
func DefaultImages() ImageConfig {
	operatorImage := envOr("OPENCHAMI_IMAGE_OPERATOR", "ghcr.io/openchami/openchami-operator:latest")
	return ImageConfig{
		SMD:             envOr("OPENCHAMI_IMAGE_SMD", "ghcr.io/openchami/smd:latest"),
		Tokensmith:      envOr("OPENCHAMI_IMAGE_TOKENSMITH", "ghcr.io/openchami/tokensmith:latest"),
		BootService:     envOr("OPENCHAMI_IMAGE_BOOT_SERVICE", "ghcr.io/openchami/boot-service:latest"),
		MetadataService: envOr("OPENCHAMI_IMAGE_METADATA_SERVICE", "ghcr.io/openchami/metadata-service:latest"),
		CoreDHCP:        envOr("OPENCHAMI_IMAGE_COREDHCP", "ghcr.io/openchami/coredhcp:latest"),
		Magellan:        envOr("OPENCHAMI_IMAGE_MAGELLAN", "ghcr.io/openchami/magellan:latest"),
		NetworkProbe:    envOr("OPENCHAMI_IMAGE_NETWORK_PROBE", operatorImage),
		Funicular:       envOr("OPENCHAMI_IMAGE_FUNICULAR", "ghcr.io/openchami/legendary-funicular:latest"),
	}
}

func envOr(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
