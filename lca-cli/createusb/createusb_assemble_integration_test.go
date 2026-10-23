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

// Integration coverage for the real privileged media-assembly path (losetup + sfdisk + mkfs +
// blkid) that the faked-seam unit tests cannot establish. It stands in a synthetic isohybrid ISO
// (a small image carrying a DOS/MBR table with two "ISO" primaries) for the real RHCOS live ISO,
// then exercises the CNF-27197-proven append-after-ISO layout end to end:
//
//   - assembleMedia copies the synthetic ISO to the front, appends the data partitions into the
//     free MBR slots after it, formats + labels them, and (self-contained) populates cluster-config;
//   - every expected filesystem label resolves on the loop device by blkid; and
//   - teardown is honest on the success path (Close detaches the loop and removes the image) and on
//     an injected mid-assembly failure (the rollback detaches the loop and removes the image).
//
// The privileged path is gated on os.Geteuid()==0 and on the required tools being installed; it
// only ever targets a loopback-attached sparse .img, never a real block device. TestMain lives in
// createusb_images_integration_test.go and is reused here.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

// requireAssemblyTools skips the test unless it runs as root with every tool the real DOS/MBR
// assembly path needs.
func requireAssemblyTools(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("real losetup/sfdisk media assembly requires root")
	}
	for _, bin := range []string{"losetup", "sfdisk", "blockdev", "udevadm", "mkfs.ext4", "mkfs.xfs", "blkid", "lsblk", "mount", "umount"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available: %v", bin, err)
		}
	}
}

// loopAttached reports whether loopDev is currently a live loop device.
func loopAttached(loopDev string) bool {
	return exec.Command("losetup", loopDev).Run() == nil //nolint:gosec // fixed device path from losetup
}

// losetup -d may complete before the kernel releases the loop device. Wait for the actual detach
// rather than asserting against that short transition.
func assertLoopDetached(t *testing.T, loopDev, message string) {
	t.Helper()
	assert.Eventually(t, func() bool { return !loopAttached(loopDev) }, 5*time.Second, 20*time.Millisecond, message)
}

func newRealMediaDeps() (*opsPartitionOps, mediaBuildDeps) {
	log := logrus.New()
	log.SetOutput(os.Stderr)
	realOps := &opsPartitionOps{
		ops: ops.NewOps(log, ops.NewRegularExecutor(log, false)),
		log: log,
	}
	// Exercise the real privileged partition path, but fake the ibi-images content population: the
	// real OCI-layout / durability-store builds need a seed image + container tooling and are
	// covered by createusb_images_integration_test.go / createusb_p2_content_integration_test.go.
	return realOps, mediaBuildDeps{partition: realOps, images: &fakeImagePopulator{}}
}

// syntheticISOResult wraps a synthetic ISO path and a stand-in lca-cli binary as an ISOBuildResult,
// so the real assembly path has a binary to stage onto ibi-images.
func syntheticISOResult(t *testing.T, isoPath string) ISOBuildResult {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "lca-cli")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/true\n"), 0o600))
	return ISOBuildResult{ISOPath: isoPath, LCABinaryPath: bin}
}

// makeSyntheticISO builds a small image carrying a DOS/MBR table with two primary partitions,
// standing in for a real RHCOS isohybrid live ISO (whose ISO9660 + EFI partitions assembleMedia
// snapshots and leaves untouched). It returns the image path.
func makeSyntheticISO(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "synthetic.iso")
	f, err := os.Create(p) //nolint:gosec // test-only path under a temp dir
	require.NoError(t, err)
	require.NoError(t, f.Truncate(32<<20)) // 32 MiB is plenty for two small primaries
	require.NoError(t, f.Close())

	loop, err := exec.Command("losetup", "--find", "--show", "--partscan", p).Output() //nolint:gosec // test-only path
	require.NoError(t, err)
	loopDev := strings.TrimSpace(string(loop))
	defer func() { _ = exec.Command("losetup", "-d", loopDev).Run() }() //nolint:gosec // fixed device path

	// Two "ISO" primaries: an ISO9660-ish data partition and an EFI system partition.
	cmd := exec.Command("sfdisk", loopDev) //nolint:gosec // fixed device path
	cmd.Stdin = strings.NewReader("label: dos\nsize=10M, type=83\nsize=5M, type=ef\n")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return p
}

