package vpcnetstate

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"

	"github.com/lstoll/k8s-vpcnet/pkg/config"
	"github.com/pkg/errors"
)

// DefaultENIMapPath is the default path on the machine we store the address
// mapping
const DefaultENIMapPath = "/var/lib/cni/vpcnet/eni_map.json"

// IFSKey is the key for the node annotation we use to persist interface
// configuration
const IFSKey = "eni-interfaces"

// ENIs is the list of ENI's we store on a node
type ENIs []*ENI

// ENI represents the configuration for a single ENI
type ENI struct {
	// EniID is the ID that AWS assigns for this interface
	EniID string `json:"eni_id"`
	// Attached indicates if it's attached to an interface
	Attached bool `json:"attached"`
	// InterfaceIP is the main IP address for the interface
	InterfaceIP net.IP `json:"interface_ip"`
	// Index is the attachment index on the instance
	Index int `json:"index"`
	// CIDRBlock is the subnet this interface is attached to
	CIDRBlock *config.IPNet `json:"cidr_block"`
	// IPs are the additional IP addresses assigned to the interface
	IPs []net.IP `json:"ips"`
	// MACAddress is the hardware MAC address for this interface
	MACAddress string `json:"mac_address"`
}

// InterfaceName returns the name this ENI should have on the host
func (e *ENI) InterfaceName() string {
	return fmt.Sprintf("eni%d", e.Index)
}

// ENIConfigFromAnnotations returns checks for configuration, returning it if found or
// nil if it isn't
func ENIConfigFromAnnotations(annotations map[string]string) (ENIs, error) {
	ifs, ok := annotations[IFSKey]
	if !ok {
		return nil, nil
	}

	nc := []*ENI{}

	err := json.Unmarshal([]byte(ifs), &nc)
	if err != nil {
		return nil, errors.Wrap(err, "Error parsing node network configuration")
	}

	return nc, nil
}

// WriteENIMap persists the map to disk
func WriteENIMap(path string, em ENIs) error {
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	b, err := json.Marshal(em)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(path, b, 0644)
	if err != nil {
		return err
	}
	return nil
}

// ReadENIMap loads the map from disk
func ReadENIMap(path string) (ENIs, error) {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	am := ENIs{}
	err = json.Unmarshal(b, &am)
	if err != nil {
		return nil, err
	}
	return am, nil
}
