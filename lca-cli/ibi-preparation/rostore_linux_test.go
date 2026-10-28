package ibi_preparation

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExchangeStoreDirsSwapsAtomically proves the default exchange helper swaps two directories in
// place via renameat2(RENAME_EXCHANGE): each path ends up holding the other's content and both paths
// exist throughout. This is the primitive the atomic publish relies on to never leave the configured
// store path absent. It exercises the real Linux-only syscall, so it lives in a Linux-only test file.
func TestExchangeStoreDirsSwapsAtomically(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	require.NoError(t, os.MkdirAll(a, 0o755))
	require.NoError(t, os.MkdirAll(b, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(a, "marker"), []byte("A"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(b, "marker"), []byte("B"), 0o600))

	require.NoError(t, defaultExchangeStoreDirs(a, b))

	assert.DirExists(t, a)
	assert.DirExists(t, b)
	gotA, err := os.ReadFile(filepath.Join(a, "marker")) //nolint:gosec // test path under t.TempDir()
	require.NoError(t, err)
	gotB, err := os.ReadFile(filepath.Join(b, "marker")) //nolint:gosec // test path under t.TempDir()
	require.NoError(t, err)
	assert.Equal(t, "B", string(gotA), "a must hold b's content after the exchange")
	assert.Equal(t, "A", string(gotB), "b must hold a's content after the exchange")
}
