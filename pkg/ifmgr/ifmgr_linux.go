package ifmgr

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"strings"

	"github.com/golang/glog"
	"github.com/lstoll/k8s-vpcnet/pkg/config"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
	uiptables "k8s.io/kubernetes/pkg/util/iptables"
)

func (i *IFMgr) ConfigureInterface(ifname string, mac string, ip *net.IPNet, subnet *net.IPNet) error {
	// Find the interface AWS attached
	ifs, err := netlink.LinkList()
	if err != nil {
		return err
	}
	var hostIf netlink.Link
	for _, i := range ifs {
		if i.Attrs().HardwareAddr.String() == mac {
			hostIf = i
		}
	}
	if hostIf == nil {
		return fmt.Errorf("No interface found on system with MAC %q", mac)
	}
	glog.V(2).Infof("Found host interface %s with MAC %s, will configure as %s", hostIf.Attrs().Name, mac, ifname)

	err = netlink.LinkSetName(hostIf, ifname)
	if err != nil {
		return errors.Wrapf(err, "Error renaming interface %s to %s", hostIf.Attrs().Name, ifname)
	}

	addr := &netlink.Addr{IPNet: ip, Label: ""}
	if err := netlink.AddrAdd(hostIf, addr); err != nil {
		return errors.Wrapf(err, "Could not add %s to %s", ip, hostIf.Attrs().Name)
	}

	if err := netlink.LinkSetUp(hostIf); err != nil {
		return errors.Wrapf(err, "Error bringing host interface %q up", hostIf.Attrs().Name)
	}

	return nil
}

func (i *IFMgr) ConfigureRoutes(ifName string, awsEniAttachIndex int, eniSubnet *net.IPNet) error {
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return errors.Wrapf(err, "failed to lookup %q", ifName)
	}

	// Write entries so we can use ip rule command easier
	err = ensureTables(ifName, awsEniAttachIndex)
	if err != nil {
		return errors.Wrap(err, "Error writing rt_tables")
	}

	// VPC gateway is first address in subnet
	gw := net.ParseIP(eniSubnet.IP.String()).To4()
	gw[3]++

	var routes []*netlink.Route

	// Set this regardless, we can treat internal service stuff as local
	routes = append(routes, &netlink.Route{
		Table:     config.FromPodRTBase + awsEniAttachIndex,
		LinkIndex: link.Attrs().Index,
		Dst:       i.Network.ServiceCIDR.IPNet(),
		//Scope:     netlink.SCOPE_LINK, Direct all service traffic via the
		// VPC gateway, it can figure stuff out.
		Scope: netlink.SCOPE_UNIVERSE,
		Gw:    gw,
	})

	// Route the greater cluster network via the ENI gateway
	routes = append(routes, &netlink.Route{
		Table:     config.FromPodRTBase + awsEniAttachIndex,
		LinkIndex: link.Attrs().Index,
		Dst:       i.Network.ClusterCIDR.IPNet(),
		Scope:     netlink.SCOPE_UNIVERSE,
		Gw:        gw,
	})

	if i.Network.PodIPMasq {
		// If we're masquerading, we want non-local interface traffic to leave
		// the main interface via it's default route, so it'll be masqeraded as
		// the host. We need to make sure local traffic and service traffic is
		// still pushed out the eni

		// TODO - have this configured, or inferred smarter?
		eth0, err := netlink.LinkByName(i.Network.HostPrimaryInterface)
		if err != nil {
			return errors.Wrapf(err, "failed to lookup %q", i.Network.HostPrimaryInterface)
		}

		routes = append(routes, &netlink.Route{
			Table:     config.FromPodRTBase + awsEniAttachIndex,
			LinkIndex: link.Attrs().Index,
			Dst:       eniSubnet,
			Scope:     netlink.SCOPE_LINK,
		})

		// route the extra networks via the ENI gateway
		for _, an := range i.Network.VPCRouted {
			routes = append(routes, &netlink.Route{
				Table:     config.FromPodRTBase + awsEniAttachIndex,
				LinkIndex: link.Attrs().Index,
				Dst:       an.IPNet(),
				Scope:     netlink.SCOPE_UNIVERSE,
				Gw:        gw,
			})
		}

		routes = append(routes, &netlink.Route{
			Table:     config.FromPodRTBase + awsEniAttachIndex,
			LinkIndex: eth0.Attrs().Index,
			Dst:       &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
			Scope:     netlink.SCOPE_UNIVERSE,
			Gw:        gw, // TODO - what happens eth0 is on another subnet?
		})

	} else {
		routes = append(routes, &netlink.Route{
			Table:     config.FromPodRTBase + awsEniAttachIndex,
			LinkIndex: link.Attrs().Index,
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
				return errors.Wrapf(err, "Error ensuring outbound route %v for ENI %d", r, awsEniAttachIndex)
			}
		}
	}
	return nil
}

