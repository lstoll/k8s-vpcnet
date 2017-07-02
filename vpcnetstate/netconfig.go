package vpcnetstate

import (
	"encoding/json"

	"github.com/pkg/errors"
)

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
