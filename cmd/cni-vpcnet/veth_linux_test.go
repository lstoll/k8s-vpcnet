package main

import (
	"flag"
	"net"
	"os/exec"
	"testing"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/lstoll/k8s-vpcnet/pkg/cni/config"
	"github.com/lstoll/k8s-vpcnet/pkg/vpcnetstate"
	"github.com/songgao/packets/ethernet"
	"github.com/songgao/water"
	"github.com/vishvananda/netlink"
)

var allowNetNS bool

const cniJSON = `{
   "cniVersion": "0.3.1",
   "name": "vpcnet",
   "type": "vpcnet"
 }`

func init() {
	flag.BoolVar(&allowNetNS, "allow-netns", false, "Run tests that configure namespaces/interfaces etc, requires superuser")
	flag.Parse()
}

func TestVeth(t *testing.T) {
	v := &vetherImpl{}

	if !allowNetNS {
		t.Skip("Not flagged in to run interface tests")
	}

	for _, tc := range []struct {
		Name string
		Masq bool
	}{
		{
			Name: "No masquerading",
			Masq: false,
		},
		{
			Name: "With masquerading",
			Masq: true,
		},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			hostNS, err := ns.NewNS()
			if err != nil {
				t.Fatalf("Error creating fake host ns [%v]", err)
			}
			defer hostNS.Close()

			contNS, err := ns.NewNS()
			if err != nil {
				t.Fatalf("Error creating fake host ns [%v]", err)
			}
			defer contNS.Close()

			pn := &podNet{
				ContainerIP:  net.ParseIP("10.9.0.5"),
				ENIIp:        net.IPNet{IP: net.ParseIP("10.9.0.3"), Mask: net.CIDRMask(32, 32)},
				ENIInterface: "eni1", // Needs to exist inside the target NS
				ENISubnet:    &net.IPNet{IP: net.ParseIP("10.9.0.0"), Mask: net.CIDRMask(24, 32)},
				ENI: &vpcnetstate.ENI{
					Index: 1,
				},
			}

			enis := vpcnetstate.ENIs{
				{
					Index: 1,
				},
				{
					Index: 2,
				},
			}

			// Create a fake ENI1 interface in the host NS
			err = hostNS.Do(func(ns.NetNS) error {
				dip := &net.IPNet{
					IP:   pn.ENIIp.IP,
					Mask: net.CIDRMask(24, 32),
				}

				_ = DummyInterface(t, pn.ENIInterface, dip, true)

				return nil
			})
			if err != nil {
				t.Fatalf("Error creating dummy eni interface [%v]", err)
			}

			// Create a fake ENI2 interface in the host NS
			err = hostNS.Do(func(ns.NetNS) error {
				dip := &net.IPNet{
					IP:   pn.ENIIp.IP,
					Mask: net.CIDRMask(24, 32),
				}

				_ = DummyInterface(t, "eni2", dip, true)

				return nil
			})
			if err != nil {
				t.Fatalf("Error creating dummy eni interface [%v]", err)
			}

			// Create a fake ETH0 interface in the host NS
			err = hostNS.Do(func(ns.NetNS) error {
				dip := &net.IPNet{
					IP:   net.ParseIP("10.9.0.2"),
					Mask: net.CIDRMask(24, 32),
				}

				_ = DummyInterface(t, "eth0", dip, true)

				return nil
			})
			if err != nil {
				t.Fatalf("Error creating dummy eni interface [%v]", err)
			}

			for i := 0; i < 10; i++ {
				// Run the creation in our fake host netns
				err = hostNS.Do(func(ns.NetNS) error {
					_, _, err = v.SetupVeth(&config.CNI{IPMasq: tc.Masq}, enis, contNS.Path(), "eth0", pn)
					return err
				})
				if err != nil {
					t.Fatalf("Error setting up veth [%+v]", err)
				}

				err = hostNS.Do(func(ns.NetNS) error {
					cmd := exec.Command("ip", "route")
					stdoutStderr, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("Error running command [%v]", err)
					}
					t.Logf("Host NS Routes:\n%s\n", stdoutStderr)

					cmd = exec.Command("ip", "rule", "show")
					stdoutStderr, err = cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("Error running command [%v]: %s\n", err, stdoutStderr)
					}
					t.Logf("Host NS ip rules:\n%s\n", stdoutStderr)

					// cmd = exec.Command("ip", "route", "show", "table", "frompod-eni1")
					// stdoutStderr, err = cmd.CombinedOutput()
					// if err != nil {
					// 	t.Fatalf("Error running command [%v]: %s\n", err, stdoutStderr)
					// }
					// t.Logf("Host NS frompod table:\n%s\n", stdoutStderr)

					// cmd = exec.Command("ip", "route", "show", "table", "topod-eni1")
					// stdoutStderr, err = cmd.CombinedOutput()
					// if err != nil {
					// 	t.Fatalf("Error running command [%v]: %s\n", err, stdoutStderr)
					// }
					// t.Logf("Host NS topod table:\n%s\n", stdoutStderr)

					err = testutils.Ping(pn.ENIIp.IP.String(), pn.ContainerIP.String(), false, 1)
					if err != nil {
						t.Errorf("Error pinging cont IP from host IP [%+v]", err)
					}
					return nil
				})
				if err != nil {
					t.Fatalf("Error running checks in host NS [%+v]", err)
				}

				err = contNS.Do(func(ns.NetNS) error {
					cmd := exec.Command("ip", "route")
					stdoutStderr, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("Error running command [%v]: %s\n", err, stdoutStderr)
					}
					t.Logf("Cont NS Routes:\n%s\n", stdoutStderr)

					err = testutils.Ping(pn.ContainerIP.String(), pn.ENIIp.IP.String(), false, 1)
					if err != nil {
						t.Errorf("Error pinging address from container [%+v]", err)
					}
					return nil
				})
				if err != nil {
					t.Fatalf("Error running checks in cont NS [%+v]", err)
				}

				err = hostNS.Do(func(ns.NetNS) error {
					err := v.TeardownVeth(&config.CNI{IPMasq: tc.Masq}, enis, contNS.Path(), "eth0", []net.IP{pn.ContainerIP})
					if err != nil {
						t.Fatalf("Error tearing down veth [%+v]", err)
					}

					routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
					if err != nil {
						t.Fatalf("Error fetching host NS routes [%+v]", err)
					}
					t.Logf("routes: %+v", routes)

					rules, err := netlink.RuleList(netlink.FAMILY_ALL)
					if err != nil {
						t.Fatalf("Error fetching host NS rules [%+v]", err)
					}
					t.Logf("rules: %+v", rules)

					return nil
				})
				if err != nil {
					t.Fatalf("Error tearing down veth [%v]")
				}
			}
		})
	}
}

