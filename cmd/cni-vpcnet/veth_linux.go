package main

import (
	"fmt"
	"net"

	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/golang/glog"
	"github.com/j-keck/arping"
	cniconfig "github.com/lstoll/k8s-vpcnet/pkg/cni/config"
	"github.com/lstoll/k8s-vpcnet/pkg/config"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
)

// When should this not just be 9000?
const mtu = 1500

type vetherImpl struct {
	cfg *cniconfig.CNI
}

func (v *vetherImpl) SetupVeth(cfg *cniconfig.CNI, netnsPath string, ifName string, podNet *podNet) (*current.Interface, *current.Interface, error) {
	hostInterface := &current.Interface{}
	containerInterface := &current.Interface{}

	contNS, err := ns.GetNS(netnsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("error opening netns %q [%v]", netnsPath, err)
	}
	defer contNS.Close()

	err = contNS.Do(func(hostNS ns.NetNS) error {
		hostVeth, contVeth0, err := ip.SetupVeth(ifName, mtu, hostNS)
		if err != nil {
			return err
		}

		hostInterface.Name = hostVeth.Name
		hostInterface.Mac = hostVeth.HardwareAddr.String()
		containerInterface.Name = contVeth0.Name
		containerInterface.Mac = contVeth0.HardwareAddr.String()
		containerInterface.Sandbox = contNS.Path()

		link, err := netlink.LinkByName(ifName)
		if err != nil {
			glog.Errorf("failed to lookup %q: %v", ifName, err)
			return fmt.Errorf("failed to lookup %q: %v", ifName, err)
		}

		if err := netlink.LinkSetUp(link); err != nil {
			glog.Errorf("failed to set %q UP: %v", ifName, err)
			return fmt.Errorf("failed to set %q UP: %v", ifName, err)
		}

		// TODO - make net.ContainerIP in to an ipnet
		contNet := &net.IPNet{
			IP:   podNet.ContainerIP,
			Mask: net.CIDRMask(32, 32),
		}

		contAddr := &netlink.Addr{IPNet: contNet, Label: ""}
		if err = netlink.AddrAdd(link, contAddr); err != nil {
			glog.Errorf("failed to add IP addr %v to %q: %v", contAddr, ifName, err)
			return fmt.Errorf("failed to add IP addr %v to %q: %v", contAddr, ifName, err)
		}

		contVeth, err := net.InterfaceByName(ifName)
		if err != nil {
			glog.Errorf("failed to look up %q: %v", ifName, err)
			return fmt.Errorf("failed to look up %q: %v", ifName, err)
		}

		// Add container side routes
		for _, r := range []netlink.Route{
			// Add a direct link route for the host's ENI IP only
			netlink.Route{
				LinkIndex: contVeth.Index,
				Dst:       &net.IPNet{IP: podNet.ENIIp.IP, Mask: net.CIDRMask(32, 32)},
				Scope:     netlink.SCOPE_LINK,
				//	Src:       podNet.ContainerIP,
			},
			// Route all other traffic via the host's ENI IP
			netlink.Route{
				LinkIndex: contVeth.Index,
				Dst:       &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
				Scope:     netlink.SCOPE_UNIVERSE,
				Gw:        podNet.ENIIp.IP,
				//Src:       podNet.ContainerIP,
			},
		} {
			if err := netlink.RouteAdd(&r); err != nil {
				return fmt.Errorf("failed to add route %v: %v", r, err)
			}
		}

		// Send a gratuitous arp for all v4 addresses
		_ = arping.GratuitousArpOverIface(podNet.ContainerIP, *contVeth)

		return nil
	})
	if err != nil {
		glog.Errorf("Error configuring interfaces in target NS %v", err)
		return nil, nil, err
	}

	/* Set up the host side of the interface back in the main NS */

	// Reload the interface here
	hostVeth, err := netlink.LinkByName(hostInterface.Name)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to lookup %q: %v", hostInterface.Name, err)
	}

	// Set up the address for the host side of the pair to be the same as the
	// ENI IP.
	hostIP := &net.IPNet{
		IP:   podNet.ENIIp.IP,
		Mask: net.CIDRMask(32, 32), // TODO - do we want specific, or same mask as ENI?
	}
	addr := &netlink.Addr{IPNet: hostIP, Label: ""}
	if err = netlink.AddrAdd(hostVeth, addr); err != nil {
		return nil, nil, fmt.Errorf("failed to add IP addr (%#v) to veth: %v", hostIP, err)
	}

	// Set up the routing rules to ensure traffic egresses the correct host interface
	for _, r := range podRoutes(podNet.ENI.Index, hostVeth.Attrs().Index, podNet.ENI.InterfaceIP, podNet.ContainerIP) {
		if err := netlink.RouteAdd(r); err != nil {
			return nil, nil, errors.Wrapf(err, "failed to add rule %v", r)
		}
	}

	for _, r := range podRules(podNet.ENI.Index, podNet.ENI.InterfaceName(), hostVeth.Attrs().Name, podNet.ContainerIP) {
		if err := netlink.RuleAdd(r); err != nil {
			return nil, nil, errors.Wrapf(err, "failed to add rule %v", r)
		}
	}

	return hostInterface, containerInterface, nil
}

