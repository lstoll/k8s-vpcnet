package allocator

import (
	"bytes"
	"net"
	"sort"
	"sync"

	"github.com/lstoll/k8s-vpcnet/pkg/vpcnetstate"
	"github.com/pkg/errors"
)

// ErrEmptyPool is returned if the IP pool is configured with 0 IPs, e.g no
// additional IPs attached to the interface
var ErrEmptyPool = errors.New("No free private IPs found on interface")

// Allocator is used for managing resource allocations.
type Allocator struct {
	State *vpcnetstate.AllocatorState

	allocatorMu sync.Mutex
}

// Allocation represents an allocated address and it's associated information
type Allocation struct {
	IP        net.IP
	ENIIP     net.IP
	ENISubnet net.IPNet
}

// New returns a configured allocator
func New(s *vpcnetstate.AllocatorState) *Allocator {
	return &Allocator{
		State: s,
	}
}

func (a *Allocator) Allocate(containerID, podID string) (*Allocation, error) {
	a.allocatorMu.Lock()
	defer a.allocatorMu.Unlock()

	var ips []net.IP
	for _, eni := range a.State.ENIs {
		ips = append(ips, eni.IPs...)
	}

	if len(ips) == 0 {
		return nil, ErrEmptyPool
	}

	// Sort to ensure consistent ordering, for handling last used etc.
	sort.Sort(netIps(ips))

	if a.State.LastFreedIP == nil {
		// Likely no last reserved. Just start from the beginning
	} else {
		// Shuffle IPs so last reserved is at the end
		for i := 0; i < len(ips); i++ {
			if ips[i].Equal(a.State.LastFreedIP) {
				ips = append(ips[i+1:], ips[:i+1]...)
			}
		}
	}

	// Walk until we find a free IPs
	var reservedIP net.IP
	for _, ip := range ips {
		// check if it's already allocated
		if _, ok := a.State.Allocations[ip.String()]; ok {
			continue
		}

		// if we get here, we're good with this IP.
		reservedIP = ip
	}

	if reservedIP == nil {
		return nil, errors.New("Could not allocate IP")
	}

	// Find the ENI that has this
	var eni *vpcnetstate.ENI
	for _, e := range a.State.ENIs {
		for _, ip := range e.IPs {
			if ip.Equal(reservedIP) {
				eni = e
			}
		}
	}

	if eni == nil {
		panic("Internal state issue - could not find ENI for IP that came from ENI list")
	}

	// Add an allocation entry
	a.State.Allocations[reservedIP.String()] = vpcnetstate.Allocation{
		ContainerID: containerID,
		PodID:       podID,
	}

	if err := a.State.Write(); err != nil {
		return nil, errors.Wrap(err, "Error writing state")
	}

	return &Allocation{
		IP:        reservedIP,
		ENIIP:     eni.InterfaceIP,
		ENISubnet: *eni.CIDRBlock.IPNet(),
	}, nil
}

// ReleaseByContainer will free the IP allocated to the given container
func (a *Allocator) ReleaseByContainer(containerID string) error {
	a.allocatorMu.Lock()
	defer a.allocatorMu.Unlock()

	var relip string

	for ip, state := range a.State.Allocations {
		if state.ContainerID == containerID {
			relip = ip
		}
	}

	if relip != "" {
		delete(a.State.Allocations, relip)
		a.State.LastFreedIP = net.ParseIP(relip)
	}

	if err := a.State.Write(); err != nil {
		return errors.Wrap(err, "Error writing state")
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
