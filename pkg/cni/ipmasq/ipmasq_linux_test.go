package ipmasq

import (
	"flag"
	"net"
	"testing"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/coreos/go-iptables/iptables"
)

var runIPMasqTests bool

func init() {
	flag.BoolVar(&runIPMasqTests, "runipmasqtests", false, "Run the ipmasq tests. Configures namespaces/interfaces/iptables, requires superuser")
	flag.Parse()
}

func TestIPMasq(t *testing.T) {
	if !runIPMasqTests {
		t.Skip("Not flagged in to run ipmasq tests")
	}

	for _, tc := range []struct {
		Name string
	}{
		{
			Name: "Basic",
		},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			// Run tests in a fake NetNS to avoid polluting the main tables.
			hostNS, err := ns.NewNS()
			if err != nil {
				t.Fatalf("Error creating fake host ns [%v]", err)
			}
			defer hostNS.Close()

			err = hostNS.Do(func(ns.NetNS) error {
				t.Log("Setting up rule")

				err := Setup("tests", "testcontainerid", net.ParseIP("192.168.99.5"), []*net.IPNet{})
				if err != nil {
					t.Fatalf("Error calling Setup [%+v]", err)
				}

				t.Log("Tearing down rule")

				err = Teardown("tests", "testcontainerid")
				if err != nil {
					t.Fatalf("Error calling Teardown [%+v]", err)
				}

				t.Log("Checking rule gone")

				ipt, err := iptables.New()
				if err != nil {
					t.Fatalf("failed to locate iptables: %+v", err)
				}
				rules, err := ipt.List("nat", "POSTROUTING")
				if err != nil {
					t.Fatalf("failed to list rules in nat POSTROUTING %+v", err)
				}
				if len(rules) != 1 || rules[0] != "-P POSTROUTING ACCEPT" {
					t.Errorf("Expected no nat POSTROUTING rules, got %+v", rules)
				}

				return nil
			})
			if err != nil {
				t.Fatalf("Error running checks in NS [%+v]", err)
			}
		})
	}
}
