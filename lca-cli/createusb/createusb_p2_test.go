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

package createusb

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIBIImagesFSType documents the settled default (ext4) so a change is a deliberate edit. The
// ibi-images partition is appended after the live ISO and formatted by the shared format stage
// (formatDataPartitions); its filesystem type is the only policy this file owns.
func TestIBIImagesFSType(t *testing.T) {
	assert.Equal(t, fsTypeExt4, ibiImagesFSType)
}

// TestStageLCABinary covers staging the shipping lca-cli binary onto the (mounted) ibi-images root:
// it is copied to <p2Root>/lca-cli with an executable mode, and a missing source fails loudly.
func TestStageLCABinary(t *testing.T) {
	t.Run("copies the binary with an executable mode", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "lca-cli")
		require.NoError(t, os.WriteFile(src, []byte("binary-bytes"), 0o600))
		p2Root := t.TempDir()

		require.NoError(t, stageLCABinary(testLogger(), src, p2Root))

		dst := filepath.Join(p2Root, "lca-cli")
		got, err := os.ReadFile(dst)
		require.NoError(t, err)
		assert.Equal(t, "binary-bytes", string(got), "the binary is copied byte-for-byte")
		fi, err := os.Stat(dst)
		require.NoError(t, err)
		assert.NotZero(t, fi.Mode()&0o111, "the staged binary must be executable so init_t can exec it")
	})

	t.Run("missing extracted binary fails loudly", func(t *testing.T) {
		err := stageLCABinary(testLogger(), "", t.TempDir())
		require.Error(t, err)
		assert.ErrorContains(t, err, "did not return an extracted lca-cli binary")
	})
}
