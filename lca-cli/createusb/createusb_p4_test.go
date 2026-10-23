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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestP4RejectsVfat covers the fixed-filesystem-type invariant guard: any non-ext4 type (notably
// vfat) is rejected with a clear error that names vfat and the journaled-filesystem requirement,
// while the fixed ext4 constant is accepted. There is no operator-facing filesystem-type input, so
// this guards against a code regression rather than user input.
func TestP4RejectsVfat(t *testing.T) {
	t.Run("vfat is rejected with a clear error", func(t *testing.T) {
		err := validateIBIStatusFSType("vfat")
		require.Error(t, err)
		msg := err.Error()
		assert.Contains(t, msg, "vfat", "the error must name vfat")
		assert.Contains(t, strings.ToLower(msg), "journaled", "the error must state the journaled-filesystem requirement")
	})

	t.Run("any other non-ext4 type is rejected", func(t *testing.T) {
		require.Error(t, validateIBIStatusFSType("xfs"))
		require.Error(t, validateIBIStatusFSType(""))
	})

	t.Run("the fixed ext4 type is accepted", func(t *testing.T) {
		assert.NoError(t, validateIBIStatusFSType(ibiStatusFSType))
		assert.NoError(t, validateIBIStatusFSType("ext4"))
	})
}

// TestIBIStatusFSType documents the fixed ibi-status filesystem type (ext4) so a change is a
// deliberate edit.
func TestIBIStatusFSType(t *testing.T) {
	assert.Equal(t, fsTypeExt4, ibiStatusFSType)
}
