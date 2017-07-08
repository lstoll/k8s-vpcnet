package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"testing"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/lstoll/k8s-vpcnet/pkg/cni/config"
	"github.com/lstoll/k8s-vpcnet/pkg/vpcnetstate"
)

type testVether struct {
	hostIf        *current.Interface
	contIf        *current.Interface
	setupErr      error
	teardownError error
}

var testMap = vpcnetstate.ENIs{
	&vpcnetstate.ENI{
		EniID:       "eni-5d232b8d",
		Attached:    true,
		InterfaceIP: "10.0.8.96",
		CIDRBlock:   "10.0.0.0/20",
		Index:       1,
		IPs:         []string{"10.0.10.32", "10.0.15.243", "10.0.15.36"},
		MACAddress:  "0a:3e:1f:4e:c6:d2",
	},
}

func (v *testVether) SetupVeth(cfg *config.CNI, contnsPath, ifName string, net *podNet) (*current.Interface, *current.Interface, error) {
	return v.hostIf, v.contIf, v.setupErr
}

func (v *testVether) TeardownVeth(netns, ifname string) error {
	return v.teardownError
}

func TestMain(t *testing.T) {
	tv := &testVether{}
	runner := &cniRunner{
		vether: tv,
	}
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

	nsPath := "/var/run/netns/dummy"
	ifName := "conteth0"

	cniJSON := fmt.Sprintf(`{
		"cniVersion": "0.3.1",
		"name": "vpcnet",
		"type": "vpcnet",
		"data_dir": "%s",
		"eni_map_path": "%s"
	}`, dataDir, eniMapPath)

	args := &skel.CmdArgs{
		ContainerID: "dummy",
		Netns:       nsPath,
		IfName:      ifName,
		StdinData:   []byte(cniJSON),
	}

	tv.contIf = &current.Interface{
		Name: "eth0",
	}

	tv.hostIf = &current.Interface{
		Name: "veth123456",
	}

	// Allocate the IP
	r, raw, err := testutils.CmdAddWithResult(nsPath, ifName, []byte(cniJSON), func() error {
		return runner.cmdAdd(args)
	})

	if err != nil {
		t.Fatalf("Error calling add command [%+v]", err)
	}

	result, ok := r.(*current.Result)
	if !ok {
		t.Fatalf("Expected result to be *current.Result: %+v", r)
	}

	t.Logf("%v", result)
	t.Log(string(raw))

	// Free the IP
	err = testutils.CmdDelWithResult(nsPath, ifName, func() error {
		return runner.cmdDel(args)
	})

	if err != nil {
		t.Fatalf("Error executing del command [%v]", err)
	}
}
