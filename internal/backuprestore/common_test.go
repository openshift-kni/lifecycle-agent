package backuprestore

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSetBackupLabelSelector(t *testing.T) {
	t.Run("set backup label selector", func(t *testing.T) {
		backup := fakeBackupCr("backupName", "1", "b")
		setBackupLabelSelector(backup)
		assert.Equal(t, backup.GetName(), backup.Spec.LabelSelector.MatchLabels[backupLabel])
	})
}
