package main

import (
	"encoding/json"

	"github.com/containernetworking/plugins/plugins/ipam/host-local/backend/disk"
	"github.com/golang/glog"
	"github.com/pkg/errors"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/lstoll/k8s-vpcnet/vpcnetstate"
)

// Net is the top level data passed in
type Net struct {
	Name       string `json:"name"`
	CNIVersion string `json:"cniVersion"`
	// Type is the type of interface plugin in use
	Type string `json:"type"`
	// Bridge is inherited from the bridge plugin, it is the name of the bridge
	// interface. This should be used to determine the correct IP range
	Bridge string      `json:"bridge"`
	IPAM   *IPAMConfig `json:"ipam"`
}

// IPAMConfig is the config for this driver
type IPAMConfig struct {
	Name string `json:"name"`
	// ENIMapPath is the optional path to read the map from. Otherwise, use default
	ENIMapPath string `json:"eni_map_path"`
	//DataDir overrides the dir that the plugin will track state in
	DataDir string `json:"data_dir"`
}

func main() {
	skel.PluginMain(cmdAdd, cmdDel, version.All)
}

func cmdAdd(args *skel.CmdArgs) error {
	conf, err := loadConfig(args.StdinData)
	if err != nil {
		return err
	}

	if conf.Type != "bridge" || conf.Bridge == "" {
		glog.Fatal("Currently only compatible with bridge and/or no bridge interface specified")
	}

	mp := conf.IPAM.ENIMapPath
	if mp == "" {
		mp = vpcnetstate.DefaultENIMapPath
	}
	em, err := vpcnetstate.ReadENIMap(mp)
	if err != nil {
		return err
	}

	store, err := disk.New(conf.IPAM.Name, conf.IPAM.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()

	alloc := &IPAllocator{
		conf:   conf,
		store:  store,
		eniMap: em,
	}

	result, err := alloc.Get(args.ContainerID)
	if err != nil {
		return err
	}

	return types.PrintResult(result, version.Current())
}

func cmdDel(args *skel.CmdArgs) error {
	conf, err := loadConfig(args.StdinData)
	if err != nil {
		return err
	}

	store, err := disk.New(conf.IPAM.Name, conf.IPAM.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()

	alloc := &IPAllocator{
		conf:  conf,
		store: store,
	}

	return alloc.Release(args.ContainerID)
}

func loadConfig(dat []byte) (*Net, error) {
	n := &Net{}
	err := json.Unmarshal(dat, n)
	if err != nil {
		return nil, errors.Wrap(err, "Error unmarshaling Net")
	}
	return n, nil
}
