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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// p2Handle builds a MediaHandle for the given mode with a fixed loop device, as assembleMedia
// would hand to the p2 step, and the fake seam backing it.
func p2Handle(mode Mode, fp *fakePartitionOps) *MediaHandle {
	return &MediaHandle{
		ImagePath:  "/tmp/media.img",
		LoopDevice: "/dev/loop8",
		Plan:       partitionPlan(mode, defaultP1ReservationBytes),
		IsImage:    true,
		partition:  fp,
	}
}

func TestCreateP2Partition(t *testing.T) {
	// The p2 (ibi-images) entry has the same fixed number and reserved offset/size in both
	// modes, so createP2Partition drives the seam identically regardless of mode.
	for _, mode := range []Mode{ModeInstallOnly, ModeSelfContained} {
		t.Run(string(mode), func(t *testing.T) {
			fp := &fakePartitionOps{}
			h := p2Handle(mode, fp)
			wantP2, ok := plannedPartition(h.Plan, labelIBIImages)
			require.True(t, ok)

			require.NoError(t, createP2Partition(testLogger(), h, mediaBuildDeps{partition: fp}))

			// p2 runs the shared create -> settle -> format sequence and stops (no mount): it
			// ships an empty filesystem.
			assert.Equal(t, []string{"createPart", "settle", "format"}, fp.calls)

			// The planned ibi-images entry (fixed number 2) was the entry created, and it was
			// formatted on the settled node as ext4 labeled ibi-images.
			require.Len(t, fp.createdParts, 1)
			assert.Equal(t, wantP2, fp.createdParts[0])
			assert.Equal(t, 2, fp.createdParts[0].Number, "ibi-images is the fixed p2 entry")
			assert.Equal(t, labelIBIImages, fp.createdParts[0].Label)
			assert.Equal(t, "/dev/loop8p2", fp.formattedDev)
			assert.Equal(t, ibiImagesFSType, fp.formatFSType)
			assert.Equal(t, labelIBIImages, fp.formatLabel)
		})
	}
}

// TestCreateP2PartitionError covers the error path: a failure in the shared create/format
// sequence is surfaced (wrapped, naming the ibi-images partition) by createP2Partition so build
// can tear the handle down.
func TestCreateP2PartitionError(t *testing.T) {
	fp := &fakePartitionOps{formatErr: fmt.Errorf("mkfs boom")}
	h := p2Handle(ModeInstallOnly, fp)

	err := createP2Partition(testLogger(), h, mediaBuildDeps{partition: fp})
	require.Error(t, err)
	assert.ErrorContains(t, err, "mkfs boom")
	assert.ErrorContains(t, err, labelIBIImages)
}

// TestCreateP2PartitionMissingEntry covers the guard for a plan that has no ibi-images entry:
// createP2Partition errors without driving the seam.
func TestCreateP2PartitionMissingEntry(t *testing.T) {
	fp := &fakePartitionOps{}
	h := &MediaHandle{LoopDevice: "/dev/loop8", Plan: PartitionPlan{Mode: ModeInstallOnly}, partition: fp}

	err := createP2Partition(testLogger(), h, mediaBuildDeps{partition: fp})
	require.Error(t, err)
	assert.ErrorContains(t, err, labelIBIImages)
	assert.Empty(t, fp.calls, "the seam must not be driven when the partition is absent")
}

// TestIBIImagesFSType documents the settled default (ext4) so a change is a deliberate edit.
func TestIBIImagesFSType(t *testing.T) {
	assert.Equal(t, fsTypeExt4, ibiImagesFSType)
}
