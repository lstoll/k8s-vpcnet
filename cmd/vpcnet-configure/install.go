package main

import (
	"io"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/pkg/errors"
)

const cniJSON = `{
   "cniVersion": "0.3.1",
   "name": "vpcbr0",
   "type": "bridge",
   "bridge": "vpcbr0",
   "hairpinMode": true,
    "ipam": {
     "type": "vpcnet"
   }
 }`

func installCNI() error {
	// copy bins
	for _, f := range []string{"vpcnet", "loopback", "bridge"} {
		err := CopyFile("/opt/cni/bin/"+f, "/cni/"+f, 0755)
		if err != nil {
			return errors.Wrap(err, "Error copying CNI bin "+f)
		}
	}

	err := ioutil.WriteFile("/etc/cni/net.d/10-vpcnet.conf", []byte(cniJSON), 0644)
	if err != nil {
		return errors.Wrap(err, "Error writing CNI configuration JSON")
	}

	return nil
}

// CopyFile copies the contents from src to dst atomically.
// If dst does not exist, CopyFile creates it with permissions perm.
// If the copy fails, CopyFile aborts and dst is preserved.
func CopyFile(dst, src string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := ioutil.TempFile(filepath.Dir(dst), "")
	if err != nil {
		return err
	}
	_, err = io.Copy(tmp, in)
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err = tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err = os.Chmod(tmp.Name(), perm); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
