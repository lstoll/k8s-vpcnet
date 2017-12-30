package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path"

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
	"k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
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
		glog.Fatal("node-name is a required argument")
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
		glog.Fatalf("Error determining current instance ID [%v]", err)
	}
	hostIPStr, err := md.GetMetadata("local-ipv4")
	if err != nil {
		glog.Fatalf("Error determining host IP address [%+v]", err)
	}
	hostIP := net.ParseIP(hostIPStr)

	if cfg.Network.HostPrimaryInterface == "" {
		glog.V(2).Info("Finding host primary interface")
		cfg.Network.HostPrimaryInterface, err = primaryInterface(md)
		if err != nil {
			glog.Fatalf("Error finding host's primary interface [%+v]", err)
		}
		glog.V(2).Infof("Primary interface is %q", cfg.Network.HostPrimaryInterface)
	}

	glog.Info("Initializing Allocator")

	alloc, err := allocator.New("")
	if err != nil {
		glog.Fatalf("Error setting up allocator [%+v]", err)
	}

	glog.Info("Initializing Kubernetes API clients")

	config, err := rest.InClusterConfig()
	if err != nil {
		glog.Fatalf("Error getting client config [%v]", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatalf("Error getting client [%v]", err)
	}

	// API access for this node
	nodeListFunc := func(options meta_v1.ListOptions) (runtime.Object, error) {
		options.LabelSelector = fmt.Sprintf("aws-instance-id=%s", iid)

		return clientset.Core().RESTClient().Get().
			Namespace(v1.NamespaceAll).
			Resource("nodes").
			VersionedParams(&options, meta_v1.ParameterCodec).
			Do().
			Get()
	}

	nodeWatchFunc := func(options meta_v1.ListOptions) (watch.Interface, error) {
		options.Watch = true
		options.LabelSelector = fmt.Sprintf("aws-instance-id=%s", iid)
		return clientset.Core().RESTClient().Get().
			Namespace(v1.NamespaceAll).
			Resource("nodes").
			VersionedParams(&options, meta_v1.ParameterCodec).
			Watch()
	}
	nodeListWatch := &cache.ListWatch{ListFunc: nodeListFunc, WatchFunc: nodeWatchFunc}

	nodeQueue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	nodeIndexer, nodeInformer := cache.NewIndexerInformer(nodeListWatch, &v1.Node{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				nodeQueue.Add(key)
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				nodeQueue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// IndexerInformer uses a delta queue, therefore for deletes we have to use this
			// key function.
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				nodeQueue.Add(key)
			}
		},
	}, cache.Indexers{})

	glog.Infof("Initializing reconciler")
	rec := &reconciler.Reconciler{
		Config:      cfg,
		Clientset:   clientset,
		NodeIndexer: nodeIndexer,
		Allocator:   alloc,
		NodeName:    nodeName,
	}

	glog.Info("Initializing IPTables manager")
	ipt := uiptables.New(uexec.New(), udbus.New(), uiptables.ProtocolIpv4)

	glog.Info("Starting run group")
	var g run.Group

	iprun, ipint := ipamService(cfg, alloc, rec)
	g.Add(iprun, ipint)

	ifrun, ifint := ifc(clientset, nodeIndexer, nodeInformer, nodeQueue, ipt, cfg, alloc, iid, hostIP, 1)
	g.Add(ifrun, ifint)

	recrun, recint := recGrp(rec)
	g.Add(recrun, recint)

	glog.Errorf("Run group terminated by [%+v]", g.Run())
}

func recGrp(rec *reconciler.Reconciler) (func() error, func(error)) {
	run := func() error {
		glog.Info("Running IP pool/pod reconciler")
		return rec.Run()
	}

	interrupt := func(error) {
		rec.Stop()
	}

	return run, interrupt
}

func ipamService(cfg *config.Config, alloc *allocator.Allocator, reconciler *reconciler.Reconciler) (func() error, func(error)) {
	ipmsvc := &ipamsvc.Service{
		Config:    cfg,
		Allocator: alloc,
		Evictor:   reconciler,
	}

	_, err := os.Stat(ipamSocketPath)
	if err == nil {
		err = os.Remove(ipamSocketPath)
		if err != nil {
			glog.Warningf("Error removing ipam socket [%+v]", err)
		}
	}
	if err := os.MkdirAll(path.Dir(ipamSocketPath), 0700); err != nil {
		glog.Fatalf("Error creating IPAM socket dir [%+v]", err)
	}
	lis, err := net.Listen("unix", ipamSocketPath)
	if err != nil {
		glog.Fatalf("Error listening on IPAM socket [%+v]", err)
	}
	if err := os.Chmod(ipamSocketPath, 0600); err != nil {
		glog.Fatalf("Error setting IPAM socket permissions [%+v]", err)
	}
	srv := grpc.NewServer()
	vpcnetpb.RegisterIPAMServer(srv, ipmsvc)

	run := func() error {
		glog.Info("Serving IPAM Server")
		return srv.Serve(lis)
	}

	interrupt := func(error) {
		srv.GracefulStop()
		lis.Close()
	}

	return run, interrupt
}

func ifc(clientset kubernetes.Interface,
	indexer cache.Indexer,
	informer cache.Controller,
	queue workqueue.RateLimitingInterface,
	ipt uiptables.Interface,
	cfg *config.Config,
	alloc *allocator.Allocator,
	iid string,
	hostIP net.IP,
	threadiness int) (func() error, func(error)) {

	c := &ifcontroller.Controller{
		Indexer:      indexer,
		Queue:        queue,
		Informer:     informer,
		InstanceID:   iid,
		VPCNetConfig: cfg,
		HostIP:       hostIP,
		ClientSet:    clientset,
		IFMgr:        ifmgr.New(cfg.Network, ipt),
		Allocator:    alloc,
		CNIInstaller: &cniinstall.Installer{
			IPAMSocketPath: ipamSocketPath,
			Config:         cfg,
		},
	}

	stop := make(chan struct{})

	run := func() error {
		glog.Info("Running node interface controller")
		c.Run(threadiness, stop)
		return nil
	}

	interrupt := func(error) {
		close(stop)
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
