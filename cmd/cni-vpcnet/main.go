package main

import (
	"context"
	"encoding/json"
	"flag"
	"net"
	"strconv"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"google.golang.org/grpc"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/lstoll/k8s-vpcnet/pkg/cni/config"
	"github.com/lstoll/k8s-vpcnet/pkg/vpcnetpb"
)

// kubeArgs are the kubernetes-specific arguments passed to this CNI plugin
type kubeArgs struct {
	types.CommonArgs

	// IP is pod's ip address
	IP net.IP

	// K8S_POD_NAME is pod's name
	K8S_POD_NAME types.UnmarshallableString

	// K8S_POD_NAMESPACE is pod's namespace
	K8S_POD_NAMESPACE types.UnmarshallableString

	// K8S_POD_INFRA_CONTAINER_ID is pod's container id
	K8S_POD_INFRA_CONTAINER_ID types.UnmarshallableString
}

type cniRunner struct{}

func main() {
	r := &cniRunner{}
	skel.PluginMain(r.cmdAdd, r.cmdDel, version.All)
}

func (c *cniRunner) cmdAdd(args *skel.CmdArgs) error {
	cfg := &config.Net{}
	err := json.Unmarshal(args.StdinData, cfg)
	if err != nil {
		return errors.Wrap(err, "Error unmarshaling config")
	}

	ka := &kubeArgs{}

	if err := types.LoadArgs(args.Args, ka); err != nil {
		return errors.Wrap(err, "Error loading CNI arguments")
	}

	initGlog(cfg)
	defer glog.Flush()

	conn, err := ipamConn(cfg.IPAM.IPAMSocketPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	ic := vpcnetpb.NewIPAMClient(conn)

	allocReq := &vpcnetpb.AddRequest{
		ContainerID:  args.ContainerID,
		PodName:      string(ka.K8S_POD_NAME),
		PodNamespace: string(ka.K8S_POD_NAMESPACE),
	}

	allocResp, err := ic.Add(context.Background(), allocReq)
	if err != nil {
		glog.Errorf("Error allocating IP address for container %s [%+v]", args.ContainerID, err)
		return err
	}

	glog.V(2).Infof(
		"Allocated IP %q for container ID %q namespace %q",
		allocResp.AllocatedIP,
		args.ContainerID,
		args.Netns,
	)

	// TODO - does DNS matter in the Kubernetes case? Maybe.

	_, defNet, err := net.ParseCIDR("0.0.0.0/0")
	if err != nil {
		return errors.Wrap(err, "Unexpected error parsing 0.0.0.0/0 !!!")
	}

	_, subnet, err := net.ParseCIDR(allocResp.SubnetCIDR)
	if err != nil {
		return errors.Wrapf(err, "Error parsing returned subnet CIDR %s", allocResp.SubnetCIDR)
	}

	result := &current.Result{
		IPs: []*current.IPConfig{
			{
				Address: net.IPNet{
					IP:   net.ParseIP(allocResp.AllocatedIP),
					Mask: subnet.Mask,
				},
				Gateway: net.ParseIP(allocResp.ENIIP),
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
	cfg := &config.Net{}
	err := json.Unmarshal(args.StdinData, cfg)
	if err != nil {
		return errors.Wrap(err, "Error unmarshaling config")
	}

	initGlog(cfg)
	defer glog.Flush()

	conn, err := ipamConn(cfg.IPAM.IPAMSocketPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	ic := vpcnetpb.NewIPAMClient(conn)

	delReq := &vpcnetpb.DelRequest{
		ContainerID: args.ContainerID,
	}

	_, err = ic.Del(context.Background(), delReq)
	if err != nil {
		// Note error, but continue anyway - don't spin the container for this
		glog.Errorf("Error releasing IP address for container %s, ignoring [%+v]", args.ContainerID, err)
	}

	return nil
}

func ipamConn(sockPath string) (*grpc.ClientConn, error) {
	conn, err := grpc.Dial(sockPath,
		grpc.WithInsecure(),
		grpc.WithDialer(dialUnix),
	)
	if err != nil {
		return nil, errors.Wrapf(err, "Error connecting to IPAM socket at %s", sockPath)
	}

	return conn, nil
}

func dialUnix(addr string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", addr, timeout)
}

func initGlog(cfg *config.Net) {
	flag.Set("logtostderr", "true")
	flag.Set("v", strconv.Itoa(cfg.IPAM.LogVerbosity))
	flag.Parse()
}
