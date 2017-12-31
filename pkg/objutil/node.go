package objutil

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
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

// ProviderIDToAWSInstanceID takes a node Provider ID, and returns the AWS
// instance ID. From
// https://github.com/kubernetes/kubernetes/blob/1a11a18/pkg/cloudprovider/providers/aws/instances.go#L36:18
func ProviderIDToAWSInstanceID(providerID string) (string, error) {
	if !strings.HasPrefix(providerID, "aws://") {
		// Assume a bare aws volume id (vol-1234...)
		// Build a URL with an empty host (AZ)
		providerID = "aws://" + "/" + "/" + providerID
	}
	url, err := url.Parse(providerID)
	if err != nil {
		return "", fmt.Errorf("Invalid instance name (%s): %v", providerID, err)
	}
	if url.Scheme != "aws" {
		return "", fmt.Errorf("Invalid scheme for AWS instance (%s)", providerID)
	}

	awsID := ""
	tokens := strings.Split(strings.Trim(url.Path, "/"), "/")
	if len(tokens) == 1 {
		// instanceId
		awsID = tokens[0]
	} else if len(tokens) == 2 {
		// az/instanceId
		awsID = tokens[1]
	}

	// We sanity check the resulting volume; the two known formats are
	// i-12345678 and i-12345678abcdef01
	if awsID == "" || !awsInstanceRegMatch.MatchString(awsID) {
		return "", fmt.Errorf("Invalid format for AWS instance (%s)", providerID)
	}

	return awsID, nil
}

// awsInstanceRegMatch represents Regex Match for AWS instance.
var awsInstanceRegMatch = regexp.MustCompile("^i-[^/]*$")
