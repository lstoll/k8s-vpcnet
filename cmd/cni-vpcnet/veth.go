package main

import (
	"net"

	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/lstoll/k8s-vpcnet/pkg/cni/config"
)

type vether interface {
	SetupVeth(cfg *config.CNI, netnsPath, ifName string, net *podNet) (*current.Interface, *current.Interface, error)
	TeardownVeth(cfg *config.CNI, nspath, ifname string, released []net.IP) error
}
