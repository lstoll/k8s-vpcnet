package ipmasq

import "net"

// Setup installs iptables rules to masquerade traffic coming from the
// described pod, and going to anywhere outside skipNets or the multicast range.
func Setup(pluginName, podID string, podIP net.IP, skipNets []*net.IPNet) error {
	return nil
}

// Teardown undoes the effects of SetupIPMasq
func Teardown(pluginName, podID string) error {
	return nil
}
