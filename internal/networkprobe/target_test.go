// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package networkprobe

import (
	"net"
	"net/netip"
	"testing"
)

const (
	testSubnet  = "10.0.0.0/24"
	testHost    = "host"
	testLocalIP = "10.0.0.5"
)

func TestTargetValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target Target
		valid  bool
	}{
		{"subnet only", Target{Subnet: testSubnet}, true},
		{"tcp", Target{Subnet: testSubnet, Host: testHost, Port: 443, Timeout: "3s"}, true},
		{"ipv6", Target{Subnet: "fd00::/64"}, true},
		{"malformed", Target{Subnet: "not-a-cidr"}, false},
		{"default v4", Target{Subnet: "0.0.0.0/0"}, false},
		{"default v6", Target{Subnet: "::/0"}, false},
		{"loopback", Target{Subnet: "127.0.0.0/8"}, false},
		{"multicast", Target{Subnet: "224.0.0.0/4"}, false},
		{"unspecified", Target{Subnet: "::/64"}, false},
		{"host only", Target{Subnet: testSubnet, Host: testHost}, false},
		{"port only", Target{Subnet: testSubnet, Port: 443}, false},
		{"negative port", Target{Subnet: testSubnet, Host: testHost, Port: -1}, false},
		{"large port", Target{Subnet: testSubnet, Host: testHost, Port: 65536}, false},
		{"blank host", Target{Subnet: testSubnet, Host: " ", Port: 443}, false},
		{"zero timeout", Target{Subnet: testSubnet, Timeout: "0s"}, false},
		{"negative timeout", Target{Subnet: testSubnet, Timeout: "-1s"}, false},
		{"bad timeout", Target{Subnet: testSubnet, Timeout: "later"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.target.Validate(); (err == nil) != tc.valid {
				t.Fatalf("Validate()=%v, valid=%v", err, tc.valid)
			}
		})
	}
}

func TestRevisionNormalizesAndTracksConfiguration(t *testing.T) {
	base := Target{Subnet: "10.0.0.1/24", Host: testHost, Port: 443}
	equivalent := Target{Subnet: testSubnet, Host: testHost, Port: 443, Timeout: "5000ms"}
	if base.Revision() != equivalent.Revision() {
		t.Fatal("equivalent targets have different revisions")
	}
	for _, changed := range []Target{
		{Subnet: "10.0.1.0/24", Host: testHost, Port: 443},
		{Subnet: base.Subnet, Host: "other", Port: 443},
		{Subnet: base.Subnet, Host: testHost, Port: 80},
		{Subnet: base.Subnet, Host: testHost, Port: 443, Timeout: "1s"},
	} {
		if base.Revision() == changed.Revision() {
			t.Fatalf("configuration change did not change revision: %+v", changed)
		}
	}
	if len(base.Revision()) > 63 {
		t.Fatal("revision exceeds label value limit")
	}
}

func TestAttached(t *testing.T) {
	v4 := netip.MustParsePrefix(testSubnet)
	for _, tc := range []struct {
		name, ip       string
		flags          net.Flags
		unusable, want bool
		subnet         netip.Prefix
	}{
		{"local subnet", testLocalIP, net.FlagUp, false, true, v4},
		{"management only despite default route", "192.168.1.5", net.FlagUp, false, false, v4},
		{"down", testLocalIP, 0, false, false, v4},
		{"loopback interface", testLocalIP, net.FlagUp | net.FlagLoopback, false, false, v4},
		{"unusable kernel state", testLocalIP, net.FlagUp, true, false, v4},
		{"network address", "10.0.0.0", net.FlagUp, false, false, v4},
		{"broadcast address", "10.0.0.255", net.FlagUp, false, false, v4},
		{"wrong family", "fd00::5", net.FlagUp, false, false, v4},
		{"vlan or bond", testLocalIP, net.FlagUp | net.FlagBroadcast, false, true, v4},
		{"ipv6", "fd00::5", net.FlagUp, false, true, netip.MustParsePrefix("fd00::/64")},
		{"ipv6 loopback", "::1", net.FlagUp, false, false, netip.MustParsePrefix("::/128")},
		{"unspecified", "0.0.0.0", net.FlagUp, false, false, netip.MustParsePrefix("0.0.0.0/32")},
		{"multicast", "224.0.0.5", net.FlagUp, false, false, netip.MustParsePrefix("224.0.0.0/24")},
		{"point to point", "10.0.0.0", net.FlagUp, false, true, netip.MustParsePrefix("10.0.0.0/31")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			interfaces := []Interface{{Flags: tc.flags, Addresses: []Address{{IP: netip.MustParseAddr(tc.ip), Unusable: tc.unusable}}}}
			if got := Attached(tc.subnet, interfaces); got != tc.want {
				t.Fatalf("Attached=%v, want %v", got, tc.want)
			}
		})
	}
	if Attached(v4, nil) {
		t.Fatal("no interfaces must fail")
	}
}
