package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/cenk/backoff"
	"github.com/golang/glog"
	"github.com/lstoll/k8s-vpcnet/vpcnetstate"
	"github.com/pkg/errors"

	"github.com/lstoll/k8s-vpcnet/version"
	"k8s.io/api/core/v1"
	api_errors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	client_v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

var (
	netconfPath string
)

// noInterfaceTaint is the key used on the taint before the node has an interface
const taintNoInterface = "k8s-vpcnet/no-interface-configured"

func main() {
	flag.Set("logtostderr", "true")

	var mode, ifname, mac, ip, cb string

	flag.StringVar(&mode, "mode", "kubernetes-auto", "Mode to run in. Default is normal on-cluster, or manual can be specified to configure directly")

	// Manual
	flag.StringVar(&mac, "host-mac", "", "[manual] MAC address of the ENI")
	flag.StringVar(&ifname, "interface-name", "", "[manual] name for the interface")
	flag.StringVar(&ip, "ip", "", "[manual] IP address for the bridge")
	flag.StringVar(&cb, "cidr-block", "", "[manual] VPC Subnet CIDR block (e.g 10.0.0.0/8")

	// Kubernetes
	flag.StringVar(&netconfPath, "netconf-path", vpcnetstate.DefaultENIMapPath, "[kubernetes] Path to write the netconf for CNI IPAM")

	flag.Parse()

	switch mode {
	case "kubernetes-auto":
		glog.Info("Installing CNI deps")
		err := installCNI()
		if err != nil {
			log.Fatalf("Error installing CNI deps [%v]", err)
		}
		glog.Info("Running node configurator in on-cluster mode")
		runk8s()
		// TODO Poll for current running pods, delete lock files for gone pods.
		// Should we just loop http://localhost:10255/pods/  ({"kind":"PodList"})
	case "manual":
		if mac == "" || ifname == "" || ip == "" || cb == "" {
			glog.Fatal("All args required in manual mode")
		}
		glog.Info("Configuring interfaces directly")
		_, subnet, err := net.ParseCIDR(cb)
		if err != nil {
			glog.Fatalf("Error parsing subnet cidr [%v]", err)
		}
		ifip := &net.IPNet{
			IP:   net.ParseIP(ip),
			Mask: subnet.Mask,
		}
		err = configureInterface(ifname, mac, ifip, subnet)
		if err != nil {
			glog.Fatalf("Error configuring interface [%v]", err)
		}
	default:
		glog.Fatalf("Invalid mode %q specified", mode)
	}
}

func runk8s() {
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		glog.Fatalf("Error getting client config [%v]", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatalf("Error getting client [%v]", err)
	}

	// TODO - how do we just watch a limted node? Do we need the controller to
	// add a label for the instance id maybe to them maybe, and filter on that?
	watchlist := cache.NewListWatchFromClient(
		clientset.Core().RESTClient(),
		"nodes", v1.NamespaceAll,
		fields.Everything(),
	)

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	indexer, informer := cache.NewIndexerInformer(watchlist, &v1.Node{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// IndexerInformer uses a delta queue, therefore for deletes we have to use this
			// key function.
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
	}, cache.Indexers{})

	nodesClient := clientset.Nodes()

	sess := session.Must(session.NewSession())
	md := ec2metadata.New(sess)
	iid, err := md.GetMetadata("instance-id")
	if err != nil {
		glog.Fatalf("Error determining current instance ID [%v]", err)
	}

	c := &controller{
		indexer:     indexer,
		queue:       queue,
		informer:    informer,
		instanceID:  iid,
		nodesClient: nodesClient,
	}

	// Now let's start the controller
	stop := make(chan struct{})
	defer close(stop)
	go c.Run(1, stop)

	// Wait forever
	select {}
}

type controller struct {
	indexer     cache.Indexer
	queue       workqueue.RateLimitingInterface
	informer    cache.Controller
	nodesClient client_v1.NodeInterface
	instanceID  string
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
			glog.V(2).Infof("Node %s has interface %s attached", node.Name, config.EniID)
			exists, err := interfaceExists(config.InterfaceName())
			if err != nil {
				return err
			}
			if !exists {
				glog.V(2).Infof("Node %s interface %s not configured, creating interface", node.Name, config.EniID)
				_, subnet, err := net.ParseCIDR(config.CIDRBlock)
				if err != nil {
					return errors.Wrap(err, "Error parsing subnet cidr")
				}
				ipn := &net.IPNet{
					IP:   net.ParseIP(config.InterfaceIP),
					Mask: subnet.Mask,
				}
				err = configureInterface(config.InterfaceName(), config.MACAddress, ipn, subnet)
				if err != nil {
					glog.Errorf("Error configuring interface %d on node %s", config.InterfaceName(), node.Name)
					return err
				}
			}
			configedENIs = append(configedENIs, config)
		}
	}

	// batch write the addresses
	glog.V(2).Infof("Node %s writing interface map for %d interfaces", node.Name, len(configedENIs))
	err = vpcnetstate.WriteENIMap(netconfPath, configedENIs)
	if err != nil {
		glog.Errorf("Error writing ENI map for %s", node.Name)
		return err
	}

	// If we get to here, we have a fully configured node. If we have the taint,
	// remove it.
	var tainted bool
	for _, t := range node.Spec.Taints {
		if t.Key == taintNoInterface &&
			t.Effect == v1.TaintEffectNoSchedule {
			tainted = true
		}
	}

	if tainted {
		c.updateNode(node.Name, func(n *v1.Node) {
			keep := []v1.Taint{}
			for _, t := range n.Spec.Taints {
				if !(t.Key == taintNoInterface &&
					t.Effect == v1.TaintEffectNoSchedule) {
					keep = append(keep, t)
				}
			}
			n.Spec.Taints = keep
		})
	}

	return nil
}

// updateNode will get/update the node until there is no conflict. The passed in
// function is used as the mutator
func (c *controller) updateNode(name string, mutator func(node *v1.Node)) error {
	node, err := c.nodesClient.Get(name, meta_v1.GetOptions{})
	if err != nil {
		return errors.Wrapf(err, "Error fetching node %s to update", node)
	}
	bo := backoff.NewExponentialBackOff()
	for {
		mutator(node)

		_, err := c.nodesClient.Update(node)
		if api_errors.IsConflict(err) {
			// Node was modified, fetch and try again
			glog.V(2).Infof("Conflict updating node %s, retrying", name)
			node, err := c.nodesClient.Get(name, meta_v1.GetOptions{})
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
	runtime.HandleError(err)
	glog.Infof("Dropping node %q out of the queue: %v", key, err)
}

func (c *controller) Run(threadiness int, stopCh chan struct{}) {
	defer runtime.HandleCrash()

	// Let the workers stop when we are done
	defer c.queue.ShutDown()
	glog.Infof("Starting Node controller version %s", version.Version)

	go c.informer.Run(stopCh)

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		runtime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
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
