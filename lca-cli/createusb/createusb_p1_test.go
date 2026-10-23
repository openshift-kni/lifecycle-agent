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
	"encoding/binary"
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
// entries (and, when sig is false, no boot signature) and returns its path. The entries carry only
// a type byte (zero start/length), so they occupy slots without reaching into the append region.
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

// writeMBRISOExtent writes a stand-in ISO of fileSize bytes whose single occupied primary declares
// the given start LBA and sector count, so its extent (start+length) can be driven past the first
// safe append offset to exercise the overlap guard.
func writeMBRISOExtent(t *testing.T, fileSize int, startLBA, sectorCount uint32) string {
	t.Helper()
	if fileSize < mbrSectorSize {
		fileSize = mbrSectorSize
	}
	b := make([]byte, fileSize)
	entry := b[mbrPartitionTableOffset:]
	entry[mbrPartitionTypeOffset] = 0x83
	binary.LittleEndian.PutUint32(entry[mbrPartitionStartLBAOffset:], startLBA)
	binary.LittleEndian.PutUint32(entry[mbrPartitionSectorCountOffset:], sectorCount)
	b[mbrSignatureOffset] = 0x55
	b[mbrSignatureOffset+1] = 0xAA
	return writeISOWithContent(t, b)
}

// mbrEntrySpec is one primary entry to stamp into a stand-in ISO's MBR: its type byte, start LBA,
// and sector count. A zero type byte with a nonzero start/count mirrors the isohybrid/ISO9660 slots
// that sfdisk still treats as occupied.
type mbrEntrySpec struct {
	typeCode    byte
	startLBA    uint32
	sectorCount uint32
}

// writeMBRISOEntries writes a stand-in ISO of fileSize bytes carrying the given primary entries (up
// to the four-slot MBR limit) and a valid boot signature, so occupancy and extent can be driven per
// entry (including zero-type-but-sized slots) to exercise the layout guards.
func writeMBRISOEntries(t *testing.T, fileSize int, entries []mbrEntrySpec) string {
	t.Helper()
	if fileSize < mbrSectorSize {
		fileSize = mbrSectorSize
	}
	b := make([]byte, fileSize)
	for i, e := range entries {
		if i >= mbrPartitionEntryCount {
			break
		}
		entry := b[mbrPartitionTableOffset+i*mbrPartitionEntrySize:]
		entry[mbrPartitionTypeOffset] = e.typeCode
		binary.LittleEndian.PutUint32(entry[mbrPartitionStartLBAOffset:], e.startLBA)
		binary.LittleEndian.PutUint32(entry[mbrPartitionSectorCountOffset:], e.sectorCount)
	}
	b[mbrSignatureOffset] = 0x55
	b[mbrSignatureOffset+1] = 0xAA
	return writeISOWithContent(t, b)
}

// TestInspectISOMBR covers MBR parsing: the used-primary count, the furthest partition extent, and
// the boot-signature and short-read guards.
func TestInspectISOMBR(t *testing.T) {
	t.Run("counts used primaries", func(t *testing.T) {
		layout, err := inspectISOMBR(writeMBRISO(t, 2, true))
		require.NoError(t, err)
		assert.Equal(t, 2, layout.usedPrimaries)
		assert.Zero(t, layout.maxEndByte, "type-only entries reach no extent")
	})
	t.Run("reports the furthest partition extent", func(t *testing.T) {
		// start=1 sector, length=10 sectors -> the partition ends at (1+10)*512 bytes.
		layout, err := inspectISOMBR(writeMBRISOExtent(t, mbrSectorSize, 1, 10))
		require.NoError(t, err)
		assert.Equal(t, 1, layout.usedPrimaries)
		assert.Equal(t, int64(11*mbrSectorSize), layout.maxEndByte)
	})
	t.Run("counts a zero-type entry with a nonzero sector count as occupied", func(t *testing.T) {
		// A zero type byte with a nonzero length is still a real partition to sfdisk, so it must be
		// counted and its extent measured rather than skipped.
		layout, err := inspectISOMBR(writeMBRISOEntries(t, mbrSectorSize, []mbrEntrySpec{
			{typeCode: 0x00, startLBA: 0, sectorCount: 10},
		}))
		require.NoError(t, err)
		assert.Equal(t, 1, layout.usedPrimaries)
		assert.Equal(t, int64(10*mbrSectorSize), layout.maxEndByte)
	})
	t.Run("missing boot signature is rejected", func(t *testing.T) {
		_, err := inspectISOMBR(writeMBRISO(t, 2, false))
		require.Error(t, err)
		assert.ErrorContains(t, err, "no MBR boot signature")
	})
	t.Run("short file is rejected", func(t *testing.T) {
		_, err := inspectISOMBR(writeISOWithContent(t, []byte("tiny")))
		require.Error(t, err)
		assert.ErrorContains(t, err, "MBR sector")
	})
}

// TestValidateISOLayout is the ISO-layout precondition: an ISO that leaves enough free primary
// slots and whose partitions stay before the append offset passes, and one that violates either
// invariant fails with a clear error before any modification.
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
	t.Run("a zero-type but sized entry still counts against the free-slot budget", func(t *testing.T) {
		// Two conventional primaries plus a zero-type entry that carries a nonzero sector count:
		// sfdisk sees three occupied slots, leaving only one free, so install-only (needs two) must
		// fail on the slot count. The sized entry stays well before the 1 MiB append offset so the
		// failure is the slot budget, not the extent guard.
		iso := writeMBRISOEntries(t, mbrSectorSize, []mbrEntrySpec{
			{typeCode: 0x83},
			{typeCode: 0x83},
			{typeCode: 0x00, startLBA: 0, sectorCount: 10},
		})
		err := validateISOLayout(testLogger(), iso, partitionPlan(ModeInstallOnly))
		require.Error(t, err)
		assert.ErrorContains(t, err, "does not leave enough free primary slots")
	})
	t.Run("a partition extending past the append offset fails", func(t *testing.T) {
		// A 512-byte ISO rounds up to a 1 MiB first-append offset; a partition spanning 2 MiB
		// (4096 sectors) reaches into the append region and must be rejected even though it
		// occupies only one slot.
		iso := writeMBRISOExtent(t, mbrSectorSize, 0, 4096)
		err := validateISOLayout(testLogger(), iso, partitionPlan(ModeInstallOnly))
		require.Error(t, err)
		assert.ErrorContains(t, err, "append region")
	})
}
