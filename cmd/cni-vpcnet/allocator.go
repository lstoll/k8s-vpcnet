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
	conf   *config.CNI
	store  *diskstore.Store
	eniMap vpcnetstate.ENIs
}

// Get returns newly allocated IP along with its config
func (a *ipAllocator) Get(id string) (*podNet, error) {
	a.store.Lock()
	defer a.store.Unlock()

	// TODO - handle multiple ENI's
	if len(a.eniMap) != 1 {
		return nil, fmt.Errorf("We can only handle a single ENI, found %d", len(a.eniMap))
	}
	config := a.eniMap[0]

	ips := config.IPs

	if len(ips) == 0 {
		return nil, ErrEmptyPool
	}

	// Sort to ensure consistent ordering, for handling last used etc.
	sort.Sort(netIps(ips))

	lastReservedIP, err := a.store.LastReservedIP(config.InterfaceName())
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
		reserved, err := a.store.Reserve(id, ip, config.InterfaceName())
		if err != nil {
			return nil, errors.Wrap(err, "Error reserving IP in store")
		}
		if reserved {
			reservedIP = ip
			break
		}
	}

	if reservedIP == nil {
		return nil, fmt.Errorf("Could not allocate IP for network %s interface %s", a.conf.Name, config.InterfaceName())
	}

	return &podNet{
		ContainerIP:  reservedIP,
		ENIIp:        net.IPNet{IP: config.InterfaceIP, Mask: config.CIDRBlock.Mask},
		ENIInterface: config.InterfaceName(),
		ENISubnet:    config.CIDRBlock.IPNet(),
		ENI:          config,
	}, nil
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
