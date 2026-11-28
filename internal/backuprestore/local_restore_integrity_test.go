/*
Copyright 2026.

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
	"strings"
	"testing"

	"github.com/go-logr/logr"
	backupcontentv1 "github.com/openshift-kni/lifecycle-agent/api/backupcontent/v1"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// These tests exercise the post-pivot backup-integrity checks (P2) through the
// public StartRestore entry. P1 (missing/leftover backup directory) is covered by
// TestStartRestoreMissingBackup, TestStartRestoreLeftoverBackupDirNoContent and
// TestStartRestoreMissingBackupWithBackupContent; the P2 layout-manifest checks by
// TestLoadAndValidateBackupLayout and TestLoadGroupRestoreConfig; and P4 (real
// create/update, status-restore and CRD-establishment results) by
// TestRestoreResourceEntryStatusGating, TestRestoreResourceEntryStatusRestorationRetries
// and the TestCreateOrUpdateResource* tests.

// writeValidBackup writes a valid on-disk backup for the given BackupContent using
// StartBackup, returning the backup root directory.
func writeValidBackup(t *testing.T) string {
	t.Helper()
	ns := newUnstructuredWithLabels("Namespace", "app-ns", "", nil)
	cm := newUnstructuredWithLabels("ConfigMap", "cm", "app-ns", nil)
	bc := &backupcontentv1.BackupContent{
		ObjectMeta: metav1.ObjectMeta{Name: "bc", Namespace: "lca-ns"},
		Spec: backupcontentv1.BackupContentSpec{
			Namespaces:          []string{"app-ns"},
			NamespacedResources: []backupcontentv1.ResourceSelector{{Kind: "configmaps"}},
		},
	}
	h, tmp := startBackupTestHandler(t, "lca-ns", []runtime.Object{ns, cm}, bc)
	_, err := h.StartBackup(context.Background(), nil, []ibuv1.BackupContentRef{{Name: "bc"}})
	require.NoError(t, err)
	return filepath.Join(tmp, LocalBackupPath)
}

// TestStartRestoreCorruptedChecksumFailsTerminally verifies P2: a backup file whose
// content no longer matches its recorded checksum fails the restore terminally
// before any resource is applied, so a tampered or partially-written backup is
// never restored.
func TestStartRestoreCorruptedChecksumFailsTerminally(t *testing.T) {
	root := writeValidBackup(t)

	// Tamper a checksummed resource file so its recomputed hash diverges from
	// checksums.sha256.
	var tampered string
	require.NoError(t, filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if tampered != "" || info.IsDir() {
			return nil
		}
		name := info.Name()
		if strings.HasSuffix(name, ".yaml") && name != restoreConfigFileName {
			tampered = path
		}
		return nil
	}))
	require.NotEmpty(t, tampered, "expected at least one resource file in the backup")
	require.NoError(t, os.WriteFile(tampered, []byte("tampered: true\n"), 0o600))

	// hostPath is still set to the temp root by writeValidBackup's handler setup.
	h := &BRHandler{Log: logr.Discard()}
	rt, err := h.StartRestore(context.Background(), nil, []ibuv1.BackupContentRef{{Name: "bc"}})
	assert.Error(t, err)
	assert.True(t, IsBRFailedError(err), "a checksum mismatch must fail the restore terminally, got: %v", err)
	assert.Contains(t, err.Error(), "checksum")
	assert.Empty(t, rt.SucceededRestores)
}

// TestValidateChecksumsDetectsTampering verifies the P2 checksum check directly: a
// missing file and a modified file are both rejected.
func TestValidateChecksumsDetectsTampering(t *testing.T) {
	t.Run("modified file is rejected", func(t *testing.T) {
		root := writeValidBackup(t)
		// Corrupt the layout manifest, which is covered by checksums.sha256.
		require.NoError(t, os.WriteFile(filepath.Join(root, backupLayoutFileName), []byte("broken"), 0o600))
		_, err := validateChecksums(root)
		assert.Error(t, err)
	})

	t.Run("deleted file is rejected", func(t *testing.T) {
		root := writeValidBackup(t)
		require.NoError(t, os.Remove(filepath.Join(root, backupLayoutFileName)))
		_, err := validateChecksums(root)
		assert.Error(t, err)
	})
}
