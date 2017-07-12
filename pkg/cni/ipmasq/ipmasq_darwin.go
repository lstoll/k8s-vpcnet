package ipmasq

import "net"

// Setup installs iptables rules to masquerade traffic coming from the
// described pod, and going to anywhere outside skipNets or the multicast range.
func Setup(chain, comment string, podIPs []net.IP, skipNets []*net.IPNet) error {
	return nil
}
