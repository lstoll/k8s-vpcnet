package main

import (
	"fmt"
	"net"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
)

const (
	mtu = 1500
)

func configureInterface(ifname string, mac string, ip *net.IPNet, subnet *net.IPNet) error {
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

	err = netlink.LinkSetName(hostNLif, ifname)
	if err != nil {
		return errors.Wrapf(err, "Error renaming interface %s to %s", hostIf.Name, ifname)
	}

	addr := &netlink.Addr{IPNet: ip, Label: ""}
	if err := netlink.AddrAdd(hostNLif, addr); err != nil {
		return errors.Wrapf(err, "Could not add %s to %s", ip, hostIf.Name)
	}

	if err := netlink.LinkSetUp(hostNLif); err != nil {
		return errors.Wrapf(err, "Error bringing host interface %q up", hostIf.Name)
	}

	// TODO - drop any routes that are created automatically?

	// Add a source route via this interface
	err = netlink.RouteAdd(&netlink.Route{
		LinkIndex: hostNLif.Attrs().Index,
		Dst:       subnet,
		Scope:     netlink.SCOPE_LINK,
		Src:       ip.IP,
	})
	if err != nil {
		return fmt.Errorf("Error adding source route to %s on interface %s", subnet.String(), ifname)
	}

	return nil
}

func interfaceExists(name string) (bool, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return false, errors.Wrapf(err, "Error checking for %q existence", name)
	}
	for _, i := range ifs {
		if i.Name == name {
			return true, nil
		}
	}
	glog.V(4).Infof("Interface %q not found in %v", name, ifs)
	return false, nil
}
