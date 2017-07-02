package main

import (
	"encoding/json"

	"github.com/containernetworking/plugins/plugins/ipam/host-local/backend/disk"
	"github.com/pkg/errors"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/lstoll/k8s-vpcnet/vpcnetstate"
)

// Net is the top level data passed in
type Net struct {
	Name       string      `json:"name"`
	CNIVersion string      `json:"cniVersion"`
	IPAM       *IPAMConfig `json:"ipam"`
}

// IPAMConfig is the config for this driver
type IPAMConfig struct {
	Name string `json:"name"`
	// Interface is the "bridge" interface we should look up in the map to get
	// IPs from
	Interface string `json:"interface"`
	// TODO - handle the more than one bridge case
}

func main() {
	skel.PluginMain(cmdAdd, cmdDel, version.All)
}

func cmdAdd(args *skel.CmdArgs) error {
	conf, err := loadConfig(args.StdinData)
	if err != nil {
		return err
	}

	em, err := vpcnetstate.ReadENIMap()
	if err != nil {
		return err
	}

	store, err := disk.New(conf.IPAM.Name, "")
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

	store, err := disk.New(conf.IPAM.Name, "")
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
