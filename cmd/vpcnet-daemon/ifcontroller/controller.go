package ifcontroller

import (
	"fmt"
	"net"
	"time"

	"github.com/golang/glog"
	"github.com/lstoll/k8s-vpcnet/pkg/nodestate"
	"github.com/lstoll/k8s-vpcnet/pkg/objutil"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/runtime"
	runtimeutil "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// DefaultThreadiness is the concurrency we run, if not explicitly set
const DefaultThreadiness = 1

// CNIInstaller will be called when interfaces are ready on this machine, to
// install the CNI plugins and config. This keeps the machine not ready until
// Pod networks have a reasonable chance of succeeding.
type CNIInstaller interface {
	// Configured returns true if CNI plugins and config are written
	Configured() (bool, error)
	// Install installs the CNI plugins and writes the CNI plugin
	Install() error
}

// IFMgr is used to configure interfaces on the node
type IFMgr interface {
	// InterfaceExists checks if the given interface is configured on the host
	InterfaceExists(name string) (bool, error)
	// ConfigureInterface should create the giving ENI interface in the host OS
	ConfigureInterface(ifname string, mac string, ip *net.IPNet, subnet *net.IPNet) error
	// ConfigureRoutes configures the host routing table to route the given pod
	// IP allocations via the correct ENI on the host
	ConfigureRoutes(ifName string, awsEniAttachIndex int, eniSubnet *net.IPNet, podIPs []net.IP) error
	// ConfigureIPMasq configures the given pod IP addresses to masquerade
	// non-local traffic via the hosts' public address, rather than sending it
	// in to the VPC
	ConfigureIPMasq(hostIP net.IP, podIPs []net.IP) error
}

// Allocator is primed with the configured address pool allocations
type Allocator interface {
	// SetENIs sets the current set of ENI's we will allocate pod addresses from
	SetENIs(enis nodestate.ENIs) error
}

// Controller is a kubernetes controller to manage ENI configuration on a node,
// based on annotations set by the eni-controller
type Controller struct {
	// Threadiness is how many concurrent workers we run, defaults to
	// DefaultThreadiness
	Threadiness int

	// instanceID is the ID of the current instance
	instanceID string
	// hostIP is the main IP for this machine
	hostIP net.IP
	// ifMgr is the Interface Management Implementation
	ifMgr IFMgr
	// allocator is the IP address allocator for pods. We update this with the
	// nodes current ENI topology
	allocator Allocator
	// cniInstaller is used to install CNI plugins and config
	cniInstaller CNIInstaller

	// access to node information, and data on when the cache is ready
	nodeLister  corelisters.NodeLister
	nodesSynced cache.InformerSynced
	// workqueue for incoming nodes to be processed, manages ordering and
	// backoff
	workqueue workqueue.RateLimitingInterface

	// stopCh is used to signal that we should shut down
	stopCh chan struct{}
}

// New creates a new Interface Controller that is configured correctly
func New(
	informerFactory informers.SharedInformerFactory,
	currInstanceID string,
	hostIP net.IP,
	ifMgr IFMgr,
	allocator Allocator,
	installer CNIInstaller,
) *Controller {

	nodeInformer := informerFactory.Core().V1().Nodes()

	controller := &Controller{
		nodeLister:   nodeInformer.Lister(),
		nodesSynced:  nodeInformer.Informer().HasSynced,
		workqueue:    workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "IFControllerNodes"),
		instanceID:   currInstanceID,
		hostIP:       hostIP,
		ifMgr:        ifMgr,
		allocator:    allocator,
		cniInstaller: installer,
	}

	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueNode,
		UpdateFunc: func(old, new interface{}) {
			newNode := new.(*corev1.Node)
			oldNode := old.(*corev1.Node)
			if newNode.ResourceVersion == oldNode.ResourceVersion {
				// This is a resync, we get an update but the object never
				// changed. We can ignore this.
				return
			}
			controller.enqueueNode(new)
		},
		// We don't need to handle deletes, if this node is deleted assume it's
		// going away
	})

	return controller
}

// Run starts the controller
func (c *Controller) Run() error {
	glog.Info("Starting node interface controller")

	c.stopCh = make(chan struct{})
	thr := c.Threadiness
	if thr == 0 {
		thr = DefaultThreadiness
	}

	// install crashhandler
	defer runtimeutil.HandleCrash()

	// Let the workers stop when we are done
	defer c.workqueue.ShutDown()

	glog.Info("Waiting for caches to sync")

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForCacheSync(c.stopCh, c.nodesSynced) {
		err := fmt.Errorf("Timed out waiting for caches to sync")
		glog.Error(err)
		runtimeutil.HandleError(err)
		return err
	}

	for i := 0; i < thr; i++ {
		go wait.Until(c.runWorker, time.Second, c.stopCh)
	}

	glog.Infof("Workers started with threadiness %d", thr)

	<-c.stopCh
	glog.Info("Node Interface controller shutting down")

	return nil
}

