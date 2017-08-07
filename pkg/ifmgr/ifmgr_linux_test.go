package ifmgr

import (
	"flag"
	"net"
	"testing"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/lstoll/k8s-vpcnet/pkg/config"
	udbus "k8s.io/kubernetes/pkg/util/dbus"
	uiptables "k8s.io/kubernetes/pkg/util/iptables"
	uexec "k8s.io/utils/exec"
)

var allowNetNS bool

func init() {
	flag.BoolVar(&allowNetNS, "allow-netns", false, "Run tests that configure namespaces/interfaces/iptables, requires superuser")
	flag.Parse()
}

func TestConfigureIPMasq(t *testing.T) {
	if !allowNetNS {
		t.Skip("Not flagged in to run netns based tests")
	}

	cfg := &config.Network{
		ClusterCIDR:                  &config.IPNet{IP: net.ParseIP("10.0.0.0"), Mask: net.CIDRMask(20, 32)},
		ServiceCIDR:                  &config.IPNet{IP: net.ParseIP("100.64.0.0"), Mask: net.CIDRMask(18, 32)},
		PodIPMasq:                    true,
		InstanceMetadataRedirectPort: 8181,
	}
	ipt := uiptables.New(uexec.New(), udbus.New(), uiptables.ProtocolIpv4)

	ifmgr := New(cfg, ipt)

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
				t.Log("Running configurator")

				err := ifmgr.ConfigureIPMasq(net.ParseIP("1.2.3.4"), []net.IP{net.ParseIP("192.168.99.5")})

				if err != nil {
					t.Fatalf("Error calling configureIPMasq [%+v]", err)
				}

				return nil
			})
			if err != nil {
				t.Fatalf("Error running checks in NS [%+v]", err)
			}
		})
	}
}
