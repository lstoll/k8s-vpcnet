package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path"
	"syscall"
	"time"

	"github.com/lstoll/k8s-vpcnet"

	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/golang/glog"
	"github.com/lstoll/k8s-vpcnet/cmd/vpcnet-daemon/cniinstall"
	"github.com/lstoll/k8s-vpcnet/cmd/vpcnet-daemon/ifcontroller"
	"github.com/lstoll/k8s-vpcnet/cmd/vpcnet-daemon/ipamsvc"
	"github.com/lstoll/k8s-vpcnet/cmd/vpcnet-daemon/reconciler"
	"github.com/lstoll/k8s-vpcnet/pkg/allocator"
	"github.com/lstoll/k8s-vpcnet/pkg/config"
	"github.com/lstoll/k8s-vpcnet/pkg/ifmgr"
	"github.com/lstoll/k8s-vpcnet/pkg/vpcnetpb"
	"github.com/oklog/run"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	runtimeutil "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	udbus "k8s.io/kubernetes/pkg/util/dbus"
	uiptables "k8s.io/kubernetes/pkg/util/iptables"
	uexec "k8s.io/utils/exec"
)

var (
	ipamSocketPath string
	configPath     string
	nodeName       string
)

func main() {
	flag.Set("logtostderr", "true")
	flag.StringVar(&ipamSocketPath, "ipam-socket-path", "/var/lib/cni/vpcnet/ipam.sock", "Path for IPAM gRPC Service Socket")
	flag.StringVar(&configPath, "config-path", config.DefaultConfigPath, "Path to load the configuration file from")
	flag.StringVar(&nodeName, "node-name", "", "(Kubernetes) name for this node (spec.nodeName)")
	flag.Parse()

	if nodeName == "" {
		glog.Fatalf("node-name is a required argument")
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		glog.Fatalf("Error loading configuration [%+v]", err)
	}

	glog.Info("Starting vpcnet daemon")

	glog.Info("Determining host information from AWS metadata APIs")

	sess := session.Must(session.NewSession())
	md := ec2metadata.New(sess)
	iid, err := md.GetMetadata("instance-id")
	if err != nil {
		runtimeutil.HandleError(err)
		glog.Fatalf("Error determining current instance ID [%+v]", err)
	}
	hostIPStr, err := md.GetMetadata("local-ipv4")
	if err != nil {
		runtimeutil.HandleError(err)
		glog.Fatalf("Error determining host IP address [%+v]", err)
	}
	hostIP := net.ParseIP(hostIPStr)

	if cfg.Network.HostPrimaryInterface == "" {
		glog.Info("Finding host primary interface")
		cfg.Network.HostPrimaryInterface, err = primaryInterface(md)
		if err != nil {
			runtimeutil.HandleError(err)
			glog.Fatalf("Error finding host's primary interface [%+v]", err)
		}
		glog.Infof("Primary interface is %q", cfg.Network.HostPrimaryInterface)
	}

	glog.Info("Initializing Allocator")

	alloc, err := allocator.New("")
	if err != nil {
		runtimeutil.HandleError(err)
		glog.Fatalf("Error setting up allocator [%+v]", err)
	}

	glog.Info("Initializing Kubernetes API clients")

	config, err := rest.InClusterConfig()
	if err != nil {
		runtimeutil.HandleError(err)
		glog.Fatal("Error getting client config [%+v]", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		runtimeutil.HandleError(err)
		glog.Fatal("Error getting client [%+v]", err)
	}

	glog.Info("Initializing IPTables manager")
	ipt := uiptables.New(uexec.New(), udbus.New(), uiptables.ProtocolIpv4)

	glog.Info("Creating run group")
	var g run.Group

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	g.Add(
		func() error {
			glog.Info("Starting signal handler")
			sig := <-sigCh
			glog.Warningf("Received Signal %+v", sig)
			return nil
		},
		func(err error) {
			glog.Infof("Signal handler shutting down by %+v", err)
			close(sigCh)
		},
	)

	// This filters us down to just this node, which is all we care about here
	nodeLabels := labels.Set{k8svpcnet.LabelAWSInstanceID: iid}
	nodeFilterOpts := func(o *metav1.ListOptions) {
		o.LabelSelector = nodeLabels.AsSelector().String()
	}

	nodeInformerFactory := informers.NewFilteredSharedInformerFactory(clientset, time.Second*30, metav1.NamespaceAll, nodeFilterOpts)
	nodeInformerStopCh := make(chan struct{})
	nodeInformerGroupStopCh := make(chan struct{})

	glog.Info("Initializing reconciler")
	rec := reconciler.New(
		clientset,
		nodeInformerFactory,
		cfg,
		alloc,
		nodeName,
	)

	iprun, ipint := ipamService(cfg, alloc, rec)
	g.Add(iprun, ipint)

	ic := ifcontroller.New(
		nodeInformerFactory,
		iid,
		hostIP,
		ifmgr.New(cfg.Network, ipt),
		alloc,
		&cniinstall.Installer{
			IPAMSocketPath: ipamSocketPath,
			Config:         cfg,
		},
	)

	g.Add(ic.Run, ic.Stop)

	g.Add(rec.Run, rec.Stop)

	g.Add(
		func() error {
			glog.Info("Starting Informer Factory")
			// This needs to be started after we've set up the Listers
			nodeInformerFactory.Start(nodeInformerStopCh)
			<-nodeInformerGroupStopCh
			return nil
		},
		func(err error) {
			glog.Infof("Informer Factory shut down by %+v", err)
			nodeInformerStopCh <- struct{}{}
			nodeInformerGroupStopCh <- struct{}{}
		},
	)

	glog.Errorf("Run group terminated with [%+v]", g.Run())
}

func ipamService(cfg *config.Config, alloc *allocator.Allocator, reconciler *reconciler.Reconciler) (func() error, func(error)) {
	ipmsvc := ipamsvc.New(
		cfg,
		alloc,
		reconciler,
	)

	_, err := os.Stat(ipamSocketPath)
	if err == nil {
		err = os.Remove(ipamSocketPath)
		if err != nil {
			glog.Warning("Error removing ipam socket")
		}
	}
	if err := os.MkdirAll(path.Dir(ipamSocketPath), 0700); err != nil {
		runtimeutil.HandleError(err)
		glog.Fatalf("Error creating IPAM socket dir [%+v]", err)
	}
	lis, err := net.Listen("unix", ipamSocketPath)
	if err != nil {
		runtimeutil.HandleError(err)
		glog.Fatalf("Error listening on IPAM socket [%+v]", err)
	}
	if err := os.Chmod(ipamSocketPath, 0600); err != nil {
		runtimeutil.HandleError(err)
		glog.Fatalf("Error setting IPAM socket permissions [%+v]", err)
	}
	srv := grpc.NewServer()
	vpcnetpb.RegisterIPAMServer(srv, ipmsvc)

	run := func() error {
		glog.Info("Serving IPAM Server")
		return srv.Serve(lis)
	}

	interrupt := func(err error) {
		glog.Infof("IPAM Server shut down by %+v", err)
		srv.GracefulStop()
		lis.Close()
	}

	return run, interrupt
}

func primaryInterface(md *ec2metadata.EC2Metadata) (string, error) {
	mac, err := md.GetMetadata("mac")
	if err != nil {
		return "", errors.Wrap(err, "Error finding interface MAC")
	}

	ifs, err := net.Interfaces()
	if err != nil {
		return "", errors.Wrap(err, "Error listing machine interfaces")
	}

	var ifName string

	for _, i := range ifs {
		if i.HardwareAddr.String() == mac {
			ifName = i.Name
		}
	}

	if ifName == "" {
		return "", fmt.Errorf("Could not find interface name for MAC %q", mac)
	}

	return ifName, nil
}
