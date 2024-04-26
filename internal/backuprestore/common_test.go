package backuprestore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
)

func TestSetBackupLabelSelector(t *testing.T) {
	t.Run("set backup label selector", func(t *testing.T) {
		backup := fakeBackupCr("backupName", "1", "b")
		setBackupLabelSelector(backup)
		assert.Equal(t, backup.GetName(), backup.Spec.LabelSelector.MatchLabels[backupLabel])
	})
}

func TestSearchForLocalVolumes(t *testing.T) {
	handler := &BRHandler{}

	// Create Backup CRs for different scenarios
	t.Run("no local volumes found", func(t *testing.T) {
		backup := &velerov1.Backup{
			Spec: velerov1.BackupSpec{
				IncludedNamespaceScopedResources: []string{"deployment", "service"},
			},
		}
		found := handler.searchForLocalVolumes(backup)
		assert.False(t, found, "Expected no local volumes to be found")
	})
	t.Run("local volume found", func(t *testing.T) {
		handler := &BRHandler{}
		backup := &velerov1.Backup{
			Spec: velerov1.BackupSpec{
				IncludedNamespaceScopedResources: []string{"localvolume", "service"},
			},
		}
		found := handler.searchForLocalVolumes(backup)
		assert.True(t, found, "Expected local volume to be found")
	})
	t.Run("local volume with capital letters found", func(t *testing.T) {
		handler := &BRHandler{}
		backup := &velerov1.Backup{
			Spec: velerov1.BackupSpec{
				IncludedNamespaceScopedResources: []string{"LocalVolume", "service"},
			},
		}
		found := handler.searchForLocalVolumes(backup)
		assert.True(t, found, "Expected local volume to be found")
	})
	t.Run("local volume with defined group found", func(t *testing.T) {
		handler := &BRHandler{}
		backup := &velerov1.Backup{
			Spec: velerov1.BackupSpec{
				IncludedNamespaceScopedResources: []string{"localvolumes.storage.openshift.io", "service"},
			},
		}
		found := handler.searchForLocalVolumes(backup)
		assert.True(t, found, "Expected local volume to be found")
	})
}
