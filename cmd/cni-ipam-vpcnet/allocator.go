package main

import (
	"bytes"
	"fmt"
	"net"
	"sort"

	"github.com/pkg/errors"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/plugins/plugins/ipam/host-local/backend/disk"
	"github.com/lstoll/k8s-vpcnet/vpcnetstate"
)

// ErrEmptyPool is returned if the IP pool is configured with 0 IPs, e.g no
// additional IPs attached to the interface
var ErrEmptyPool = errors.New("No free private IPs found on interface")

// IPAllocator is the implementation of the actual allocator
type IPAllocator struct {
	conf   *Net
	store  *disk.Store
	eniMap vpcnetstate.ENIMap
}

// Get returns newly allocated IP along with its config
func (a *IPAllocator) Get(id string) (types.Result, error) {
	a.store.Lock()
	defer a.store.Unlock()

	brIf := a.conf.IPAM.Interface

	config, ok := a.eniMap[brIf]
	if !ok {
		return nil, fmt.Errorf("No entry for interface %s in ENI config map", brIf)
	}

	ret := &current.Result{}

	// bridge mode is always this, and routed via host
	_, subnet, err := net.ParseCIDR("0.0.0.0/32")
	if err != nil {
		panic(err)
	}

	// route to brip/32 is a interface route
	brNet := net.IPNet{
		IP:   net.ParseIP(config.InterfaceIP),
		Mask: net.IPv4Mask(255, 255, 255, 255),
	}

	_, defNet, err := net.ParseCIDR("0.0.0.0/0")
	if err != nil {
		panic(err)
	}

	ret.Routes = []*types.Route{
		{
			Dst: brNet,
		},
		{
			Dst: *defNet,
			GW:  net.ParseIP(config.InterfaceIP),
		},
	}

	ips := []net.IP{}
	for _, i := range config.IPs {
		ips = append(ips, net.ParseIP(i))
	}

	if len(ips) == 0 {
		return nil, ErrEmptyPool
	}

	// Sort to ensure consistent ordering, for handling last used etc.
	sort.Sort(netIps(ips))

	lastReservedIP, err := a.store.LastReservedIP(brIf)
	if err != nil || lastReservedIP == nil {
		// Likely no last reserved. Just start from the beginning
	} else {
		// Shuffle IPs so last reserved is at the end
		for i := 0; i < len(ips); i++ {
			if ips[i].Equal(lastReservedIP) {
				ips = append(ips[i+1:], ips[:i+1]...)
			}
		}
	}

	// Walk until we find a free IPs
	var reservedIP net.IP
	for _, ip := range ips {
		reserved, err := a.store.Reserve(id, ip, brIf)
		if err != nil {
			return nil, err
		}
		if reserved {
			reservedIP = ip
			break
		}
	}

	if reservedIP == nil {
		return nil, fmt.Errorf("Could not allocate IP for network %s interface %s", a.conf.Name, brIf)
	}

	ret.IPs = []*current.IPConfig{
		{
			Version: "4",
			Address: net.IPNet{
				IP:   reservedIP,
				Mask: subnet.Mask,
			},
		},
	}

	return ret, nil
}

// Release releases all IPs allocated for the container with given ID
func (a *IPAllocator) Release(id string) error {
	a.store.Lock()
	defer a.store.Unlock()

	err := a.store.ReleaseByID(id)
	if err != nil {
		return err
	}
	return nil
}

// Classic Golang
type netIps []net.IP

func (n netIps) Len() int {
	return len(n)
}

func (n netIps) Less(i, j int) bool {
	return bytes.Compare(n[i][:], n[j][:]) < 0
}

func (n netIps) Swap(i, j int) {
	n[i], n[j] = n[j], n[i]
}
