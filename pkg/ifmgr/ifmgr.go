// Package ifmgr is used for managing interfaces and their routing/rules
package ifmgr

import (
	"github.com/lstoll/k8s-vpcnet/pkg/config"
	uiptables "k8s.io/kubernetes/pkg/util/iptables"
)

// IFMgr is used for managing host ENI interfaces
type IFMgr struct {
	Network  *config.Network
	IPTables uiptables.Interface
}

func New(netConf *config.Network, ipt uiptables.Interface) *IFMgr {
	return &IFMgr{
		Network:  netConf,
		IPTables: ipt,
	}
}
