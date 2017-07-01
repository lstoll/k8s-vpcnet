package main

import (
	"net"

	"github.com/golang/glog"
)

func configureInterface(bridgeName, mac string, ip *net.IPNet) error {
	glog.Infof("no-op add of iface %q mac %q ip %q", bridgeName, mac, ip)
	return nil
}