// assertISOPreservedAfterAppend is the required partition-table read-back: it proves the live ISO at
// the front of the assembled image is byte-for-byte identical to the original ISO after the data
// partitions were appended with `sfdisk --append`. The append legitimately fills the previously-free
// MBR primary slots with the new partition entries, so those slots are expected to differ; the ISO's
// boot code, its own (occupied) partition entries, its boot signature, and every byte after the MBR
// sector must be unchanged.
func assertISOPreservedAfterAppend(t *testing.T, origISO []byte, imagePath string) {
	t.Helper()
	require.GreaterOrEqual(t, len(origISO), mbrSectorSize, "the original ISO must carry an MBR sector")

	f, err := os.Open(imagePath) //nolint:gosec // test-only path under a temp dir
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	front := make([]byte, len(origISO))
	_, err = io.ReadFull(f, front)
	require.NoError(t, err, "reading the live ISO back from the front of the assembled image")

	assert.Equal(t, origISO[:mbrPartitionTableOffset], front[:mbrPartitionTableOffset],
		"MBR boot code must be unchanged by sfdisk --append")
	assert.Equal(t, origISO[mbrSignatureOffset:mbrSectorSize], front[mbrSignatureOffset:mbrSectorSize],
		"MBR boot signature must be unchanged by sfdisk --append")

	appended := 0
	for i := 0; i < mbrPartitionEntryCount; i++ {
		off := mbrPartitionTableOffset + i*mbrPartitionEntrySize
		origEntry := origISO[off : off+mbrPartitionEntrySize]
		newEntry := front[off : off+mbrPartitionEntrySize]
		if origEntry[mbrPartitionTypeOffset] != 0x00 {
			assert.Equal(t, origEntry, newEntry, "the ISO's own MBR entry %d must be unchanged by the append", i)
		} else if !bytes.Equal(origEntry, newEntry) {
			appended++
		}
	}
	assert.Positive(t, appended, "sfdisk --append must add the data-partition entries into the ISO's free MBR slots")

	assert.Equal(t, origISO[mbrSectorSize:], front[mbrSectorSize:],
		"every ISO byte after the MBR sector must be byte-for-byte unchanged by the append")
}

// resolveLabel returns the loop-device partition node whose filesystem label matches label.
func resolveLabel(t *testing.T, realOps *opsPartitionOps, loopDev, label string) string {
	t.Helper()
	dev, err := realOps.ResolvePartitionByFSLabel(loopDev, label)
	require.NoError(t, err, "label %q must resolve on %s", label, loopDev)
	return dev
}

