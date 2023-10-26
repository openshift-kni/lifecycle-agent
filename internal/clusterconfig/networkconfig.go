package clusterconfig

import (
	"context"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
	cp "github.com/otiai10/copy"
)

const networkDir = "/opt/openshift/network-configuration"

// UpgradeNetworkConfigGather Gather network config files from host
type UpgradeNetworkConfigGather struct {
	Log logr.Logger
}

var listOfPaths = []string{
	"/host/etc/hostname",
	"/host/etc/NetworkManager/system-connections",
	"/host/var/run/nodeip-configuration/primary-ip"}

// FetchNetworkConfig gather network files and copy them
func (r *UpgradeNetworkConfigGather) FetchNetworkConfig(ctx context.Context, ostreeDir string) error {
	r.Log.Info("Fetching node network files")
	dir, err := r.configDir(ostreeDir)
	if err != nil {
		return err
	}

	for _, path := range listOfPaths {
		r.Log.Info("Copying network files", "file", path, "to", dir)
		err = cp.Copy(path, filepath.Join(dir, filepath.Base(path)))
		if err != nil {
			return err
		}
	}
	r.Log.Info("Done fetching node network files")
	return nil
}

// configDir returns the files directory for the given cluster config

func (r *UpgradeNetworkConfigGather) configDir(dir string) (string, error) {
	filesDir := filepath.Join(dir, networkDir)
	if err := os.MkdirAll(filesDir, 0700); err != nil {
		return "", err
	}
	return filesDir, nil
}
