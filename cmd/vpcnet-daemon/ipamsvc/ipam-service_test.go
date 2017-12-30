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

	fullCalled    bool
	notFullCalled bool
}

func (f *fakeEvictor) reset() {
	f.lastEvictedPodName = ""
	f.lastEvictedPodNamespace = ""

	f.fullCalled = false
	f.notFullCalled = false
}

func (f *fakeEvictor) EvictPod(namespace, name string) error {
	f.lastEvictedPodName = name
	f.lastEvictedPodNamespace = namespace
	return nil
}

func (f *fakeEvictor) PoolFull() error {
	f.fullCalled = true
	return nil
}

func (f *fakeEvictor) PoolNotFull() error {
	f.notFullCalled = true
	return nil
}

type fakeAllocator struct {
	nextErr error
	freeIPs int
}

func (f *fakeAllocator) Allocate(containerID, podName, podNamspace string) (*allocator.Allocation, error) {
	return &allocator.Allocation{}, f.nextErr
}

func (f *fakeAllocator) ReleaseByContainer(containerID string) error {
	return nil
}

func (f *fakeAllocator) FreeAddressCount() int {
	return f.freeIPs
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
	alloc.freeIPs = 0
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

	if !evict.fullCalled {
		t.Errorf("Expected Full to be called on evictor")
	}

	evict.reset()

	alloc.nextErr = nil
	alloc.freeIPs = 5

	_, err = svc.Add(context.Background(), &vpcnetpb.AddRequest{
		PodName:      "pod-name",
		PodNamespace: "pod-namespace",
	})
	if err != nil {
		t.Error("Unexpected error: [%+v]", err)
	}

	if evict.fullCalled {
		t.Errorf("Full should not have been called on evictor")
	}
	if !evict.notFullCalled {
		t.Errorf("NotFull should have been called on evictor")
	}

	evict.reset()

	alloc.nextErr = nil
	alloc.freeIPs = 0

	_, err = svc.Add(context.Background(), &vpcnetpb.AddRequest{
		PodName:      "pod-name",
		PodNamespace: "pod-namespace",
	})
	if err != nil {
		t.Error("Unexpected error: [%+v]", err)
	}

	if !evict.fullCalled {
		t.Errorf("Full should have been called on evictor")
	}
	if evict.notFullCalled {
		t.Errorf("NotFull should not have been called on evictor")
	}
}
