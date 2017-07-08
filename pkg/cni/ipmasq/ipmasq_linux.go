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
	"crypto/sha512"
	"fmt"
	"log"
	"net"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/mgutz/str"
	"github.com/pkg/errors"
)

const (
	maxChainLength = 28
	chainPrefix    = "CNI-"
	prefixLength   = len(chainPrefix)
)

// Setup installs iptables rules to masquerade traffic coming from the
// described pod, and going to anywhere outside skipNets or the multicast range.
func Setup(pluginName, podID string, podIP net.IP, skipNets []*net.IPNet) error {
	isV6 := podIP.To4() == nil

	var ipt *iptables.IPTables
	var err error
	var multicastNet string

	chain := formatChainName(pluginName, podID)
	comment := formatComment(pluginName, podID)

	if isV6 {
		ipt, err = iptables.NewWithProtocol(iptables.ProtocolIPv6)
		multicastNet = "ff00::/8"
	} else {
		ipt, err = iptables.NewWithProtocol(iptables.ProtocolIPv4)
		multicastNet = "224.0.0.0/4"
	}
	if err != nil {
		return fmt.Errorf("failed to locate iptables: %v", err)
	}

	// Create chain if doesn't exist
	exists := false
	chains, err := ipt.ListChains("nat")
	if err != nil {
		return fmt.Errorf("failed to list chains: %v", err)
	}
	for _, ch := range chains {
		if ch == chain {
			exists = true
			break
		}
	}
	if !exists {
		if err = ipt.NewChain("nat", chain); err != nil {
			return err
		}
	}

	// Packets to these network should not be touched
	for _, sn := range skipNets {
		if err := ipt.AppendUnique("nat", chain, "-d", sn.String(), "-j", "ACCEPT", "-m", "comment", "--comment", comment); err != nil {
			return err
		}
	}

	// Don't masquerade multicast - pods should be able to talk to other pods
	// on the local network via multicast.
	if err := ipt.AppendUnique("nat", chain, "!", "-d", multicastNet, "-j", "MASQUERADE", "-m", "comment", "--comment", comment); err != nil {
		return err
	}

	return ipt.AppendUnique("nat", "POSTROUTING", "-s", podIP.String()+"/32", "-j", chain, "-m", "comment", "--comment", comment)
}

// Teardown undoes the effects of SetupIPMasq
func Teardown(pluginName, podID string) error {
	chain := formatChainName(pluginName, podID)
	comment := formatComment(pluginName, podID)

	ipt, err := iptables.New()
	if err != nil {
		return fmt.Errorf("failed to locate iptables: %v", err)
	}
	if err != nil {
		return fmt.Errorf("failed to locate iptables: %v", err)
	}

	rules, err := ipt.List("nat", "POSTROUTING")
	if err != nil {
		return errors.Wrap(err, "failed to list rules in nat POSTROUTING")
	}
	for _, r := range rules {
		if strings.Contains(r, fmt.Sprintf("--comment %q", comment)) {
			// Break it down in to individual items, dropping the -A and chain
			// TODO - could this be less fragile?
			dr := str.ToArgv(strings.TrimPrefix(r, "-A POSTROUTING"))
			log.Printf("Would delete %s", dr)
			err := ipt.Delete("nat", "POSTROUTING", dr...)
			if err != nil {
				return errors.Wrapf(err, "Error deleting %s from nat POSTROUTING", r)
			}
		}
	}

	if err = ipt.ClearChain("nat", chain); err != nil {
		return errors.Wrapf(err, "Error clearing chain %q from nat table", chain)
	}

	if err = ipt.DeleteChain("nat", chain); err != nil {
		return errors.Wrapf(err, "Error deleting chain %q from nat table", chain)
	}

	return nil
}

// Generates a chain name to be used with iptables.
// Ensures that the generated chain name is exactly
// maxChainLength chars in length
func formatChainName(name string, id string) string {
	chainBytes := sha512.Sum512([]byte(name + id))
	chain := fmt.Sprintf("%s%x", chainPrefix, chainBytes)
	return chain[:maxChainLength]
}

// FormatComment returns a comment used for easier
// rule identification within iptables.
func formatComment(name string, id string) string {
	return fmt.Sprintf("%s/%s", name, id)
}
