// Copyright 2015 CNI authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ipmasq

import (
	"net"

	"github.com/coreos/go-iptables/iptables"
	"github.com/pkg/errors"
)

const (
	maxChainLength = 28
	chainPrefix    = "CNI-"
	prefixLength   = len(chainPrefix)
)

// Setup installs iptables rules to masquerade traffic coming from the
// described pod IPs, and going to anywhere outside skipNets or the multicast range.
func Setup(chain, comment string, podIPs []net.IP, skipNets []*net.IPNet) error {
	var ipt *iptables.IPTables
	var err error
	var multicastNet string

	ipt, err = iptables.NewWithProtocol(iptables.ProtocolIPv4)
	multicastNet = "224.0.0.0/4"

	if err != nil {
		return errors.Wrap(err, "failed to locate iptables: %v")
	}

	// Create chain if doesn't exist
	exists := false
	chains, err := ipt.ListChains("nat")
	if err != nil {
		return errors.Wrap(err, "failed to list chains")
	}
	for _, ch := range chains {
		if ch == chain {
			exists = true
			break
		}
	}
	if !exists {
		if err = ipt.NewChain("nat", chain); err != nil {
			return errors.Wrap(err, "Error creating chain")
		}
	}

	// Packets to these network should not be touched
	for _, sn := range skipNets {
		if err := ipt.AppendUnique("nat", chain, "-d", sn.String(), "-j", "ACCEPT", "-m", "comment", "--comment", comment); err != nil {
			return errors.Wrap(err, "Error adding skip rule")
		}
	}

	// Don't masquerade multicast - pods should be able to talk to other pods
	// on the local network via multicast.
	if err := ipt.AppendUnique("nat", chain, "!", "-d", multicastNet, "-j", "MASQUERADE", "-m", "comment", "--comment", comment); err != nil {
		return errors.Wrap(err, "Error adding skip rule for multicast")
	}

	for _, ip := range podIPs {
		err := ipt.AppendUnique("nat", "POSTROUTING", "-s", ip.String()+"/32", "-j", chain, "-m", "comment", "--comment", comment)
		if err != nil {
			return errors.Wrapf(err, "Error inserting jump for pod IP %v", ip)
		}
	}
	return nil
}
