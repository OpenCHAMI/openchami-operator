//go:build !linux

/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package main

import (
	"fmt"
	"net"
	"runtime"
)

// defaultRouteCheck on non-Linux platforms returns an error so the binary
// reports false for the route check. The probe is only deployed in Linux
// containers; this stub exists so the package builds during local development
// on macOS/Windows.
func defaultRouteCheck(_ net.IP) error {
	return fmt.Errorf("netlink route check unsupported on %s", runtime.GOOS)
}
