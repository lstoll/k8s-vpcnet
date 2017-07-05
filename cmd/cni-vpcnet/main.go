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
	"github.com/lstoll/k8s-vpcnet/vpcnetstate"
)

var defaultVether vether = &vetherImpl{}

// Net is the top level data passed in
type Net struct {
	Name       string `json:"name"`
	CNIVersion string `json:"cniVersion"`
	// Type is the type of interface plugin in use
	Type string `json:"type"`

	// ENIMapPath is the optional path to read the map from. Otherwise, use default
	ENIMapPath string `json:"eni_map_path"`
	//DataDir overrides the dir that the plugin will track state in
	DataDir string `json:"data_dir"`

	// IPMasq will write outbound masquerate iptables rules
	IPMasq bool `json:"ip_masq"`

	// LogLevel is the glog v flag to set
	LogVerbosity int `json:"log_verbosity"`
}

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

func main() {
	skel.PluginMain(cmdAdd, cmdDel, version.All)
}

func cmdAdd(args *skel.CmdArgs) error {
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

	hostIf, containerIf, err := defaultVether.SetupVeth(conf, args.Netns, args.IfName, alloced)
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

func cmdDel(args *skel.CmdArgs) error {
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
		defaultVether.TeardownVeth(args.Netns, args.IfName)
	} else {
		glog.Warningf("Skipping delete of interface for container %q, netns is empty", args.ContainerID)
	}

	// TODO - tear down iptables rule too. Could lean on better persistence here too.

	return nil
}

func loadConfig(dat []byte) (*Net, vpcnetstate.ENIs, error) {
	n := &Net{}
	err := json.Unmarshal(dat, n)
	if err != nil {
		return nil, nil, errors.Wrap(err, "Error unmarshaling Net")
	}
	mp := n.ENIMapPath
	if mp == "" {
		mp = vpcnetstate.DefaultENIMapPath
	}
	em, err := vpcnetstate.ReadENIMap(mp)
	if err != nil {
		return nil, nil, errors.Wrap(err, "Error loading ENI configuration")
	}

	return n, em, nil
}

func initGlog(cfg *Net) {
	flag.Set("logtostderr", "true")
	flag.Set("v", strconv.Itoa(cfg.LogVerbosity))
	flag.Parse()
}
