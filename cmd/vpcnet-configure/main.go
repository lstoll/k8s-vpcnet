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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	runtimeutil "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	client_v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	udbus "k8s.io/kubernetes/pkg/util/dbus"
	uiptables "k8s.io/kubernetes/pkg/util/iptables"
	uexec "k8s.io/utils/exec"
)

var (
	netconfPath string
)

// noInterfaceTaint is the key used on the taint before the node has an interface
const taintNoInterface = "k8s-vpcnet/no-interface-configured"

// taintNoIPs is applied to the node when there are no free IPs for pods.
// pods with net: host can tolerate this to get scheduled anyway
const taintNoIPs = "k8s-vpcnet/no-free-ips"

func main() {
	flag.Set("logtostderr", "true")

	flag.StringVar(&netconfPath, "netconf-path", vpcnetstate.DefaultENIMapPath, "[kubernetes] Path to write the netconf for CNI IPAM")

	flag.Parse()

	cfg, err := config.Load(config.DefaultConfigPath)
	if err != nil {
		log.Fatalf("Error loading configuration [%+v]", err)
	}

	glog.Info("Installing CNI deps")
	err = installCNI(cfg)
	if err != nil {
		log.Fatalf("Error installing CNI deps [%v]", err)
	}

	glog.Info("Running node configurator in on-cluster mode")
	runk8s(cfg)
	// TODO Poll for current running pods, delete lock files for gone pods.
	// Should we just loop http://localhost:10255/pods/  ({"kind":"PodList"})
}

func runk8s(vpcnetConfig *config.Config) {
	sess := session.Must(session.NewSession())
	md := ec2metadata.New(sess)
	iid, err := md.GetMetadata("instance-id")
	if err != nil {
		glog.Fatalf("Error determining current instance ID [%v]", err)
	}
	hostIPStr, err := md.GetMetadata("local-ipv4")
	if err != nil {
		glog.Fatalf("Error determining host IP address [%+v]", err)
	}
	hostIP := net.ParseIP(hostIPStr)

	if vpcnetConfig.Network.HostPrimaryInterface == "" {
		glog.V(2).Info("Finding host primary interface")
		vpcnetConfig.Network.HostPrimaryInterface, err = primaryInterface(md)
		if err != nil {
			glog.Fatalf("Error finding host's primary interface [%+v]", err)
		}
		glog.V(2).Infof("Primary interface is %q", vpcnetConfig.Network.HostPrimaryInterface)
	}

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
	// watchlist := cache.NewListWatchFromClient(
	// 	clientset.Core().RESTClient(),
	// 	"nodes", v1.NamespaceAll,
	// 	fields.OneTermEqualSelector("aws-instance-id", iid),
	// )

	selector := labels.NewSelector()
	iidReq, err := labels.NewRequirement("aws-instance-id", selection.Equals, []string{iid})
	if err != nil {
		glog.Fatalf("Instance ID label selector is not valid [%+v]", err)
	}
	selector = selector.Add(*iidReq)

	listFunc := func(options meta_v1.ListOptions) (runtime.Object, error) {
		return clientset.Core().RESTClient().Get().
			Namespace(v1.NamespaceAll).
			Resource("nodes").
			VersionedParams(&options, meta_v1.ParameterCodec).
			LabelsSelectorParam(selector).
			Do().
			Get()
	}

	watchFunc := func(options meta_v1.ListOptions) (watch.Interface, error) {
		options.Watch = true
		return clientset.Core().RESTClient().Get().
			Namespace(v1.NamespaceAll).
			Resource("nodes").
			VersionedParams(&options, meta_v1.ParameterCodec).
			LabelsSelectorParam(selector).
			Watch()
	}
	lw := &cache.ListWatch{ListFunc: listFunc, WatchFunc: watchFunc}

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	indexer, informer := cache.NewIndexerInformer(lw, &v1.Node{}, 0, cache.ResourceEventHandlerFuncs{
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

	ipt := uiptables.New(uexec.New(), udbus.New(), uiptables.ProtocolIpv4)

	// Run up the controller
	c := &controller{
		indexer:      indexer,
		queue:        queue,
		informer:     informer,
		instanceID:   iid,
		vpcnetConfig: vpcnetConfig,
		hostIP:       hostIP,
		clientSet:    clientset,
		IFMgr:        ifmgr.New(vpcnetConfig.Network, ipt),
	}

	stop := make(chan struct{})
	defer close(stop)
	go c.Run(1, stop)

	// Wait forever
	select {}
}

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
		updateNode(c.clientSet.Core().Nodes(), node.Name, func(n *v1.Node) {
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

	// if we don't have the reconciler, kick that off
	if c.reconciler == nil {
		store, err := diskstore.New(cniconfig.CNIName, "")
		if err != nil {
			glog.Fatalf("Error opening diskstore [%+v]", err)
		}
		defer store.Close()

		c.reconciler = &reconciler{
			store:     store,
			indexer:   c.indexer,
			nodeName:  node.Name,
			clientSet: c.clientSet,
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

func primaryInterface(md *ec2metadata.EC2Metadata) (string, error) {
	mac, err := md.GetMetadata("mac")
	if err != nil {
		return "", errors.Wrap(err, "Error finding interface MAC")
	}

	ifs, err := net.Interfaces()
	if err != nil {
		return "", errors.Wrap(err, "Error listing machine interfaces")
	}

	var ifName string

	for _, i := range ifs {
		if i.HardwareAddr.String() == mac {
			ifName = i.Name
		}
	}

	if ifName == "" {
		return "", fmt.Errorf("Could not find interface name for MAC %q", mac)
	}

	return ifName, nil
}
