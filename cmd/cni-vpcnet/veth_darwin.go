package main

import (
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/lstoll/k8s-vpcnet/pkg/cni/config"
)

type vetherImpl struct{}

func (v *vetherImpl) SetupVeth(cfg *config.CNI, contnsPath, ifName string, net *podNet) (*current.Interface, *current.Interface, error) {
	return nil, nil, nil
}

func (v *vetherImpl) TeardownVeth(netns, ifname string) error {
	return nil
}
