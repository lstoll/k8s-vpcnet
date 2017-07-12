package ipmasq

import (
	"flag"
	"net"
	"testing"

	"github.com/containernetworking/plugins/pkg/ns"
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

				err := Setup("chain", "somecomment", []net.IP{net.ParseIP("192.168.99.5")}, []*net.IPNet{})
				if err != nil {
					t.Fatalf("Error calling Setup [%+v]", err)
				}

				return nil
			})
			if err != nil {
				t.Fatalf("Error running checks in NS [%+v]", err)
			}
		})
	}
}
