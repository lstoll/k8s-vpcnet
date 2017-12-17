package config

import (
	"encoding/json"
	"io/ioutil"
	"os"

	"github.com/containernetworking/cni/pkg/version"
	"github.com/lstoll/k8s-vpcnet/pkg/config"
	"github.com/pkg/errors"
)

const cniConfigPath = "/etc/cni/net.d/10-vpcnet.conf"

const CNIName = "vpcnet"

// CNIConfigPath is where the configuration is written to for CNI
const CNIConfigPath = "/etc/cni/net.d/10-vpcnet.conf"

// Net represents the top-level CNI config
type Net struct {
	Name       string `json:"name"`
	CNIVersion string `json:"cniVersion"`
	Type       string `json:"type"`
	IPAM       *IPAM  `json:"ipam"`
}

// IPAM represents the configuration that our CNI plugin receives
type IPAM struct {
	// Type of ipam to use, should always be vpcnet
	Type string `json:"type"`
	// ENIMapPath is the optional path to read the map from. Otherwise, use default
	IPAMSocketPath string `json:"ipam_socket"`

	// LogLevel is the glog v flag to set
	LogVerbosity int `json:"log_verbosity"`
}

// WriteCNIConfig will take the apps master config, and write out the CNI
// specific config to the default disk location
func WriteCNIConfig(c *config.Config, ipamPath string) error {
	err := os.MkdirAll("/etc/cni/net.d", 0755)
	if err != nil {
		return errors.Wrap(err, "Error creating /etc/cni/net.d")
	}

	// Build the config
	cnicfg := &Net{
		Name:       CNIName,
		CNIVersion: version.Current(),
		// Type is always ptp
		Type: "ptp",
		IPAM: &IPAM{
			Type:           "vpcnet",
			IPAMSocketPath: ipamPath,
			LogVerbosity:   c.Logging.CNIVLevel,
		},
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
