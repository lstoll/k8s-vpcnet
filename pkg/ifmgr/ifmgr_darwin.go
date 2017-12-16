package ifmgr

import "net"

func (i *IFMgr) ConfigureInterface(ifname string, mac string, ip *net.IPNet, subnet *net.IPNet) error {
	return nil
}

func (i *IFMgr) ConfigureRoutes(ifName string, awsEniAttachIndex int, eniSubnet *net.IPNet, podIPs []net.IP) error {
	return nil
}

func (i *IFMgr) ConfigureIPMasq(hostIP net.IP, podIPs []net.IP) error {
	return nil
}

func (i *IFMgr) InterfaceExists(name string) (bool, error) {
	return false, nil
}
