package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/golang/glog"
	"github.com/lstoll/k8s-vpcnet/cmd/eni-controller/enicontroller"
	"github.com/oklog/run"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	flag.Set("logtostderr", "true")

	flag.Parse()

	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		glog.Fatalf("Error getting client config [%v]", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatalf("Error getting client [%v]", err)
	}

	sess := session.Must(session.NewSession())
	md := ec2metadata.New(sess)
	currRegion, err := md.Region()
	if err != nil {
		glog.Fatalf("Error determining region [%v]", err)
	}
	sess = session.Must(session.NewSession(
		&aws.Config{
			Region: aws.String(currRegion),
		},
	))
	ec2 := ec2.New(sess)

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

	informerFactory := informers.NewSharedInformerFactory(clientset, time.Second*30)
	informerStopCh := make(chan struct{})
	informerGroupStopCh := make(chan struct{})

	g.Add(
		func() error {
			glog.Info("Starting Informer Factory")
			informerFactory.Start(informerStopCh)
			<-informerGroupStopCh
			return nil
		},
		func(err error) {
			glog.Infof("Informer Factory shut down by %+v", err)
			informerStopCh <- struct{}{}
			informerGroupStopCh <- struct{}{}
		},
	)

	controller := enicontroller.New(
		clientset,
		informerFactory,
		ec2,
	)

	g.Add(controller.Run, controller.Stop)

	glog.Errorf("Run group terminated with [%+v]", g.Run())
}
