package config

import (
	"io/ioutil"
	"net"

	"github.com/BurntSushi/toml"
	"github.com/pkg/errors"
)

// TODO - dynamically update this, provide a mechanism for watching changes.

// DefaultConfigPath represents the path the config would be located at using
// the defauly manifest
const DefaultConfigPath = "/etc/vpcnet/config.toml"

// IPNet is a net.IPNet that can be unserialized from CIDR notation as a
// encoding.TextUnmarshaler
type IPNet net.IPNet

// UnmarshalText will unmarshal this IPNet from CIDR text notation
func (n *IPNet) UnmarshalText(text []byte) error {
	ip, cidr, err := net.ParseCIDR(string(text))
	if err != nil {
		return err
	}
	n.IP = ip
	n.Mask = cidr.Mask
	return nil
}

// MarshalText will marshal this IPNet to CIDR text notation
func (n *IPNet) MarshalText() ([]byte, error) {
	if n == nil {
		return nil, nil
	}
	ipn := net.IPNet(*n)
	return []byte(ipn.String()), nil
}

// IPNet returns self as a *net.IPNet
func (n *IPNet) IPNet() *net.IPNet {
	if n == nil {
		return nil
	}
	ipn := net.IPNet(*n)
	return &ipn
}

// Config is the master config type for the app
type Config struct {
	// Network is all the networking related configuration for the cluster
	Network *Network `toml:"network"`
	// Logging is where the logging configuration ends up
	Logging *Logging `toml:"logging"`
}

const (
	// FromPodRTBase is the base number we use + attach index to number the from
	// pod routing table
	FromPodRTBase = 120
)

// Network is the network topology related configuration for this cluster
type Network struct {
	// ClusterCIDR is the CIDR in which pods will run in
	ClusterCIDR *IPNet `toml:"cluster_cidr"`
	// ServiceCIDR is the CIDR for cluster services
	ServiceCIDR *IPNet `toml:"service_cidr"`
	// PodIPMasq indicated if we should masquerade external pod traffic from the
	// hosts's main interface
	PodIPMasq bool `toml:"pod_ip_masq"`
	// VPCRouted are additional addresses to route via the VPC subnet gateway,
	// rather than via the host's IP.
	VPCRouted []*IPNet `toml:"vpc_routed"`
	// InstanceMetadataRedirectPort is the port on the machines main IP we
	// should redirect all instance metadata traffic to. Use with kube2iam
	InstanceMetadataRedirectPort int `toml:"instance_metadata_redirect_port"`
	// HostPrimaryInterface is the name of the main interface in the machine,
	// i.e what masq should egress. If not set, inferred from metadata API.
	HostPrimaryInterface string `toml:"host_primary_interface"`
}

// Logging is the master configuration for this app. It is updated from a ConfigMap
type Logging struct {
	// CNIVLevel is used to set the logging verbosity of the CNI plugin
	CNIVLevel int `toml:"cni_v_level"`
}

// Load will retrieve and parse the config from disk
func Load(path string) (*Config, error) {
	dat, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, errors.Wrap(err, "Error loading configuration file from disk")
	}

	c := &Config{}
	_, err = toml.Decode(string(dat), &c)
	if err != nil {
		return nil, errors.Wrap(err, "Error decoding configuration file data")
	}

	// build the CNI config up

	return c, nil
}
