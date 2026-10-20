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
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
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

// requireP2Tools extends requireLoopAndSgdisk with the extra tools the p2 format path needs
// (partition-table refresh, udev settle, ext4 formatter, and blkid for verification).
func requireP2Tools(t *testing.T) {
	t.Helper()
	requireLoopAndSgdisk(t)
	for _, bin := range []string{"partprobe", "udevadm", "mkfs.ext4", "blkid"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available: %v", bin, err)
		}
	}
}

// loopAttached reports whether loopDev is currently a live loop device.
func loopAttached(loopDev string) bool {
	return exec.Command("losetup", loopDev).Run() == nil //nolint:gosec // fixed device path from losetup
}

// losetup -d may complete before the kernel releases the loop device. Wait for
// the actual detach rather than asserting against that short transition.
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
	assertLoopDetached(t, loopDev, "Close must detach the loop device")
	_, statErr := os.Stat(img)
	assert.True(t, os.IsNotExist(statErr), "Close must remove the discarded image")
}

// requireP3Tools skips unless the real p3 media path's tools are available, in addition to the
// root-gating and loopback/sgdisk tools requireLoopAndSgdisk checks.
func requireP3Tools(t *testing.T) {
	t.Helper()
	requireLoopAndSgdisk(t)
	for _, bin := range []string{"mkfs.ext4", "partprobe", "udevadm", "mount", "umount", "blkid"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available: %v", bin, err)
		}
	}
}

// requireP4Tools skips unless the real p4 media path's tools are available: p4 drives the same
// create -> settle (partprobe + udevadm) -> format (mkfs.ext4) sequence as p2/p3, and the p4
// assertions below also need blkid and mount/umount.
func requireP4Tools(t *testing.T) {
	t.Helper()
	requireLoopAndSgdisk(t)
	for _, bin := range []string{"partprobe", "udevadm", "mkfs.ext4", "blkid", "mount", "umount"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available: %v", bin, err)
		}
	}
}

// TestAssembleMediaRealP3 exercises the production p3 cluster-config media stage end-to-end on
// a real loopback-attached image: assembleMedia creates the reserved p3 entry, formats it
// ext4, labels it cluster-config, mounts it, and populates it from a temp p3Root. The on-disk
// GPT entry, FS label, and populated tree are inspected on the live handle before Close.
func TestAssembleMediaRealP3(t *testing.T) {
	requireP3Tools(t)

	realOps, deps := newRealMediaDeps()
	img := filepath.Join(t.TempDir(), "media.img")

	// A representative rendered p3 tree: manifest.json + a cluster manifest + an extra manifest.
	p3Root := writeTree(t, t.TempDir(), map[string]string{
		filepath.Join(common.ClusterConfigDir, common.SeedReconfigurationFileName): `{"apiVersion":1}`,
		filepath.Join(common.ClusterConfigDir, common.ManifestsDir, "a.yaml"):      "kind: A\n",
		filepath.Join(common.ExtraManifestsDir, "extra.yaml"):                      "kind: Extra\n",
	})

	handle, err := assembleMedia(context.Background(), testLogger(), validSelfContained(),
		BuildOptions{OutputPath: img, P3Root: p3Root}, deps)
	require.NoError(t, err)
	require.NotNil(t, handle)
	// Guard against a leaked loop device / image if an assertion below fails.
	defer func() { _ = handle.Close() }()

	p3, ok := plannedPartition(handle.Plan, labelClusterConfig)
	require.True(t, ok, "a self-contained plan must reserve the cluster-config (p3) slot")

	// The on-disk GPT now carries the cluster-config entry at its planned 1 MiB-aligned offset.
	entries, err := realOps.ReadPartitionTable(handle.LoopDevice)
	require.NoError(t, err)
	var found *PartitionEntry
	for i := range entries {
		if entries[i].Number == p3.Number {
			found = &entries[i]
		}
	}
	require.NotNil(t, found, "the cluster-config (p3) entry must be present on disk")
	assert.Equal(t, labelClusterConfig, found.Name)
	assert.Equal(t, p3.Offset/sectorSize, found.Start, "p3 starts at the planned 1 MiB-aligned offset")

	// A clean sgdisk --verify confirms the table is still valid after appending p3.
	verifyOut, verr := realOps.ops.RunInHostNamespace("sgdisk", "--verify", handle.LoopDevice)
	require.NoError(t, verr, verifyOut)

	// The partition carries an ext4 filesystem whose FS label is cluster-config.
	partDev := partitionDevicePath(handle.LoopDevice, p3.Number)
	labelOut, err := exec.Command("blkid", "-s", "LABEL", "-o", "value", partDev).Output() //nolint:gosec // fixed partition device path
	require.NoError(t, err)
	assert.Equal(t, labelClusterConfig, strings.TrimSpace(string(labelOut)))

	// Re-mount the (now unmounted) partition and confirm the populated tree matches the source
	// byte-for-byte with identical relative layout.
	mnt := t.TempDir()
	require.NoError(t, exec.Command("mount", partDev, mnt).Run()) //nolint:gosec // fixed partition device path + temp mountpoint
	defer func() { _ = exec.Command("umount", mnt).Run() }()      //nolint:gosec // fixed temp mountpoint
	assert.Equal(t, snapshotTree(p3Root), snapshotTree(mnt))
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
	assertLoopDetached(t, wrapped.attachedDev, "rollback must detach the loop device")
	_, statErr := os.Stat(img)
	assert.True(t, os.IsNotExist(statErr), "rollback must remove the partial image")
}