func (i *IFMgr) ConfigureIPMasq(hostIP net.IP, podIPs []net.IP) error {
	chain := uiptables.Chain("K8S-VPCNET")
	comment := "k8s-vpcnet generated rules for masquerading outbound pod traffic"
	multicastNet := &net.IPNet{IP: net.ParseIP("224.0.0.0"), Mask: net.CIDRMask(4, 32)}

	_, err := i.IPTables.EnsureChain(uiptables.TableNAT, chain)
	if err != nil {
		return errors.Wrapf(err, "failed to ensure chain %q", chain)
	}

	// Packets to these network should pass through like normal
	nonmasqNets := []*net.IPNet{i.Network.ClusterCIDR.IPNet(), i.Network.ServiceCIDR.IPNet(), multicastNet}
	for _, an := range i.Network.VPCRouted {
		nonmasqNets = append(nonmasqNets, an.IPNet())
	}
	for _, sn := range nonmasqNets {
		if _, err := i.IPTables.EnsureRule(uiptables.Append, uiptables.TableNAT, chain, "-d", sn.String(), "-j", "ACCEPT", "-m", "comment", "--comment", comment); err != nil {
			return errors.Wrap(err, "Error adding skip rule")
		}
	}

	// Don't masquerade multicast - pods should be able to talk to other pods
	// on the local network via multicast.
	_, err = i.IPTables.EnsureRule(uiptables.Append, uiptables.TableNAT, chain, "-j", "MASQUERADE", "-m", "comment", "--comment", comment)
	if err != nil {
		return errors.Wrap(err, "Error adding masquerade rull")
	}

	for _, ip := range podIPs {
		_, err := i.IPTables.EnsureRule(uiptables.Append, uiptables.TableNAT, uiptables.ChainPostrouting, "-s", ip.String()+"/32", "-j", string(chain), "-m", "comment", "--comment", comment)
		if err != nil {
			return errors.Wrapf(err, "Error inserting jump for pod IP %v", ip)
		}

		if i.Network.InstanceMetadataRedirectPort != 0 {
			// Redirect instance metadata traffic to this port on the hosts main IP.
			_, err := i.IPTables.EnsureRule(uiptables.Append, uiptables.TableNAT, uiptables.ChainPrerouting,
				"-s", ip.String()+"/32", "-d", "169.254.169.254", "-p", "tcp", "--dport", "80",
				"-j", "DNAT", "--to-destination", fmt.Sprintf("%s:%d", hostIP.String(), i.Network.InstanceMetadataRedirectPort),
				"-m", "comment", "--comment", comment,
			)
			if err != nil {
				return errors.Wrapf(err, "Error inserting jump for pod IP %v", ip)
			}
		}
	}

	return nil
}

func (i *IFMgr) InterfaceExists(name string) (bool, error) {
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

// ensureTables ensures that the route table names are written to
// /etc/iproute2/rt_tables. We don't need this, but it makes visibility on the
// host easier.
func ensureTables(ifName string, eniAttachIndex int) error {
	curr, err := ioutil.ReadFile("/etc/iproute2/rt_tables")
	if err != nil {
		return errors.Wrap(err, "Error reading route table file")
	}
	for _, l := range []string{
		fmt.Sprintf("%d frompod-%s", config.FromPodRTBase+eniAttachIndex, ifName),
		//fmt.Sprintf("%d topod-%s", config.ToPodRTBase+eniAttachIndex, ifName),
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
