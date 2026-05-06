// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

// Package version holds build-time version information set via ldflags.
package version

// Variables set by Makefile ldflags at build time.
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)
