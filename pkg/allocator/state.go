package allocator

import (
	"encoding/json"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/lstoll/k8s-vpcnet/pkg/nodestate"
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
	// this node. This it the data that is annotated on the node object
	ENIs nodestate.ENIs `json:"enis"`
	// Allocations are a map of IP addresses to information about the allocation
	Allocations map[string]Allocation `json:"allocations"`
	// LastFreeIP is tracked to try and allocate an address far away from this.
	LastFreedIP net.IP

	path   string
	pathMu sync.Mutex
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