// TestAssembleMediaRealLoopback exercises the production seam end-to-end for both modes against a
// real loopback-attached image: the synthetic ISO is written to the front, the data partitions are
// appended after it and formatted, every expected filesystem label resolves, and self-contained's
// cluster-config is populated. Teardown is asserted honest on the success path.
func TestAssembleMediaRealLoopback(t *testing.T) {
	requireAssemblyTools(t)

	for _, tc := range []struct {
		name       string
		manifest   func() *Manifest
		selfConfig bool
		wantLabels []string
	}{
		{"install-only", validInstallOnly, false, []string{labelIBIImages, labelIBIStatus}},
		{"self-contained", validSelfContained, true, []string{labelIBIImages, labelClusterConfig, labelIBIStatus}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			realOps, deps := newRealMediaDeps()
			iso := makeSyntheticISO(t)
			// Snapshot the original ISO bytes (and thus its MBR) before assembly, for the
			// read-back that proves the append never touched the ISO's own bytes or entries.
			origISO, err := os.ReadFile(iso)
			require.NoError(t, err)
			img := filepath.Join(t.TempDir(), "media.img")

			opts := BuildOptions{OutputPath: img}
			var p3Root string
			if tc.selfConfig {
				p3Root = writeTree(t, t.TempDir(), map[string]string{
					filepath.Join(common.ClusterConfigDir, common.SeedReconfigurationFileName): `{"apiVersion":1}`,
					filepath.Join(common.ClusterConfigDir, common.ManifestsDir, "a.yaml"):      "kind: A\n",
					filepath.Join(common.ExtraManifestsDir, "extra.yaml"):                      "kind: Extra\n",
				})
				opts.P3Root = p3Root
			}

			handle, err := assembleMedia(context.Background(), testLogger(), tc.manifest(), syntheticISOResult(t, iso), opts, deps)
			require.NoError(t, err)
			require.NotNil(t, handle)
			// Guard against a leaked loop device / image if an assertion below fails.
			defer func() { _ = handle.Close() }()

			loopDev := handle.LoopDevice
			require.NotEmpty(t, loopDev)
			assert.Len(t, handle.ISOParts, 2, "the synthetic ISO's two primaries are snapshotted and left untouched")

			// Every expected filesystem label resolves on the loop device.
			for _, label := range tc.wantLabels {
				dev := resolveLabel(t, realOps, loopDev, label)
				assert.True(t, strings.HasPrefix(dev, loopDev), "%s resolves to a partition of %s", label, loopDev)
			}

			// Self-contained: the cluster-config partition carries the populated tree byte-for-byte.
			if tc.selfConfig {
				dev := resolveLabel(t, realOps, loopDev, labelClusterConfig)
				mnt := t.TempDir()
				require.NoError(t, exec.Command("mount", dev, mnt).Run()) //nolint:gosec // fixed node + temp mountpoint
				assert.Equal(t, snapshotTree(p3Root), snapshotTree(mnt))
				// Unmount before Close: a loop device with a mounted partition is not released by
				// losetup -d (autoclear keeps it attached until the last unmount), so a lingering
				// mount here would make assertLoopDetached below fail.
				require.NoError(t, exec.Command("umount", mnt).Run()) //nolint:gosec // fixed temp mountpoint
			}

			// Read-back: the live ISO at the front of the image is byte-for-byte unchanged (its own
			// MBR entries included) after the data partitions were appended with sfdisk --append.
			assertISOPreservedAfterAppend(t, origISO, handle.ImagePath)

			// Success teardown: Close detaches the loop device and removes the image.
			require.NoError(t, handle.Close())
			assertLoopDetached(t, loopDev, "Close must detach the loop device")
			_, statErr := os.Stat(img)
			assert.True(t, os.IsNotExist(statErr), "Close must remove the discarded image")
		})
	}
}

// failAppendOps wraps the real seam but injects an AppendDataPartitions failure, so the rollback
// can be exercised against real losetup resources (the ISO is written and the loop attached for
// real; the injected failure triggers assembleMedia's cleanup).
type failAppendOps struct {
	partitionOps
	attachedDev string
}

func (f *failAppendOps) AttachLoopback(imgPath string) (string, error) {
	dev, err := f.partitionOps.AttachLoopback(imgPath)
	f.attachedDev = dev
	return dev, err
}

func (f *failAppendOps) AppendDataPartitions(string, PartitionPlan, int64) error {
	return fmt.Errorf("injected append failure")
}

// TestAssembleMediaRealRollback verifies that a mid-assembly failure on the real path detaches the
// loop device and removes the partial image.
func TestAssembleMediaRealRollback(t *testing.T) {
	requireAssemblyTools(t)

	realOps, _ := newRealMediaDeps()
	wrapped := &failAppendOps{partitionOps: realOps}
	iso := makeSyntheticISO(t)
	img := filepath.Join(t.TempDir(), "media.img")

	handle, err := assembleMedia(context.Background(), testLogger(), validInstallOnly(), syntheticISOResult(t, iso),
		BuildOptions{OutputPath: img}, mediaBuildDeps{partition: wrapped, images: &fakeImagePopulator{}})
	require.Error(t, err)
	assert.Nil(t, handle)
	assert.ErrorContains(t, err, "injected append failure")

	require.NotEmpty(t, wrapped.attachedDev, "the loop device must have been attached before the injected failure")
	assertLoopDetached(t, wrapped.attachedDev, "rollback must detach the loop device")
	_, statErr := os.Stat(img)
	assert.True(t, os.IsNotExist(statErr), "rollback must remove the partial image")
}
