package config

import (
	"encoding/json"
	"io/ioutil"
	"net"
	"os"

	"github.com/containernetworking/cni/pkg/version"
	"github.com/lstoll/k8s-vpcnet/pkg/config"
	"github.com/pkg/errors"
)

const cniConfigPath = "/etc/cni/net.d/10-vpcnet.conf"

const CNIName = "vpcnet"

// CNIConfigPath is where the configuration is written to for CNI
const CNIConfigPath = "/etc/cni/net.d/10-vpcnet.conf"

// CNI represents the configuration that our CNI plugin receives
type CNI struct {
	Name       string `json:"name"`
	CNIVersion string `json:"cniVersion"`
	// Type is the type of interface plugin in use
	Type string `json:"type"`

	// ENIMapPath is the optional path to read the map from. Otherwise, use default
	ENIMapPath string `json:"eni_map_path"`
	//DataDir overrides the dir that the plugin will track state in
	DataDir string `json:"data_dir"`

	// IPMasq will write outbound masquerate iptables rules
	IPMasq bool `json:"ip_masq"`

	// ClusterCIDR is the CIDR in which pods will run in
	ClusterCIDR *net.IPNet `json:"cluster_cidr"`

	// ServiceCIDR is the CIDR for the cluster services network. This is needed
	// to ensure the correct routing for this interface.
	ServiceCIDR *net.IPNet `json:"service_cidr"`

	// LogLevel is the glog v flag to set
	LogVerbosity int `json:"log_verbosity"`
}

// WriteCNIConfig will take the apps master config, and write out the CNI
// specific config to the default disk location
func WriteCNIConfig(c *config.Config) error {
	err := os.MkdirAll("/etc/cni/net.d", 0755)
	if err != nil {
		return errors.Wrap(err, "Error creating /etc/cni/net.d")
	}

	// Build the config
	cnicfg := &CNI{
		Name:       CNIName,
		CNIVersion: version.Current(),
		Type:       "vpcnet",
		// Let the paths just use the defaults
		IPMasq:       c.Network.PodIPMasq,
		ServiceCIDR:  c.Network.ServiceCIDR.IPNet(),
		ClusterCIDR:  c.Network.ClusterCIDR.IPNet(),
		LogVerbosity: c.Logging.CNIVLevel,
	}

	cniJSON, err := json.Marshal(cnicfg)
	if err != nil {
		return errors.Wrap(err, "Error marshaling CNI JSON")
	}

	err = ioutil.WriteFile(CNIConfigPath, cniJSON, 0644)
	if err != nil {
		return errors.Wrap(err, "Error writing CNI configuration JSON")
	}

	return nil
}
