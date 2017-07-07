package main

import "github.com/containernetworking/cni/pkg/types/current"

type vether interface {
	SetupVeth(cfg *Net, netnsPath, ifName string, net *podNet) (*current.Interface, *current.Interface, error)
	TeardownVeth(netnsPath, ifname string) error
}
