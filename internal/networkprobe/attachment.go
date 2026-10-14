// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package networkprobe

import (
	"net"
	"net/netip"
)

// Address is a local address with its kernel usability state.
type Address struct {
	IP       netip.Addr
	Unusable bool
}

// Interface is the platform-independent input to host attachment evaluation.
type Interface struct {
	Flags     net.Flags
	Addresses []Address
}

// Attached reports whether an up, non-loopback interface has a usable unicast
// address in the target network. Interface names and routing tables are irrelevant.
func Attached(target netip.Prefix, interfaces []Interface) bool {
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		for _, addr := range iface.Addresses {
			ip := addr.IP
			if !addr.Unusable && ip.IsGlobalUnicast() && !ip.IsLoopback() && target.Contains(ip) && usableIPv4(target, ip) {
				return true
			}
		}
	}
	return false
}

// IPv4 networks wider than /31 reserve their network and broadcast addresses.
func usableIPv4(target netip.Prefix, ip netip.Addr) bool {
	if !ip.Is4() || target.Bits() >= 31 {
		return true
	}
	if ip == target.Masked().Addr() {
		return false
	}
	bytes := ip.As4()
	value := uint32(bytes[0])<<24 | uint32(bytes[1])<<16 | uint32(bytes[2])<<8 | uint32(bytes[3])
	hostMask := uint32(1)<<(32-target.Bits()) - 1
	return value&hostMask != hostMask
}
