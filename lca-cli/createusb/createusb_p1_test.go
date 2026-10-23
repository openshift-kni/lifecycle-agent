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
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeISOWithContent writes content to a temp ISO file and returns its path.
func writeISOWithContent(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "live.iso")
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return path
}

// TestWriteLiveISOToFront covers the happy path: the ISO is copied byte-for-byte to the front of a
// freshly created image (preserving its DOS/MBR table) and the image is grown to the full media
// size.
func TestWriteLiveISOToFront(t *testing.T) {
	content := []byte("isohybrid-mbr-and-content")
	iso := writeISOWithContent(t, content)
	img := filepath.Join(t.TempDir(), "out.img")
	const total = int64(1) << 20

	require.NoError(t, writeLiveISOToFront(img, iso, total))

	fi, err := os.Stat(img)
	require.NoError(t, err)
	assert.Equal(t, total, fi.Size(), "the image is grown to the full media size")

	got, err := os.ReadFile(img)
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(got, content), "the ISO bytes are preserved at the front (LBA 0)")
}

// TestWriteLiveISOToFrontRejectsOversizeISO covers the guard: an ISO larger than the planned media
// size fails before any image is created.
func TestWriteLiveISOToFrontRejectsOversizeISO(t *testing.T) {
	iso := writeISOWithContent(t, make([]byte, 2048))
	img := filepath.Join(t.TempDir(), "out.img")

	err := writeLiveISOToFront(img, iso, 1024)
	require.ErrorContains(t, err, "larger than the planned media size")
	_, statErr := os.Stat(img)
	assert.True(t, os.IsNotExist(statErr), "no image is created when the ISO does not fit")
}

// TestWriteLiveISOToFrontRefusesOverwrite covers the destructive-overwrite guard: an existing
// output path is not overwritten.
func TestWriteLiveISOToFrontRefusesOverwrite(t *testing.T) {
	iso := writeISOWithContent(t, []byte("iso"))
	img := filepath.Join(t.TempDir(), "out.img")
	const existing = "pre-existing"
	require.NoError(t, os.WriteFile(img, []byte(existing), 0o600))

	err := writeLiveISOToFront(img, iso, 1<<20)
	require.ErrorContains(t, err, "already exists")
	got, readErr := os.ReadFile(img)
	require.NoError(t, readErr)
	assert.Equal(t, existing, string(got), "an existing output file must not be modified")
}

// TestWriteLiveISOToFrontMissingISO covers the guard for a missing source ISO.
func TestWriteLiveISOToFrontMissingISO(t *testing.T) {
	img := filepath.Join(t.TempDir(), "out.img")
	err := writeLiveISOToFront(img, filepath.Join(t.TempDir(), "nope.iso"), 1<<20)
	require.Error(t, err)
	assert.ErrorContains(t, err, "opening live ISO")
}

// writeMBRISO writes a stand-in ISO with a valid MBR carrying `used` non-empty primary partition
// entries (and, when sig is false, no boot signature) and returns its path.
func writeMBRISO(t *testing.T, used int, sig bool) string {
	t.Helper()
	b := make([]byte, mbrSectorSize)
	for i := 0; i < used && i < mbrPartitionEntryCount; i++ {
		b[mbrPartitionTableOffset+i*mbrPartitionEntrySize+mbrPartitionTypeOffset] = 0x83
	}
	if sig {
		b[mbrSignatureOffset] = 0x55
		b[mbrSignatureOffset+1] = 0xAA
	}
	return writeISOWithContent(t, b)
}

// TestCountISOPrimaryPartitions covers MBR parsing: the used primary count and the boot-signature
// and short-read guards.
func TestCountISOPrimaryPartitions(t *testing.T) {
	t.Run("counts used primaries", func(t *testing.T) {
		n, err := countISOPrimaryPartitions(writeMBRISO(t, 2, true))
		require.NoError(t, err)
		assert.Equal(t, 2, n)
	})
	t.Run("missing boot signature is rejected", func(t *testing.T) {
		_, err := countISOPrimaryPartitions(writeMBRISO(t, 2, false))
		require.Error(t, err)
		assert.ErrorContains(t, err, "no MBR boot signature")
	})
	t.Run("short file is rejected", func(t *testing.T) {
		_, err := countISOPrimaryPartitions(writeISOWithContent(t, []byte("tiny")))
		require.Error(t, err)
		assert.ErrorContains(t, err, "MBR sector")
	})
}

// TestValidateISOLayout is the ISO-layout precondition: an ISO that leaves enough free primary
// slots passes, and one that does not fails with a clear error before any modification.
func TestValidateISOLayout(t *testing.T) {
	t.Run("two used primaries leaves enough for either mode", func(t *testing.T) {
		iso := writeMBRISO(t, 2, true)
		require.NoError(t, validateISOLayout(testLogger(), iso, partitionPlan(ModeInstallOnly)))
		require.NoError(t, validateISOLayout(testLogger(), iso, partitionPlan(ModeSelfContained)))
	})
	t.Run("three used primaries does not leave enough and fails", func(t *testing.T) {
		iso := writeMBRISO(t, 3, true)
		err := validateISOLayout(testLogger(), iso, partitionPlan(ModeInstallOnly))
		require.Error(t, err)
		assert.ErrorContains(t, err, "does not leave enough free primary slots")
	})
	t.Run("all four used fails", func(t *testing.T) {
		iso := writeMBRISO(t, 4, true)
		err := validateISOLayout(testLogger(), iso, partitionPlan(ModeSelfContained))
		require.Error(t, err)
		assert.ErrorContains(t, err, "does not leave enough free primary slots")
	})
}
