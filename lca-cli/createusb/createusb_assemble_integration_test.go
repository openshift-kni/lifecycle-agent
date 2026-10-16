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

// Integration coverage for the real privileged media-assembly path (losetup + sgdisk):
// a sparse .img is allocated, attached to a loop device, and given a base GPT with the
// placeholder p1 partition, then the result is checked against exactly the properties the
// faked-seam unit tests cannot establish:
//
//   - the on-disk GPT parses back with the placeholder p1 at its planned 1 MiB-aligned
//     offset and reserved size;
//   - sgdisk --verify passes, confirming a valid table with the backup GPT at the device
//     end (no relocation needed because the container is allocated full-size up front); and
//   - teardown is honest on both the success path (Close detaches the loop and removes the
//     image) and an injected mid-assembly failure (the rollback detaches the loop and
//     removes the partial image).
//
// The privileged path is gated on os.Geteuid()==0 (matching createusb_images_integration_test.go)
// and on losetup/sgdisk being installed; it only ever targets a loopback-attached sparse
// .img, never a real block device. TestMain lives in createusb_images_integration_test.go
// and is reused here.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

// requireLoopAndSgdisk skips the test unless it runs as root with losetup and sgdisk
// available, mirroring the root-gating in createusb_images_integration_test.go.
func requireLoopAndSgdisk(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("real losetup/sgdisk partitioning requires root")
	}
	for _, bin := range []string{"losetup", "sgdisk"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available: %v", bin, err)
		}
	}
}

// loopAttached reports whether loopDev is currently a live loop device.
func loopAttached(loopDev string) bool {
	return exec.Command("losetup", loopDev).Run() == nil //nolint:gosec // fixed device path from losetup
}

func newRealMediaDeps() (*opsPartitionOps, mediaBuildDeps) {
	log := logrus.New()
	log.SetOutput(os.Stderr)
	realOps := &opsPartitionOps{
		ops: ops.NewOps(log, ops.NewRegularExecutor(log, false)),
		log: log,
	}
	return realOps, mediaBuildDeps{partition: realOps}
}

// TestAssembleMediaRealLoopback exercises the production seam end-to-end against a real
// loopback-attached sparse image and asserts the parsed base layout and honest teardown.
func TestAssembleMediaRealLoopback(t *testing.T) {
	requireLoopAndSgdisk(t)

	realOps, deps := newRealMediaDeps()
	plan := partitionPlan(ModeInstallOnly, defaultP1ReservationBytes)
	img := filepath.Join(t.TempDir(), "media.img")

	handle, err := assembleMedia(context.Background(), testLogger(), validInstallOnly(),
		BuildOptions{OutputPath: img}, deps)
	require.NoError(t, err)
	require.NotNil(t, handle)
	// Guard against a leaked loop device / image if an assertion below fails.
	defer func() { _ = handle.Close() }()

	loopDev := handle.LoopDevice
	require.NotEmpty(t, loopDev)

	// The on-disk GPT parses back with exactly the placeholder p1 this story creates.
	entries, err := realOps.ReadPartitionTable(loopDev)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the placeholder p1 is created in this story")
	p1 := entries[0]
	assert.Equal(t, 1, p1.Number)
	assert.Equal(t, labelLiveISO, p1.Name)
	assert.Equal(t, plan.Partitions[0].Offset/sectorSize, p1.Start, "p1 starts at the planned 1 MiB-aligned offset")
	assert.Zero(t, p1.Start%(alignmentBytes/sectorSize), "p1 start must be 1 MiB-aligned")
	assert.Equal(t, plan.Partitions[0].Size, p1.Size, "p1 reserves the planned live-ISO region")

	// A clean sgdisk --verify confirms a valid table with the backup GPT at the device end.
	verifyOut, verr := realOps.ops.RunInHostNamespace("sgdisk", "--verify", loopDev)
	require.NoError(t, verr, verifyOut)

	// Success teardown: Close detaches the loop device and removes the discarded image.
	require.NoError(t, handle.Close())
	assert.False(t, loopAttached(loopDev), "Close must detach the loop device")
	_, statErr := os.Stat(img)
	assert.True(t, os.IsNotExist(statErr), "Close must remove the discarded image")
}

// failCreateOps wraps the real seam but injects a CreateBaseLayout failure, so the
// rollback can be exercised against real losetup resources.
type failCreateOps struct {
	partitionOps
	attachedDev string
}

func (f *failCreateOps) AttachLoopback(imgPath string) (string, error) {
	dev, err := f.partitionOps.AttachLoopback(imgPath)
	f.attachedDev = dev
	return dev, err
}

func (f *failCreateOps) CreateBaseLayout(string, PartitionPlan) error {
	return fmt.Errorf("injected create-layout failure")
}

// TestAssembleMediaRealRollback verifies that a mid-assembly failure on the real path
// detaches the loop device and removes the partial image.
func TestAssembleMediaRealRollback(t *testing.T) {
	requireLoopAndSgdisk(t)

	realOps, _ := newRealMediaDeps()
	wrapped := &failCreateOps{partitionOps: realOps}
	img := filepath.Join(t.TempDir(), "media.img")

	handle, err := assembleMedia(context.Background(), testLogger(), validInstallOnly(),
		BuildOptions{OutputPath: img}, mediaBuildDeps{partition: wrapped})
	require.Error(t, err)
	assert.Nil(t, handle)

	require.NotEmpty(t, wrapped.attachedDev, "the loop device must have been attached before the injected failure")
	assert.False(t, loopAttached(wrapped.attachedDev), "rollback must detach the loop device")
	_, statErr := os.Stat(img)
	assert.True(t, os.IsNotExist(statErr), "rollback must remove the partial image")
}
