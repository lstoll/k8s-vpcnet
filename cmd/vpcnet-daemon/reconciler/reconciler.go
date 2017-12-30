package reconciler

import (
	"fmt"
	"time"

	"github.com/golang/glog"
	"github.com/lstoll/k8s-vpcnet/pkg/config"
	"github.com/lstoll/k8s-vpcnet/pkg/objutil"
	"github.com/pkg/errors"
	"k8s.io/api/core/v1"
	api_errors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// taintNoIPs is applied to the node when there are no free IPs for pods.
// pods with net: host can tolerate this to get scheduled anyway
const taintNoIPs = "k8s-vpcnet/no-free-ips"

// DefaultIPPoolCheckInterval is the default interval in which we check to see
// if the ip allocation pool is exhausted.
const DefaultIPPoolCheckInterval = 5 * time.Second

// Allocator defines what we expect to query from an allocator
type Allocator interface {
	// FreeAddressCount returns the number of unallocated IP addresses.
	FreeAddressCount() int
}

// Reconciler is a background process used to reconcile network state, and
// kubernetes cluster state it takes care of managing failed pods, and avoiding
// the scheduling of new pods
type Reconciler struct {
	// Config is the configuration for this VPCnet
	Config *config.Config
	// Clientset is the client for this cluster
	Clientset kubernetes.Interface
	// NodeIndexer is a cache.Indexer for this node. It is used for reading the
	// current state of this Node, rather than making an outgoing API call
	NodeIndexer cache.Indexer
	// Allocator is used for checking the state of the IP pool
	Allocator Allocator
	// NodeName is the name of the current node. What we retrieve from the
	// indexer
	NodeName string

	// IPPoolCheckInterval is how often we check the IP pool see how many
	// addresses are free. if not set, Default will be used
	IPPoolCheckInterval time.Duration

	stopC chan struct{}
}

// Run is the main loop for the reconciler. This call blocks until it is complete
func (r *Reconciler) Run() error {
	r.stopC = make(chan struct{})

	ipdur := r.IPPoolCheckInterval
	if ipdur == 0 {
		ipdur = DefaultIPPoolCheckInterval
	}

	ipCheckTicker := time.NewTicker(ipdur)

	for {
		select {
		case <-ipCheckTicker.C:
			if err := r.ipCheck(); err != nil {
				glog.Errorf("Error running IP pool check [%+v]", err)
			}
		case <-r.stopC:
			// wrap up
			ipCheckTicker.Stop()
			r.stopC = nil
			return nil
		}
	}
}

// Stop terminates the reconciler
func (r *Reconciler) Stop() {
	if r.stopC != nil {
		r.stopC <- struct{}{}
	}
}

// EvictPod will delete the pod from the system. This can be used if it's failed
// on this host. Before the delete is processed, we will trigger the IP check to
// try and taint the node first (if configured to), to prevent any replacement
// pod being re-scheduled on this node.
func (r *Reconciler) EvictPod(namespace, name string) error {
	if err := r.ipCheck(); err != nil {
		// ignore, non-fatal
		glog.Errorf("Error running pre-eviction IP pool check [%+v]", err)
	}
	glog.Infof("Deleting pod %s.%s", namespace, name)
	// TODO - is just killing the pods the best path forward?
	err := r.Clientset.CoreV1().Pods(namespace).Delete(name, &meta_v1.DeleteOptions{})
	if err != nil && !api_errors.IsNotFound(err) {
		return errors.Wrapf(err, "Error deleting pod %q from namespace %q", name, namespace)
	}
	return nil
}

func (r *Reconciler) ipCheck() error {
	if !r.Config.TaintWhenNoIPs {
		return nil // checking will have no effect, so just bail early
	}

	// fetch the current node object
	obj, exists, err := r.NodeIndexer.GetByKey(r.NodeName)
	if err != nil {
		return errors.Wrapf(err, "Fetching object with key %s from store failed", r.NodeName)
	}
	if !exists {
		return fmt.Errorf("could not find node %s in index", r.NodeName)
	}

	node, ok := obj.(*v1.Node)
	if !ok {
		return fmt.Errorf("object with key %s [%v] is not a node", r.NodeName, obj)
	}

	// handle ip check
	if r.Allocator.FreeAddressCount() < 1 {
		// uh oh, we're full. Taint the node if not already
		if !objutil.HasTaint(node, taintNoIPs, v1.TaintEffectNoSchedule) {
			if err := objutil.UpdateNode(r.Clientset.CoreV1().Nodes(), r.NodeName,
				objutil.AddTaint(taintNoIPs, v1.TaintEffectNoSchedule)); err != nil {
				return errors.Wrap(err, "Error tainting node")
			}
		}
	} else {
		// we have capacity! Ensure we don't have the taint
		if objutil.HasTaint(node, taintNoIPs, v1.TaintEffectNoSchedule) {
			if err := objutil.UpdateNode(r.Clientset.CoreV1().Nodes(), r.NodeName,
				objutil.RemoveTaint(taintNoIPs, v1.TaintEffectNoSchedule)); err != nil {
				return errors.Wrap(err, "Error untainting node")
			}
		}
	}

	return nil
}
