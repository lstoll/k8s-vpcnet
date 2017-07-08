package main

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"strings"

	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/golang/glog"
	"github.com/j-keck/arping"
	"github.com/lstoll/k8s-vpcnet/pkg/cni/config"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
)

const (
	fromPodRTBase = 120
	toPodRTBase   = 160
)

// When should this not just be 9000?
const mtu = 1500

type vetherImpl struct {
	cfg *config.CNI
}

func (v *vetherImpl) SetupVeth(cfg *config.CNI, netnsPath string, ifName string, podNet *podNet) (*current.Interface, *current.Interface, error) {
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

	// Fetch the ENI
	hostENIIf, err := netlink.LinkByName(podNet.ENIInterface)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to lookup %q: %v", podNet.ENIInterface, err)
	}

	// Write entries so we can use ip rule command easier
	err = ensureTables(podNet.ENI.Index)
	if err != nil {
		return nil, nil, errors.Wrap(err, "Error writing rt_tables")
	}

	// Ensure we have the default out route for this eni's frompod table
	err = ensureFromPodRoute(podNet.ENI.Index, hostENIIf.Attrs().Index, podNet.ENISubnet, cfg.ServiceCIDR, cfg.IPMasq)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "Error ensuring from pod default route exists for eni%d", podNet.ENI.Index)
	}

	for _, r := range podRoutes(podNet.ENI.Index, hostVeth.Attrs().Index, net.ParseIP(podNet.ENI.InterfaceIP), podNet.ContainerIP) {
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
			Table:     toPodRTBase + eniAttachIndex,
			LinkIndex: vethLinkIndex,
			Dst:       &net.IPNet{IP: containerIP, Mask: net.CIDRMask(32, 32)},
			Scope:     netlink.SCOPE_LINK,
		},
	}
}

func podRules(eniAttachIndex int, eniName, vethName string, containerIP net.IP) []*netlink.Rule {
	// Traffic from anywhere to pod IP "iif" the host eni eth should jump to toPodRT
	fromRule := netlink.NewRule()
	fromRule.Table = toPodRTBase + eniAttachIndex
	fromRule.IifName = eniName
	fromRule.Src = &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
	fromRule.Dst = &net.IPNet{IP: containerIP, Mask: net.CIDRMask(32, 32)}

	// Traffic from pod IP iif the veth should jump to fromPodRT
	toRule := netlink.NewRule()
	toRule.Table = fromPodRTBase + eniAttachIndex
	toRule.IifName = vethName
	toRule.Src = &net.IPNet{IP: containerIP, Mask: net.CIDRMask(32, 32)}

	return []*netlink.Rule{fromRule, toRule}
}

func ensureFromPodRoute(eniAttachIndex, eniLinkIndex int, eniSubnet *net.IPNet, serviceCIDR *net.IPNet, ipMasq bool) error {
	// VPC gateway is first address in subnet
	gw := net.ParseIP(eniSubnet.IP.String()).To4()
	gw[3]++

	var routes []*netlink.Route

	// Set this regardless, we can treat internal service stuff as local
	if serviceCIDR != nil {
		routes = append(routes, &netlink.Route{
			Table:     fromPodRTBase + eniAttachIndex,
			LinkIndex: eniLinkIndex,
			Dst:       serviceCIDR,
			Scope:     netlink.SCOPE_LINK,
		})
	}

	if ipMasq {
		// If we're masquerading, we want non-local interface traffic to leave
		// the main interface via it's default route, so it'll be masqeraded as
		// the host. We need to make sure local traffic and service traffic is
		// still pushed out the eni

		// TODO - have this configured, or inferred smarter?
		eth0, err := netlink.LinkByName("eth0")
		if err != nil {
			return errors.Wrap(err, "failed to lookup eth0")
		}

		routes = append(routes, &netlink.Route{
			Table:     fromPodRTBase + eniAttachIndex,
			LinkIndex: eniLinkIndex,
			Dst:       eniSubnet,
			Scope:     netlink.SCOPE_LINK,
		})

		routes = append(routes, &netlink.Route{
			Table:     fromPodRTBase + eniAttachIndex,
			LinkIndex: eth0.Attrs().Index,
			Dst:       &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
			Scope:     netlink.SCOPE_UNIVERSE,
			Gw:        gw, // TODO - what happens if this interface is on another subnet?
		})

	} else {
		routes = append(routes, &netlink.Route{
			Table:     fromPodRTBase + eniAttachIndex,
			LinkIndex: eniLinkIndex,
			Dst:       &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
			Scope:     netlink.SCOPE_UNIVERSE,
			Gw:        gw,
		})

	}

	// fromPodRT should default route from via the ENI interface
	for _, r := range routes {
		err := netlink.RouteAdd(r)
		if err != nil {
			if !strings.HasSuffix(err.Error(), "file exists") { // ignore dupes
				return errors.Wrapf(err, "Error ensuring outbound route %v for ENI %d", r, eniAttachIndex)
			}
		}
	}
	return nil
}

func (v *vetherImpl) TeardownVeth(nspath, ifname string) error {
	return ns.WithNetNSPath(nspath, func(_ ns.NetNS) error {
		var err error
		_, err = ip.DelLinkByNameAddr(ifname, netlink.FAMILY_V4)
		if err != nil && err == ip.ErrLinkNotFound {
			return nil
		}
		return err
	})

	// TODO - clean up the route/rule entries To do this properly, we'll
	// probably need to persist ENI information in a way we can load by
	// container id/ns and use here
}

// ensureTables ensures that the route table names are written to
// /etc/iproute2/rt_tables. We don't need this, but it makes visibility on the
// host easier.
func ensureTables(eniAttachIndex int) error {
	curr, err := ioutil.ReadFile("/etc/iproute2/rt_tables")
	if err != nil {
		return errors.Wrap(err, "Error reading route table file")
	}
	for _, l := range []string{
		fmt.Sprintf("%d frompod-eni%d", fromPodRTBase+eniAttachIndex, eniAttachIndex),
		fmt.Sprintf("%d topod-eni%d", toPodRTBase+eniAttachIndex, eniAttachIndex),
	} {
		if !strings.Contains(string(curr), l+"\n") {
			f, err := os.OpenFile("/etc/iproute2/rt_tables", os.O_RDWR|os.O_APPEND, 0640)
			if err != nil {
				return errors.Wrap(err, "error opening rt file for append")
			}
			_, err = fmt.Fprintln(f, l+"\n")
			if err != nil {
				f.Close()
				return errors.Wrapf(err, "error writing %q to rt file", l)
			}
			_ = f.Close()
		}
	}
	return nil
}
