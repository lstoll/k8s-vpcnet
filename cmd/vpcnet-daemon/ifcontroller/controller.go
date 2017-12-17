package ifcontroller

import (
	"fmt"
	"net"
	"time"

	"github.com/golang/glog"
	"github.com/lstoll/k8s-vpcnet/pkg/allocator"
	"github.com/lstoll/k8s-vpcnet/pkg/config"
	"github.com/lstoll/k8s-vpcnet/pkg/ifmgr"
	"github.com/lstoll/k8s-vpcnet/pkg/nodestate"
	"github.com/lstoll/k8s-vpcnet/version"
	"github.com/pkg/errors"
	"k8s.io/api/core/v1"
	runtimeutil "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// CNIInstaller will be called when interfaces are ready on this machine, to
// install the CNI plugins and config. This keeps the machine not ready until
// Pod networks have a reasonable chance of succeeding.
type CNIInstaller interface {
	// Configured returns true if CNI plugins and config are written
	Configured() (bool, error)
	// Install installs the CNI plugins and writes the CNI plugin
	Install() error
}

// Controller is a kubernetes controller to manage ENI configuration on a node,
// based on annotations set by the eni-controller
type Controller struct {
	// Indexer is the cache.Indexer
	Indexer cache.Indexer
	// Queue is the work queue
	Queue workqueue.RateLimitingInterface
	// Informer is the information
	Informer cache.Controller
	// ClientSet is the API client for the Kubernetes Cluster
	ClientSet kubernetes.Interface
	// InstanceID is the ID of the current instance
	InstanceID string
	// VPCNetConfig is the configuration for this network
	VPCNetConfig *config.Config
	// HostIP is the main IP for this machine
	HostIP net.IP
	// IFMgr is the Interface Management Implementation
	IFMgr *ifmgr.IFMgr
	// Allocator is the IP address allocator for pods. We update this with the
	// nodes current ENI topology
	Allocator *allocator.Allocator
	// CNIInstaller is used to install CNI plugins and config
	CNIInstaller CNIInstaller
}

func (c *Controller) handleNode(key string) error {
	obj, exists, err := c.Indexer.GetByKey(key)
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

	if node.Spec.ExternalID != c.InstanceID {
		glog.V(4).Infof("Skipping node %s, its instance ID %s does not match local %s", node.Name, node.Spec.ExternalID, c.InstanceID)
		return nil
	}

	// Check to see if we have a configuration
	nc, err := nodestate.ENIConfigFromAnnotations(node.Annotations)

	if nc == nil {
		glog.Infof("Skipping node %s, has no network configuration", node.Name)
		return nil
	}

	configedENIs := nodestate.ENIs{}

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
					c.HostIP,
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

	if err := c.Allocator.SetENIs(configedENIs); err != nil {
		glog.Errorf("Error updating ENIs on Allocator for node %s", node.Name)
		return err
	}

	// If we get to here, we have a fully configured node. If we have the taint,
	// remove it.
	cc, err := c.CNIInstaller.Configured()
	if err != nil {
		glog.Errorf("Error checking if CNI configured [%+v]", err)
		return errors.Wrap(err, "Error checking if CNI configured")
	}

	if !cc {
		glog.Info("Installing & configuring CNI")
		if err := c.CNIInstaller.Install(); err != nil {
			return errors.Wrap(err, "Error installing CNI deps")
		}
	}

	return nil
}

func (c *Controller) processNextItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := c.Queue.Get()
	if quit {
		return false
	}
	// Tell the queue that we are done with processing this key. This unblocks the key for other workers
	// This allows safe parallel processing because two pods with the same key are never processed in
	// parallel.
	defer c.Queue.Done(key)

	// Invoke the method containing the business logic
	err := c.handleNode(key.(string))
	// Handle the error if something went wrong during the execution of the business logic
	c.handleErr(err, key)
	return true
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		c.Queue.Forget(key)
		return
	}

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if c.Queue.NumRequeues(key) < 5 {
		glog.Infof("Error syncing pod %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.Queue.AddRateLimited(key)
		return
	}

	c.Queue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtimeutil.HandleError(err)

	glog.Fatalf("Repeated errors processing node %q [%+v]", key, err)
}

func (c *Controller) Run(threadiness int, stopCh chan struct{}) {
	defer runtimeutil.HandleCrash()

	// Let the workers stop when we are done
	defer c.Queue.ShutDown()
	glog.Infof("Starting Node controller version %s", version.Version)

	go c.Informer.Run(stopCh)

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForCacheSync(stopCh, c.Informer.HasSynced) {
		runtimeutil.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	glog.Info("Stopping Node controller")
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}
