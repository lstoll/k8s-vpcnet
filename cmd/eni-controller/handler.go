package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/cenk/backoff"
	"github.com/golang/glog"
	"github.com/lstoll/k8s-vpcnet/vpcnetstate"
	"github.com/pkg/errors"
	"k8s.io/api/core/v1"
	api_errors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// noInterfaceTaint is the key used on the taint before the node has an interface
	taintNoInterface = "k8s-vpcnet/no-interface-configured"
	// ipAddrCount is the number of addresses to assign to an ENI
	// TODO - table out for instance type
	ipAddrCount = 10
	// brIf is the name of the interface we're using, for now.
	brIf = "vpcbr0"
)

// handleNode is the business logic of the controller.
func (c *Controller) handleNode(key string) error {
	obj, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		glog.Errorf("Fetching object with key %s from store failed with %v", key, err)
		return err
	}

	node, ok := obj.(*v1.Node)
	if !ok {
		glog.Errorf("Object with key %s [%v] is not a node!", key, obj)
		return nil
	}

	if !exists {
		// Below we will warm up our cache with a Pod, so that we will see a delete for one pod
		glog.V(2).Infof("Node %s does not exist anymore", key)
		// TODO - cleanup here. make sure eni's/ips/etc gone
		return c.cleanupNode(node)
	}
	glog.Infof("Sync/Add/Update for Node %s", node.Name)

	// Check to see if we have a configuration
	nc, err := vpcnetstate.ENIConfigFromAnnotations(node.Annotations)

	// if we have to congfiguration, taint the node that we don't ASAP to avoid
	// pods being scheuled on the node. We should ensure our configuration
	// daemonset tolerates this.
	if nc == nil && !hasTaint(node, taintNoInterface, v1.TaintEffectNoSchedule) {
		glog.Infof("Node %s has no configuration, tainting with %s", node.Name, taintNoInterface)
		c.updateNode(node.Name, func(n *v1.Node) {
			n.Spec.Taints = append(n.Spec.Taints,
				v1.Taint{
					Key:    taintNoInterface,
					Effect: v1.TaintEffectNoSchedule,
				},
			)
		})
		// and bail out, our update will triger a new watch loop that'll pass this
		return nil
	}

	// If we have no configuration, create and provision an ENI, and store the
	// configuration on the interface
	if nc == nil {
		glog.Infof("Node %s has no configuration, creating an ENI", node.Name)
		newIf, err := c.createENI(node)
		if err != nil {
			return err
		}
		// Static for now, we can change this when we use more than one ENI
		// Start at 1, host IF is 0
		newIf.Index = 1
		ifsJSON, err := json.Marshal(vpcnetstate.ENIMap{brIf: newIf})
		if err != nil {
			glog.Infof("Node %s - error marshaling interface %v [%v]", node.Name, newIf, err)
			return err
		}
		c.updateNode(node.Name, func(n *v1.Node) {
			n.Annotations[vpcnetstate.IFSKey] = string(ifsJSON)
		})
		// and bail out, next watch can take over
		return nil
	}

	// If we have an unattached interface, attach it
	if !nc[brIf].Attached {
		glog.Infof("Node %s has an unattached ENI, attaching it", node.Name)
		err := c.attachENI(node, nc[brIf])
		if err != nil {
			return err
		}
		// Static for now, we can change this when we use more than one ENI
		nc[brIf].Attached = true
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

	return nil
}

// cleanupNode is called when the node no longer exists. Ensure the ENI and IPs
// are free
func (c *Controller) cleanupNode(node *v1.Node) error {
	// TODO  implement
	return nil
}

// updateNode will get/update the node until there is no conflict. The passed in
// function is used as the mutator
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
func (c *Controller) createENI(node *v1.Node) (*vpcnetstate.ENI, error) {
	inst, err := c.getInstance(node)
	if err != nil {
		return nil, err
	}

	subnetID := *inst.SubnetId

	subResp, err := c.ec2Client.DescribeSubnets(
		&ec2.DescribeSubnetsInput{SubnetIds: aws.StringSlice([]string{subnetID})},
	)
	if err != nil {
		glog.Errorf("Error fetching subnet info for %s [%v]", subnetID, err)
		return nil, err
	}
	if len(subResp.Subnets) != 1 {
		return nil, fmt.Errorf("Expected one subnet match for %q, got %v", subnetID, subResp.Subnets)
	}

	securityGroups := []string{}
	for _, sg := range inst.SecurityGroups {
		securityGroups = append(securityGroups, *sg.GroupId)
	}

	ceniResp, err := c.ec2Client.CreateNetworkInterface(
		&ec2.CreateNetworkInterfaceInput{
			Description:                    aws.String(fmt.Sprintf("K8S ENI for instance %s node %s", *inst.InstanceId, node.Name)),
			Groups:                         aws.StringSlice(securityGroups),
			SubnetId:                       aws.String(subnetID),
			SecondaryPrivateIpAddressCount: aws.Int64(int64(ipAddrCount - 1)), // -1 for the "host" IP
		},
	)
	if err != nil {
		glog.Errorf("Error creating network interface for node %s [%v]", node.Name, err)
		return nil, err
	}

	ips := []string{}
	for _, ip := range ceniResp.NetworkInterface.PrivateIpAddresses {
		ips = append(ips, *ip.PrivateIpAddress)
	}

	return &vpcnetstate.ENI{
		EniID:       *ceniResp.NetworkInterface.NetworkInterfaceId,
		Attached:    false,
		InterfaceIP: *ceniResp.NetworkInterface.PrivateIpAddress,
		CIDRBlock:   *subResp.Subnets[0].CidrBlock,
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
