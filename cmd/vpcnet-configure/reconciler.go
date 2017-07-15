package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cenk/backoff"
	"github.com/golang/glog"
	"github.com/lstoll/k8s-vpcnet/pkg/cni/diskstore"
	"github.com/lstoll/k8s-vpcnet/pkg/vpcnetstate"
	"github.com/pkg/errors"
	"k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// podsURL is the URL to fetch node pods from the kubelet insecure port
var podsURL = "http://localhost:10255/pods"

var podsClient = &http.Client{Timeout: 5 * time.Second}

type reconciler struct {
	store     *diskstore.Store
	indexer   cache.Indexer
	nodeName  string
	clientSet kubernetes.Interface
}

// Reconcile will check the pods on this node vs. the allocated IP addresses,
// freeing any allocations for pods that don't exist. If the IP pool is
// exhausted, it will taint the node as NoFreeIPs and evict any pods that are
// bound but have no IPs
func (r *reconciler) Reconcile() error {
	podList, err := fetchPodList()
	if err != nil {
		return errors.Wrap(err, "Error fetching pod list")
	}

	// Compare reservations to current pod list, freeing any reserved IPs that
	// do not have a corresponding pod

	alloced, err := r.store.Reservations()
	if err != nil {
		return errors.Wrap(err, "Error fetching reserved IPs")
	}

	for _, ip := range alloced {
		var found bool
		for _, p := range podList.Items {
			if net.ParseIP(p.Status.PodIP).Equal(ip) {
				found = true
			}
		}
		if !found {
			glog.Infof("Dangling reservation found for %s, freeing", ip.String())
			err := r.store.Release(ip)
			if err != nil {
				return errors.Wrapf(err, "Error releasing IP %v", ip)
			}
		}
	}

	// Check the number of reserved IPs against the size of the pool in the map
	// If we're at capacity, put a taint on the node to prevent further
	// scheduling. Then look for containers that have a status of
	// "ContainerCannotRun", with message "cannot join network" (?). Evict or
	// delete the pod.
	obj, exists, err := r.indexer.GetByKey(r.nodeName)
	if err != nil {
		return errors.Wrapf(err, "Fetching object with key %s from store failed", r.nodeName)
	}
	if !exists {
		return fmt.Errorf("could not find node %s in index", r.nodeName)
	}

	node, ok := obj.(*v1.Node)
	if !ok {
		return fmt.Errorf("object with key %s [%v] is not a node", r.nodeName, obj)
	}

	enis, err := vpcnetstate.ENIConfigFromAnnotations(node.Annotations)
	if err != nil {
		return errors.Wrapf(err, "Error reading ENI map from annotations")
	}
	var ipPoolSize int

	for _, eni := range enis {
		ipPoolSize += len(eni.IPs)
	}

	if len(alloced) >= ipPoolSize {
		// We're full! Make sure we have the relevant taint
		var tainted bool
		for _, t := range node.Spec.Taints {
			if t.Key == taintNoIPs &&
				t.Effect == v1.TaintEffectNoSchedule {
				tainted = true
			}
		}
		if !tainted {
			updateNode(r.clientSet.Core().Nodes(), node.Name, func(n *v1.Node) {
				n.Spec.Taints = append(n.Spec.Taints, v1.Taint{
					Key:    taintNoIPs,
					Effect: v1.TaintEffectNoSchedule,
				})
			})
		}

		// TODO - these errors are weak and docker specific. Figure out a better
		// way to work out if things are bad.
		var failedPods []v1.Pod
		for _, p := range podList.Items {
			for _, cs := range p.Status.ContainerStatuses {
				if cs.State.Waiting != nil &&
					cs.State.Waiting.Message == "Start Container Failed" &&
					strings.Contains(cs.State.Waiting.Reason, "cannot join network of a non running container") {
					failedPods = append(failedPods, p)
				}
			}
		}

		for _, p := range failedPods {
			glog.Infof("Deleting oversubscribed pod %s.%s", p.Namespace, p.Name)
			// TODO - is just killing the pods the best path forward?
			err = r.clientSet.Core().Pods(p.Namespace).Delete(p.Name, &meta_v1.DeleteOptions{})
			if err != nil {
				return errors.Wrapf(err, "Error deleting pod %q from namespace %q", p.Name, p.Namespace)
			}
		}
	}

	// If we're below capacity, check the node object for the taint. If it's
	// set, clear it. We can check via the cache from the main loop, to avoid
	// having to call out to the API each time - it'll be updated from the watch
	if len(alloced) < ipPoolSize {
		var tainted bool
		for _, t := range node.Spec.Taints {
			if t.Key == taintNoIPs &&
				t.Effect == v1.TaintEffectNoSchedule {
				tainted = true
			}
		}

		if tainted {
			updateNode(r.clientSet.Core().Nodes(), node.Name, func(n *v1.Node) {
				keep := []v1.Taint{}
				for _, t := range n.Spec.Taints {
					if !(t.Key == taintNoIPs &&
						t.Effect == v1.TaintEffectNoSchedule) {
						keep = append(keep, t)
					}
				}
				n.Spec.Taints = keep
			})
		}
	}

	return nil
}

func fetchPodList() (*v1.PodList, error) {
	result := &v1.PodList{}

	fetch := func() error {
		r, err := podsClient.Get(podsURL)
		if err != nil {
			return errors.Wrap(err, "Error fetching pods data")
		}
		defer r.Body.Close()

		err = json.NewDecoder(r.Body).Decode(result)
		if err != nil {
			return errors.Wrap(err, "Error decoding pods JSON body")
		}
		return nil
	}

	ebo := backoff.NewExponentialBackOff()
	ebo.MaxElapsedTime = 30 * time.Second

	err := backoff.Retry(fetch, ebo)
	if err != nil {
		return nil, errors.Wrapf(err, "Error fetching pods list from %s", podsURL)
	}

	return result, nil
}