// TestCreateP2PartitionRealLoopback exercises the production p2 path end-to-end against a
// real loopback-attached image: assembleMedia lays down the base GPT + placeholder p1, then
// createP2Partition adds and formats the empty p2 (ibi-images) partition. It asserts the
// on-disk table now has p1+p2, that sgdisk --verify is clean, that p2 reports the expected
// ext4 filesystem and ibi-images label, and that the formatted partition actually mounts.
func TestCreateP2PartitionRealLoopback(t *testing.T) {
	requireP2Tools(t)

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

	require.NoError(t, createP2Partition(testLogger(), handle, deps))

	// The on-disk GPT now parses back with p1 (placeholder) + p2 (ibi-images).
	entries, err := realOps.ReadPartitionTable(loopDev)
	require.NoError(t, err)
	require.Len(t, entries, 2, "the base p1 plus the new p2 must be present")

	wantP2, ok := plannedPartition(plan, labelIBIImages)
	require.True(t, ok)
	var p2 *PartitionEntry
	for i := range entries {
		if entries[i].Number == wantP2.Number {
			p2 = &entries[i]
		}
	}
	require.NotNil(t, p2, "the p2 entry must exist in the table")
	assert.Equal(t, labelIBIImages, p2.Name)
	assert.Equal(t, wantP2.Offset/sectorSize, p2.Start, "p2 starts at the planned 1 MiB-aligned offset")
	assert.Zero(t, p2.Start%(alignmentBytes/sectorSize), "p2 start must be 1 MiB-aligned")
	assert.Equal(t, wantP2.Size, p2.Size, "p2 reserves the planned ibi-images region")

	// A clean sgdisk --verify confirms the table is still valid after appending p2.
	verifyOut, verr := realOps.ops.RunInHostNamespace("sgdisk", "--verify", loopDev)
	require.NoError(t, verr, verifyOut)

	// The formatted p2 reports the expected ext4 filesystem and ibi-images label.
	node := partitionDevicePath(loopDev, wantP2.Number)
	blkidOut, berr := realOps.ops.RunInHostNamespace("blkid", "-o", "export", node)
	require.NoError(t, berr, blkidOut)
	assert.Contains(t, blkidOut, "LABEL="+labelIBIImages)
	assert.Contains(t, blkidOut, "TYPE="+ibiImagesFSType)

	// The filesystem actually mounts and unmounts cleanly, and p2 is left empty -- this
	// story copies no content (only the filesystem's own lost+found may be present).
	mountDir := t.TempDir()
	mountOut, merr := exec.Command("mount", node, mountDir).CombinedOutput() //nolint:gosec // fixed loopback node + test temp dir
	require.NoError(t, merr, string(mountOut))
	mounted := true
	// Safety net: unmount if an assertion below fails before the explicit unmount runs.
	defer func() {
		if mounted {
			_, _ = exec.Command("umount", mountDir).CombinedOutput() //nolint:gosec // fixed test temp dir
		}
	}()

	names, rerr := os.ReadDir(mountDir)
	require.NoError(t, rerr)
	for _, e := range names {
		assert.Equal(t, "lost+found", e.Name(), "p2 must be empty apart from the filesystem's lost+found")
	}

	// Unmount p2 before tearing the handle down: a loop device that is still backing a mounted
	// filesystem is busy, and the kernel can defer its detachment until the unmount completes,
	// which would make the loopAttached assertion below flaky.
	umountOut, uerr := exec.Command("umount", mountDir).CombinedOutput() //nolint:gosec // fixed test temp dir
	require.NoError(t, uerr, string(umountOut))
	mounted = false

	// Success teardown: Close detaches the loop device and removes the discarded image.
	require.NoError(t, handle.Close())
	assertLoopDetached(t, loopDev, "Close must detach the loop device")
	_, statErr := os.Stat(img)
	assert.True(t, os.IsNotExist(statErr), "Close must remove the discarded image")
}

