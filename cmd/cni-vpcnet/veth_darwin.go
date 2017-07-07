package main

import "github.com/containernetworking/cni/pkg/types/current"

type vetherImpl struct{}

func (v *vetherImpl) SetupVeth(cfg *Net, contnsPath, ifName string, net *podNet) (*current.Interface, *current.Interface, error) {
	return nil, nil, nil
}

func (v *vetherImpl) TeardownVeth(netns, ifname string) error {
	return nil
}
