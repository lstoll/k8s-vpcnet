package nodestate

import (
	"encoding/json"
	"fmt"
	"net"

	"github.com/lstoll/k8s-vpcnet/pkg/config"
	"github.com/pkg/errors"
)

// DefaultENIMapPath is the default path on the machine we store the address
// mapping
const DefaultENIMapPath = "/var/lib/cni/vpcnet/eni_map.json"

// IFSKey is the key for the node annotation we use to persist interface
// configuration
const IFSKey = "k8s-vpcnet/eni-interfaces"

// EC2InfoKey is the key for the node annotation we persist instance information
// under
const EC2InfoKey = "k8s-vpcnet/ec2-instance-info"

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

// AWSInstanceInfo represents additional instance metadata that we store on the
// Node object, to avoid having to continually fetch it from the EC2 API
type AWSInstanceInfo struct {
	// InstanceType is the AWS type for this instance, e.g c4.large
	InstanceType string `json:"instance_type"`
	// SubnetID is the ID of the VPC subnet this instance lives in.
	SubnetID string `json:"subnet_id"`
	// SubnetCIDR is the netblock the subnet lives in
	SubnetCIDR *config.IPNet `json:"subnet_cidr"`
	// SecurityGroupIDs is the list of IDs for the security groups this instance
	// has
	SecurityGroupIDs []string `json:"security_group_ids"`
}

// AWSInstanceInfoFromAnnotations returns checks for instance metadata,
// returning it if found or nil if it isn't
func AWSInstanceInfoFromAnnotations(annotations map[string]string) (*AWSInstanceInfo, error) {
	ifs, ok := annotations[EC2InfoKey]
	if !ok {
		return nil, nil
	}

	ii := &AWSInstanceInfo{}

	err := json.Unmarshal([]byte(ifs), ii)
	if err != nil {
		return nil, errors.Wrap(err, "Error parsing instance info")
	}

	return ii, nil
}
