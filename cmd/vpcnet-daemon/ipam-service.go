package main

import (
	"context"

	"github.com/golang/glog"
	"github.com/lstoll/k8s-vpcnet/pkg/allocator"
	"github.com/lstoll/k8s-vpcnet/pkg/vpcnetpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

var _ vpcnetpb.IPAMServer = &IPAMService{}

type IPAMService struct {
	Allocator *allocator.Allocator
}

func (i *IPAMService) Add(ctx context.Context, req *vpcnetpb.AddRequest) (*vpcnetpb.AddResponse, error) {
	a, err := i.Allocator.Allocate(req.ContainerID, req.PodName, req.PodNamespace)
	if err != nil {
		glog.Errorf("Error calling allocator Allocate for Container %q Pod %s/%s: [%+v}", req.ContainerID, req.PodNamespace, req.PodName, err)
		return nil, grpc.Errorf(codes.Internal, "Error allocating address: %q", err.Error())
	}

	return &vpcnetpb.AddResponse{
		AllocatedIP: a.IP.String(),
		ENIIP:       a.ENIIP.String(),
		SubnetCIDR:  a.ENISubnet.String(),
	}, nil
}

func (i *IPAMService) Del(ctx context.Context, req *vpcnetpb.DelRequest) (*vpcnetpb.DelResponse, error) {
	err := i.Allocator.ReleaseByContainer(req.ContainerID)
	if err != nil {
		glog.Errorf("Error calling allocator Release for Container %q: [%+v}", req.ContainerID, err)
		return nil, grpc.Errorf(codes.Internal, "Error releasing address: %q", err.Error())
	}

	return &vpcnetpb.DelResponse{}, nil
}
