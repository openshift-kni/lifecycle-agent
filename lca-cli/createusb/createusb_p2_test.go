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
			wantP2, err := partitionByLabel(h.Plan, labelIBIImages)
			require.NoError(t, err)

			require.NoError(t, createP2Partition(testLogger(), h, mediaBuildDeps{partition: fp}))

			// Exactly one AddPartition call, driven with the planned ibi-images entry on the
			// handle's loop device and the ibi-images filesystem type.
			assert.Equal(t, []string{"add"}, fp.calls)
			require.Len(t, fp.added, 1)
			got := fp.added[0]
			assert.Equal(t, "/dev/loop8", got.dev)
			assert.Equal(t, wantP2, got.part)
			assert.Equal(t, 2, got.part.Number, "ibi-images is the fixed p2 entry")
			assert.Equal(t, labelIBIImages, got.part.Label)
			assert.Equal(t, ibiImagesFSType, got.fsType)
		})
	}
}

// TestCreateP2PartitionAddError covers the error path: an AddPartition failure is surfaced
// (wrapped) by createP2Partition so build can tear the handle down.
func TestCreateP2PartitionAddError(t *testing.T) {
	fp := &fakePartitionOps{addErr: fmt.Errorf("sgdisk boom")}
	h := p2Handle(ModeInstallOnly, fp)

	err := createP2Partition(testLogger(), h, mediaBuildDeps{partition: fp})
	require.Error(t, err)
	assert.ErrorContains(t, err, "sgdisk boom")
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

func TestPartitionByLabel(t *testing.T) {
	t.Run("returns the ibi-images entry for both modes", func(t *testing.T) {
		for _, mode := range []Mode{ModeInstallOnly, ModeSelfContained} {
			plan := partitionPlan(mode, defaultP1ReservationBytes)
			p2, err := partitionByLabel(plan, labelIBIImages)
			require.NoError(t, err, "mode %s", mode)
			assert.Equal(t, 2, p2.Number)
			assert.Equal(t, labelIBIImages, p2.Label)
			assert.Equal(t, p2ReservationBytes, p2.Size)
		}
	})

	t.Run("errors when the label is absent", func(t *testing.T) {
		// install-only omits cluster-config (p3), so it is not in the plan.
		_, err := partitionByLabel(partitionPlan(ModeInstallOnly, defaultP1ReservationBytes), labelClusterConfig)
		require.Error(t, err)
		assert.ErrorContains(t, err, labelClusterConfig)
	})
}

func TestPartitionDevice(t *testing.T) {
	assert.Equal(t, "/dev/loop0p2", partitionDevice("/dev/loop0", 2))
	assert.Equal(t, "/dev/loop12p4", partitionDevice("/dev/loop12", 4))
}

// TestMkfsCommand covers the per-filesystem formatter selection: ext4 and xfs use their own
// force flags (no cross-type reuse) and an unsupported type is a hard error.
func TestMkfsCommand(t *testing.T) {
	t.Run("ext4 uses -F", func(t *testing.T) {
		args, err := mkfsCommand(fsTypeExt4, labelIBIImages, "/dev/loop0p2")
		require.NoError(t, err)
		assert.Equal(t, []string{"mkfs.ext4", "-F", "-L", labelIBIImages, "/dev/loop0p2"}, args)
	})
	t.Run("xfs uses -f", func(t *testing.T) {
		args, err := mkfsCommand(fsTypeXFS, labelIBIImages, "/dev/loop0p2")
		require.NoError(t, err)
		assert.Equal(t, []string{"mkfs.xfs", "-f", "-L", labelIBIImages, "/dev/loop0p2"}, args)
	})
	t.Run("unknown type is a hard error", func(t *testing.T) {
		args, err := mkfsCommand("btrfs", labelIBIImages, "/dev/loop0p2")
		require.Error(t, err)
		assert.Nil(t, args)
		assert.ErrorContains(t, err, "unsupported filesystem type")
	})
}

// TestIBIImagesFSType documents the settled default (ext4) so a change is a deliberate edit.
func TestIBIImagesFSType(t *testing.T) {
	assert.Equal(t, fsTypeExt4, ibiImagesFSType)
}
