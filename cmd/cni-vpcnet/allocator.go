package main

import (
	"bytes"
	"fmt"
	"net"
	"sort"

	"github.com/pkg/errors"

	"github.com/lstoll/k8s-vpcnet/pkg/cni/config"
	"github.com/lstoll/k8s-vpcnet/pkg/cni/diskstore"
	"github.com/lstoll/k8s-vpcnet/pkg/vpcnetstate"
)

// ErrEmptyPool is returned if the IP pool is configured with 0 IPs, e.g no
// additional IPs attached to the interface
var ErrEmptyPool = errors.New("No free private IPs found on interface")

// IPAllocator is the implementation of the actual allocator
type ipAllocator struct {
	name   string
	conf   *config.IPAM
	store  *diskstore.Store
	eniMap vpcnetstate.ENIs
}

type ifFreeIPs struct {
	ifName string
	ips    []net.IP
}

// Get returns newly allocated IP along with the ENI address (and subnet mask).
func (a *ipAllocator) Get(id string) (alloced net.IP, eniAddr *net.IPNet, err error) {
	a.store.Lock()
	defer a.store.Unlock()

	var ips []net.IP
	for _, eni := range a.eniMap {
		ips = append(ips, eni.IPs...)
	}

	if len(ips) == 0 {
		return nil, nil, ErrEmptyPool
	}

	// Sort to ensure consistent ordering, for handling last used etc.
	sort.Sort(netIps(ips))

	lastReservedIP, err := a.store.LastReservedIP(a.name)
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
		reserved, err := a.store.Reserve(id, ip, a.name)
		if err != nil {
			return nil, nil, errors.Wrap(err, "Error reserving IP in store")
		}
		if reserved {
			reservedIP = ip
			break
		}
	}

	if reservedIP == nil {
		return nil, nil, fmt.Errorf("Could not allocate IP for network %s", a.name)
	}

	// Find the ENI that has this
	var eni *vpcnetstate.ENI
	for _, e := range a.eniMap {
		for _, ip := range e.IPs {
			if ip.Equal(reservedIP) {
				eni = e
			}
		}
	}

	if eni == nil {
		panic("Internal state issue - could not find ENI for IP that came from ENI list")
	}

	eniNet := eni.CIDRBlock.IPNet()
	eniNet.IP = eni.InterfaceIP

	return reservedIP, eniNet, nil
}

// Release releases all IPs allocated for the container with given ID, and
// returns the released addresses.
func (a *ipAllocator) Release(id string) ([]net.IP, error) {
	a.store.Lock()
	defer a.store.Unlock()

	r, err := a.store.ReleaseByID(id)
	if err != nil {
		return nil, err
	}
	return r, nil
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
