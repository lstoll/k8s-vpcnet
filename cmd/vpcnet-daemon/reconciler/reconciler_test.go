package reconciler

import (
	"testing"
	"time"

	"github.com/lstoll/k8s-vpcnet/pkg/objutil"

	"github.com/lstoll/k8s-vpcnet/pkg/config"
	"k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/staging/src/k8s.io/client-go/kubernetes/fake"
)

type fakeAllocator struct {
	Free int
}

func (f *fakeAllocator) FreeAddressCount() int {
	return f.Free
}

var _ Allocator = &fakeAllocator{}

func TestReconcilerTainting(t *testing.T) {
	for _, tc := range []struct {
		Name string

		// Number of free IPS
		FreeCount int
		// If we start with a tainted node
		NodeTainted bool
		// If we should expect a call to taint the node
		ExpectTaint bool
	}{
		{
			Name:        "Plenty of free IPs",
			FreeCount:   10,
			NodeTainted: false,
			ExpectTaint: false,
		},
		{
			Name:        "Free IPs, starting tainted",
			FreeCount:   10,
			NodeTainted: true,
			ExpectTaint: false,
		},
		{
			Name:        "No free IPs, starting untainted",
			FreeCount:   0,
			NodeTainted: false,
			ExpectTaint: true,
		},
		{
			Name:        "No free IPs, starting tainted",
			FreeCount:   0,
			NodeTainted: true,
			ExpectTaint: true,
		},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			t.Logf("Starting %s", tc.Name)

			alloc := &fakeAllocator{Free: tc.FreeCount}

			testNode := &v1.Node{
				ObjectMeta: meta_v1.ObjectMeta{
					Name: "test-node",
				},
			}
			if tc.NodeTainted {
				testNode.Spec.Taints = append(testNode.Spec.Taints,
					v1.Taint{Key: taintNoIPs, Effect: v1.TaintEffectNoSchedule})
			}

			clientset := fake.NewSimpleClientset(testNode)
			informerFactory := informers.NewSharedInformerFactory(clientset, 0)

			stopCh := make(chan struct{})
			go informerFactory.Start(stopCh)
			defer func() { stopCh <- struct{}{} }()

			r := New(
				clientset,
				informerFactory,
				&config.Config{TaintWhenNoIPs: true},
				alloc,
				"test-node",
			)

			if !cache.WaitForCacheSync(stopCh, r.nodesSynced) {
				t.Fatal("Error waiting for caches to sync")
			}

			r.IPPoolCheckInterval = 1 * time.Nanosecond

			if err := r.ipCheck(); err != nil {
				t.Errorf("Error calling ipCheck [%+v]", err)
			}

			node, err := clientset.Core().Nodes().Get("test-node", meta_v1.GetOptions{})
			if err != nil {
				t.Fatalf("Error fetching test-node [%+v]", err)
			}

			hasTaint := objutil.HasTaint(node, taintNoIPs, v1.TaintEffectNoSchedule)
			if hasTaint != tc.ExpectTaint {
				t.Errorf("Expected node tainted: %t but got %t", hasTaint, tc.ExpectTaint)
			}
		})
	}
}
