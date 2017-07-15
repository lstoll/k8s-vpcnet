package main

import (
	"encoding/json"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/lstoll/k8s-vpcnet/pkg/cni/diskstore"
	"github.com/lstoll/k8s-vpcnet/pkg/vpcnetstate"
	"k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

func TestReconciler(t *testing.T) {
	var podsData []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(podsData)
	}))
	defer ts.Close()

	defer func(url string) { podsURL = url }(podsURL)
	podsURL = ts.URL

	for _, tc := range []struct {
		Name        string
		Allocations []net.IP
		ENIs        vpcnetstate.ENIs
		Pods        []v1.Pod

		// The IPs that should be released
		ExpectFreedIPs []net.IP
		// If we should expect a call to taint the node
		ExpectTaint bool
		// Pod ID's we should expect to see still in the API
		ExpectRemainingPods []string
	}{
		{
			Name: "Dangling IP allocation",
			Allocations: []net.IP{
				net.ParseIP("1.2.3.4"),
				net.ParseIP("4.5.6.7"),
			},
			ENIs: vpcnetstate.ENIs{&vpcnetstate.ENI{
				IPs: []net.IP{
					net.ParseIP("1.2.3.4"),
					net.ParseIP("4.5.6.7"),
					net.ParseIP("8.9.9.9"),
				},
			}}, Pods: []v1.Pod{
				{
					ObjectMeta: meta_v1.ObjectMeta{
						Name: "test-pod-ok",
					},
					Status: v1.PodStatus{
						PodIP: "1.2.3.4",
					},
				},
			},
			ExpectFreedIPs: []net.IP{
				net.ParseIP("4.5.6.7"),
			},
			ExpectTaint:         false,
			ExpectRemainingPods: []string{"test-pod-ok"},
		},
		{
			Name: "More allocated than in pool",
			Allocations: []net.IP{
				net.ParseIP("1.2.3.4"),
			},
			ENIs: vpcnetstate.ENIs{&vpcnetstate.ENI{
				IPs: []net.IP{
					net.ParseIP("1.2.3.4"),
				},
			}},
			Pods: []v1.Pod{
				{
					ObjectMeta: meta_v1.ObjectMeta{
						Name:      "test-pod-ok",
						Namespace: "default",
					},
					Status: v1.PodStatus{
						PodIP: "1.2.3.4",
					},
				},
				{
					ObjectMeta: meta_v1.ObjectMeta{
						Name:      "test-pod-overprovisioned",
						Namespace: "default",
					},
					Status: v1.PodStatus{
						ContainerStatuses: []v1.ContainerStatus{
							{
								State: v1.ContainerState{
									Waiting: &v1.ContainerStateWaiting{
										Message: "Start Container Failed",
										Reason:  "cannot join network of a non running container",
									},
								},
							},
						},
					},
				},
			},
			ExpectFreedIPs:      []net.IP{},
			ExpectTaint:         true, // We're full
			ExpectRemainingPods: []string{"test-pod-ok"},
		},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			t.Logf("Starting %s", tc.Name)

			dir, err := ioutil.TempDir("", "testreconciler")
			if err != nil {
				t.Fatalf("Error creating temp directory [%+v]", err)
			}
			defer os.RemoveAll(dir)

			ds, err := diskstore.New("testing", dir+"/reservations")
			if err != nil {
				t.Fatalf("Error creating disk store [%+v]", err)
			}
			for _, ip := range tc.Allocations {
				ok, err := ds.Reserve("id", ip, "range")
				if !ok || err != nil {
					t.Fatalf("Error reserving IP [%+v] ok: %t", err, ok)
				}
			}

			eniMapPath := dir + "/enis.json"
			err = vpcnetstate.WriteENIMap(eniMapPath, tc.ENIs)
			if err != nil {
				t.Fatalf("Error writing ENI map [%+v]", err)
			}

			podsData, err = json.Marshal(&v1.PodList{
				Items: tc.Pods,
			})

			testNode := &v1.Node{ObjectMeta: meta_v1.ObjectMeta{Name: "test-node"}}

			apiObjs := []runtime.Object{}
			apiObjs = append(apiObjs, testNode)
			for idx := range tc.Pods {
				apiObjs = append(apiObjs, &tc.Pods[idx])
			}

			r := &reconciler{
				store:      ds,
				eniMapPath: eniMapPath,
				indexer:    newIndexer(testNode),
				nodeName:   "test-node",
				clientSet:  fake.NewSimpleClientset(apiObjs...),
			}

			err = r.Reconcile()
			if err != nil {
				t.Fatalf("Error calling Reconcile [%+v]", err)
			}

			stillReserved, err := ds.Reservations()
			if err != nil {
				t.Fatalf("Error fetching reservations [%+v]", err)
			}
			for _, ip := range stillReserved {
				for _, expectFree := range tc.ExpectFreedIPs {
					if ip.Equal(expectFree) {
						t.Errorf("Expected IP %s to be freed, but found in current reservations", expectFree.String())
					}
				}
			}

			node, err := r.clientSet.Core().Nodes().Get("test-node", meta_v1.GetOptions{})
			if err != nil {
				t.Fatalf("Error fetching test-node [%+v]", err)
			}
			var tainted bool
			for _, t := range node.Spec.Taints {
				if t.Key == taintNoIPs &&
					t.Effect == v1.TaintEffectNoSchedule {
					tainted = true
				}
			}

			if tc.ExpectTaint && !tainted {
				t.Error("Expected node to be tainted, but it was not")
			}

			if !tc.ExpectTaint && tainted {
				t.Error("Expected node to not be tainted, but it is")
			}

			pods, err := r.clientSet.Core().Pods("").List(meta_v1.ListOptions{})
			if err != nil {
				t.Fatalf("Error fetching pods list [%+v]", err)
			}

			foundNames := []string{}
			for _, ap := range pods.Items {
				foundNames = append(foundNames, ap.Name)
			}
			if !reflect.DeepEqual(foundNames, tc.ExpectRemainingPods) {
				t.Errorf("Expected remaining pods to be %+v, but found %+v", tc.ExpectRemainingPods, foundNames)
			}

		})
	}
}

func newIndexer(node *v1.Node) cache.Indexer {
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	err := idx.Add(node)
	if err != nil {
		panic(err)
	}
	return idx
}
