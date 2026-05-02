/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

// Package main implements the openchami network probe.
//
// The probe runs as a per-node DaemonSet (see
// internal/reconcilers/networkprobe.go). At each interval it:
//
//  1. Inspects the node's routing table to confirm a route exists into the
//     configured provision subnet (and, optionally, the BMC subnet).
//     Implementation: github.com/vishvananda/netlink RouteGet on a sample
//     IP from each subnet.
//  2. Optionally TCP-dials a validation host:port within each subnet using
//     net.DialTimeout.
//  3. Patches the local Node object with two labels per cluster:
//     openchami.org/{cluster}/provision-network-ready = "true" | "false"
//     openchami.org/{cluster}/bmc-network-ready       = "true" | "false"
//     Uses the in-cluster ServiceAccount (network-probe ClusterRole grants
//     get;patch on nodes) and a strategic-merge patch.
//  4. Sleeps PROBE_INTERVAL_SECONDS and repeats.
//
// TODO(phase06): replace this stub with the loop above. The full
// implementation requires github.com/vishvananda/netlink (Linux-only). The
// stub is sufficient for the operator unit tests and the Phase 6 check
// script; a real probe loop is wired up in a follow-up.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintf(os.Stderr,
		"openchami probe stub for cluster=%q node=%q\n",
		os.Getenv("PROBE_CLUSTER_NAME"),
		os.Getenv("NODE_NAME"),
	)
	// Exit 0 immediately so the DaemonSet's restart policy does not enter a
	// CrashLoopBackOff while the rest of the operator wiring is exercised.
	os.Exit(0)
}
