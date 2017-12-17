package main

import (
	"fmt"
	"log"
	"net"
	"time"

	"github.com/cenk/backoff"
	"github.com/golang/glog"
	cniconfig "github.com/lstoll/k8s-vpcnet/pkg/cni/config"
	"github.com/lstoll/k8s-vpcnet/pkg/cni/diskstore"
	"github.com/lstoll/k8s-vpcnet/pkg/config"
	"github.com/lstoll/k8s-vpcnet/pkg/ifmgr"
	"github.com/lstoll/k8s-vpcnet/pkg/vpcnetstate"
	"github.com/lstoll/k8s-vpcnet/version"
	"github.com/pkg/errors"
	"k8s.io/api/core/v1"
	api_errors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimeutil "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	client_v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

type controller struct {
	indexer      cache.Indexer
	queue        workqueue.RateLimitingInterface
	informer     cache.Controller
	clientSet    kubernetes.Interface
	instanceID   string
	vpcnetConfig *config.Config
	hostIP       net.IP
	reconciler   *reconciler
	IFMgr        *ifmgr.IFMgr
	IPAMState    *vpcnetstate.AllocatorState
}

func (c *controller) handleNode(key string) error {
	obj, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		glog.Errorf("Fetching object with key %s from store failed with %v", key, err)
		return err
	}

	if !exists {
		glog.V(2).Infof("Node %s does not exist anymore, why are we still running?", key)
	}

	node, ok := obj.(*v1.Node)
	if !ok {
		glog.Errorf("Object with key %s [%v] is not a node!", key, obj)
		return nil
	}

	if node.Spec.ExternalID != c.instanceID {
		glog.V(4).Infof("Skipping node %s, its instance ID %s does not match local %s", node.Name, node.Spec.ExternalID, c.instanceID)
		return nil
	}

	// Check to see if we have a configuration
	nc, err := vpcnetstate.ENIConfigFromAnnotations(node.Annotations)

	if nc == nil {
		glog.Infof("Skipping node %s, has no network configuration", node.Name)
		return nil
	}

	configedENIs := vpcnetstate.ENIs{}

	for _, config := range nc {
		if config.Attached {
			glog.V(2).Infof("Node %s has interface %s attached with MAC %s at index %d", node.Name, config.EniID, config.MACAddress, config.Index)
			exists, err := c.IFMgr.InterfaceExists(config.InterfaceName())
			if err != nil {
				return err
			}
			if !exists {
				glog.V(2).Infof("Node %s interface %s not configured, creating interface", node.Name, config.EniID)
				ipn := &net.IPNet{
					IP:   config.InterfaceIP,
					Mask: config.CIDRBlock.Mask,
				}
				err = c.IFMgr.ConfigureInterface(config.InterfaceName(), config.MACAddress, ipn, config.CIDRBlock.IPNet())
				if err != nil {
					glog.Errorf("Error configuring interface %s on node %s [%+v]", config.InterfaceName(), node.Name, err)
					return err
				}

				err = c.IFMgr.ConfigureRoutes(
					config.InterfaceName(),
					config.Index,
					config.CIDRBlock.IPNet(),
					config.IPs,
				)
				if err != nil {
					glog.Errorf("Error configuring routes for interface %s on node %s [%+v]", config.InterfaceName(), node.Name, err)
					return errors.Wrapf(err, "Error configuring routes for interface %s on node %s", config.InterfaceName(), node.Name)
				}

				err = c.IFMgr.ConfigureIPMasq(
					c.hostIP,
					config.IPs,
				)
				if err != nil {
					glog.Errorf("Error configuring ipmasq for interface %s on node %s [%+v]", config.InterfaceName(), node.Name, err)
					return errors.Wrapf(err, "Error configuring ipmasq for interface %s on node %s", config.InterfaceName(), node.Name)
				}
			}
			configedENIs = append(configedENIs, config)
		}
	}

	// Update the state of the IPAM to know about this ENI
	glog.V(2).Infof("Node %s updating IPAM state for %d interfaces", node.Name, len(configedENIs))
	c.IPAMState.ENIs = configedENIs
	if err := c.IPAMState.Write(); err != nil {
		glog.Errorf("Error writing ENI map for %s", node.Name)
		return err
	}

	// If we get to here, we have a fully configured node. If we have the taint,
	// remove it.
	cc, err := cniConfigured()
	if err != nil {
		glog.Errorf("Error checking if CNI configured [%+v]", err)
		return errors.Wrap(err, "Error checking if CNI configured")
	}

	if !cc {
		glog.Info("Installing & configuring CNI")
		if err := installCNI(c.vpcnetConfig); err != nil {
			log.Fatalf("Error installing CNI deps [%v]", err)
		}
	}

	// if we don't have the reconciler, kick that off
	if c.reconciler == nil {
		store, err := diskstore.New(cniconfig.CNIName, "")
		if err != nil {
			glog.Fatalf("Error opening diskstore [%+v]", err)
		}
		defer store.Close()

		c.reconciler = &reconciler{
			store:        store,
			indexer:      c.indexer,
			nodeName:     node.Name,
			clientSet:    c.clientSet,
			missingSince: map[string]time.Time{},
		}

		go func() {
			for {
				err := c.reconciler.Reconcile()
				if err != nil {
					glog.Fatalf("Error reconciling pods to allocations")
				}
				time.Sleep(1 * time.Second)
			}
		}()
	}

	return nil
}

