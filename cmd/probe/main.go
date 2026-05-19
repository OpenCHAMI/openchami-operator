// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

// Package main implements the openchami network probe.
//
// The probe runs as a per-node DaemonSet (see
// internal/reconcilers/networkprobe.go). At each interval it:
//
//  1. Inspects the node's routing table to confirm a route exists into the
//     configured provision subnet (and, optionally, the BMC subnet).
//     Implementation: github.com/vishvananda/netlink RouteGet on a sample
//     IP from each subnet (Linux only).
//
//  2. Optionally TCP-dials a validation host:port within each subnet using
//     net.DialTimeout.
//
//  3. Patches the local Node object with two labels per cluster:
//
//     openchami.org/{cluster}-provision-network-ready = "true" | "false"
//     openchami.org/{cluster}-bmc-network-ready       = "true" | "false"
//
//     using the in-cluster ServiceAccount (network-probe ClusterRole grants
//     get;patch on nodes) and a strategic-merge patch.
//
//  4. Sleeps PROBE_INTERVAL_SECONDS and repeats.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	defaultIntervalSeconds = 300
	defaultDialTimeout     = 5 * time.Second
	probeLabelTrue         = "true"
	probeLabelFalse        = "false"

	envClusterName     = "PROBE_CLUSTER_NAME"
	envIntervalSeconds = "PROBE_INTERVAL_SECONDS"
	envNodeName        = "NODE_NAME"

	envProvisionSubnet  = "PROBE_PROVISION_SUBNET"
	envProvisionHost    = "PROBE_PROVISION_HOST"
	envProvisionPort    = "PROBE_PROVISION_PORT"
	envProvisionTimeout = "PROBE_PROVISION_TIMEOUT"

	envBMCSubnet  = "PROBE_BMC_SUBNET"
	envBMCHost    = "PROBE_BMC_HOST"
	envBMCPort    = "PROBE_BMC_PORT"
	envBMCTimeout = "PROBE_BMC_TIMEOUT"
)

// targetConfig captures the resolved spec for a single probe target. nil means
// the target was not configured in spec and should be skipped. sampleIP is the
// IP RouteGet is called against — the network address is fine because the
// kernel only uses it to look up the relevant route, not to deliver a packet.
//
// When the operator-supplied env vars produce a config that cannot be
// evaluated (malformed CIDR, port out of range, partial validate-host
// settings), parseTarget returns a config with valid=false and parseErr set
// to the reason; checkTarget short-circuits to label=false rather than
// crashing the binary. This preserves the SubReconciler contract that the
// DaemonSet "stays up, but reports no eligible nodes" — which is what the
// NoEligibleNodes condition path in networkprobe.go expects.
type targetConfig struct {
	subnet   *net.IPNet
	sampleIP net.IP
	host     string
	port     int
	timeout  time.Duration
	valid    bool
	parseErr string
}

// probeRunner owns one probe pod's per-tick logic. Behaviour-bearing fields
// (routeCheck/dialCheck/patchLabels/now) are exported as struct fields so
// tests can inject deterministic stand-ins without having to spin up a real
// netlink socket or kube-apiserver.
type probeRunner struct {
	clusterName string
	nodeName    string
	interval    time.Duration
	provision   *targetConfig
	bmc         *targetConfig
	logger      *log.Logger

	routeCheck  func(net.IP) error
	dialCheck   func(addr string, timeout time.Duration) error
	patchLabels func(ctx context.Context, node string, labels map[string]string) error
	now         func() time.Time
}

// run loops forever (or until ctx is cancelled), calling runOnce every
// intervalSeconds. The first tick fires immediately so a freshly-started
// pod reports its result without waiting a full interval.
func (r *probeRunner) run(ctx context.Context) error {
	if err := r.runOnce(ctx); err != nil {
		r.logger.Printf("runOnce: %v", err)
	}
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := r.runOnce(ctx); err != nil {
				r.logger.Printf("runOnce: %v", err)
			}
		}
	}
}

// runOnce evaluates each configured target and patches the node's labels in
// a single API call. Returns the patch error so callers can log; per-target
// failures (route or TCP) are encoded as label=false rather than error.
func (r *probeRunner) runOnce(ctx context.Context) error {
	labels := map[string]string{}
	if r.provision != nil {
		labels[probeLabelKey(r.clusterName, "provision")] = boolStr(r.checkTarget("provision", r.provision))
	}
	if r.bmc != nil {
		labels[probeLabelKey(r.clusterName, "bmc")] = boolStr(r.checkTarget("bmc", r.bmc))
	}
	if len(labels) == 0 {
		r.logger.Printf("no targets configured; skipping label patch")
		return nil
	}
	return r.patchLabels(ctx, r.nodeName, labels)
}

// checkTarget runs the route-table check first, then (if configured) a TCP
// dial check. Either failure flips the resulting label to "false". The kind
// argument is only used in log lines. A target whose env vars failed to
// parse (valid=false) reports false unconditionally.
func (r *probeRunner) checkTarget(kind string, t *targetConfig) bool {
	if !t.valid {
		r.logger.Printf("%s target unusable: %s", kind, t.parseErr)
		return false
	}
	if err := r.routeCheck(t.sampleIP); err != nil {
		r.logger.Printf("%s route check failed for %s: %v", kind, t.subnet, err)
		return false
	}
	if t.host == "" {
		return true
	}
	addr := net.JoinHostPort(t.host, strconv.Itoa(t.port))
	if err := r.dialCheck(addr, t.timeout); err != nil {
		r.logger.Printf("%s dial check failed for %s: %v", kind, addr, err)
		return false
	}
	return true
}

