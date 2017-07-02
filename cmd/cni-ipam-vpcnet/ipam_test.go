package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path"
	"testing"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/lstoll/k8s-vpcnet/vpcnetstate"
)

const testBr = "vpcbr0"

var testMap = vpcnetstate.ENIMap{
	testBr: &vpcnetstate.ENI{
		EniID:       "eni-5d232b8d",
		Attached:    true,
		InterfaceIP: "10.0.8.96",
		CIDRBlock:   "10.0.0.0/20",
		Index:       1,
		IPs:         []string{"10.0.8.96", "10.0.10.32", "10.0.15.243", "10.0.15.36"},
		MACAddress:  "0a:3e:1f:4e:c6:d2",
	},
}

func TestIPAM(t *testing.T) {
	workDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(workDir)

	jsb, err := json.Marshal(testMap)
	if err != nil {
		t.Fatal(err)
	}
	eniMapPath := path.Join(workDir, "eni_map.json")
	err = ioutil.WriteFile(eniMapPath, jsb, 0644)
	if err != nil {
		t.Fatal(err)
	}

	dataDir := path.Join(workDir, "cni")
	err = os.MkdirAll(dataDir, 0755)
	if err != nil {
		t.Fatal(err)
	}

	conf := fmt.Sprintf(`{
		"cniVersion": "0.3.1",
		"name": "vpcbr",
		"type": "bridge",
		"bridge": "%s",
		"ipam": {
			"type": "vpcnet",
			"eni_map_path": "%s",
			"data_dir": "%s"
		}
	}`, testBr, eniMapPath, dataDir)

	nsPath := "/var/run/netns/dummy"
	ifName := "conteth0"

	args := &skel.CmdArgs{
		ContainerID: "dummy",
		Netns:       nsPath,
		IfName:      ifName,
		StdinData:   []byte(conf),
	}

	// Allocate the IP
	r, raw, err := testutils.CmdAddWithResult(nsPath, ifName, []byte(conf), func() error {
		return cmdAdd(args)
	})

	if err != nil {
		t.Fatalf("Error executing add command [%v]", err)
	}

	t.Logf("Add raw response: %s\n", string(raw))

	result, ok := r.(*current.Result)
	if !ok {
		t.Fatalf("Expected result to be *current.Result: %+v", r)
	}

	found := false
	for _, i := range testMap[testBr].IPs {
		if result.IPs[0].Address.IP.Equal(net.ParseIP(i)) {
			found = true
		}
	}
	if !found {
		t.Errorf("Allocated IP %q that we're not configured to issue from %v", result.IPs[0].Address.IP.String(), testMap[testBr].IPs)
	}

	// Free the IP
	err = testutils.CmdDelWithResult(nsPath, ifName, func() error {
		return cmdDel(args)
	})

	if err != nil {
		t.Fatalf("Error executing del command [%v]", err)
	}
}
