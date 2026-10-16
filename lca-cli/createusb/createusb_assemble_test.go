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
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testLogger returns a logrus logger whose output is discarded, for tests that pass a
// *logrus.Logger into the assembly functions.
func testLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return l
}

// fakePartitionOps is a partitionOps fake that records the ordered privileged-op calls and
// lets each op's result be injected, so the pure assembly logic (allocation, ordering,
// rollback) is unit-testable without root.
type fakePartitionOps struct {
	attachDev string
	attachErr error
	createErr error
	detachErr error
	readOut   []PartitionEntry
	readErr   error

	calls     []string
	attached  []string
	detached  []string
	createdOn []string
	lastPlan  PartitionPlan
}

func (f *fakePartitionOps) AttachLoopback(imgPath string) (string, error) {
	f.calls = append(f.calls, "attach")
	f.attached = append(f.attached, imgPath)
	if f.attachErr != nil {
		return "", f.attachErr
	}
	dev := f.attachDev
	if dev == "" {
		dev = "/dev/loopfake"
	}
	return dev, nil
}

func (f *fakePartitionOps) DetachLoopback(loopDev string) error {
	f.calls = append(f.calls, "detach")
	f.detached = append(f.detached, loopDev)
	return f.detachErr
}

func (f *fakePartitionOps) CreateBaseLayout(dev string, plan PartitionPlan) error {
	f.calls = append(f.calls, "create")
	f.createdOn = append(f.createdOn, dev)
	f.lastPlan = plan
	return f.createErr
}

func (f *fakePartitionOps) ReadPartitionTable(dev string) ([]PartitionEntry, error) {
	f.calls = append(f.calls, "read")
	return f.readOut, f.readErr
}

func TestPartitionPlanPerMode(t *testing.T) {
	const p1 = int64(3) << 30 // a fixed placeholder p1 size for deterministic offsets

	t.Run("install-only is p1+p2+p4", func(t *testing.T) {
		plan := partitionPlan(ModeInstallOnly, p1)
		labels := partitionLabels(plan)
		assert.Equal(t, []string{labelLiveISO, labelIBIImages, labelIBIStatus}, labels)
		// ibi-status keeps its fixed number 4 even though cluster-config (p3) is omitted:
		// the layout is p1/p2/p4, not a renumbered p1/p2/p3.
		assert.Equal(t, []int{1, 2, 4}, partitionNumbers(plan))
		assertPlanInvariants(t, plan, p1)
	})

	t.Run("self-contained is p1+p2+p3+p4", func(t *testing.T) {
		plan := partitionPlan(ModeSelfContained, p1)
		labels := partitionLabels(plan)
		assert.Equal(t, []string{labelLiveISO, labelIBIImages, labelClusterConfig, labelIBIStatus}, labels)
		assert.Equal(t, []int{1, 2, 3, 4}, partitionNumbers(plan))
		assertPlanInvariants(t, plan, p1)
	})
}

// assertPlanInvariants checks the layout contract: the placeholder p1 sized as requested,
// every start 1 MiB-aligned and non-overlapping in order, strictly increasing partition
// numbers (p1 first), the first partition past the front GPT reservation, and a container
// large enough to hold the last partition plus the tail backup-GPT reservation. The exact
// numbers per mode are asserted by TestPartitionPlanPerMode.
func assertPlanInvariants(t *testing.T, plan PartitionPlan, wantP1 int64) {
	t.Helper()
	require.NotEmpty(t, plan.Partitions)
	assert.Equal(t, wantP1, plan.Partitions[0].Size, "p1 must be the requested placeholder size")
	assert.Equal(t, 1, plan.Partitions[0].Number, "the first partition must be p1")
	assert.GreaterOrEqual(t, plan.Partitions[0].Offset, gptFrontReservedBytes, "p1 must start past the front GPT area")

	prevEnd := int64(0)
	prevNumber := 0
	for _, p := range plan.Partitions {
		assert.Greater(t, p.Number, prevNumber, "partition numbers must strictly increase in layout order")
		assert.Zero(t, p.Offset%alignmentBytes, "partition %d offset must be 1 MiB-aligned", p.Number)
		assert.GreaterOrEqual(t, p.Offset, prevEnd, "partition %d must not overlap the previous one", p.Number)
		assert.Positive(t, p.Size, "partition %d must reserve a positive size", p.Number)
		prevEnd = p.Offset + p.Size
		prevNumber = p.Number
	}

	last := plan.Partitions[len(plan.Partitions)-1]
	total := containerSize(plan)
	assert.GreaterOrEqual(t, total, last.Offset+last.Size+gptBackupReservedBytes,
		"container must hold the last partition plus the backup-GPT reservation")
	assert.Zero(t, total%alignmentBytes, "container size must be 1 MiB-aligned")
}