func probeLabelKey(cp, kind string) string {
	return fmt.Sprintf("openchami.org/%s-%s-network-ready", cp, kind)
}

func boolStr(b bool) string {
	if b {
		return probeLabelTrue
	}
	return probeLabelFalse
}

// parseTarget builds a targetConfig from the (subnet, host, port, timeout)
// quartet of env vars. Returns nil when subnet is empty (target not
// configured). When subnet is non-empty but the supplied values are
// malformed, returns a non-nil config with valid=false and parseErr
// populated; the caller is expected to keep running and report label=false
// for that target.
func parseTarget(subnet, host, port, timeout string) *targetConfig {
	if subnet == "" {
		return nil
	}
	_, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return &targetConfig{
			parseErr: fmt.Sprintf("parsing subnet %q: %v", subnet, err),
		}
	}
	t := &targetConfig{
		subnet:   ipNet,
		sampleIP: ipNet.IP,
		timeout:  defaultDialTimeout,
	}
	if host != "" {
		if port == "" {
			return &targetConfig{
				parseErr: fmt.Sprintf("validate host %q without port", host),
			}
		}
		p, perr := strconv.Atoi(port)
		if perr != nil {
			return &targetConfig{
				parseErr: fmt.Sprintf("parsing port %q: %v", port, perr),
			}
		}
		if p <= 0 || p > 65535 {
			return &targetConfig{
				parseErr: fmt.Sprintf("port %d out of range", p),
			}
		}
		t.host = host
		t.port = p
	}
	if timeout != "" {
		d, terr := time.ParseDuration(timeout)
		if terr != nil {
			return &targetConfig{
				parseErr: fmt.Sprintf("parsing timeout %q: %v", timeout, terr),
			}
		}
		t.timeout = d
	}
	t.valid = true
	return t
}

// runnerFromEnv builds a probeRunner from the operator-injected env vars.
// On success the runner has its routeCheck/dialCheck/patchLabels/now hooks
// wired to production implementations; tests construct the runner directly.
func runnerFromEnv(cs kubernetes.Interface, logger *log.Logger) (*probeRunner, error) {
	cp := os.Getenv(envClusterName)
	if cp == "" {
		return nil, fmt.Errorf("%s required", envClusterName)
	}
	node := os.Getenv(envNodeName)
	if node == "" {
		return nil, fmt.Errorf("%s required", envNodeName)
	}
	intervalSec := defaultIntervalSeconds
	if v := os.Getenv(envIntervalSeconds); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("parsing %s=%q: %w", envIntervalSeconds, v, err)
		}
		if n > 0 {
			intervalSec = n
		}
	}
	provision := parseTarget(
		os.Getenv(envProvisionSubnet),
		os.Getenv(envProvisionHost),
		os.Getenv(envProvisionPort),
		os.Getenv(envProvisionTimeout),
	)
	bmc := parseTarget(
		os.Getenv(envBMCSubnet),
		os.Getenv(envBMCHost),
		os.Getenv(envBMCPort),
		os.Getenv(envBMCTimeout),
	)
	if provision == nil && bmc == nil {
		return nil, errors.New("no probe targets configured (set PROBE_PROVISION_SUBNET and/or PROBE_BMC_SUBNET)")
	}
	return &probeRunner{
		clusterName: cp,
		nodeName:    node,
		interval:    time.Duration(intervalSec) * time.Second,
		provision:   provision,
		bmc:         bmc,
		logger:      logger,
		routeCheck:  defaultRouteCheck,
		dialCheck:   defaultDialCheck,
		patchLabels: func(ctx context.Context, n string, labels map[string]string) error {
			return patchNodeLabels(ctx, cs, n, labels)
		},
		now: time.Now,
	}, nil
}

// defaultDialCheck performs a single TCP dial with the given timeout and
// closes the connection immediately on success. Used when validate-host is
// configured for a target.
func defaultDialCheck(addr string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// patchNodeLabels issues a strategic-merge patch that sets the supplied
// labels on the named Node and leaves the rest of the object untouched. The
// label keys come from probeLabelKey, so unrelated labels (set by the kubelet,
// the scheduler, or a sibling cluster's probe) survive the patch.
func patchNodeLabels(ctx context.Context, cs kubernetes.Interface, node string, labels map[string]string) error {
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"labels": labels},
	})
	if err != nil {
		return fmt.Errorf("marshalling label patch: %w", err)
	}
	_, err = cs.CoreV1().Nodes().Patch(ctx, node, types.StrategicMergePatchType, body, metav1.PatchOptions{
		FieldManager: "openchami-network-probe",
	})
	if err != nil {
		return fmt.Errorf("patching node %s: %w", node, err)
	}
	return nil
}

func main() {
	logger := log.New(os.Stderr, "probe ", log.LstdFlags|log.LUTC)
	cfg, err := rest.InClusterConfig()
	if err != nil {
		logger.Fatalf("in-cluster config: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.Fatalf("kubernetes client: %v", err)
	}
	r, err := runnerFromEnv(cs, logger)
	if err != nil {
		logger.Fatalf("runner from env: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger.Printf("starting probe cluster=%s node=%s interval=%s", r.clusterName, r.nodeName, r.interval)
	if err := r.run(ctx); err != nil {
		logger.Fatalf("probe loop: %v", err)
	}
}
