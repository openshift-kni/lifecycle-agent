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
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStartRestoreMissingBackup verifies that a missing backup directory is
// only tolerated when no backup content was configured; when content was
// configured but no backup is on disk the restore must fail hard.
func TestStartRestoreMissingBackup(t *testing.T) {
	tmpDir := t.TempDir()
	origHostPath := hostPath
	hostPath = tmpDir
	defer func() { hostPath = origHostPath }()

	h := &BRHandler{Log: logr.Discard()}

	t.Run("no content configured and no backup on disk skips restore", func(t *testing.T) {
		rt, err := h.StartRestore(context.Background(), nil)
		assert.NoError(t, err)
		assert.NotNil(t, rt)
		assert.Empty(t, rt.FailedRestores)
	})

	t.Run("content configured but no backup on disk fails hard", func(t *testing.T) {
		rt, err := h.StartRestore(context.Background(), []ibuv1.ConfigMapRef{{Name: "backup", Namespace: "ns"}})
		assert.Error(t, err)
		assert.True(t, IsBRFailedError(err))
		assert.NotNil(t, rt)
	})
}

// TestStartRestoreCompletedMarker verifies that when the backup directory exists
// and already carries the restore-completed marker, StartRestore skips resource
// restoration and succeeds without touching the backup contents (idempotent
// re-entry after a successful restore).
func TestStartRestoreCompletedMarker(t *testing.T) {
	tmpDir := t.TempDir()
	origHostPath := hostPath
	hostPath = tmpDir
	defer func() { hostPath = origHostPath }()

	backupPath := filepath.Join(tmpDir, LocalBackupPath)
	require.NoError(t, os.MkdirAll(backupPath, 0o700))
	// Deliberately leave an invalid backup (no checksums.sha256). If the marker is
	// honored the restore short-circuits before checksum validation would fail.
	require.NoError(t, os.WriteFile(filepath.Join(backupPath, restoreCompletedMarker), nil, 0o600))

	h := &BRHandler{Log: logr.Discard()}

	rt, err := h.StartRestore(context.Background(), []ibuv1.ConfigMapRef{{Name: "backup", Namespace: "ns"}})
	assert.NoError(t, err)
	assert.NotNil(t, rt)
	assert.Empty(t, rt.SucceededRestores)
	assert.Empty(t, rt.FailedRestores)
}

// TestMarkRestoreCompleted verifies the marker is created atomically inside the
// backup directory, is idempotent across repeated calls, and that a subsequent
// StartRestore then skips restoration.
func TestMarkRestoreCompleted(t *testing.T) {
	tmpDir := t.TempDir()
	origHostPath := hostPath
	hostPath = tmpDir
	defer func() { hostPath = origHostPath }()

	backupPath := filepath.Join(tmpDir, LocalBackupPath)
	require.NoError(t, os.MkdirAll(backupPath, 0o700))

	h := &BRHandler{Log: logr.Discard()}

	markerPath := filepath.Join(backupPath, restoreCompletedMarker)

	require.NoError(t, h.MarkRestoreCompleted(context.Background()))
	info, err := os.Stat(markerPath)
	require.NoError(t, err)
	assert.False(t, info.IsDir())
	assert.Zero(t, info.Size())

	// No leftover temp files should remain in the backup directory.
	entries, err := os.ReadDir(backupPath)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	assert.Equal(t, restoreCompletedMarker, entries[0].Name())

	// Calling again must succeed (idempotent).
	require.NoError(t, h.MarkRestoreCompleted(context.Background()))

	// With the marker in place, StartRestore skips restoration successfully.
	rt, err := h.StartRestore(context.Background(), []ibuv1.ConfigMapRef{{Name: "backup", Namespace: "ns"}})
	assert.NoError(t, err)
	assert.Empty(t, rt.SucceededRestores)
}
