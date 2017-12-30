package objutil

import (
	"time"

	"github.com/cenk/backoff"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	"k8s.io/api/core/v1"
	api_errors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typed_v1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// UpdateNode will get/update the node until there is no conflict. The passed in
// function is used as the mutator for the current node state
func UpdateNode(nc typed_v1.NodeInterface, name string, mutator func(node *v1.Node)) error {
	node, err := nc.Get(name, meta_v1.GetOptions{})
	if err != nil {
		return errors.Wrapf(err, "Error fetching node %s to update", node)
	}
	bo := backoff.NewExponentialBackOff()
	for {
		mutator(node)

		_, err := nc.Update(node)
		if api_errors.IsConflict(err) {
			// Node was modified, fetch and try again
			glog.V(2).Infof("Conflict updating node %s, retrying", name)
			node, err = nc.Get(name, meta_v1.GetOptions{})
			if err != nil {
				return errors.Wrapf(err, "Error fetching node %s to update", node)
			}
		} else if err != nil {
			return errors.Wrapf(err, "Error updating node %s", name)
		} else {
			break
		}

		time.Sleep(bo.NextBackOff())
		// TODO - prevent indefinite loops?
	}
	return nil
}

// HasTaint returns true if the node has a taint matching the given key and
// effect
func HasTaint(node *v1.Node, key string, effect v1.TaintEffect) bool {
	var tainted bool
	for _, t := range node.Spec.Taints {
		if t.Key == key &&
			t.Effect == effect {
			tainted = true
		}
	}
	return tainted
}

// AddTaint returns a mutator func that will add the given taint to the node
func AddTaint(key string, effect v1.TaintEffect) func(node *v1.Node) {
	return func(n *v1.Node) {
		n.Spec.Taints = append(n.Spec.Taints, v1.Taint{
			Key:    key,
			Effect: effect,
		})
	}
}

// RemoveTaint returns a mutator func that will remove the given taint spec from the object
func RemoveTaint(key string, effect v1.TaintEffect) func(node *v1.Node) {
	return func(n *v1.Node) {
		keep := []v1.Taint{}
		for _, t := range n.Spec.Taints {
			if !(t.Key == key &&
				t.Effect == effect) {
				keep = append(keep, t)
			}
		}
		n.Spec.Taints = keep
	}
}
