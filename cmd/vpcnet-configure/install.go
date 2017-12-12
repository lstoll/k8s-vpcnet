package main

import (
	"io"
	"io/ioutil"
	"os"
	"path/filepath"

	cniconfig "github.com/lstoll/k8s-vpcnet/pkg/cni/config"
	"github.com/lstoll/k8s-vpcnet/pkg/config"
	"github.com/pkg/errors"
)

func cniConfigured() (bool, error) {
	if _, err := os.Stat(cniconfig.CNIConfigPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, errors.Wrap(err, "Error checking CNI config path")
	}
	return true, nil
}

func installCNI(cfg *config.Config) error {
	err := os.MkdirAll("/opt/cni/bin", 0755)
	if err != nil {
		return errors.Wrap(err, "Error creating /opt/cni/bin/")
	}
	// copy bins
	for _, f := range []string{"vpcnet", "loopback"} {
		err := CopyFile("/opt/cni/bin/"+f, "/cni/"+f, 0755)
		if err != nil {
			return errors.Wrap(err, "Error copying CNI bin "+f)
		}
	}

	err = cniconfig.WriteCNIConfig(cfg)
	if err != nil {
		return errors.Wrap(err, "Error writing CNI configuration.")
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
