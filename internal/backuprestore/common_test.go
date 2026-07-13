package backuprestore

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractBackupSpecsFromConfigmaps(t *testing.T) {
	t.Run("empty configmaps returns empty specs", func(t *testing.T) {
		specs, err := ExtractBackupSpecsFromConfigmaps(nil)
		assert.NoError(t, err)
		assert.Empty(t, specs)
	})
}

func TestSortBackupSpecsByApplyWave(t *testing.T) {
	t.Run("empty specs returns nil", func(t *testing.T) {
		groups, err := SortBackupSpecsByApplyWave(nil)
		assert.NoError(t, err)
		assert.Nil(t, groups)
	})

	t.Run("specs are grouped and sorted by wave", func(t *testing.T) {
		specs := []BackupSpec{
			{Name: "b", ApplyWave: "2"},
			{Name: "a", ApplyWave: "1"},
			{Name: "c", ApplyWave: "1"},
			{Name: "d", ApplyWave: "2"},
		}
		groups, err := SortBackupSpecsByApplyWave(specs)
		assert.NoError(t, err)
		assert.Equal(t, 2, len(groups))
		assert.Equal(t, 2, len(groups[0]))
		assert.Equal(t, "a", groups[0][0].Name)
		assert.Equal(t, "c", groups[0][1].Name)
		assert.Equal(t, 2, len(groups[1]))
		assert.Equal(t, "b", groups[1][0].Name)
		assert.Equal(t, "d", groups[1][1].Name)
	})
}
