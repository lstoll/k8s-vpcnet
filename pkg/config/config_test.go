package config

import (
	"net"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestIPNet(t *testing.T) {
	cidrStr := "192.168.0.5/24"
	ipn := &IPNet{}
	err := ipn.UnmarshalText([]byte(cidrStr))
	if err != nil {
		t.Errorf("Error unmarshaling %s [%+v]", cidrStr, err)
	}
	if !ipn.IP.Equal(net.ParseIP("192.168.0.5")) {
		t.Errorf("Expected IP 192.168.0.5, got %s", ipn.IP.String())
	}
	if ipn.Mask.String() != "ffffff00" {
		t.Errorf("Expected Mask ffffff00, got %+v", ipn.Mask.String())
	}
}

const sampleConfig = `
[network]
service_cidr = "100.64.0.0/10"
pod_ip_masq = true

[logging]
cni_v_level = 2
`

func TestLoading(t *testing.T) {
	c := &Config{}
	_, err := toml.Decode(sampleConfig, &c)
	if err != nil {
		t.Errorf("Error decoding [%+v]", err)
	}
	if c.Network.ServiceCIDR.IP.String() != "100.64.0.0" {
		t.Errorf("Issue parsing service cidr, expected it to have IP 100.64.0.0, it had %s", c.Network.ServiceCIDR.IP.String())
	}
}
