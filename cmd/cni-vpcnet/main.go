package main

import (
	"encoding/json"
	"flag"
	"net"
	"strconv"

	"github.com/golang/glog"
	"github.com/pkg/errors"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/lstoll/k8s-vpcnet/pkg/cni/config"
	"github.com/lstoll/k8s-vpcnet/pkg/cni/diskstore"
	"github.com/lstoll/k8s-vpcnet/pkg/vpcnetstate"
)

type cniRunner struct{}

func main() {
	r := &cniRunner{}
	skel.PluginMain(r.cmdAdd, r.cmdDel, version.All)
}

func (c *cniRunner) cmdAdd(args *skel.CmdArgs) error {
	conf, em, err := loadConfig(args.StdinData)
	if err != nil {
		return err
	}

	initGlog(conf)
	defer glog.Flush()

	store, err := diskstore.New(conf.Name, conf.IPAM.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()

	alloc := &ipAllocator{
		name:   conf.Name,
		conf:   conf.IPAM,
		store:  store,
		eniMap: em,
	}

	alloced, eni, err := alloc.Get(args.ContainerID)
	if err != nil {
		glog.Errorf("Error allocating IP address for container %s [%+v]", args.ContainerID, err)
		return err
	}

	glog.V(2).Infof(
		"Allocated IP %q for container ID %q namespace %q",
		alloced.String(),
		args.ContainerID,
		args.Netns,
	)

	// TODO - does DNS matter in the Kubernetes case? Maybe.

	_, defNet, err := net.ParseCIDR("0.0.0.0/0")
	if err != nil {
		return errors.Wrap(err, "Unexpected error parsing 0.0.0.0/0 !!!")
	}

	result := &current.Result{
		IPs: []*current.IPConfig{
			{
				Address: net.IPNet{IP: alloced, Mask: eni.Mask},
				Gateway: eni.IP,
			},
		},
		Routes: []*types.Route{
			// Always return a route for 0.0.0.0/0, the configurator will handle
			// where this actually ends up on the host side.
			{
				Dst: *defNet,
			},
		},
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

	store, err := diskstore.New(conf.Name, conf.IPAM.DataDir)
	if err != nil {
		glog.Errorf("Error opening disk store [%+v]", err)
		return err
	}
	defer store.Close()

	alloc := &ipAllocator{
		name:  conf.Name,
		conf:  conf.IPAM,
		store: store,
	}

	_, err = alloc.Release(args.ContainerID)
	if err != nil {
		// Note error, but continue anyway
		glog.Errorf("Error releasing IP address for container %s, ignoring [%+v]", args.ContainerID, err)
	}

	return nil
}

func loadConfig(dat []byte) (*config.Net, vpcnetstate.ENIs, error) {
	cfg := &config.Net{}
	err := json.Unmarshal(dat, cfg)
	if err != nil {
		return nil, nil, errors.Wrap(err, "Error unmarshaling Net")
	}
	mp := cfg.IPAM.ENIMapPath
	if mp == "" {
		mp = vpcnetstate.DefaultENIMapPath
	}
	em, err := vpcnetstate.ReadENIMap(mp)
	if err != nil {
		return nil, nil, errors.Wrap(err, "Error loading ENI configuration")
	}

	return cfg, em, nil
}

func initGlog(cfg *config.Net) {
	flag.Set("logtostderr", "true")
	flag.Set("v", strconv.Itoa(cfg.IPAM.LogVerbosity))
	flag.Parse()
}
