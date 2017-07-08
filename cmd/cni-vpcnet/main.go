package main

import (
	"encoding/json"
	"flag"
	"net"
	"strconv"

	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/utils"
	"github.com/containernetworking/plugins/plugins/ipam/host-local/backend/disk"
	"github.com/golang/glog"
	"github.com/pkg/errors"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/lstoll/k8s-vpcnet/pkg/cni/config"
	"github.com/lstoll/k8s-vpcnet/pkg/vpcnetstate"
)

type podNet struct {
	// ContainerIP is the IP to allocate to the container
	ContainerIP net.IP
	// ENIIp is the address of the ENI interface on the host
	ENIIp net.IPNet
	// ENIInterface is the host interface for the ENI
	ENIInterface string
	// ENI Subnet is the CIDR net the ENI lives in
	ENISubnet *net.IPNet
	// ENI is the ENI the pod will be associated with.
	ENI *vpcnetstate.ENI
}

type cniRunner struct {
	vether vether
}

func main() {
	r := &cniRunner{
		vether: &vetherImpl{},
	}
	skel.PluginMain(r.cmdAdd, r.cmdDel, version.All)
}

func (c *cniRunner) cmdAdd(args *skel.CmdArgs) error {
	conf, em, err := loadConfig(args.StdinData)
	if err != nil {
		return err
	}

	initGlog(conf)
	defer glog.Flush()

	store, err := disk.New(conf.Name, conf.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()

	alloc := &ipAllocator{
		conf:   conf,
		store:  store,
		eniMap: em,
	}

	alloced, err := alloc.Get(args.ContainerID)
	if err != nil {
		return err
	}

	glog.V(2).Infof(
		"Allocated IP %q on eni %q for container ID %q namespace %q",
		alloced.ContainerIP,
		alloced.ENIInterface,
		args.ContainerID,
		args.Netns,
	)

	hostIf, containerIf, err := c.vether.SetupVeth(conf, args.Netns, args.IfName, alloced)
	if err != nil {
		return err
	}

	glog.V(2).Infof(
		"Created host interface %q container interface %q for container ID %q namespace %q",
		hostIf.Name,
		containerIf.Name,
		args.ContainerID,
		args.Netns,
	)

	result := &current.Result{
		Interfaces: []*current.Interface{hostIf, containerIf},
		IPs: []*current.IPConfig{
			{
				Address: net.IPNet{IP: alloced.ContainerIP, Mask: net.CIDRMask(32, 32)},
			},
		},
	}

	if conf.IPMasq {
		chain := utils.FormatChainName(conf.Name, args.ContainerID)
		comment := utils.FormatComment(conf.Name, args.ContainerID)
		err := ip.SetupIPMasq(
			&net.IPNet{IP: alloced.ContainerIP, Mask: alloced.ENISubnet.Mask},
			chain,
			comment,
		)
		if err != nil {
			return errors.Wrap(err, "Error inserting IPTables rule")
		}
	}

	err = types.PrintResult(result, version.Current())
	if err != nil {
		return errors.Wrap(err, "Error printing result")
	}

	return nil
}

func (c *cniRunner) cmdDel(args *skel.CmdArgs) error {
	conf, _, err := loadConfig(args.StdinData)
	if err != nil {
		return err
	}

	initGlog(conf)
	defer glog.Flush()

	store, err := disk.New(conf.Name, conf.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()

	alloc := &ipAllocator{
		conf:  conf,
		store: store,
	}

	err = alloc.Release(args.ContainerID)
	if err != nil {
		return errors.Wrap(err, "Error releasing IP")
	}

	if args.Netns != "" {
		c.vether.TeardownVeth(args.Netns, args.IfName)
	} else {
		glog.Warningf("Skipping delete of interface for container %q, netns is empty", args.ContainerID)
	}

	// TODO - tear down iptables rule too. Could lean on better persistence here too.

	return nil
}

func loadConfig(dat []byte) (*config.CNI, vpcnetstate.ENIs, error) {
	cfg := &config.CNI{}
	err := json.Unmarshal(dat, cfg)
	if err != nil {
		return nil, nil, errors.Wrap(err, "Error unmarshaling Net")
	}
	mp := cfg.ENIMapPath
	if mp == "" {
		mp = vpcnetstate.DefaultENIMapPath
	}
	em, err := vpcnetstate.ReadENIMap(mp)
	if err != nil {
		return nil, nil, errors.Wrap(err, "Error loading ENI configuration")
	}

	return cfg, em, nil
}

func initGlog(cfg *config.CNI) {
	flag.Set("logtostderr", "true")
	flag.Set("v", strconv.Itoa(cfg.LogVerbosity))
	flag.Parse()
}
