package vpcnetstate

import (
	"encoding/json"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/pkg/errors"
)

// DefaultStatePath is where the state data is persisted, unless otherwise
// specified
const DefaultStatePath = "/var/lib/cni/vpcnet/state.json"

// AllocatorState represents the working state of the vpcnet IPAM. It tracks the
// current node ENI configuration, as well as what addresses are used and by
// what Pods. It can be snapshotted/ read from disk
type AllocatorState struct {
	// ENIs represents the ENI interfaces and addresses that are configured on
	// this node
	ENIs ENIs `json:"enis"`
	// Allocations are a map of IP addresses to information about the allocation
	Allocations map[string]Allocation `json:"allocations"`
	// LastFreeIP is tracked to try and allocate an address far away from this.
	LastFreedIP net.IP

	path   string
	pathMu sync.Mutex
}

// Allocation represents the information about the consumer of an IP address
type Allocation struct {
	// ContainerID is the ID passed in to the CNI plugin for add/delete
	ContainerID string `json:"namespace"`
	// PodID is Kubernetes ID for the pod using this allocation
	PodID string `json:"pod_id"`
}

// NewAllocatorState returns a AllocatorState preloaded with the current state from statePath.
// if statePath is empty, the default will be used
func NewAllocatorState(statePath string) (*AllocatorState, error) {
	sp := statePath
	if sp == "" {
		sp = DefaultStatePath
	}

	s := &AllocatorState{}

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

	return s, nil
}

// Write outputs the current state to disk
func (a *AllocatorState) Write() error {
	a.pathMu.Lock()
	defer a.pathMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(a.path), 0755); err != nil {
		return errors.Wrapf(err, "Error creating state directory %s", a.path)
	}
	b, err := json.Marshal(a)
	if err != nil {
		return errors.Wrap(err, "Error marshaling current state")
	}
	err = ioutil.WriteFile(a.path, b, 0644)
	if err != nil {
		return errors.Wrapf(err, "Error writing current state to %s", a.path)
	}
	return nil
}