// Stop stops the controller
func (c *Controller) Stop(err error) {
	glog.Infof("Node interface controller shut down by %+v", err)

	if c.stopCh != nil {
		c.stopCh <- struct{}{}
	}
	c.stopCh = nil
}

// enqueueNode takes a Node object and converts it into namespace/name key, then
// puts it on the work queue
func (c *Controller) enqueueNode(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	c.workqueue.AddRateLimited(key)
}

func (c *Controller) handleNode(key string) error {
	node, err := c.nodeLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			glog.Errorf("Object no longer exists [%+v]", err)
			runtime.HandleError(fmt.Errorf("Node '%s' in work queue no longer exists", key))
			return nil
		}

		glog.Errorf("Fetching object from store failed [%+v]", err)
		return err
	}

	if node.Spec.ProviderID == "" {
		// No provider ID, skip
		return nil
	}

	iid, err := objutil.ProviderIDToAWSInstanceID(node.Spec.ProviderID)
	if err != nil {
		glog.Errorf("Can't convert provider ID %q to Instance ID [%+v]", node.Spec.ProviderID, err)
		return err
	}

	if iid != c.instanceID {
		// not this node, skip
		return nil
	}

	// Check to see if we have a configuration
	nc, err := nodestate.ENIConfigFromAnnotations(node.Annotations)
	if err != nil {
		glog.Errorf("Failed to load ENI config from node annotations [%+v]", err)
		return err
	}

	if nc == nil {
		glog.Infof("Skipping node %s, has no network configuration", node.Name)
		return nil
	}

	configedENIs := nodestate.ENIs{}

	for _, config := range nc {
		if config.Attached {
			glog.Infof("Node %s has interface %s attached with MAC %s at index %d", node.Name, config.EniID, config.MACAddress, config.Index)
			exists, err := c.ifMgr.InterfaceExists(config.InterfaceName())
			if err != nil {
				return err
			}
			if !exists {
				glog.Infof("Node %s interface %s not configured, creating interface", node.Name, config.EniID)
				ipn := &net.IPNet{
					IP:   config.InterfaceIP,
					Mask: config.CIDRBlock.Mask,
				}
				err = c.ifMgr.ConfigureInterface(config.InterfaceName(), config.MACAddress, ipn, config.CIDRBlock.IPNet())
				if err != nil {
					glog.Errorf("Error configuring interface %s on node %s [%+v]", config.InterfaceName(), node.Name, err)
					return err
				}

				err = c.ifMgr.ConfigureRoutes(
					config.InterfaceName(),
					config.Index,
					config.CIDRBlock.IPNet(),
					config.IPs,
				)
				if err != nil {
					glog.Errorf("Error configuring routes for interface %s on node %s [%+v]", config.InterfaceName(), node.Name, err)
					return errors.Wrapf(err, "Error configuring routes for interface %s on node %s", config.InterfaceName(), node.Name)
				}

				err = c.ifMgr.ConfigureIPMasq(
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
	glog.Infof("Node %s updating IPAM state for %d interfaces", node.Name, len(configedENIs))

	if err := c.allocator.SetENIs(configedENIs); err != nil {
		glog.Errorf("Error updating ENIs on Allocator for node %s", node.Name)
		return err
	}

	// If we get to here, we have a fully configured node. If we have the taint,
	// remove it.
	cc, err := c.cniInstaller.Configured()
	if err != nil {
		glog.Errorf("Error checking if CNI configured [%+v]", err)
		return errors.Wrap(err, "Error checking if CNI configured")
	}

	if !cc {
		glog.Info("Installing & configuring CNI")
		if err := c.cniInstaller.Install(); err != nil {
			return errors.Wrap(err, "Error installing CNI deps")
		}
	}

	return nil
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling handleNode.
func (c *Controller) processNextItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := c.workqueue.Get()
	if quit {
		return false
	}

	// Tell the queue that we are done with processing this key. This unblocks the key for other workers
	// This allows safe parallel processing because two pods with the same key are never processed in
	// parallel.
	defer c.workqueue.Done(key)

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
		c.workqueue.Forget(key)
		return
	}

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if c.workqueue.NumRequeues(key) < 5 {
		glog.Errorf("Error syncing node %v, retrying [%+v]", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.workqueue.AddRateLimited(key)
		return
	}

	c.workqueue.Forget(key)

	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtimeutil.HandleError(err)

	// terminate the app
	glog.Fatalf("Repeated errors processing node %q, terminating [%+v]", key, err)
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}
