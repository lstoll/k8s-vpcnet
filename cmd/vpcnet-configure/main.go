package main

import (
	"flag"
	"net"

	"github.com/golang/glog"
)

func main() {
	flag.Set("logtostderr", "true")

	var mode, mac, bridge, ip, cb string

	flag.StringVar(&mode, "mode", "kubernetes-auto", "Mode to run in. Default is normal on-cluster, or manual can be specified to configure directly")

	flag.StringVar(&mac, "host-mac", "", "[manual] MAC address of the ENI")
	flag.StringVar(&bridge, "bridge-name", "", "[manual] name for the bridge")
	flag.StringVar(&ip, "ip", "", "[manual] IP address for the bridge")
	flag.StringVar(&cb, "cidr-block", "", "[manual] VPC Subnet CIDR block (e.g 10.0.0.0/8")

	flag.Parse()

	switch mode {
	case "kubernetes-auto":
		glog.Info("Running node configurator in on-cluster mode")
		glog.Fatal("NOT SET UP")
	case "manual":
		if mac == "" || bridge == "" || ip == "" || cb == "" {
			glog.Fatal("All args required in manual mode")
		}
		glog.Info("Configuring interfaces directly")
		_, ipn, err := net.ParseCIDR(cb)
		if err != nil {
			glog.Fatalf("Error parsing subnet cidr [%v]", err)
		}
		ipn.IP = net.ParseIP(ip)
		err = configureInterface(bridge, mac, ipn)
		if err != nil {
			glog.Fatalf("Error configuring interface [%v]", err)
		}
	default:
		glog.Fatalf("Invalid mode %q specified", mode)
	}

	// find ourself in the kubernetes api
	//     https://stackoverflow.com/questions/35008011/kubernetes-how-do-i-know-what-node-im-on

	// watch for interfaces/ips defined on the node

	// for each interface, configure the bridge (OS-specific)

	// for the IP's, write them out to the host path where the ipam can see them

	// Poll for current running pods, delete lock files for gone pods
}
