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
	"testing"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/stretchr/testify/assert"
)

func TestFetchLvmConfig(t *testing.T) {
	testcases := []struct {
		name               string
		lvmFilesToCopy     []string
		expectedErr        bool
		expectedNumOfFiles int
	}{
		{
			name:               "Lvm devices file exists",
			lvmFilesToCopy:     []string{common.LvmDevicesPath},
			expectedErr:        false,
			expectedNumOfFiles: 1,
		},
		{
			name:               "lvm devices file does not exist",
			lvmFilesToCopy:     []string{},
			expectedErr:        false,
			expectedNumOfFiles: 0,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()

			// Create the original files
			hostPath = filepath.Join(tmpDir, "host")
			for _, file := range tc.lvmFilesToCopy {
				lvmFilePath := filepath.Join(hostPath, file)
				if err := os.MkdirAll(filepath.Dir(lvmFilePath), 0o700); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				file, err := os.Create(lvmFilePath)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				file.Close()
			}

			ucc := &UpgradeClusterConfigGather{
				Log: logr.Discard(),
			}

			err := ucc.FetchLvmConfig(context.Background(), tmpDir)
			if err != nil {
				if tc.expectedErr {
					assert.Error(t, err)
				} else {
					t.Errorf("unexpected error: %v", err)
				}
			}

			// Verify that the lvm devices file was copied
			if len(tc.lvmFilesToCopy) != 0 {
				for _, file := range tc.lvmFilesToCopy {
					lvmConfigPath := filepath.Join(tmpDir, common.OptOpenshift, common.LvmConfigDir)
					lvmDevicesPath := filepath.Join(lvmConfigPath, filepath.Base(file))
					_, err := os.Stat(lvmDevicesPath)
					assert.NoError(t, err)
				}
			}
			assert.Equal(t, tc.expectedNumOfFiles, len(tc.lvmFilesToCopy))
		})
	}
}
