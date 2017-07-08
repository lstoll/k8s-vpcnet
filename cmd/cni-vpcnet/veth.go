package main

import (
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/lstoll/k8s-vpcnet/pkg/cni/config"
)

type vether interface {
	SetupVeth(cfg *config.CNI, netnsPath, ifName string, net *podNet) (*current.Interface, *current.Interface, error)
	TeardownVeth(netnsPath, ifname string) error
}
