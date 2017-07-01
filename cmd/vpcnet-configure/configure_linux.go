package main

import (
	"fmt"
	"net"
	"syscall"

	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
)

const (
	mtu = 1500
)

func configureInterface(bridgeName, mac string, ip *net.IPNet) error {
	// Find the interface AWS attached
	ifs, err := net.Interfaces()
	if err != nil {
		return err
	}
	var hostIf *net.Interface
	for _, i := range ifs {
		if i.HardwareAddr.String() == mac {
			hostIf = &i
		}
	}
	if hostIf == nil {
		return fmt.Errorf("No interface found on system with MAC %q", mac)
	}

	hostNLif, err := netlink.LinkByName(hostIf.Name)
	if err != nil {
		return errors.Wrapf(err, "Error getting link %q", hostIf.Name)
	}

	// Create a bridge
	br := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name:   bridgeName,
			MTU:    mtu,
			TxQLen: -1,
		},
	}

	err = netlink.LinkAdd(br)
	if err != nil && err != syscall.EEXIST {
		return errors.Wrapf(err, "Error adding %q", bridgeName)
	}

	gbr, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return errors.Wrapf(err, "Could not re-find bridge %q", bridgeName)
	}
	br, ok := gbr.(*netlink.Bridge)
	if !ok {
		return errors.Wrapf(err, "%q already exists, but is not a bridge", bridgeName)
	}

	// Get the host interface, set it's master device to be the bridge
	err = netlink.LinkSetMaster(hostNLif, br)
	if err != nil {
		return errors.Wrapf(err, "Error setting %q as master for %q", bridgeName, hostIf.Name)
	}

	if err := netlink.LinkSetUp(br); err != nil {
		return errors.Wrapf(err, "Error bringing bridge %q up", bridgeName)
	}

	if err := netlink.LinkSetUp(hostNLif); err != nil {
		return errors.Wrapf(err, "Error bringing host interface %q up", hostIf.Name)
	}

	// Set the bridge's IP
	addr := &netlink.Addr{IPNet: ip, Label: ""}
	if err := netlink.AddrAdd(br, addr); err != nil {
		return errors.Wrapf(err, "Could not add %s to %s", ip, bridgeName)
	}

	return nil
}
