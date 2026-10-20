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
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// p4Handle builds a MediaHandle for the given mode with a fixed loop device, as assembleMedia
// would hand to the p4 step, and the fake seam backing it.
func p4Handle(mode Mode, fp *fakePartitionOps) *MediaHandle {
	return &MediaHandle{
		ImagePath:  "/tmp/media.img",
		LoopDevice: "/dev/loop8",
		Plan:       partitionPlan(mode, defaultP1ReservationBytes),
		IsImage:    true,
		partition:  fp,
	}
}

// TestCreateP4Partition covers the happy path over both modes: p4 (ibi-status) has the same fixed
// number and reserved offset/size in install-only (p1/p2/p4) and self-contained (p1/p2/p3/p4), so
// createP4Partition drives the shared create -> settle -> format sequence identically, formats the
// resolved node ext4 with the ibi-status label, and stops -- no mount, no write, no read-only op
// (p4 ships empty, writable, and unmounted).
func TestCreateP4Partition(t *testing.T) {
	for _, mode := range []Mode{ModeInstallOnly, ModeSelfContained} {
		t.Run(string(mode), func(t *testing.T) {
			fp := &fakePartitionOps{}
			h := p4Handle(mode, fp)
			wantP4, ok := plannedPartition(h.Plan, labelIBIStatus)
			require.True(t, ok)

			require.NoError(t, createP4Partition(testLogger(), h, mediaBuildDeps{partition: fp}))

			// p4 runs the shared create -> settle -> format sequence and stops (no mount): it ships
			// an empty filesystem.
			assert.Equal(t, []string{"createPart", "settle", "format"}, fp.calls)

			// The planned ibi-status entry (fixed number 4) was the entry created, and it was
			// formatted on the settled node as ext4 labeled ibi-status.
			require.Len(t, fp.createdParts, 1)
			assert.Equal(t, wantP4, fp.createdParts[0])
			assert.Equal(t, 4, fp.createdParts[0].Number, "ibi-status is the fixed p4 entry")
			assert.Equal(t, labelIBIStatus, fp.createdParts[0].Label)
			assert.Equal(t, p4ReservationBytes, fp.createdParts[0].Size)
			assert.Equal(t, "/dev/loop8p4", fp.formattedDev)
			assert.Equal(t, ibiStatusFSType, fp.formatFSType)
			assert.Equal(t, labelIBIStatus, fp.formatLabel)
		})
	}
}

// TestCreateP4PartitionError covers the error path: a failure in the shared create/format sequence
// is surfaced (wrapped, naming the ibi-status partition) by createP4Partition so build can tear the
// handle down.
func TestCreateP4PartitionError(t *testing.T) {
	fp := &fakePartitionOps{formatErr: fmt.Errorf("mkfs boom")}
	h := p4Handle(ModeInstallOnly, fp)

	err := createP4Partition(testLogger(), h, mediaBuildDeps{partition: fp})
	require.Error(t, err)
	assert.ErrorContains(t, err, "mkfs boom")
	assert.ErrorContains(t, err, labelIBIStatus)
}

// TestCreateP4PartitionMissingEntry covers the guard for a plan that has no ibi-status entry:
// createP4Partition errors without driving the seam rather than silently skipping p4.
func TestCreateP4PartitionMissingEntry(t *testing.T) {
	fp := &fakePartitionOps{}
	h := &MediaHandle{LoopDevice: "/dev/loop8", Plan: PartitionPlan{Mode: ModeInstallOnly}, partition: fp}

	err := createP4Partition(testLogger(), h, mediaBuildDeps{partition: fp})
	require.Error(t, err)
	assert.ErrorContains(t, err, labelIBIStatus)
	assert.Empty(t, fp.calls, "the seam must not be driven when the partition is absent")
}

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

// TestIBIStatusFSType documents the fixed p4 filesystem type (ext4) so a change is a deliberate
// edit.
func TestIBIStatusFSType(t *testing.T) {
	assert.Equal(t, fsTypeExt4, ibiStatusFSType)
}
