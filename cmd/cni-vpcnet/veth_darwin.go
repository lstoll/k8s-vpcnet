package main

import (
	"net"

	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/lstoll/k8s-vpcnet/pkg/cni/config"
	"github.com/lstoll/k8s-vpcnet/pkg/vpcnetstate"
)

type vetherImpl struct{}

func (v *vetherImpl) SetupVeth(cfg *config.CNI, enis vpcnetstate.ENIs, contnsPath, ifName string, net *podNet) (*current.Interface, *current.Interface, error) {
	return nil, nil, nil
}

func (v *vetherImpl) TeardownVeth(cfg *config.CNI, enis vpcnetstate.ENIs, nspath, ifname string, released []net.IP) error {
	return nil
}
