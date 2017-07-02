package vpcnetstate

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
)

// ENIMapPath is the path on the machine we store the address mapping
const ENIMapPath = "/var/lib/cni/vpcnet/eni_map.json"

// WriteENIMap persists the map to disk
func WriteENIMap(em ENIMap) error {
	_ = os.MkdirAll(filepath.Dir(ENIMapPath), 0755)
	b, err := json.Marshal(em)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(ENIMapPath, b, 0644)
	if err != nil {
		return err
	}
	return nil
}

// ReadENIMap loads the map from disk
func ReadENIMap() (ENIMap, error) {
	b, err := ioutil.ReadFile(ENIMapPath)
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
