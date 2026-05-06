// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// quietLogger discards probe-runner log output so tests stay readable.
func quietLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

// newRunner returns a probeRunner pre-wired with no-op hooks so each test
// can replace whichever hook it needs without re-stating the boilerplate.
func newRunner(cluster string) *probeRunner {
	return &probeRunner{
		clusterName: cluster,
		nodeName:    "node1",
		interval:    time.Second,
		logger:      quietLogger(),
		routeCheck:  func(net.IP) error { return nil },
		dialCheck:   func(string, time.Duration) error { return nil },
		patchLabels: func(_ context.Context, _ string, _ map[string]string) error { return nil },
		now:         time.Now,
	}
}

func mustParseTarget(t *testing.T, subnet, host, port, timeout string) *targetConfig {
	t.Helper()
	tc := parseTarget(subnet, host, port, timeout)
	if tc == nil {
		t.Fatalf("parseTarget(%q,...) returned nil", subnet)
	}
	if !tc.valid {
		t.Fatalf("parseTarget(%q,...) marked invalid: %s", subnet, tc.parseErr)
	}
	return tc
}

func TestRunOnce_ProvisionReachableLabelsTrue(t *testing.T) {
	r := newRunner("alpha")
	r.provision = mustParseTarget(t, "10.0.0.0/24", "10.0.0.5", "80", "1s")

	var captured map[string]string
	r.patchLabels = func(_ context.Context, node string, labels map[string]string) error {
		if node != "node1" {
			t.Errorf("expected node node1, got %q", node)
		}
		captured = labels
		return nil
	}

	if err := r.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}

	want := probeLabelKey("alpha", "provision")
	if got := captured[want]; got != probeLabelTrue {
		t.Errorf("expected %s=true, got %q", want, got)
	}
	if _, found := captured[probeLabelKey("alpha", "bmc")]; found {
		t.Errorf("bmc label set when bmc target unconfigured: %+v", captured)
	}
}

func TestRunOnce_ProvisionRouteFailsLabelsFalse(t *testing.T) {
	r := newRunner("alpha")
	r.provision = mustParseTarget(t, "10.0.0.0/24", "", "", "")
	r.routeCheck = func(net.IP) error { return errors.New("no route") }

	var captured map[string]string
	r.patchLabels = func(_ context.Context, _ string, labels map[string]string) error {
		captured = labels
		return nil
	}

	if err := r.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}

	if got := captured[probeLabelKey("alpha", "provision")]; got != probeLabelFalse {
		t.Errorf("expected provision label false, got %q", got)
	}
}

func TestRunOnce_ValidateHostUnreachableLabelsFalse(t *testing.T) {
	r := newRunner("alpha")
	r.provision = mustParseTarget(t, "10.0.0.0/24", "10.0.0.5", "80", "1s")
	r.dialCheck = func(string, time.Duration) error { return errors.New("connection refused") }

	var captured map[string]string
	r.patchLabels = func(_ context.Context, _ string, labels map[string]string) error {
		captured = labels
		return nil
	}

	if err := r.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}

	if got := captured[probeLabelKey("alpha", "provision")]; got != probeLabelFalse {
		t.Errorf("expected provision label false on dial failure, got %q", got)
	}
}

func TestRunOnce_OnlyBMCConfigured_NoProvisionLabel(t *testing.T) {
	r := newRunner("beta")
	r.bmc = mustParseTarget(t, "10.1.0.0/24", "", "", "")

	var captured map[string]string
	r.patchLabels = func(_ context.Context, _ string, labels map[string]string) error {
		captured = labels
		return nil
	}

	if err := r.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}

	if _, found := captured[probeLabelKey("beta", "provision")]; found {
		t.Errorf("provision label set when provision target unconfigured: %+v", captured)
	}
	if got := captured[probeLabelKey("beta", "bmc")]; got != probeLabelTrue {
		t.Errorf("expected bmc label true, got %q", got)
	}
}

func TestRunOnce_NoTargetsNoPatch(t *testing.T) {
	r := newRunner("gamma")
	called := false
	r.patchLabels = func(_ context.Context, _ string, _ map[string]string) error {
		called = true
		return nil
	}
	if err := r.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if called {
		t.Errorf("expected patchLabels skipped when no targets configured")
	}
}

func TestPatchNodeLabels_StrategicMergePreservesOtherLabels(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "n1",
			Labels: map[string]string{"existing": "keep"},
		},
	})
	wantKey := probeLabelKey("alpha", "provision")
	if err := patchNodeLabels(context.Background(), cs, "n1", map[string]string{
		wantKey: probeLabelTrue,
	}); err != nil {
		t.Fatalf("patchNodeLabels: %v", err)
	}

	got, err := cs.CoreV1().Nodes().Get(context.Background(), "n1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Labels["existing"] != "keep" {
		t.Errorf("existing label clobbered: %+v", got.Labels)
	}
	if got.Labels[wantKey] != probeLabelTrue {
		t.Errorf("expected %s=true, got %+v", wantKey, got.Labels)
	}
}

func TestParseTarget_Empty(t *testing.T) {
	if tc := parseTarget("", "", "", ""); tc != nil {
		t.Errorf("expected nil target for empty subnet, got %+v", tc)
	}
}

func TestParseTarget_InvalidCIDR_MarksInvalid(t *testing.T) {
	tc := parseTarget("999.999.999.999/32", "", "", "")
	if tc == nil {
		t.Fatalf("expected non-nil target with valid=false")
	}
	if tc.valid {
		t.Errorf("expected valid=false for malformed CIDR")
	}
	if tc.parseErr == "" {
		t.Errorf("expected parseErr to be populated")
	}
}

func TestParseTarget_HostWithoutPort_MarksInvalid(t *testing.T) {
	tc := parseTarget("10.0.0.0/24", "host", "", "")
	if tc == nil || tc.valid {
		t.Errorf("expected valid=false for host without port: %+v", tc)
	}
}

// TestRunOnce_InvalidProvisionTarget asserts that an unparseable subnet
// surfaces as label=false rather than crashing the binary; this is the
// behaviour E2E-06 relies on to drive the NoEligibleNodes condition.
func TestRunOnce_InvalidProvisionTarget(t *testing.T) {
	r := newRunner("delta")
	r.provision = parseTarget("999.999.999.999/32", "", "", "")

	var captured map[string]string
	r.patchLabels = func(_ context.Context, _ string, labels map[string]string) error {
		captured = labels
		return nil
	}
	if err := r.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if got := captured[probeLabelKey("delta", "provision")]; got != probeLabelFalse {
		t.Errorf("expected provision label false for invalid subnet, got %q", got)
	}
}

func TestParseTarget_TimeoutDefault(t *testing.T) {
	tc := mustParseTarget(t, "10.0.0.0/24", "", "", "")
	if tc.timeout != defaultDialTimeout {
		t.Errorf("expected default timeout %s, got %s", defaultDialTimeout, tc.timeout)
	}
}
