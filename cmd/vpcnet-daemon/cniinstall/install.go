package cniinstall

import (
	"io"
	"io/ioutil"
	"os"
	"path/filepath"

	cniconfig "github.com/lstoll/k8s-vpcnet/pkg/cni/config"
	"github.com/lstoll/k8s-vpcnet/pkg/config"
	"github.com/pkg/errors"
)

// Installer is used to install and configure the CNI plugins
type Installer struct {
	// IPAMSocketPath is the path where the socket for the gRPC IPAM service
	// resides
	IPAMSocketPath string
	// Config is the configuration for this VPCNet environment
	Config *config.Config
}

// Configured returns true if CNI is already configured
func (c *Installer) Configured() (bool, error) {
	if _, err := os.Stat(cniconfig.CNIConfigPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, errors.Wrap(err, "Error checking CNI config path")
	}
	return true, nil
}

// Install copies the CNI plugins in to path, and writes out a configuration
func (c *Installer) Install() error {
	err := os.MkdirAll("/opt/cni/bin", 0755)
	if err != nil {
		return errors.Wrap(err, "Error creating /opt/cni/bin/")
	}
	// copy bins
	for _, f := range []string{"vpcnet", "loopback", "ptp"} {
		err := copyFile("/opt/cni/bin/"+f, "/cni/"+f, 0755)
		if err != nil {
			return errors.Wrap(err, "Error copying CNI bin "+f)
		}
	}

	err = cniconfig.WriteCNIConfig(c.Config, c.IPAMSocketPath)
	if err != nil {
		return errors.Wrap(err, "Error writing CNI configuration.")
	}

	return nil
}

// CopyFile copies the contents from src to dst atomically.
// If dst does not exist, CopyFile creates it with permissions perm.
// If the copy fails, CopyFile aborts and dst is preserved.
func copyFile(dst, src string, perm os.FileMode) error {
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
