# k8s-vpcnet

[![Build Status](https://travis-ci.org/lstoll/k8s-vpcnet.svg?branch=master)](https://travis-ci.org/lstoll/k8s-vpcnet)

STATUS: _Prototype_. Still rough with many known and unknown issues

Kubernetes networking implementation for AWS that assigns Pods direct VPC routable IP addresses without using an overlay or hitting the 50/100 route limit.

This is done by utilizing ENI's with additional IPs bound. This is managed via a controller that creates the ENI's and IP bindings, a DaemonSet that configures interfaces and writes a CNI configuration, and a CNI plugin that configures the pod network and routing appropriately.

[Design Doc](https://docs.google.com/document/d/1A5uQ22KlRCy-SR7OtRqTTROPc2BGBYc-nosG2-Pd2fs)

# Deploying

The eni-controller should be pinned to the master node, or a node with the following IAM privileges

((TODO)) - exact IAM policy example

```
ec2:CreateNetworkInterface
ec2:ModifyNetworkInterfaceAttribute
ec2:AttachNetworkInterface
ec2:DescribeSubnets
```

Check out manifest-latest.yml, and customize as needed. Apply to cluster, enjoy, and please report all the bugs you will inevitably find.

# Components

## eni-controller

Only single instance of this should be run in the cluster currently. This will watch for node updates. When a node is seen that has a AWS Instance ID the following steps will be taken:

* Add "k8s-vpcnet/no-interface-configured" taint to the node to avoid pods until ready.
* Add the max number of ENI's to the node
* Attach the ENI's to the instance
* Allocate the max number of IPs per ENI's to the node
* Label the node with the instance ID

## vpcnet-daemon

This runs as a DaemonSet on all nodes. It installs the CNI plugin on the node. This watches for node changes by the instance ID label. When the node has ENI's and IPs allocated it will create the network interface in the OS, and set up the appropriate routes and iptables entries. It will then write out a config for the CNI plugin

## CNI Plugin

Using the configuration the configure process write out, when called it will allocate an IP for the pod and configure a veth pair and routing for the namespace.

# More

This is still in flux and a work in progress. The core concepts are comfortable, but the implementation needs more testing in large environments and handlers for all the inevetable edge cases.

The licensing of this is TBD right now, still trying to work it out appropriately. If this is an immediately blocker for you please let me know and I'll hurry it up.

I can be contacted at lincoln.stoll@gmail.com or @lstoll in the kubernetes slack.
