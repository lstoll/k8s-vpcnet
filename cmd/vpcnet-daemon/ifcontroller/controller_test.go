package ifcontroller

import (
	"encoding/json"
	"net"
	"testing"

	"github.com/lstoll/k8s-vpcnet/pkg/config"
	"github.com/lstoll/k8s-vpcnet/pkg/nodestate"
	"k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/informers"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/kubernetes/staging/src/k8s.io/client-go/kubernetes/fake"
)

type fakeIFMgr struct {
	configuredInterfaces []string
}

var _ IFMgr = &fakeIFMgr{}

func (i *fakeIFMgr) ConfigureInterface(ifname string, mac string, ip *net.IPNet, subnet *net.IPNet) error {
	i.configuredInterfaces = append(i.configuredInterfaces, ifname)
	return nil
}

func (i *fakeIFMgr) ConfigureRoutes(ifName string, awsEniAttachIndex int, eniSubnet *net.IPNet, podIPs []net.IP) error {
	return nil
}

func (i *fakeIFMgr) ConfigureIPMasq(hostIP net.IP, podIPs []net.IP) error {
	return nil
}

func (i *fakeIFMgr) InterfaceExists(name string) (bool, error) {
	return false, nil
}

type fakeAllocator struct {
}

func (f *fakeAllocator) SetENIs(enis nodestate.ENIs) error {
	return nil
}

var _ Allocator = &fakeAllocator{}

type fakeInstaller struct {
}

func (f *fakeInstaller) Configured() (bool, error) {
	return false, nil
}

func (f *fakeInstaller) Install() error {
	return nil
}

var _ CNIInstaller = &fakeInstaller{}

func TestIFController(t *testing.T) {

	alloc := &fakeAllocator{}
	ifmgr := &fakeIFMgr{}
	installer := &fakeInstaller{}

	node := &v1.Node{
		ObjectMeta: meta_v1.ObjectMeta{
			Name: "test-node",
		},
		Spec: v1.NodeSpec{
			ProviderID: "aws:///i-1234",
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
		informerFactory,
		"i-1234",
		net.ParseIP("10.0.0.5"),
		ifmgr,
		alloc,
		installer,
	)

	t.Log("Handle an empty node")

	if err := c.handleNode("test-node"); err != nil {
		t.Fatalf("Unexpected error [%+v]", err)
	}

	t.Log("Add a provider ID to the node but no ENIs, should basically no-op")

	node.Spec.ProviderID = "aws:///i-1234"
	go func() {
		fakeWatch.Add(node)
	}()

	if err := c.handleNode("test-node"); err != nil {
		t.Fatalf("Unexpected error [%+v]", err)
	}

	t.Log("Add an ENI to the node, loop a few times and it should be configured")

	_, ipn, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	nc := nodestate.ENIs{
		{
			EniID:       "eni-1234",
			Attached:    true,
			InterfaceIP: net.ParseIP("10.0.0.50"),
			Index:       1,
			CIDRBlock:   (*config.IPNet)(ipn),
			IPs:         []net.IP{},
			MACAddress:  "aa:bb:cc:dd:ee",
		},
	}
	ifsJSON, err := json.Marshal(&nc)
	if err != nil {
		t.Fatal(err)
	}
	node.Annotations = map[string]string{nodestate.IFSKey: string(ifsJSON)}
	fakeWatch.Add(node)

	if err := c.handleNode("test-node"); err != nil {
		t.Fatalf("Unexpected error [%+v]", err)
	}

	if len(ifmgr.configuredInterfaces) != 1 {
		t.Errorf("Expected 1 configured interface, got %d", len(ifmgr.configuredInterfaces))
	}
}
