//go:build !linux

// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"net/netip"
	"runtime"
)

// defaultAttachmentCheck on non-Linux platforms returns an error so the binary
// reports false for the host attachment check. The probe is only deployed in Linux
// containers; this stub exists so the package builds during local development
// on macOS/Windows.
func defaultAttachmentCheck(_ netip.Prefix) error {
	return fmt.Errorf("netlink host attachment check unsupported on %s", runtime.GOOS)
}