func partitionLabels(plan PartitionPlan) []string {
	labels := make([]string, 0, len(plan.Partitions))
	for _, p := range plan.Partitions {
		labels = append(labels, p.Label)
	}
	return labels
}

func partitionNumbers(plan PartitionPlan) []int {
	numbers := make([]int, 0, len(plan.Partitions))
	for _, p := range plan.Partitions {
		numbers = append(numbers, p.Number)
	}
	return numbers
}

func TestAssembleMediaFoundation(t *testing.T) {
	img := filepath.Join(t.TempDir(), "out.img")
	fp := &fakePartitionOps{attachDev: "/dev/loop9"}

	handle, err := assembleMedia(context.Background(), testLogger(), validInstallOnly(),
		BuildOptions{OutputPath: img}, mediaBuildDeps{partition: fp})
	require.NoError(t, err)
	require.NotNil(t, handle)

	// The handle's fields reflect the allocated container and computed plan.
	wantPlan := partitionPlan(ModeInstallOnly, defaultP1ReservationBytes)
	assert.Equal(t, img, handle.ImagePath)
	assert.Equal(t, "/dev/loop9", handle.LoopDevice)
	assert.True(t, handle.IsImage)
	assert.Equal(t, wantPlan, handle.Plan)

	// The seam was driven in order: attach the loopback, then create the base layout on it.
	assert.Equal(t, []string{"attach", "create"}, fp.calls)
	assert.Equal(t, []string{img}, fp.attached)
	assert.Equal(t, []string{"/dev/loop9"}, fp.createdOn)
	assert.Equal(t, wantPlan, fp.lastPlan)

	// The sparse image was allocated at the full planned container size, and success leaves
	// it in place with the loop still attached (the caller owns teardown).
	fi, statErr := os.Stat(img)
	require.NoError(t, statErr)
	assert.Equal(t, containerSize(wantPlan), fi.Size())
	assert.NotContains(t, fp.calls, "detach", "success must not tear the handle down")

	// Close is the caller's single teardown entry point: it detaches the loop and removes
	// the (discarded) image.
	require.NoError(t, handle.Close())
	assert.Equal(t, []string{"attach", "create", "detach"}, fp.calls)
	assert.Equal(t, []string{"/dev/loop9"}, fp.detached)
	_, statErr = os.Stat(img)
	assert.True(t, os.IsNotExist(statErr), "Close must remove the discarded image")
}

func TestAssembleMediaCleanupOnError(t *testing.T) {
	t.Run("attach failure removes the image and detaches nothing", func(t *testing.T) {
		img := filepath.Join(t.TempDir(), "out.img")
		fp := &fakePartitionOps{attachErr: fmt.Errorf("losetup boom")}

		handle, err := assembleMedia(context.Background(), testLogger(), validInstallOnly(),
			BuildOptions{OutputPath: img}, mediaBuildDeps{partition: fp})
		require.Error(t, err)
		assert.Nil(t, handle)

		assert.Equal(t, []string{"attach"}, fp.calls, "no detach when nothing was attached")
		assert.Empty(t, fp.detached)
		_, statErr := os.Stat(img)
		assert.True(t, os.IsNotExist(statErr), "the partial image must be removed on failure")
	})

	t.Run("create-layout failure detaches the loop and removes the image", func(t *testing.T) {
		img := filepath.Join(t.TempDir(), "out.img")
		fp := &fakePartitionOps{attachDev: "/dev/loop5", createErr: fmt.Errorf("sgdisk boom")}

		handle, err := assembleMedia(context.Background(), testLogger(), validInstallOnly(),
			BuildOptions{OutputPath: img}, mediaBuildDeps{partition: fp})
		require.Error(t, err)
		assert.Nil(t, handle)

		assert.Equal(t, []string{"attach", "create", "detach"}, fp.calls)
		assert.Equal(t, []string{"/dev/loop5"}, fp.detached)
		_, statErr := os.Stat(img)
		assert.True(t, os.IsNotExist(statErr), "the partial image must be removed on failure")
	})
}

