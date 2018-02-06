package enicontroller

import (
	"testing"

	"k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/informers"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/kubernetes/staging/src/k8s.io/client-go/kubernetes/fake"
)

type fakeEC2 struct {
	EC2Client
}

var _ EC2Client = &fakeEC2{}

func TestENIController(t *testing.T) {

	ec2 := &fakeEC2{}

	node := &v1.Node{
		ObjectMeta: meta_v1.ObjectMeta{
			Name: "test-node",
		},
	}

	fakeWatch := watch.NewFake()
	clientset := fake.NewSimpleClientset(node)
	clientset.PrependWatchReactor("nodes", kubetesting.DefaultWatchReactor(fakeWatch, nil))
	informerFactory := informers.NewSharedInformerFactory(clientset, 0)

	stopCh := make(chan struct{})
	go informerFactory.Start(stopCh)
	defer func() { stopCh <- struct{}{} }()

	c := New(
		clientset,
		informerFactory,
		ec2,
		-1,
		-1,
	)

	t.Log("Handle an empty node")

	if err := c.handleNode("test-node"); err != nil {
		t.Fatalf("Unexpected error [%+v]", err)
	}
}
