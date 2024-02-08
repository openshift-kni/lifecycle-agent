/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package clusterconfig

import (
	"context"
	"os"
	"path/filepath"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	cp "github.com/otiai10/copy"
)

func (r *UpgradeClusterConfigGather) FetchLvmConfig(ctx context.Context, ostreeDir string) error {
	r.Log.Info("Fetching node lvm files")
	lvmConfigPath := filepath.Join(ostreeDir, common.OptOpenshift, common.LvmConfigDir)
	if err := os.MkdirAll(lvmConfigPath, 0o700); err != nil {
		return err
	}

	r.Log.Info("Copying lvm devices file", "file", common.LvmDevicesPath, "to", lvmConfigPath)
	err := cp.Copy(
		filepath.Join(hostPath, common.LvmDevicesPath),
		filepath.Join(lvmConfigPath, filepath.Base(common.LvmDevicesPath)))
	if err != nil {
		if os.IsNotExist(err) {
			r.Log.Info("lvm devices file does not exist")
			return nil
		}
		return err
	}
	r.Log.Info("Done fetching node lvm files")
	return nil
}