// failP2CreateOps wraps the real seam but injects a CreatePartition failure, so the p2 cleanup
// path can be exercised against real losetup/sgdisk resources (the base p1 is created for real;
// the injected failure in the shared create step aborts p2 before it formats, so the test needs
// no partprobe/udev/mkfs tooling).
type failP2CreateOps struct {
	partitionOps
	attachedDev string
}

func (f *failP2CreateOps) AttachLoopback(imgPath string) (string, error) {
	dev, err := f.partitionOps.AttachLoopback(imgPath)
	f.attachedDev = dev
	return dev, err
}

func (f *failP2CreateOps) CreatePartition(string, PlannedPartition) error {
	return fmt.Errorf("injected create-partition failure")
}

// TestBuildP2RealRollback verifies that a p2 failure on the real path tears the handle down:
// the loop device is detached and the partial image removed (the base p1 was created for
// real; the injected CreatePartition failure triggers build's cleanup).
func TestBuildP2RealRollback(t *testing.T) {
	requireLoopAndSgdisk(t)

	realOps, _ := newRealMediaDeps()
	wrapped := &failP2CreateOps{partitionOps: realOps}
	img := filepath.Join(t.TempDir(), "media.img")

	err := build(context.Background(), testLogger(), validInstallOnly(),
		BuildOptions{OutputPath: img}, mediaBuildDeps{partition: wrapped})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "injected create-partition failure")

	require.NotEmpty(t, wrapped.attachedDev, "the loop device must have been attached before the injected failure")
	assertLoopDetached(t, wrapped.attachedDev, "a p2 failure must detach the loop device")
	_, statErr := os.Stat(img)
	assert.True(t, os.IsNotExist(statErr), "a p2 failure must remove the partial image")
}

