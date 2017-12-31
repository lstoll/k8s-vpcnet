package enicontroller

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/golang/glog"
	k8svpcnet "github.com/lstoll/k8s-vpcnet"
	"github.com/lstoll/k8s-vpcnet/pkg/config"
	"github.com/lstoll/k8s-vpcnet/pkg/nodestate"
	"github.com/lstoll/k8s-vpcnet/pkg/objutil"
	"github.com/pkg/errors"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	runtimeutil "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// DefaultThreadiness is the concurrency we run, if not explicitly set
const DefaultThreadiness = 2

// EC2Client represents the operations we perform on the EC2 API
type EC2Client interface {
	DescribeInstancesPages(*ec2.DescribeInstancesInput, func(*ec2.DescribeInstancesOutput, bool) bool) error
	CreateNetworkInterface(*ec2.CreateNetworkInterfaceInput) (*ec2.CreateNetworkInterfaceOutput, error)
	ModifyNetworkInterfaceAttribute(*ec2.ModifyNetworkInterfaceAttributeInput) (*ec2.ModifyNetworkInterfaceAttributeOutput, error)
	AttachNetworkInterface(*ec2.AttachNetworkInterfaceInput) (*ec2.AttachNetworkInterfaceOutput, error)
	DescribeSubnets(*ec2.DescribeSubnetsInput) (*ec2.DescribeSubnetsOutput, error)
}

type Controller struct {
	// Threadiness is how many concurrent workers we run, defaults to
	// DefaultThreadiness
	Threadiness int

	ec2Client EC2Client

	client kubernetes.Interface
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
	client kubernetes.Interface,
	informerFactory informers.SharedInformerFactory,
	ec2 EC2Client,
) *Controller {

	nodeInformer := informerFactory.Core().V1().Nodes()

	controller := &Controller{
		nodeLister:  nodeInformer.Lister(),
		nodesSynced: nodeInformer.Informer().HasSynced,
		workqueue:   workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ENINodes"),
		ec2Client:   ec2,
		client:      client,
	}

	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueNode,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueNode(new)
		},
		DeleteFunc: controller.enqueueNode,
	})

	return controller
}

// Run starts the controller
func (c *Controller) Run() error {
	glog.Info("Starting ENI controller")

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
	glog.Info("ENI controller shutting down")

	return nil
}

// Stop stops the controller
func (c *Controller) Stop(err error) {
	glog.Infof("ENI controller shut down by %+v", err)

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
		runtimeutil.HandleError(err)
		return
	}
	c.workqueue.AddRateLimited(key)
}

