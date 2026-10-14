//go:build linux

// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"net/netip"
	"syscall"

	"github.com/openchami/openchami-operator/internal/networkprobe"
	"github.com/vishvananda/netlink"
)

// defaultAttachmentCheck reads the host namespace's interfaces and addresses.
// A route via a gateway cannot substitute for local subnet attachment.
func defaultAttachmentCheck(target netip.Prefix) error {
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("listing host interfaces: %w", err)
	}
	interfaces := make([]networkprobe.Interface, 0, len(links))
	for _, link := range links {
		addresses, listErr := netlink.AddrList(link, netlink.FAMILY_ALL)
		if listErr != nil {
			return fmt.Errorf("listing addresses for %s: %w", link.Attrs().Name, listErr)
		}
		iface := networkprobe.Interface{Flags: link.Attrs().Flags}
		for _, addr := range addresses {
			if addr.IPNet == nil {
				continue
			}
			ip, ok := netip.AddrFromSlice(addr.IP)
			if !ok {
				continue
			}
			iface.Addresses = append(iface.Addresses, networkprobe.Address{
				IP:       ip.Unmap(),
				Unusable: addr.Flags&(syscall.IFA_F_TENTATIVE|syscall.IFA_F_DADFAILED|syscall.IFA_F_DEPRECATED) != 0,
			})
		}
		interfaces = append(interfaces, iface)
	}
	if networkprobe.Attached(target, interfaces) {
		return nil
	}
	return fmt.Errorf("no usable address on an up non-loopback host interface in %s", target)
}
