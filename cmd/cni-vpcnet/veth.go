package main

import (
	"net"

	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/lstoll/k8s-vpcnet/pkg/cni/config"
	"github.com/lstoll/k8s-vpcnet/pkg/vpcnetstate"
)

type vether interface {
	SetupVeth(cfg *config.CNI, enis vpcnetstate.ENIs, netnsPath, ifName string, net *podNet) (*current.Interface, *current.Interface, error)
	TeardownVeth(cfg *config.CNI, enis vpcnetstate.ENIs, nspath, ifname string, released []net.IP) error
}
