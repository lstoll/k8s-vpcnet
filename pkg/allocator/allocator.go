package allocator

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net"
	"os"
	"sort"
	"sync"

	"github.com/lstoll/k8s-vpcnet/pkg/nodestate"
	"github.com/pkg/errors"
)

// ErrEmptyPool is returned if the IP pool is configured with 0 IPs, e.g no
// additional IPs attached to the interface
var ErrEmptyPool = errors.New("No free private IPs found on interface")

// Allocator is used for managing resource allocations. It can snapshot and
// restore its state to disk
type Allocator struct {
	state *State

	// we wrap everything in this, to prevent conflicting allocations and
	// control access to the allocation map
	allocatorMu sync.Mutex
}

// Allocation represents an allocated address and it's associated information
type Allocation struct {
	// ContainerID is the ID passed in to the CNI plugin for add/delete
	ContainerID string
	// PodName is Kubernetes Name for the pod using this allocation
	PodName string
	// PodNamespace is Kubernetes Namespace for the pod using this allocation
	PodNamespace string
	// IP is the address that was allocated
	IP net.IP
	// ENIIP is the address of the ENI that is the 'parent' of this address
	ENIIP net.IP
	// ENISubnet is the subnet the ENI resides in.
	ENISubnet net.IPNet
}

// New returns a configured allocator, with an optional state path. If statePath
// is empty, the default will be used
func New(statePath string) (*Allocator, error) {
	sp := statePath
	if sp == "" {
		sp = DefaultStatePath
	}

	s := &State{}

	if _, err := os.Stat(sp); !os.IsNotExist(err) {
		b, err := ioutil.ReadFile(sp)
		if err != nil {
			return nil, errors.Wrap(err, "Error reading existing state")
		}
		err = json.Unmarshal(b, s)
		if err != nil {
			return nil, errors.Wrap(err, "Error unmarshaling existing state")
		}
	}

	s.path = sp

	if s.Allocations == nil {
		s.Allocations = make(map[string]Allocation)
	}

	return &Allocator{
		state: s,
	}, nil
}

func (a *Allocator) SetENIs(enis nodestate.ENIs) error {
	a.allocatorMu.Lock()
	defer a.allocatorMu.Unlock()

	a.state.ENIs = enis

	if err := a.state.Write(); err != nil {
		return errors.Wrap(err, "Error writing state")
	}

	return nil
}

func (a *Allocator) Allocate(containerID, podName, podNamspace string) (*Allocation, error) {
	a.allocatorMu.Lock()
	defer a.allocatorMu.Unlock()

	var ips []net.IP
	for _, eni := range a.state.ENIs {
		ips = append(ips, eni.IPs...)
	}

	if len(ips) == 0 {
		return nil, ErrEmptyPool
	}

	// Sort to ensure consistent ordering, for handling last used etc.
	sort.Sort(netIps(ips))

	if a.state.LastFreedIP == nil {
		// Likely no last reserved. Just start from the beginning
	} else {
		// Shuffle IPs so last reserved is at the end
		for i := 0; i < len(ips); i++ {
			if ips[i].Equal(a.state.LastFreedIP) {
				ips = append(ips[i+1:], ips[:i+1]...)
			}
		}
	}

	// Walk until we find a free IPs
	var reservedIP net.IP
	for _, ip := range ips {
		// check if it's already allocated
		if _, ok := a.state.Allocations[ip.String()]; ok {
			continue
		}

		// if we get here, we're good with this IP.
		reservedIP = ip
	}

	if reservedIP == nil {
		return nil, errors.New("Could not allocate IP")
	}

	// Find the ENI that has this
	var eni *nodestate.ENI
	for _, e := range a.state.ENIs {
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
	allocation := Allocation{
		IP:           reservedIP,
		ENIIP:        eni.InterfaceIP,
		ENISubnet:    *eni.CIDRBlock.IPNet(),
		ContainerID:  containerID,
		PodName:      podName,
		PodNamespace: podNamspace,
	}

	a.state.Allocations[reservedIP.String()] = allocation

	if err := a.state.Write(); err != nil {
		return nil, errors.Wrap(err, "Error writing state")
	}

	return &allocation, nil
}

// ReleaseByContainer will free the IP allocated to the given container
func (a *Allocator) ReleaseByContainer(containerID string) error {
	a.allocatorMu.Lock()
	defer a.allocatorMu.Unlock()

	var relip string

	for ip, state := range a.state.Allocations {
		if state.ContainerID == containerID {
			relip = ip
		}
	}

	if relip != "" {
		delete(a.state.Allocations, relip)
		a.state.LastFreedIP = net.ParseIP(relip)
	}

	if err := a.state.Write(); err != nil {
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
