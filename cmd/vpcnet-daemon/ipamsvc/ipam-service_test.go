package ipamsvc

import (
	"context"
	"testing"

	"github.com/lstoll/k8s-vpcnet/pkg/allocator"
	"github.com/lstoll/k8s-vpcnet/pkg/config"
	"github.com/lstoll/k8s-vpcnet/pkg/vpcnetpb"
)

type fakeEvictor struct {
	lastEvictedPodName      string
	lastEvictedPodNamespace string
}

func (f *fakeEvictor) EvictPod(namespace, name string) error {
	f.lastEvictedPodName = name
	f.lastEvictedPodNamespace = namespace
	return nil
}

type fakeAllocator struct {
	nextErr error
}

func (f *fakeAllocator) Allocate(containerID, podName, podNamspace string) (*allocator.Allocation, error) {
	return &allocator.Allocation{}, f.nextErr
}

func (f *fakeAllocator) ReleaseByContainer(containerID string) error {
	return nil
}

func TestEviction(t *testing.T) {
	alloc := &fakeAllocator{}
	evict := &fakeEvictor{}

	svc := &Service{
		Allocator: alloc,
		Evictor:   evict,
		Config:    &config.Config{DeletePodWhenNoIPs: true},
	}

	_, err := svc.Add(context.Background(), &vpcnetpb.AddRequest{
		PodName:      "pod-name",
		PodNamespace: "pod-namespace",
	})
	if err != nil {
		t.Errorf("Unexpected rror calling clean Add [%+v]", err)
	}

	alloc.nextErr = allocator.ErrNoFreeIPs
	_, err = svc.Add(context.Background(), &vpcnetpb.AddRequest{
		PodName:      "pod-name",
		PodNamespace: "pod-namespace",
	})
	if err == nil {
		t.Error("Expected error, got none")
	}

	if evict.lastEvictedPodName != "pod-name" || evict.lastEvictedPodNamespace != "pod-namespace" {
		t.Errorf("Unexpected pod evicted, ns: %s name: %s", evict.lastEvictedPodNamespace, evict.lastEvictedPodName)
	}
}
