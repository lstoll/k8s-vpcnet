package vpcnetstate

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/pkg/errors"
)

// DefaultENIMapPath is the default path on the machine we store the address
// mapping
const DefaultENIMapPath = "/var/lib/cni/vpcnet/eni_map.json"

// IFSKey is the key for the node annotation we use to persist interface
// configuration
const IFSKey = "eni-interfaces"

// ENIMap is the data type we store on a node
type ENIMap map[string]*ENI

// ENI represents the configuration for a single ENI
type ENI struct {
	EniID       string   `json:"eni_id"`
	Attached    bool     `json:"attached"`
	InterfaceIP string   `json:"interface_ip"`
	CIDRBlock   string   `json:"cidr_block"`
	Index       int      `json:"index"`
	IPs         []string `json:"ips"`
	MACAddress  string   `json:"mac_address"`
}

// ENIConfigFromAnnotations returns checks for configuration, returning it if found or
// nil if it isn't
func ENIConfigFromAnnotations(annotations map[string]string) (ENIMap, error) {
	ifs, ok := annotations[IFSKey]
	if !ok {
		return nil, nil
	}

	nc := make(map[string]*ENI)

	err := json.Unmarshal([]byte(ifs), &nc)
	if err != nil {
		return nil, errors.Wrap(err, "Error parsing node network configuration")
	}

	return nc, nil
}

// WriteENIMap persists the map to disk
func WriteENIMap(path string, em ENIMap) error {
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
func ReadENIMap(path string) (ENIMap, error) {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	am := ENIMap{}
	err = json.Unmarshal(b, &am)
	if err != nil {
		return nil, err
	}
	return am, nil
}
