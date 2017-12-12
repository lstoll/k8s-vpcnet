package main

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/cenk/backoff"
	"github.com/golang/glog"
	k8svpcnet "github.com/lstoll/k8s-vpcnet"
	"github.com/lstoll/k8s-vpcnet/pkg/config"
	"github.com/lstoll/k8s-vpcnet/pkg/vpcnetstate"
	"github.com/pkg/errors"
	"k8s.io/api/core/v1"
	api_errors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// handleNode is the business logic of the controller.
func (c *Controller) handleNode(key string) error {
	obj, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		glog.Errorf("Fetching object with key %s from store failed with %v", key, err)
		return err
	}

	if !exists {
		// Below we will warm up our cache with a Pod, so that we will see a delete for one pod
		glog.V(2).Infof("Node %s does not exist anymore", key)
		// TODO - cleanup here. make sure eni's/ips/etc gone
		return c.cleanupNode(key)
	}

	node, ok := obj.(*v1.Node)
	if !ok {
		glog.Errorf("Object with key %s [%v] is not a node!", key, obj)
		return nil
	}
	glog.V(2).Infof("Sync/Add/Update for Node %s", node.Name)

	// Make sure we have EC2 instance id
	if node.Spec.ExternalID == "" {
		glog.V(2).Infof("Node %s has no ExternalID set, skipping", key)
		return nil
	}

	// See if we've stored additional instance data. If not, fetch and set
	instanceInfo, err := vpcnetstate.AWSInstanceInfoFromAnnotations(node.Annotations)
	if err != nil {
		return errors.Wrapf(err, "Error unpacking instance info from annotations on node %s", node.Name)
	}
	if instanceInfo == nil {
		inst, err := c.getInstance(node)
		if err != nil {
			return errors.Wrapf(err, "Error fetching instance for node %s", node.Name)
		}
		instanceInfo = &vpcnetstate.AWSInstanceInfo{
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

		err = c.updateNode(node.Name, func(n *v1.Node) {
			n.Annotations[vpcnetstate.EC2InfoKey] = string(iiJSON)
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
	nc, err := vpcnetstate.ENIConfigFromAnnotations(node.Annotations)
	if err != nil {
		return errors.Wrapf(err, "Error parsing ENI config from annotation for node %s", node.Name)
	}
	// If not, start a new empty one
	if nc == nil {
		nc = vpcnetstate.ENIs{}
	}

	// Also label the node with the instance ID, to facilitate the node agents
	// watching for their own node, rather than all of them
	if _, ok := node.Labels["aws-instance-id"]; !ok {
		glog.Infof("Node %s has no aws-instance-id label, adding", node.Name)
		err = c.updateNode(node.Name, func(n *v1.Node) {
			n.Labels["aws-instance-id"] = node.Spec.ExternalID
		})
		if err != nil {
			return errors.Wrapf(err, "Error updating node %s", node.Name)
		}
		return nil // we just updated, wait for next watch trigger
	}

	// If we have an unattached interface, attach it
	for _, eni := range nc {
		if !eni.Attached {
			glog.Infof("Node %s has an unattached ENI at index %d, attaching it", eni.Index, node.Name)
			err := c.attachENI(node, eni)
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
			c.updateNode(node.Name, func(n *v1.Node) {
				n.Annotations[vpcnetstate.IFSKey] = string(ifsJSON)
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
		err = c.updateNode(node.Name, func(n *v1.Node) {
			n.Annotations[vpcnetstate.IFSKey] = string(ifsJSON)
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

// updateNode will get/update the node until there is no conflict. The passed in
// function is used as the mutator.
func (c *Controller) updateNode(name string, mutator func(node *v1.Node)) error {
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
			node, err = c.nodesClient.Get(name, meta_v1.GetOptions{})
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

// getInstance will return the *ec2.Instance for the given node
func (c *Controller) getInstance(node *v1.Node) (*ec2.Instance, error) {
	instances := []*ec2.Instance{}
	instQuery := &ec2.DescribeInstancesInput{InstanceIds: aws.StringSlice([]string{node.Spec.ExternalID})}
	err := c.ec2Client.DescribeInstancesPages(instQuery, func(o *ec2.DescribeInstancesOutput, _ bool) bool {
		for _, r := range o.Reservations {
			for _, i := range r.Instances {
				instances = append(instances, i)
			}
		}
		return true
	})
	if err != nil {
		glog.Errorf("Error fetching ec2 instances for node %s [%v]", node.Name, err)
		return nil, err
	}
	if len(instances) != 1 {
		err := fmt.Errorf("Incorrect number of instances found for %s [%v]", node.Name, instances)
		glog.Error(err)
		return nil, err
	}
	return instances[0], nil
}

// createENI will create a new ENI with the number of IPs noted. It will return
// the interface definition
func (c *Controller) createENI(nodeName string, instanceInfo *vpcnetstate.AWSInstanceInfo, numIPs int) (*vpcnetstate.ENI, error) {
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

	return &vpcnetstate.ENI{
		EniID:       *ceniResp.NetworkInterface.NetworkInterfaceId,
		Attached:    false,
		InterfaceIP: net.ParseIP(*ceniResp.NetworkInterface.PrivateIpAddress),
		IPs:         ips,
		MACAddress:  *ceniResp.NetworkInterface.MacAddress,
	}, nil
}

// attachENI will attach the provided ENI interface to the given node
func (c *Controller) attachENI(node *v1.Node, eni *vpcnetstate.ENI) error {
	if eni.Attached {
		return fmt.Errorf("Cannot attach %s to %s, it is flagged as being attached", eni.EniID, node.Name)
	}
	// attach interface
	attachResp, err := c.ec2Client.AttachNetworkInterface(
		&ec2.AttachNetworkInterfaceInput{
			InstanceId:         aws.String(node.Spec.ExternalID),
			DeviceIndex:        aws.Int64(int64(eni.Index)),
			NetworkInterfaceId: &eni.EniID,
		},
	)
	if err != nil {
		glog.Errorf("Error attaching network interface %s to node %s [%v]", eni.EniID, node.Name, err)
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
		glog.Errorf("Error setting deletion policy on interface %s for node %s [%v]", eni.EniID, node.Name, err)
		return err
	}

	return nil
}

// hasTaint returns true if node has matching taints
func hasTaint(node *v1.Node, key string, effect v1.TaintEffect) bool {
	for _, t := range node.Spec.Taints {
		if t.Key == key && t.Effect == effect {
			return true
		}
	}
	return false
}

// numAttached returns the number of attached ENI interfaces
func numAttached(enis vpcnetstate.ENIs) int {
	num := 0
	for _, eni := range enis {
		if eni.Attached {
			num++
		}
	}
	return num
}
