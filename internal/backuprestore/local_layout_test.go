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

package backuprestore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsAcceptedLayoutVersion verifies the semver acceptance rule: the current
// layout version is accepted, while malformed, empty, different-major and
// future (newer-than-current) versions are rejected.
func TestIsAcceptedLayoutVersion(t *testing.T) {
	testcases := []struct {
		name     string
		version  string
		accepted bool
	}{
		{name: "current version is accepted", version: currentLayoutVersion, accepted: true},
		{name: "empty string is rejected", version: "", accepted: false},
		{name: "non-semver is rejected", version: "not-a-semver", accepted: false},
		{name: "older major is rejected", version: "0.9.0", accepted: false},
		{name: "future major is rejected", version: "2.0.0", accepted: false},
		{name: "future minor is rejected", version: "1.1.0", accepted: false},
		{name: "future patch is rejected", version: "1.0.1", accepted: false},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.accepted, isAcceptedLayoutVersion(tc.version))
		})
	}
}

// TestWriteBackupLayoutManifest verifies the manifest is written with the
// expected metadata, its timestamp is RFC3339, and its returned checksum line
// matches the written file and uses the root-relative file name.
func TestWriteBackupLayoutManifest(t *testing.T) {
	dir := t.TempDir()

	checksumLine, err := writeBackupLayoutManifest(dir, 7, []string{"b1", "b2"})
	require.NoError(t, err)

	// The checksum line is "<hash>  <relPath>" and uses the root-level file name.
	assert.Contains(t, checksumLine, "  "+backupLayoutFileName)

	data, err := os.ReadFile(filepath.Join(dir, backupLayoutFileName))
	require.NoError(t, err)

	manifest := &backupLayoutManifest{}
	require.NoError(t, json.Unmarshal(data, manifest))
	assert.Equal(t, currentLayoutVersion, manifest.LayoutVersion)
	assert.Equal(t, version.Version, manifest.LCAVersion)
	assert.Equal(t, 7, manifest.ResourceCount)
	assert.Equal(t, []string{"b1", "b2"}, manifest.BackupSpecNames)
	_, perr := time.Parse(time.RFC3339, manifest.Timestamp)
	assert.NoError(t, perr, "timestamp must be RFC3339 parseable")

	// The manifest written round-trips through the restore-side validation.
	validated := map[string]bool{backupLayoutFileName: true}
	got, err := loadAndValidateBackupLayout(dir, validated, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, manifest, got)
}

// TestLoadAndValidateBackupLayout verifies the restore-side gate rejects a
// missing, unlisted, malformed, or unknown/future-version manifest with a
// terminal BRFailedError and accepts a well-formed current-version manifest.
func TestLoadAndValidateBackupLayout(t *testing.T) {
	t.Run("missing manifest is rejected", func(t *testing.T) {
		dir := t.TempDir()
		_, err := loadAndValidateBackupLayout(dir, map[string]bool{}, logr.Discard())
		require.Error(t, err)
		assert.True(t, IsBRFailedError(err))
		assert.Contains(t, err.Error(), "required but missing")
	})

	t.Run("manifest not in checksum manifest is rejected", func(t *testing.T) {
		dir := t.TempDir()
		_, err := writeBackupLayoutManifest(dir, 1, []string{"b1"})
		require.NoError(t, err)
		// The file exists on disk but is absent from the validated set.
		_, err = loadAndValidateBackupLayout(dir, map[string]bool{}, logr.Discard())
		require.Error(t, err)
		assert.True(t, IsBRFailedError(err))
		assert.Contains(t, err.Error(), "not listed in the checksum manifest")
	})

	t.Run("malformed manifest is rejected", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, backupLayoutFileName), []byte("{not json"), 0o600))
		_, err := loadAndValidateBackupLayout(dir, map[string]bool{backupLayoutFileName: true}, logr.Discard())
		require.Error(t, err)
		assert.True(t, IsBRFailedError(err))
		assert.Contains(t, err.Error(), "malformed")
	})

	t.Run("unknown/future layout version is rejected", func(t *testing.T) {
		dir := t.TempDir()
		data, err := json.Marshal(backupLayoutManifest{
			LayoutVersion:   "2.0.0",
			LCAVersion:      "test",
			Timestamp:       time.Now().UTC().Format(time.RFC3339),
			ResourceCount:   1,
			BackupSpecNames: []string{"b1"},
		})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, backupLayoutFileName), data, 0o600))
		_, err = loadAndValidateBackupLayout(dir, map[string]bool{backupLayoutFileName: true}, logr.Discard())
		require.Error(t, err)
		assert.True(t, IsBRFailedError(err))
		assert.Contains(t, err.Error(), "not supported")
	})

	t.Run("well-formed current-version manifest is accepted", func(t *testing.T) {
		dir := t.TempDir()
		_, err := writeBackupLayoutManifest(dir, 2, []string{"b1"})
		require.NoError(t, err)
		m, err := loadAndValidateBackupLayout(dir, map[string]bool{backupLayoutFileName: true}, logr.Discard())
		require.NoError(t, err)
		assert.Equal(t, currentLayoutVersion, m.LayoutVersion)
		assert.Equal(t, 2, m.ResourceCount)
	})
}