type dummyIF struct {
	PingRespond bool
	T           *testing.T
	Stop        chan (struct{})
	Iface       *water.Interface
}

func DummyInterface(t *testing.T, name string, addr *net.IPNet, pingRespond bool) *dummyIF {
	config := water.Config{
		DeviceType: water.TAP,
	}
	config.Name = name

	ifce, err := water.New(config)
	if err != nil {
		t.Fatalf("Error creating TAP interface [%+v]")
	}

	go func() {
		var frame ethernet.Frame

		for {
			frame.Resize(1500)
			n, err := ifce.Read([]byte(frame))
			if err != nil {
				t.Logf("Error reading from TAP IF [%+v]")
			}
			frame = frame[:n]
			t.Logf("[%s] Dst: %s\n", name, frame.Destination())
			t.Logf("[%s] Src: %s\n", name, frame.Source())
			t.Logf("[%s] Ethertype: % x\n", name, frame.Ethertype())
			t.Logf("[%s] Payload: % x\n", name, frame.Payload())
		}
	}()

	nif, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("Error getting dummy ENI interface [%v]")
	}

	naddr := &netlink.Addr{IPNet: addr, Label: ""}
	if err := netlink.AddrAdd(nif, naddr); err != nil {
		t.Fatalf("Could not add %s to dummy ENI [%v]", addr, err)
	}

	if err := netlink.LinkSetUp(nif); err != nil {
		t.Fatalf("Error bringing dummy ENI up [%v]", err)
	}

	d := &dummyIF{
		PingRespond: pingRespond,
		T:           t,
		Iface:       ifce,
	}

	return d
}
