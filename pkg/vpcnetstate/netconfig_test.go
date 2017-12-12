package vpcnetstate

import "testing"

func TestReadENIMAP(t *testing.T) {
	m, err := ReadENIMap("testdata/eniMap.json")
	if err != nil {
		t.Fatalf("Unexpected error reading ENI Map [%+v]", err)
	}

	if len(m) != 2 {
		t.Errorf("Expected 2 ENIs, got %d", len(m))
	}
}