// updateNode will get/update the node until there is no conflict. The passed in
// function is used as the mutator
func updateNode(client client_v1.NodeInterface, name string, mutator func(node *v1.Node)) error {
	node, err := client.Get(name, meta_v1.GetOptions{})
	if err != nil {
		return errors.Wrapf(err, "Error fetching node %s to update", node)
	}
	bo := backoff.NewExponentialBackOff()
	for {
		mutator(node)

		_, err := client.Update(node)
		if api_errors.IsConflict(err) {
			// Node was modified, fetch and try again
			glog.V(2).Infof("Conflict updating node %s, retrying", name)
			node, err = client.Get(name, meta_v1.GetOptions{})
			if err != nil {
				return errors.Wrapf(err, "Error fetching node %s to update", node)
			}
		} else if err != nil {
			return errors.Wrapf(err, "Error updating node %s", name)
		} else {
			break
		}

		time.Sleep(bo.NextBackOff())
		// TODO - prevent indefinite loops?
	}
	glog.V(2).Infof("Node %s updated", name)
	return nil
}

func (c *controller) processNextItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	// Tell the queue that we are done with processing this key. This unblocks the key for other workers
	// This allows safe parallel processing because two pods with the same key are never processed in
	// parallel.
	defer c.queue.Done(key)

	// Invoke the method containing the business logic
	err := c.handleNode(key.(string))
	// Handle the error if something went wrong during the execution of the business logic
	c.handleErr(err, key)
	return true
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *controller) handleErr(err error, key interface{}) {
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		c.queue.Forget(key)
		return
	}

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if c.queue.NumRequeues(key) < 5 {
		glog.Infof("Error syncing pod %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.queue.AddRateLimited(key)
		return
	}

	c.queue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtimeutil.HandleError(err)

	glog.Fatalf("Repeated errors processing node %q [%+v]", key, err)
}

func (c *controller) Run(threadiness int, stopCh chan struct{}) {
	defer runtimeutil.HandleCrash()

	// Let the workers stop when we are done
	defer c.queue.ShutDown()
	glog.Infof("Starting Node controller version %s", version.Version)

	go c.informer.Run(stopCh)

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		runtimeutil.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	glog.Info("Stopping Node controller")
}

func (c *controller) runWorker() {
	for c.processNextItem() {
	}
}
