// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/netip"
	"testing"
	"time"

	"github.com/openchami/openchami-operator/internal/networkprobe"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	probeTestSubnet = "10.0.0.0/24"
	probeTestHost   = "host"
)

// quietLogger discards probe-runner log output so tests stay readable.
func quietLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func TestRunOnce_AttachmentLostRevokesPreviouslyTrueLabels(t *testing.T) {
	r := newRunner("alpha")
	r.provision = mustParseTarget(t, probeTestSubnet, probeTestHost, "443", "")
	cs := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: r.nodeName,
		Labels: map[string]string{networkprobe.ReadyLabel("other", "provision"): probeLabelTrue}}})
	r.patchLabels = func(ctx context.Context, node string, labels map[string]string) error {
		return patchNodeLabels(ctx, cs, node, labels)
	}
	for _, attached := range []bool{true, false} {
		r.attachmentCheck = func(netip.Prefix) error {
			if !attached {
				return errors.New("management address only, despite default route")
			}
			return nil
		}
		dialed := false
		r.dialCheck = func(string, time.Duration) error { dialed = true; return nil }
		if err := r.runOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		node, err := cs.CoreV1().Nodes().Get(context.Background(), r.nodeName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if node.Labels[probeLabelKey("alpha", "provision")] != boolStr(attached) {
			t.Fatalf("labels=%v", node.Labels)
		}
		wantRevision := ""
		if attached {
			wantRevision = r.provision.revision
		}
		if node.Labels[networkprobe.RevisionLabel("alpha", "provision")] != wantRevision {
			t.Fatal("revision not patched with readiness")
		}
		if node.Labels[networkprobe.ReadyLabel("other", "provision")] != probeLabelTrue {
			t.Fatal("other cluster label changed")
		}
		if dialed != attached {
			t.Fatal("TCP must run only after attachment passes")
		}
		if !attached {
			legacyLabels := map[string]string{probeLabelKey("alpha", "provision"): probeLabelTrue}
			if err := patchNodeLabels(context.Background(), cs, r.nodeName, legacyLabels); err != nil {
				t.Fatal(err)
			}
			legacy, err := cs.CoreV1().Nodes().Get(context.Background(), r.nodeName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if legacy.Labels[networkprobe.RevisionLabel("alpha", "provision")] == r.provision.revision {
				t.Fatal("late legacy ready=true restored attestation")
			}
		}
	}
}

func TestParseTarget_InvalidSettings(t *testing.T) {
	for _, tc := range []struct{ subnet, host, port, timeout string }{
		{"0.0.0.0/0", "", "", ""}, {"::/0", "", "", ""},
		{probeTestSubnet, "", "443", ""}, {probeTestSubnet, probeTestHost, "0", ""},
		{probeTestSubnet, probeTestHost, "65536", ""}, {probeTestSubnet, probeTestHost, "-1", ""},
		{probeTestSubnet, "", "", "0s"}, {probeTestSubnet, "", "", "-1s"},
	} {
		target := parseTarget(tc.subnet, tc.host, tc.port, tc.timeout)
		if target == nil || target.valid {
			t.Fatalf("invalid target accepted: %+v", tc)
		}
	}
}

func TestRunnerFromEnv_RevisionAndInterval(t *testing.T) {
	t.Setenv(envClusterName, "alpha")
	t.Setenv(envNodeName, "node1")
	t.Setenv(envProvisionSubnet, probeTestSubnet)
	t.Setenv(envProvisionHost, "")
	t.Setenv(envProvisionPort, "")
	t.Setenv(envProvisionTimeout, "")
	t.Setenv(envBMCSubnet, "")
	t.Setenv(envIntervalSeconds, "30")
	for _, revision := range []string{"", "old-algorithm", (networkprobe.Target{Subnet: probeTestSubnet}).Revision()} {
		t.Setenv("PROBE_PROVISION_REVISION", revision)
		r, err := runnerFromEnv(fake.NewSimpleClientset(), quietLogger())
		if err != nil {
			t.Fatal(err)
		}
		wantValid := revision == r.provision.revision
		if r.provision.valid != wantValid {
			t.Fatalf("revision %q: valid=%v", revision, r.provision.valid)
		}
	}
	for _, interval := range []string{"0", "-1", "invalid", "9223372036854775807"} {
		t.Setenv(envIntervalSeconds, interval)
		if _, err := runnerFromEnv(fake.NewSimpleClientset(), quietLogger()); err == nil {
			t.Fatalf("interval %q accepted", interval)
		}
	}
}

// newRunner returns a probeRunner pre-wired with no-op hooks so each test
// can replace whichever hook it needs without re-stating the boilerplate.
func newRunner(cp string) *probeRunner {
	return &probeRunner{
		clusterName:     cp,
		nodeName:        "node1",
		interval:        time.Second,
		logger:          quietLogger(),
		attachmentCheck: func(netip.Prefix) error { return nil },
		dialCheck:       func(string, time.Duration) error { return nil },
		patchLabels:     func(_ context.Context, _ string, _ map[string]string) error { return nil },
		now:             time.Now,
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
	r.provision = mustParseTarget(t, probeTestSubnet, "10.0.0.5", "80", "1s")

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

func TestRunOnce_ProvisionAttachmentFailsLabelsFalse(t *testing.T) {
	r := newRunner("alpha")
	r.provision = mustParseTarget(t, probeTestSubnet, "", "", "")
	r.attachmentCheck = func(netip.Prefix) error { return errors.New("listing host interfaces failed") }

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
	r.provision = mustParseTarget(t, probeTestSubnet, "10.0.0.5", "80", "1s")
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
	tc := parseTarget(probeTestSubnet, probeTestHost, "", "")
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
	tc := mustParseTarget(t, probeTestSubnet, "", "", "")
	if tc.timeout != defaultDialTimeout {
		t.Errorf("expected default timeout %s, got %s", defaultDialTimeout, tc.timeout)
	}
}
