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
	runtimeutil "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
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
	// IPPoolCheckInterval is how often we check the IP pool see how many
	// addresses are free. if not set, Default will be used
	IPPoolCheckInterval time.Duration

	// Config is the configuration for this VPCnet
	config *config.Config
	// allocator is used for checking the state of the IP pool
	allocator Allocator
	// nodeName is the name of the current node. What we retrieve from the
	// indexer
	nodeName string

	// Clientset is the client for this cluster
	clientset kubernetes.Interface
	// NodeLister is a Lister for nodes. It is used for reading the
	// current state of this Node, rather than making an outgoing API call
	nodeLister  corelisters.NodeLister
	nodesSynced cache.InformerSynced

	stopC chan struct{}
}

// New returns a Reconciler configured for use
func New(
	client kubernetes.Interface,
	informerFactory informers.SharedInformerFactory,
	cfg *config.Config,
	allocator Allocator,
	nodeName string,

) *Reconciler {
	nodeInformer := informerFactory.Core().V1().Nodes()

	return &Reconciler{
		config:      cfg,
		clientset:   client,
		nodeLister:  nodeInformer.Lister(),
		nodesSynced: nodeInformer.Informer().HasSynced,
		allocator:   allocator,
		nodeName:    nodeName,
	}
}

// Run is the main loop for the reconciler. This call blocks until it is complete
func (r *Reconciler) Run() error {
	r.stopC = make(chan struct{})

	glog.Info("Starting pod/address reconciler")

	glog.Info("Waiting for caches to sync")

	// Wait for all involved caches to be synced,
	if !cache.WaitForCacheSync(r.stopC, r.nodesSynced) {
		err := fmt.Errorf("Timed out waiting for caches to sync")
		glog.Error(err)
		runtimeutil.HandleError(err)
		return err
	}

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
func (r *Reconciler) Stop(err error) {
	glog.Infof("Reconciler shut down by %+v", err)
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
	glog.Infof("Deleting pod %s/%s", namespace, name)
	// TODO - is just killing the pods the best path forward?
	err := r.clientset.CoreV1().Pods(namespace).Delete(name, &meta_v1.DeleteOptions{})
	if err != nil && !api_errors.IsNotFound(err) {
		return errors.Wrapf(err, "Error deleting pod %q from namespace %q", name, namespace)
	}
	return nil
}

// PoolFull will handle the state where there are no free IPs. This is safe to
// call repeatedly, it uses the cache to check current state. Obeys
// configuration around if we should taint or not.
func (r *Reconciler) PoolFull() error {
	if !r.config.TaintWhenNoIPs {
		return nil // checking will have no effect, so just bail early
	}

	node, err := r.nodeLister.Get(r.nodeName)
	if err != nil {
		return errors.Wrapf(err, "Fetching object with key %q from store failed", r.nodeName)
	}

	// uh oh, we're full. Taint the node if not already
	if !objutil.HasTaint(node, taintNoIPs, v1.TaintEffectNoSchedule) {
		if err := objutil.UpdateNode(r.clientset.CoreV1().Nodes(), r.nodeName,
			objutil.AddTaint(taintNoIPs, v1.TaintEffectNoSchedule)); err != nil {
			return errors.Wrap(err, "Error tainting node")
		}
	}

	return nil
}

// PoolNotFull will handle the state where there are free IPs. This is safe to
// call repeatedly, it uses the cache to check current state. Obeys
// configuration around if we should taint or not.
func (r *Reconciler) PoolNotFull() error {
	if !r.config.TaintWhenNoIPs {
		return nil // checking will have no effect, so just bail early
	}

	node, err := r.nodeLister.Get(r.nodeName)
	if err != nil {
		return errors.Wrapf(err, "Fetching object with key %q from store failed", r.nodeName)
	}

	// we have capacity! Ensure we don't have the taint
	if objutil.HasTaint(node, taintNoIPs, v1.TaintEffectNoSchedule) {
		if err := objutil.UpdateNode(r.clientset.CoreV1().Nodes(), r.nodeName,
			objutil.RemoveTaint(taintNoIPs, v1.TaintEffectNoSchedule)); err != nil {
			return errors.Wrap(err, "Error untainting node")
		}
	}

	return nil
}

func (r *Reconciler) ipCheck() error {
	if !r.config.TaintWhenNoIPs {
		return nil // checking will have no effect, so just bail early
	}

	// handle ip check
	if r.allocator.FreeAddressCount() < 1 {
		return r.PoolFull()
	}
	return r.PoolNotFull()
}