// handleNode is the business logic of the controller.
func (c *Controller) handleNode(key string) error {
	node, err := c.nodeLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			glog.Infof("Node %q no longer exists [%+v]", key, err)
			return c.cleanupNode(key)
		}

		glog.Errorf("Fetching object from store failed [%+v]", err)
		return err
	}

	glog.V(2).Infof("Sync/Add/Update for Node %s", node.Name)

	// Make sure we have EC2 instance id
	if node.Spec.ProviderID == "" {
		glog.V(2).Infof("Node %s has no ProviderID set, skipping", key)
		return nil
	}

	iid, err := objutil.ProviderIDToAWSInstanceID(node.Spec.ProviderID)
	if err != nil {
		glog.Errorf("Can't convert provider ID %q to Instance ID [%+v]", node.Spec.ProviderID, err)
		return err
	}

	// See if we've stored additional instance data. If not, fetch and set
	instanceInfo, err := nodestate.AWSInstanceInfoFromAnnotations(node.Annotations)
	if err != nil {
		return errors.Wrapf(err, "Error unpacking instance info from annotations on node %s", node.Name)
	}
	if instanceInfo == nil {
		inst, err := c.getInstance(node.Name, iid)
		if err != nil {
			return errors.Wrapf(err, "Error fetching instance for node %s", node.Name)
		}
		instanceInfo = &nodestate.AWSInstanceInfo{
			InstanceType: *inst.InstanceType,
			SubnetID:     *inst.SubnetId,
		}
		for _, sg := range inst.SecurityGroups {
			instanceInfo.SecurityGroupIDs = append(instanceInfo.SecurityGroupIDs, *sg.GroupId)
		}

		subResp, err := c.ec2Client.DescribeSubnets(
			&ec2.DescribeSubnetsInput{SubnetIds: aws.StringSlice([]string{*inst.SubnetId})},
		)
		if err != nil {
			return errors.Wrapf(err, "Error fetching subnet info for %s", *inst.SubnetId)
		}
		if len(subResp.Subnets) != 1 {
			return fmt.Errorf("Expected one subnet match for %q, got %v", *inst.SubnetId, subResp.Subnets)
		}

		_, cb, err := net.ParseCIDR(*subResp.Subnets[0].CidrBlock)
		if err != nil {
			return errors.Wrapf(err, "Error getting ipnet from %q", *subResp.Subnets[0].CidrBlock)
		}
		instanceInfo.SubnetCIDR = (*config.IPNet)(cb)

		iiJSON, err := json.Marshal(instanceInfo)
		if err != nil {
			return errors.Wrap(err, "Error marshaling instance info JSON")
		}

		err = objutil.UpdateNode(c.client.CoreV1().Nodes(), node.Name, func(n *v1.Node) {
			n.Annotations[nodestate.EC2InfoKey] = string(iiJSON)
		})
		if err != nil {
			return errors.Wrapf(err, "error writing instance info for node %s", node.Name)
		}
		return nil // always bail out after an update, to let the watch re-trigger
	}

	numENIs, ok := k8svpcnet.InstanceENIsAvailable[instanceInfo.InstanceType]
	if !ok {
		return fmt.Errorf(
			"we have no ENI mapping info for node %s's instance type %s, cannot configure interfaces",
			node.Name, instanceInfo.InstanceType,
		)
	}
	// drop one, to account for the default first interface
	numENIs = numENIs - 1

	numIPs, ok := k8svpcnet.InstanceIPsAvailable[instanceInfo.InstanceType]
	if !ok {
		return fmt.Errorf(
			"we have no IP mapping info for node %s's instance type %s, cannot configure interfaces",
			node.Name, instanceInfo.InstanceType,
		)
	}

	// Check to see if we have a ENI configuration
	nc, err := nodestate.ENIConfigFromAnnotations(node.Annotations)
	if err != nil {
		return errors.Wrapf(err, "Error parsing ENI config from annotation for node %s", node.Name)
	}
	// If not, start a new empty one
	if nc == nil {
		nc = nodestate.ENIs{}
	}

	// Also label the node with the instance ID, to facilitate the node agents
	// watching for their own node, rather than all of them
	if _, ok := node.Labels[k8svpcnet.LabelAWSInstanceID]; !ok {
		glog.Infof("Node %s has no %s label, adding", node.Name, k8svpcnet.LabelAWSInstanceID)
		err = objutil.UpdateNode(c.client.CoreV1().Nodes(), node.Name, func(n *v1.Node) {
			n.Labels[k8svpcnet.LabelAWSInstanceID] = iid
		})
		if err != nil {
			return errors.Wrapf(err, "Error updating node %s", node.Name)
		}
		return nil // we just updated, wait for next watch trigger
	}

	// If we have an unattached interface, attach it
	for _, eni := range nc {
		if !eni.Attached {
			glog.Infof("Node %s has an unattached ENI at index %d, attaching it", node.Name, eni.Index)
			err := c.attachENI(node.Name, iid, eni)
			if err != nil {
				return err
			}
			// Static for now, we can change this when we use more than one ENI
			eni.Attached = true
			ifsJSON, err := json.Marshal(&nc)
			if err != nil {
				glog.Infof("Node %s - error marshaling interface %v [%v]", node.Name, nc, err)
				return err
			}
			err = objutil.UpdateNode(c.client.CoreV1().Nodes(), node.Name, func(n *v1.Node) {
				n.Annotations[nodestate.IFSKey] = string(ifsJSON)
			})
			// and bail out, next watch can take over
			return nil
		}
	}

	// If we don't have the max number of ENI's, add one. Do one at a time to make sure
	// we have them persisted
	if len(nc) < numENIs {
		glog.Infof("Adding an ENI to node %s", node.Name)
		newIf, err := c.createENI(node.Name, instanceInfo, numIPs)
		if err != nil {
			return err
		}
		// Start at 1, host IF is 0
		newIf.Index = len(nc) + 1
		newIf.CIDRBlock = instanceInfo.SubnetCIDR
		nc = append(nc, newIf)
		ifsJSON, err := json.Marshal(nc)
		if err != nil {
			glog.Infof("Node %s - error marshaling interface %v [%v]", node.Name, newIf, err)
			return err
		}
		err = objutil.UpdateNode(c.client.CoreV1().Nodes(), node.Name, func(n *v1.Node) {
			n.Annotations[nodestate.IFSKey] = string(ifsJSON)
		})
		if err != nil {
			return errors.Wrapf(err, "Error updating ENI annotation on node %s", node.Name)
		}
		// and bail out, next watch can take over
		return nil
	}

	return nil
}