// TestAssembleMediaRefusesToOverwriteExistingOutput covers the destructive-overwrite guard:
// when the --output path already exists, assembleMedia must fail without truncating or
// removing the existing file and without reaching any privileged op. A build never
// completes in this story, so clobbering a reused path would destroy the operator's file
// for a command that then reports failure.
func TestAssembleMediaRefusesToOverwriteExistingOutput(t *testing.T) {
	img := filepath.Join(t.TempDir(), "out.img")
	const existing = "pre-existing image contents"
	require.NoError(t, os.WriteFile(img, []byte(existing), 0o600))

	fp := &fakePartitionOps{attachDev: "/dev/loop1"}
	handle, err := assembleMedia(context.Background(), testLogger(), validInstallOnly(),
		BuildOptions{OutputPath: img}, mediaBuildDeps{partition: fp})
	require.Error(t, err)
	assert.Nil(t, handle)
	assert.Contains(t, err.Error(), "already exists")

	// The pre-existing file is left untouched: not truncated, not removed by rollback, and
	// no privileged op was reached.
	got, readErr := os.ReadFile(img)
	require.NoError(t, readErr)
	assert.Equal(t, existing, string(got), "an existing output file must not be modified or removed")
	assert.Empty(t, fp.calls, "no privileged op may run when the output path already exists")
}

// TestCloseDetachFailurePreservesAndRetries covers Close's behavior when the detach fails:
// it must keep LoopDevice set and leave the backing image in place (the loop may still be
// attached, so removing it would orphan the attachment and lose the retry handle), and a
// later Close must be able to detach and remove once the cause clears.
func TestCloseDetachFailurePreservesAndRetries(t *testing.T) {
	img := filepath.Join(t.TempDir(), "out.img")
	require.NoError(t, os.WriteFile(img, []byte("backing"), 0o600))
	fp := &fakePartitionOps{detachErr: fmt.Errorf("losetup -d boom")}
	h := &MediaHandle{ImagePath: img, LoopDevice: "/dev/loop7", IsImage: true, partition: fp}

	// First Close: detach fails, so the device handle and backing image are preserved.
	err := h.Close()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "detaching loop device")
	assert.Equal(t, "/dev/loop7", h.LoopDevice, "LoopDevice must be preserved for a retry when detach fails")
	assert.FileExists(t, img, "the backing image must not be removed while the loop may still be attached")

	// Clear the failure and retry: the detach now succeeds and the image is removed.
	fp.detachErr = nil
	require.NoError(t, h.Close())
	assert.Empty(t, h.LoopDevice)
	_, statErr := os.Stat(img)
	assert.True(t, os.IsNotExist(statErr), "a successful retry must remove the image")
	assert.Equal(t, []string{"detach", "detach"}, fp.calls, "detach is attempted again on retry")
}

func TestParseSgdiskTable(t *testing.T) {
	// A representative `sgdisk -p` output with a header, totals, the column header, and one
	// placeholder p1 row. parseSgdiskTable must pick out only the data row and derive the
	// byte size from the sector range.
	out := `Disk /dev/loop0: 21495808 sectors, 10.3 GiB
Sector size (logical/physical): 512/512 bytes
Disk identifier (GUID): 00000000-0000-0000-0000-000000000000
Partition table holds up to 128 entries
Main partition table begins at sector 2 and ends at sector 33
First usable sector is 34, last usable sector is 21495774
Partitions will be aligned on 2048-sector boundaries
Total free space is 17301437 sectors (8.3 GiB)

Number  Start (sector)    End (sector)  Size       Code  Name
   1            2048         4196351   2.0 GiB     8300  live-iso
`
	entries := parseSgdiskTable(out)
	require.Len(t, entries, 1)
	e := entries[0]
	assert.Equal(t, 1, e.Number)
	assert.Equal(t, int64(2048), e.Start)
	assert.Equal(t, int64(4196351), e.End)
	assert.Equal(t, (int64(4196351)-int64(2048)+1)*sectorSize, e.Size)
	assert.Equal(t, "8300", e.Type)
	assert.Equal(t, "live-iso", e.Name)
}
