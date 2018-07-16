# k8s-vpcnet

[![Build Status](https://travis-ci.org/lstoll/k8s-vpcnet.svg?branch=master)](https://travis-ci.org/lstoll/k8s-vpcnet)

## STATUS: **Deprecated.** Use https://github.com/aws/amazon-vpc-cni-k8s/

Kubernetes networking implementation for AWS that assigns Pods direct VPC routable IP addresses without using an overlay or hitting the 50/100 route limit.

This is done by utilizing ENI's with additional IPs bound. This is managed via a central controller that creates the ENI's and IP bindings, a DaemonSet that configures interfaces and writes a CNI configuration, and a CNI plugin that acts as an IPAM. For the actual container interfaces, the normal [CNI ptp plugin](https://github.com/containernetworking/plugins/blob/master/plugins/main/ptp/README.md) is used.

This is not the only way to to manage this on AWS. Alternatives are:
* Amazon's [amazon-vpc-cni-k8s](https://github.com/aws/amazon-vpc-cni-k8s)
* Lyft's [cni-ipvlan-vpc-k8s](https://github.com/lyft/cni-ipvlan-vpc-k8s)

If you're just looking for something general purpose, I'd recommend checking out AWS's. It'll probably be the easiest to get support for, and will plug in with their managed Kubernetes service. If you're looking for the best networking performance, Lyft's is what you want.

Development on this project is ongoing, and will become more opinionated.

### Current design items of note:

#### Centralized network configuration.

Unlike the other projects, ENIs, addresses and their attachment are managed via a central daemon as opposed to running per-node. This makes troubleshooting issues in this area simpler, and removes the need for every node to have IAM permissions. We also configured the maximum number of ENIs and addresses as soon as the node comes online, and before it starts accepting pods. This removes moving parts in normal node operation, simplifying the path of pod creation and minimizing external failures affecting this.

#### Up-front configuration per-pod

The daemon creates the host interfaces and configures all possible iptables rules and routes up front. This again streamlines the process of bringing a pod online. The existence of this daemon and it's informed involvement in the pod address allocation process provides a good place to implement NetworkPolicy and IAM management without suffering from configuration races. We can also put pods in different security groups/VPC subnets based on annotations.

#### Address pool management

A much smaller address pool than Kubernetes normally expects is available to AWS instances. To work around this, the system can manage a taint (`k8s-vpcnet/no-free-ips:NoSchedule`) on nodes to prevent pods being scheduled on nodes with an exhausted pool. Pods that fail to create can also optionally be automatically deleted. This is preferred over `--max-pods`, as it can take host network pods in to account


[Early Design Doc](https://docs.google.com/document/d/1A5uQ22KlRCy-SR7OtRqTTROPc2BGBYc-nosG2-Pd2fs)

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

This runs as a DaemonSet on all nodes. It installs the CNI plugins on the node. This watches for node changes by the instance ID label. When the node has ENI's and IPs allocated it will create the network interface in the OS, and set up the appropriate routes and iptables entries. It will then write out a config for the CNI plugin. It exports a gRPC service over a unix socket that the CNI IPAM plugin uses to request addresses.

## CNI Plugin

IPAM-only plugin that communicates with the vpcnet-daemon to assign an IP address

# Contact

I can be contacted at lincoln.stoll@gmail.com or @lstoll in the kubernetes slack.
