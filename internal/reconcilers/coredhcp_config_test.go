// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"strings"
	"testing"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

const (
	testLeaseStartA = "10.0.0.10"
	testLeaseEndA   = "10.0.0.20"
)

// TestRenderCoreDHCPConfig pins the YAML the operator hands to coredhcp.
// Coredhcp's binary parses this with strict YAML and a documented schema;
// a regression here would surface as a fatal config-load error in the pod
// (which is exactly the bug this rendering was added to fix).
func TestRenderCoreDHCPConfig(t *testing.T) {
	cluster := newCluster("alpha")
	cluster.Spec.Services.CoreDHCP = openchamiv1alpha1.CoreDHCPSpec{
		Enabled: true,
		LeaseRanges: []openchamiv1alpha1.DHCPLeaseRange{{
			Subnet: "192.168.100.0/24",
			Start:  "192.168.100.100",
			End:    "192.168.100.200",
		}},
		KnownLeaseDuration:   "2h",
		UnknownLeaseDuration: "10m",
	}

	got, err := renderCoreDHCPConfig(cluster)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	// Lock the pieces the binary actually parses. Whitespace is intentional
	// — coredhcp's YAML loader is strict about indentation under `plugins:`.
	wantContains := []string{
		"server4:",
		`    - "0.0.0.0:67"`,
		"  plugins:",
		"    - lease_time: 2h",
		"    - server_id: 192.168.100.1",
		"    - router: 192.168.100.1",
		"    - netmask: 255.255.255.0",
		"    - range: /tmp/coredhcp-leases.txt 192.168.100.100 192.168.100.200 10m",
	}
	for _, w := range wantContains {
		if !strings.Contains(got, w) {
			t.Errorf("rendered config missing %q\n--- output ---\n%s", w, got)
		}
	}
}

func TestRenderCoreDHCPConfig_DefaultsLeaseDurations(t *testing.T) {
	cluster := newCluster("alpha")
	cluster.Spec.Services.CoreDHCP = openchamiv1alpha1.CoreDHCPSpec{
		Enabled: true,
		LeaseRanges: []openchamiv1alpha1.DHCPLeaseRange{{
			Subnet: "10.0.0.0/24",
			Start:  testLeaseStartA,
			End:    testLeaseEndA,
		}},
	}
	got, err := renderCoreDHCPConfig(cluster)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(got, "lease_time: 1h") {
		t.Errorf("expected default known lease_time=1h, got:\n%s", got)
	}
	if !strings.Contains(got, " 10m\n") && !strings.Contains(got, " 5m\n") {
		// The webhook default for UnknownLeaseDuration is 5m; with no
		// override that is what should land in the range plugin.
		t.Errorf("expected default unknown lease duration in range, got:\n%s", got)
	}
}

func TestRenderCoreDHCPConfig_RejectsEmptyLeaseRanges(t *testing.T) {
	cluster := newCluster("alpha")
	cluster.Spec.Services.CoreDHCP = openchamiv1alpha1.CoreDHCPSpec{Enabled: true}
	if _, err := renderCoreDHCPConfig(cluster); err == nil {
		t.Fatal("expected error when leaseRanges is empty")
	}
}

func TestRenderCoreDHCPConfig_RejectsInvalidSubnet(t *testing.T) {
	cluster := newCluster("alpha")
	cluster.Spec.Services.CoreDHCP = openchamiv1alpha1.CoreDHCPSpec{
		Enabled: true,
		LeaseRanges: []openchamiv1alpha1.DHCPLeaseRange{{
			Subnet: "not-a-cidr",
			Start:  testLeaseStartA,
			End:    testLeaseEndA,
		}},
	}
	if _, err := renderCoreDHCPConfig(cluster); err == nil {
		t.Fatal("expected error for invalid CIDR")
	}
}

func TestRenderCoreDHCPConfig_NotesIgnoredAdditionalRanges(t *testing.T) {
	cluster := newCluster("alpha")
	cluster.Spec.Services.CoreDHCP = openchamiv1alpha1.CoreDHCPSpec{
		Enabled: true,
		LeaseRanges: []openchamiv1alpha1.DHCPLeaseRange{
			{Subnet: "10.0.0.0/24", Start: testLeaseStartA, End: testLeaseEndA},
			{Subnet: "10.0.1.0/24", Start: "10.0.1.10", End: "10.0.1.20"},
		},
	}
	got, err := renderCoreDHCPConfig(cluster)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(got, "additional leaseRanges ignored") {
		t.Errorf("expected warning comment about additional leaseRanges, got:\n%s", got)
	}
}
