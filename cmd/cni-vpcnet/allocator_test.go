package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"testing"

	"github.com/lstoll/k8s-vpcnet/pkg/cni/config"
	"github.com/lstoll/k8s-vpcnet/pkg/cni/diskstore"
)

func TestAllocator(t *testing.T) {
	workDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(workDir)

	store, err := diskstore.New("test-store", workDir)
	if err != nil {
		t.Fatalf("Error opening disk store [%+v]", err)
	}
	defer store.Close()

	alloc := &ipAllocator{
		name:   "tester",
		eniMap: testMap,
		store:  store,
		conf:   &config.IPAM{},
	}

	// try allocating and deallocating max addresses

	for i := 0; i < 6; i++ {
		_, _, err := alloc.Get(fmt.Sprintf("abc%d", i))
		if err != nil {
			t.Fatalf("Error getting address %d [%+v]", i, err)
		}
	}

	_, _, err = alloc.Get("abc7")
	if err == nil {
		t.Fatal("Expected error allocating 7th address")
	}

	for i := 0; i < 6; i++ {
		_, err := alloc.Release(fmt.Sprintf("abc%d", i))
		if err != nil {
			t.Fatalf("Error getting address %d [%+v]", i, err)
		}
	}
}