// cleanupNode is called when the node no longer exists. Ensure the ENI and IPs
// are free
func (c *Controller) cleanupNode(key string) error {
	// TODO  implement
	return nil
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
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

// getInstance will return the *ec2.Instance for the given node
func (c *Controller) getInstance(nodeName, instanceID string) (*ec2.Instance, error) {
	instances := []*ec2.Instance{}
	instQuery := &ec2.DescribeInstancesInput{InstanceIds: aws.StringSlice([]string{instanceID})}
	err := c.ec2Client.DescribeInstancesPages(instQuery, func(o *ec2.DescribeInstancesOutput, _ bool) bool {
		for _, r := range o.Reservations {
			for _, i := range r.Instances {
				instances = append(instances, i)
			}
		}
		return true
	})
	if err != nil {
		glog.Errorf("Error fetching ec2 instances for node %s [%v]", nodeName, err)
		return nil, err
	}
	if len(instances) != 1 {
		err := fmt.Errorf("Incorrect number of instances found for %s [%v]", nodeName, instances)
		glog.Error(err)
		return nil, err
	}
	return instances[0], nil
}

// createENI will create a new ENI with the number of IPs noted. It will return
// the interface definition
func (c *Controller) createENI(nodeName string, instanceInfo *nodestate.AWSInstanceInfo, numIPs int) (*nodestate.ENI, error) {
	ceniResp, err := c.ec2Client.CreateNetworkInterface(
		&ec2.CreateNetworkInterfaceInput{
			Description:                    aws.String(fmt.Sprintf("K8S ENI for node %s", nodeName)),
			Groups:                         aws.StringSlice(instanceInfo.SecurityGroupIDs),
			SubnetId:                       aws.String(instanceInfo.SubnetID),
			SecondaryPrivateIpAddressCount: aws.Int64(int64(numIPs - 1)), // -1 for the "host" IP
		},
	)
	if err != nil {
		glog.Errorf("Error creating network interface for node %s [%v]", nodeName, err)
		return nil, err
	}

	ips := []net.IP{}
	for _, ip := range ceniResp.NetworkInterface.PrivateIpAddresses {
		if *ip.PrivateIpAddress != *ceniResp.NetworkInterface.PrivateIpAddress {
			ips = append(ips, net.ParseIP(*ip.PrivateIpAddress))
		}
	}

	return &nodestate.ENI{
		EniID:       *ceniResp.NetworkInterface.NetworkInterfaceId,
		Attached:    false,
		InterfaceIP: net.ParseIP(*ceniResp.NetworkInterface.PrivateIpAddress),
		IPs:         ips,
		MACAddress:  *ceniResp.NetworkInterface.MacAddress,
	}, nil
}

// attachENI will attach the provided ENI interface to the given node
func (c *Controller) attachENI(nodeName, instanceID string, eni *nodestate.ENI) error {
	if eni.Attached {
		return fmt.Errorf("Cannot attach %s to %s, it is flagged as being attached", eni.EniID, nodeName)
	}
	// attach interface
	attachResp, err := c.ec2Client.AttachNetworkInterface(
		&ec2.AttachNetworkInterfaceInput{
			InstanceId:         aws.String(instanceID),
			DeviceIndex:        aws.Int64(int64(eni.Index)),
			NetworkInterfaceId: &eni.EniID,
		},
	)
	if err != nil {
		glog.Errorf("Error attaching network interface %s to node %s [%v]", eni.EniID, nodeName, err)
		return err
	}

	// set interface deletion policy, so we mostly should auto-cleanup
	_, err = c.ec2Client.ModifyNetworkInterfaceAttribute(
		&ec2.ModifyNetworkInterfaceAttributeInput{
			NetworkInterfaceId: &eni.EniID,
			Attachment: &ec2.NetworkInterfaceAttachmentChanges{
				AttachmentId:        attachResp.AttachmentId,
				DeleteOnTermination: aws.Bool(true),
			},
		},
	)
	if err != nil {
		glog.Errorf("Error setting deletion policy on interface %s for node %s [%v]", eni.EniID, nodeName, err)
		return err
	}

	return nil
}
