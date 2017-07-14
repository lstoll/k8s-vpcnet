package main

import (
	"net"

	"github.com/golang/glog"
	"github.com/lstoll/k8s-vpcnet/pkg/config"
)

func configureInterface(name string, mac string, ip *net.IPNet, subnet *net.IPNet) error {
	glog.Infof("no-op add of iface %s mac %q ip %q subnet %q", name, mac, ip, subnet)
	return nil
}

func interfaceExists(name string) (bool, error) {
	glog.Infof("no-op add of interfaceExists %q", name)
	return true, nil
}

func configureIPMasq(cfg *config.Network, hostIP net.IP, ips []net.IP) error {
	return nil
}

func configureRoutes(cfg *config.Network, ifName string, awsEniAttachIndex int, eniSubnet *net.IPNet) error {
	return nil
}
