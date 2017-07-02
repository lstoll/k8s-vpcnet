// Copyright 2015 CNI authors
// Modifications copyright 2017 Lincoln Stoll
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

package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/containernetworking/plugins/plugins/ipam/host-local/backend/disk"
	"github.com/pkg/errors"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/lstoll/k8s-vpcnet/vpcnetstate"
)

// Net is the top level data passed in
type Net struct {
	Name       string      `json:"name"`
	CNIVersion string      `json:"cniVersion"`
	IPAM       *IPAMConfig `json:"ipam"`
}

// IPAMConfig is the config for this driver
type IPAMConfig struct {
	Name string `json:"name"`
	// Interface is the "bridge" interface we should look up in the map to get
	// IPs from
	Interface string `json:"interface"`
	// TODO - handle the more than one bridge case
}

func main() {
	skel.PluginMain(cmdAdd, cmdDel, version.All)
}

func cmdAdd(args *skel.CmdArgs) error {
	conf, err := loadConfig(args.StdinData)
	if err != nil {
		return err
	}

	em, err := vpcnetstate.ReadENIMap()
	if err != nil {
		return err
	}

	store, err := disk.New(conf.IPAM.Name, "")
	if err != nil {
		return err
	}
	defer store.Close()

	alloc := &IPAllocator{
		conf:   conf,
		store:  store,
		eniMap: em,
	}

	lt := &current.Result{}

	result.Routes = ipamConf.Routes

	return types.PrintResult(result, confVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	conf, err := loadConfig(args.StdinData)
	if err != nil {
		return err
	}

	store, err := disk.New(conf.IPAM.Name, "")
	if err != nil {
		return err
	}
	defer store.Close()

	if errors != nil {
		return fmt.Errorf(strings.Join(errors, ";"))
	}
	return nil
}

func loadConfig(dat []byte) (*Net, error) {
	n := &Net{}
	err := json.Unmarshal(dat, n)
	if err != nil {
		return nil, errors.Wrap(err, "Error unmarshaling Net")
	}
	return n, nil
}
