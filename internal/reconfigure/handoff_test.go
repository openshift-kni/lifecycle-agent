package reconfigure

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWriteHandoffOnce_WriteOnce proves the handoff is written once and a second write
// preserves the original anchor, mirroring the installation-configuration.service
// Restart=on-failure retry behaviour.
func TestWriteHandoffOnce_WriteOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lca", "reconfigure-handoff.json")

	first := &Handoff{
		Profile:            "self-contained",
		ConvergenceTimeout: "90m",
		PostPivotStartTime: "2026-01-01T00:00:00Z",
		SeedVersion:        "4.18.3",
	}
	require.NoError(t, WriteHandoffOnce(path, first))

	// A second run (e.g. a post-pivot restart) must not reset the anchor.
	second := &Handoff{
		Profile:            "self-contained",
		ConvergenceTimeout: "30m",
		PostPivotStartTime: "2026-06-01T12:00:00Z",
		SeedVersion:        "4.18.3",
	}
	require.NoError(t, WriteHandoffOnce(path, second))

	got, err := ReadHandoff(path)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, first.PostPivotStartTime, got.PostPivotStartTime)
	assert.Equal(t, first.ConvergenceTimeout, got.ConvergenceTimeout)

	// 0600 and no temp file left behind.
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

// TestReadHandoff proves absent is a clean nil, present round-trips, and malformed is an error
// the caller can fail safe on.
func TestReadHandoff(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		got, err := ReadHandoff(filepath.Join(t.TempDir(), "missing.json"))
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("present", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "handoff.json")
		require.NoError(t, WriteHandoffOnce(path, &Handoff{Profile: "self-contained", PostPivotStartTime: "2026-01-01T00:00:00Z"}))
		got, err := ReadHandoff(path)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "self-contained", got.Profile)
	})

	t.Run("malformed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "handoff.json")
		require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
		got, err := ReadHandoff(path)
		require.Error(t, err)
		assert.Nil(t, got)
	})
}