func podRoutes(eniAttachIndex, vethLinkIndex int, eniIP net.IP, containerIP net.IP) []*netlink.Route {
	return []*netlink.Route{
		// Route in the main table for the host to the pod
		{
			LinkIndex: vethLinkIndex,
			Dst: &net.IPNet{
				IP:   containerIP,
				Mask: net.CIDRMask(32, 32),
			},
			Scope: netlink.SCOPE_LINK,
			Src:   eniIP,
		},
		// toPodRT should have link scope route to pod IP via pod
		{
			Table:     config.ToPodRTBase + eniAttachIndex,
			LinkIndex: vethLinkIndex,
			Dst:       &net.IPNet{IP: containerIP, Mask: net.CIDRMask(32, 32)},
			Scope:     netlink.SCOPE_LINK,
		},
	}
}

func podRules(eniAttachIndex int, eniName, vethName string, containerIP net.IP) []*netlink.Rule {
	// Traffic from anywhere to pod IP "iif" the host eni eth should jump to toPodRT
	fromRule := netlink.NewRule()
	fromRule.Table = config.ToPodRTBase + eniAttachIndex
	fromRule.IifName = eniName
	fromRule.Src = &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
	fromRule.Dst = &net.IPNet{IP: containerIP, Mask: net.CIDRMask(32, 32)}

	// Traffic from pod IP iif the veth should jump to fromPodRT
	toRule := netlink.NewRule()
	toRule.Table = config.FromPodRTBase + eniAttachIndex
	toRule.IifName = vethName
	toRule.Src = &net.IPNet{IP: containerIP, Mask: net.CIDRMask(32, 32)}

	return []*netlink.Rule{fromRule, toRule}
}

func (v *vetherImpl) TeardownVeth(cfg *cniconfig.CNI, nspath, ifname string, released []net.IP) error {
	err := ns.WithNetNSPath(nspath, func(_ ns.NetNS) error {
		var err error
		_, err = ip.DelLinkByNameAddr(ifname, netlink.FAMILY_V4)
		if err != nil && err == ip.ErrLinkNotFound {
			return nil
		}
		return err
	})
	if err != nil {
		return errors.Wrapf(err, "Error tearing down veth for namespace %s", nspath)
	}

	// TODO - clean up the route/rule entries To do this properly, we'll
	// probably need to persist ENI information in a way we can load by
	// container id/ns and use here
	for _, ip := range released {
		routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
		if err != nil {
			return errors.Wrap(err, "Error fetching routes")
		}
		for _, route := range routes {
			glog.V(4).Infof("NS %s IP %s: Checking route %+v", nspath, ip.String(), route)
			if (route.Src != nil && route.Src.Equal(ip)) ||
				(route.Dst != nil && route.Dst.IP.Equal(ip) && route.Dst.Mask.String() == net.CIDRMask(32, 32).String()) {
				glog.V(4).Infof("NS %s IP %s: Deleting route %+v", nspath, ip.String(), route)
				err = netlink.RouteDel(&route)
				if err != nil {
					return errors.Wrapf(err, "Error deleting route %+v", route)
				}
			}
		}

		rules, err := netlink.RuleList(netlink.FAMILY_ALL)
		if err != nil {
			return errors.Wrap(err, "Error fetching rules")
		}
		for _, rule := range rules {
			glog.V(4).Infof("NS %s IP %s: Checking rule %+v", nspath, ip.String(), rule)
			if (rule.Src != nil && rule.Src.IP.Equal(ip) && rule.Src.Mask.String() == net.CIDRMask(32, 32).String()) ||
				(rule.Dst != nil && rule.Dst.IP.Equal(ip) && rule.Dst.Mask.String() == net.CIDRMask(32, 32).String()) {
				glog.V(4).Infof("NS %s IP %s: Deleting rule %+v", nspath, ip.String(), rule)
				err = netlink.RuleDel(&rule)
				if err != nil {
					return errors.Wrapf(err, "Error deleting rule %+v", rule)
				}
			}
		}
	}

	return nil
}
