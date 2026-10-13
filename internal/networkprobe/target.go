// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

// Package networkprobe shares target validation and result revisions between
// admission, the operator, and the probe binary. It has no Kubernetes dependency.
package networkprobe

import (
	"crypto/sha256"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// AlgorithmVersion must change whenever the eligibility predicate changes.
const AlgorithmVersion = "host-attachment-v1"

// DefaultTimeout is used when optional TCP validation has no timeout override.
const DefaultTimeout = 5 * time.Second

// Target is the configured subnet and optional TCP validation endpoint.
type Target struct {
	Subnet, Host, Timeout string
	Port                  int
}

// Prefix validates a target network and returns its canonical prefix.
func Prefix(subnet string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(subnet)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("invalid subnet %q: %w", subnet, err)
	}
	p = p.Masked()
	if p.Bits() == 0 || !p.Addr().IsGlobalUnicast() || p.Addr().IsLoopback() || p.Addr().Is4In6() {
		return netip.Prefix{}, fmt.Errorf("subnet %q must be a non-default unicast network", subnet)
	}
	return p, nil
}

// Validate rejects configurations which cannot safely establish eligibility.
func (t Target) Validate() error {
	if _, err := Prefix(t.Subnet); err != nil {
		return err
	}
	if (t.Host == "") != (t.Port == 0) || (t.Host != "" && strings.TrimSpace(t.Host) != t.Host) {
		return fmt.Errorf("validateHost and validatePort must be supplied together")
	}
	if t.Port < 0 || t.Port > 65535 {
		return fmt.Errorf("validatePort must be between 1 and 65535 when supplied")
	}
	if t.Timeout != "" {
		d, err := time.ParseDuration(t.Timeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("validateTimeout must be a positive duration")
		}
	}
	return nil
}

// Revision binds a result to both configuration and eligibility algorithm.
// Invalid configurations still have a deterministic revision, but cannot pass.
func (t Target) Revision() string {
	if p, err := Prefix(t.Subnet); err == nil {
		t.Subnet = p.String()
	}
	timeout := DefaultTimeout.String()
	if d, err := time.ParseDuration(t.Timeout); err == nil {
		timeout = d.String()
	} else if t.Timeout != "" {
		timeout = t.Timeout
	}
	digest := sha256.Sum256(fmt.Appendf(nil, "%q|%q|%q|%d|%q", AlgorithmVersion, t.Subnet, t.Host, t.Port, timeout))
	return fmt.Sprintf("%x", digest[:16])
}

// ReadyLabel is the existing cluster-scoped readiness label.
func ReadyLabel(cluster, kind string) string {
	return fmt.Sprintf("openchami.org/%s-%s-network-ready", cluster, kind)
}

// RevisionLabel accompanies readiness in the same Node patch.
func RevisionLabel(cluster, kind string) string {
	return fmt.Sprintf("openchami.org/%s-%s-probe-revision", cluster, kind)
}