// TestCreateP4PartitionRealLoopback exercises the combined media layout end-to-end on a real
// loopback-attached image for both modes: assembleMedia lays down the base GPT + placeholder p1
// (and, for self-contained with a supplied tree, the populated p3), then createP2Partition and
// createP4Partition add the empty p2 and p4 partitions through the shared create -> settle -> format
// sequence. It asserts the on-disk table carries the full per-mode partition set at the planned
// offsets (install-only p1/p2/p4; self-contained p1/p2/p3/p4), that sgdisk --verify is clean, and
// that p4 is an empty, writable, unmounted ext4 filesystem labeled ibi-status: blkid reports
// TYPE=ext4/LABEL=ibi-status, the finished image ships p4 unmounted, it contains no
// application-created files (only ext4's own lost+found), and a read-write mount round-trips a
// write/fsync/readback.
func TestCreateP4PartitionRealLoopback(t *testing.T) {
	requireP4Tools(t)

	for _, tc := range []struct {
		name       string
		manifest   func() *Manifest
		mode       Mode
		selfConfig bool
		wantLabels []string
	}{
		{"install-only", validInstallOnly, ModeInstallOnly, false, []string{labelLiveISO, labelIBIImages, labelIBIStatus}},
		{"self-contained", validSelfContained, ModeSelfContained, true, []string{labelLiveISO, labelIBIImages, labelClusterConfig, labelIBIStatus}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			realOps, deps := newRealMediaDeps()
			plan := partitionPlan(tc.mode, defaultP1ReservationBytes)
			img := filepath.Join(t.TempDir(), "media.img")

			opts := BuildOptions{OutputPath: img}
			if tc.selfConfig {
				// Supply a rendered p3 tree so the self-contained path materializes the full
				// p1/p2/p3/p4 layout on disk (the live command path does not thread this yet; that is
				// CNF-27171).
				opts.P3Root = writeTree(t, t.TempDir(), map[string]string{
					filepath.Join(common.ClusterConfigDir, common.SeedReconfigurationFileName): `{"apiVersion":1}`,
				})
			}

			handle, err := assembleMedia(context.Background(), testLogger(), tc.manifest(), opts, deps)
			require.NoError(t, err)
			require.NotNil(t, handle)
			// Guard against a leaked loop device / image if an assertion below fails.
			defer func() { _ = handle.Close() }()

			loopDev := handle.LoopDevice
			require.NotEmpty(t, loopDev)

			require.NoError(t, createP2Partition(testLogger(), handle, deps))
			require.NoError(t, createP4Partition(testLogger(), handle, deps))

			// The on-disk GPT parses back with the full per-mode partition set at the planned offsets.
			entries, err := realOps.ReadPartitionTable(loopDev)
			require.NoError(t, err)
			require.Len(t, entries, len(tc.wantLabels), "the full per-mode partition set must be on disk")
			byNumber := map[int]PartitionEntry{}
			for _, e := range entries {
				byNumber[e.Number] = e
			}
			for _, want := range plan.Partitions {
				got, ok := byNumber[want.Number]
				require.True(t, ok, "partition %d (%s) must be on disk", want.Number, want.Label)
				assert.Equal(t, want.Label, got.Name, "partition %d GPT name", want.Number)
				assert.Equal(t, want.Offset/sectorSize, got.Start, "partition %d starts at the planned offset", want.Number)
				assert.Zero(t, got.Start%(alignmentBytes/sectorSize), "partition %d start must be 1 MiB-aligned", want.Number)
			}

			// A clean sgdisk --verify confirms the table is still valid after appending p2 and p4.
			verifyOut, verr := realOps.ops.RunInHostNamespace("sgdisk", "--verify", loopDev)
			require.NoError(t, verr, verifyOut)

			// p4: the finished image ships it unmounted and it reports ext4 / ibi-status.
			p4Plan, ok := plannedPartition(plan, labelIBIStatus)
			require.True(t, ok)
			partDev := partitionDevicePath(loopDev, p4Plan.Number)
			assert.False(t, isMounted(t, partDev), "p4 must ship unmounted")

			blk, berr := exec.Command("blkid", "-o", "export", partDev).CombinedOutput() //nolint:gosec // fixed loopback node
			require.NoError(t, berr, string(blk))
			fields := parseBlkidExport(string(blk))
			assert.Equal(t, "ext4", fields["TYPE"], "p4 must be formatted ext4")
			assert.Equal(t, labelIBIStatus, fields["LABEL"], "p4 filesystem label must be ibi-status")

			// The filesystem mounts read-write: it contains only ext4's own lost+found (no
			// application-created files), and a write/fsync/readback round-trips.
			mnt := t.TempDir()
			out, merr := exec.Command("mount", partDev, mnt).CombinedOutput() //nolint:gosec // fixed loopback node + temp mountpoint
			require.NoError(t, merr, string(out))
			unmounted := false
			defer func() {
				if !unmounted {
					_ = exec.Command("umount", mnt).Run() //nolint:gosec // fixed temp mountpoint
				}
			}()

			appFiles := []string{}
			for _, name := range dirEntryNames(t, mnt) {
				if name != "lost+found" {
					appFiles = append(appFiles, name)
				}
			}
			assert.Empty(t, appFiles, "a freshly formatted p4 must contain no application-created files")

			const payload = "ibi-status writable check"
			probe := filepath.Join(mnt, "write-probe")
			f, ferr := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // test-only path under a temp mount
			require.NoError(t, ferr, "p4 must be writable (no read-only attribute)")
			_, werr := f.WriteString(payload)
			require.NoError(t, werr)
			require.NoError(t, f.Sync())
			require.NoError(t, f.Close())

			got, rerr := os.ReadFile(probe) //nolint:gosec // test-only path under a temp mount
			require.NoError(t, rerr)
			assert.Equal(t, payload, string(got), "a written file must read back")

			// Unmount before teardown: the finished image ships the filesystem unmounted.
			require.NoError(t, exec.Command("umount", mnt).Run()) //nolint:gosec // fixed temp mountpoint
			unmounted = true

			// Success teardown: Close detaches the loop device and removes the discarded image.
			require.NoError(t, handle.Close())
			assertLoopDetached(t, loopDev, "Close must detach the loop device")
			_, statErr := os.Stat(img)
			assert.True(t, os.IsNotExist(statErr), "Close must remove the discarded image")
		})
	}
}

// isMounted reports whether dev currently appears as a mount source in /proc/mounts.
func isMounted(t *testing.T, dev string) bool {
	t.Helper()
	data, err := os.ReadFile("/proc/mounts")
	require.NoError(t, err)
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == dev {
			return true
		}
	}
	return false
}

// parseBlkidExport parses `blkid -o export` output (KEY=VALUE lines) into a map.
func parseBlkidExport(out string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		fields[k] = v
	}
	return fields
}

// dirEntryNames returns the sorted names of the entries directly under dir.
func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}
