//go:build linux

// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

// defaultRouteCheck queries the kernel routing table for a route to target.
// Returns nil if a route exists; an error otherwise.
func defaultRouteCheck(target net.IP) error {
	routes, err := netlink.RouteGet(target)
	if err != nil {
		return fmt.Errorf("netlink RouteGet %s: %w", target, err)
	}
	if len(routes) == 0 {
		return fmt.Errorf("no route to %s", target)
	}
	return nil
}
