package main

import (
	"net"

	"github.com/golang/glog"
)

func configureInterface(name string, mac string, ip *net.IPNet, subnet *net.IPNet) error {
	glog.Infof("no-op add of iface %s mac %q ip %q subnet %q", name, mac, ip, subnet)
	return nil
}

func interfaceExists(name string) (bool, error) {
	glog.Infof("no-op add of interfaceExists %q", name)
	return true, nil
}
