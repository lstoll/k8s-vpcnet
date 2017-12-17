package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path"
	"strings"

	"github.com/lstoll/k8s-vpcnet/pkg/vpcnetpb"
	"google.golang.org/grpc"

	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/golang/glog"
	"github.com/lstoll/k8s-vpcnet/pkg/allocator"
	"github.com/lstoll/k8s-vpcnet/pkg/config"
	"github.com/lstoll/k8s-vpcnet/pkg/ifmgr"
	"github.com/pkg/errors"
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

// taintNoIPs is applied to the node when there are no free IPs for pods.
// pods with net: host can tolerate this to get scheduled anyway
const taintNoIPs = "k8s-vpcnet/no-free-ips"

var (
	ipamSocketPath string
)

func main() {
	flag.Set("logtostderr", "true")
	flag.StringVar(&ipamSocketPath, "ipam-socket-path", "/var/lib/cni/vpcnet/ipam.sock", "Path for IPAM gRPC Service Socket")
	flag.Parse()

	cfg, err := config.Load(config.DefaultConfigPath)
	if err != nil {
		log.Fatalf("Error loading configuration [%+v]", err)
	}

	glog.Info("Running node configurator")

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

	glog.Info("Setting up Allocator")
	alloc, err := allocator.New("")
	if err != nil {
		glog.Fatalf("Error setting up allocator [%+v]", err)
	}

	glog.Info("Starting IPAM Server")

	ipmsvc := &IPAMService{
		Allocator: alloc,
	}

	_, err = os.Stat(ipamSocketPath)
	if err == nil {
		err = os.Remove(ipamSocketPath)
		if err != nil {
			glog.Warningf("Error removing ipam socket [%+v]", err)
		}
	}
	if err := os.MkdirAll(path.Dir(ipamSocketPath), 0700); err != nil {
		glog.Fatalf("Error creating IPAM socket dir [%+v]", err)
	}
	ulis, err := net.Listen("unix", ipamSocketPath)
	if err != nil {
		glog.Fatalf("Error listening on IPAM socket [%+v]", err)
	}
	if err := os.Chmod(ipamSocketPath, 0600); err != nil {
		glog.Fatalf("Error setting IPAM socket permissions [%+v]", err)
	}
	us := grpc.NewServer()
	vpcnetpb.RegisterIPAMServer(us, ipmsvc)

	go func() {
		if err := us.Serve(ulis); err != nil {
			if !strings.Contains(err.Error(), "use of closed network connection") {
				glog.Fatalf("IPAM Server Serve error [%+v]", err)
			}
		}
	}()

	// Setting up Kubernetes API configuration
	config, err := rest.InClusterConfig()
	if err != nil {
		glog.Fatalf("Error getting client config [%v]", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatalf("Error getting client [%v]", err)
	}

	// TODO - how do we just watch a limted node? Do we need the controller to
	// add a label for the instance id maybe to them maybe, and filter on that?
	// watchlist := cache.NewListWatchFromClient(
	// 	clientset.Core().RESTClient(),
	// 	"nodes", v1.NamespaceAll,
	// 	fields.OneTermEqualSelector("aws-instance-id", iid),
	// )

	listFunc := func(options meta_v1.ListOptions) (runtime.Object, error) {
		options.LabelSelector = fmt.Sprintf("aws-instance-id=%s", iid)

		return clientset.Core().RESTClient().Get().
			Namespace(v1.NamespaceAll).
			Resource("nodes").
			VersionedParams(&options, meta_v1.ParameterCodec).
			Do().
			Get()
	}

	watchFunc := func(options meta_v1.ListOptions) (watch.Interface, error) {
		options.Watch = true
		options.LabelSelector = fmt.Sprintf("aws-instance-id=%s", iid)
		return clientset.Core().RESTClient().Get().
			Namespace(v1.NamespaceAll).
			Resource("nodes").
			VersionedParams(&options, meta_v1.ParameterCodec).
			Watch()
	}
	lw := &cache.ListWatch{ListFunc: listFunc, WatchFunc: watchFunc}

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	indexer, informer := cache.NewIndexerInformer(lw, &v1.Node{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// IndexerInformer uses a delta queue, therefore for deletes we have to use this
			// key function.
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
	}, cache.Indexers{})

	ipt := uiptables.New(uexec.New(), udbus.New(), uiptables.ProtocolIpv4)

	// Run up the controller
	c := &controller{
		indexer:      indexer,
		queue:        queue,
		informer:     informer,
		instanceID:   iid,
		vpcnetConfig: cfg,
		hostIP:       hostIP,
		clientSet:    clientset,
		IFMgr:        ifmgr.New(cfg.Network, ipt),
		Allocator:    alloc,
	}

	stop := make(chan struct{})
	defer close(stop)
	go c.Run(1, stop)

	// Wait forever
	select {}
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