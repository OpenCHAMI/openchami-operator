/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

// Package version holds build-time version information set via ldflags.
package version

// Variables set by Makefile ldflags at build time.
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)
