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
	"strings"
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

// fakePartitionOps is a partitionOps fake that records the ordered privileged-op calls and lets
// each op's result be injected, so the pure assembly logic (ISO-to-front, append, format,
// populate, rollback) is unit-testable without root.
type fakePartitionOps struct {
	attachDev string
	attachErr error
	detachErr error

	snapshot    []string
	snapshotErr error

	appendErr error

	dataNodes    []string
	dataNodesErr error

	formatErr error

	resolveDev string
	resolveErr error
	// resolveByLabel, when set, maps a filesystem label to the device node ResolvePartitionByFSLabel
	// returns for it (overriding resolveDev). resolvedLabels records every label resolved, in order.
	resolveByLabel map[string]string
	resolvedLabels []string

	mountErr  error
	umountErr error
	// umountErrMatch, when set, scopes umountErr to only the mountpoints whose path contains it
	// (e.g. "create-usb-p3-" to fail only the cluster-config unmount). Empty applies umountErr to
	// every unmount.
	umountErrMatch string
	// onMount, if set, runs after a successful MountPartition with the mountpoint path, so a test
	// can perturb the mountpoint (e.g. force a populate failure) deterministically.
	onMount func(mountpoint string)

	calls               []string
	attached            []string
	detached            []string
	appendedPlans       []PartitionPlan
	appendedStartSector []int64

	// format recording: every formatted node, in order, with its fs type and label.
	formattedDevs    []string
	formattedFSTypes []string
	formattedLabels  []string

	mountedDev string
	mountpoint string
	// mountSnapshot is the rel-path->content tree captured from the mountpoint at
	// UnmountPartition, so a test can assert what was populated before the (simulated) unmount
	// made the files no longer visible at the mountpoint.
	mountSnapshot map[string]string
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

func (f *fakePartitionOps) SnapshotPartitions(dev string) ([]string, error) {
	f.calls = append(f.calls, "snapshot")
	if f.snapshotErr != nil {
		return nil, f.snapshotErr
	}
	if f.snapshot != nil {
		return f.snapshot, nil
	}
	// Default: a realistic RHCOS isohybrid ISO exposes two partitions (ISO9660 + EFI).
	return []string{dev + "p1", dev + "p2"}, nil
}

func (f *fakePartitionOps) AppendDataPartitions(dev string, plan PartitionPlan, startSector int64) error {
	f.calls = append(f.calls, "append")
	f.appendedPlans = append(f.appendedPlans, plan)
	f.appendedStartSector = append(f.appendedStartSector, startSector)
	return f.appendErr
}

func (f *fakePartitionOps) DataPartitionNodes(dev string, isoParts []string) ([]string, error) {
	f.calls = append(f.calls, "dataNodes")
	if f.dataNodesErr != nil {
		return nil, f.dataNodesErr
	}
	return f.dataNodes, nil
}

func (f *fakePartitionOps) FormatPartition(partDev, fsType, label string) error {
	f.calls = append(f.calls, "format")
	f.formattedDevs = append(f.formattedDevs, partDev)
	f.formattedFSTypes = append(f.formattedFSTypes, fsType)
	f.formattedLabels = append(f.formattedLabels, label)
	return f.formatErr
}

func (f *fakePartitionOps) ResolvePartitionByFSLabel(dev, label string) (string, error) {
	f.calls = append(f.calls, "resolve")
	f.resolvedLabels = append(f.resolvedLabels, label)
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	if node, ok := f.resolveByLabel[label]; ok {
		return node, nil
	}
	dev2 := f.resolveDev
	if dev2 == "" {
		dev2 = dev + "p5"
	}
	return dev2, nil
}

func (f *fakePartitionOps) MountPartition(partDev, mountpoint string) error {
	f.calls = append(f.calls, "mount")
	f.mountedDev = partDev
	f.mountpoint = mountpoint
	if f.mountErr != nil {
		return f.mountErr
	}
	if f.onMount != nil {
		f.onMount(mountpoint)
	}
	return nil
}

func (f *fakePartitionOps) UnmountPartition(mountpoint string) error {
	f.calls = append(f.calls, "umount")
	// Simulate a real unmount: snapshot what was populated, then clear the mountpoint so the
	// (now-empty) directory can be removed -- on a host the files live on the unmounted
	// filesystem and are no longer visible at the mountpoint.
	f.mountSnapshot = snapshotTree(mountpoint)
	clearDir(mountpoint)
	if f.umountErr != nil && (f.umountErrMatch == "" || strings.Contains(mountpoint, f.umountErrMatch)) {
		return f.umountErr
	}
	return nil
}

// fakeISOBuilder is a liveISOBuilder fake: it writes a stand-in ISO file (with a valid MBR that
// leaves enough free primary slots) and a stand-in lca-cli binary under the work directory and
// returns their paths, so build() can be exercised without network/podman.
type fakeISOBuilder struct {
	isoBytes int
	usedMBR  int // number of used MBR primaries the stand-in ISO advertises (0 -> default 2)
	buildErr error
	gotOpts  ISOBuildOptions
}

func (f *fakeISOBuilder) Build(_ context.Context, _ *logrus.Logger, _ *Manifest, opts ISOBuildOptions) (ISOBuildResult, error) {
	f.gotOpts = opts
	if f.buildErr != nil {
		return ISOBuildResult{}, f.buildErr
	}
	size := f.isoBytes
	if size == 0 {
		size = 1024
	}
	used := f.usedMBR
	if used == 0 {
		used = 2
	}
	isoPath := filepath.Join(opts.WorkDir, "live.iso")
	if err := os.WriteFile(isoPath, mbrISOBytes(size, used), 0o600); err != nil {
		return ISOBuildResult{}, err
	}
	binPath := filepath.Join(opts.WorkDir, "lca-cli")
	if err := os.WriteFile(binPath, []byte("#!/bin/true\n"), 0o600); err != nil {
		return ISOBuildResult{}, err
	}
	return ISOBuildResult{ISOPath: isoPath, LCABinaryPath: binPath}, nil
}

// fakeImagePopulator is an imagePopulator fake that records calls and returns injectable results,
// so populateIBIImages is unit-testable without real image mirroring / overlay stores.
type fakeImagePopulator struct {
	layoutErr error
	storeErr  error
	// gotP2Root / gotP3Root capture the roots BuildLayout was called with.
	gotP2Root string
	gotP3Root string
	gotArch   string
	calls     []string
}

func (f *fakeImagePopulator) BuildLayout(_ context.Context, _ *logrus.Logger, _ *Manifest, _ ImageSetOptions, _, p2Root, p3Root string) (*LayoutBuildResult, error) {
	f.calls = append(f.calls, "buildLayout")
	f.gotP2Root = p2Root
	f.gotP3Root = p3Root
	if f.layoutErr != nil {
		return nil, f.layoutErr
	}
	// The real BuildLayout renders the self-contained p3 config tree under p3Root. Model that for
	// an empty render directory so the downstream cluster-config populate has something to copy; a
	// pre-rendered (non-empty) p3Root is left untouched.
	if p3Root != "" {
		if entries, err := os.ReadDir(p3Root); err == nil && len(entries) == 0 {
			_ = os.WriteFile(filepath.Join(p3Root, "manifest.json"), []byte(`{"apiVersion":1}`), 0o600)
		}
	}
	return &LayoutBuildResult{Size: SizeReport{StagedBytes: 42}}, nil
}

func (f *fakeImagePopulator) BuildStore(_ context.Context, _ *logrus.Logger, _ *Manifest, p2Root, _, targetArch string) (*StoreBuildResult, error) {
	f.calls = append(f.calls, "buildStore")
	f.gotArch = targetArch
	if f.storeErr != nil {
		return nil, f.storeErr
	}
	return &StoreBuildResult{StorePath: filepath.Join(p2Root, "store"), ImportedImages: 1}, nil
}

// mbrISOBytes returns a stand-in ISO image of totalSize bytes carrying a valid MBR: the 0x55AA boot
// signature and `used` non-empty primary partition entries, so validateISOLayout accepts it.
func mbrISOBytes(totalSize, used int) []byte {
	if totalSize < mbrSectorSize {
		totalSize = mbrSectorSize
	}
	b := make([]byte, totalSize)
	for i := 0; i < used && i < mbrPartitionEntryCount; i++ {
		b[mbrPartitionTableOffset+i*mbrPartitionEntrySize+mbrPartitionTypeOffset] = 0x83
	}
	b[mbrSignatureOffset] = 0x55
	b[mbrSignatureOffset+1] = 0xAA
	return b
}

// writeTempISO writes a stand-in ISO of size bytes (with a valid MBR, two used primaries) to a temp
// file and returns its path, standing in for a built customized live ISO.
func writeTempISO(t *testing.T, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "live.iso")
	require.NoError(t, os.WriteFile(path, mbrISOBytes(size, 2), 0o600))
	return path
}

// writeTempISOResult writes a stand-in ISO and lca-cli binary and returns an ISOBuildResult, for
// driving assembleMedia directly.
func writeTempISOResult(t *testing.T, size int) ISOBuildResult {
	t.Helper()
	dir := t.TempDir()
	isoPath := filepath.Join(dir, "live.iso")
	require.NoError(t, os.WriteFile(isoPath, mbrISOBytes(size, 2), 0o600))
	binPath := filepath.Join(dir, "lca-cli")
	require.NoError(t, os.WriteFile(binPath, []byte("#!/bin/true\n"), 0o600))
	return ISOBuildResult{ISOPath: isoPath, LCABinaryPath: binPath}
}

func TestPartitionPlanPerMode(t *testing.T) {
	t.Run("install-only appends ibi-images + ibi-status after the ISO", func(t *testing.T) {
		plan := partitionPlan(ModeInstallOnly)
		assert.Equal(t, []string{labelIBIImages, labelIBIStatus}, partitionLabels(plan))
		images, ok := plannedPartition(plan, labelIBIImages)
		require.True(t, ok)
		assert.Equal(t, ibiImagesFSType, images.FSType)
		assert.Equal(t, p2ReservationBytes, images.Size)
		status, ok := plannedPartition(plan, labelIBIStatus)
		require.True(t, ok)
		assert.Equal(t, ibiStatusFSType, status.FSType)
		assert.Equal(t, p4ReservationBytes, status.Size)
		_, ok = plannedPartition(plan, labelClusterConfig)
		assert.False(t, ok, "install-only has no cluster-config partition")
	})

	t.Run("self-contained appends ibi-images + cluster-config + ibi-status after the ISO", func(t *testing.T) {
		plan := partitionPlan(ModeSelfContained)
		assert.Equal(t, []string{labelIBIImages, labelClusterConfig, labelIBIStatus}, partitionLabels(plan))
		config, ok := plannedPartition(plan, labelClusterConfig)
		require.True(t, ok)
		assert.Equal(t, clusterConfigFSType, config.FSType)
		assert.Equal(t, p3ReservationBytes, config.Size)
	})
}

func partitionLabels(plan PartitionPlan) []string {
	labels := make([]string, 0, len(plan.Partitions))
	for _, p := range plan.Partitions {
		labels = append(labels, p.Label)
	}
	return labels
}

func TestContainerSize(t *testing.T) {
	iso := writeTempISO(t, int(mib)+1) // just over 1 MiB so alignUp rounds to 2 MiB

	installTotal, err := containerSize(iso, partitionPlan(ModeInstallOnly))
	require.NoError(t, err)
	assert.Zero(t, installTotal%mib, "container size must be MiB-aligned")
	assert.GreaterOrEqual(t, installTotal, 2*mib+p2ReservationBytes+p4ReservationBytes+slackBytes,
		"container must hold the ISO (rounded up), the data partitions, and the slack")

	scTotal, err := containerSize(iso, partitionPlan(ModeSelfContained))
	require.NoError(t, err)
	assert.Equal(t, installTotal+p3ReservationBytes+extSlackBytes, scTotal,
		"self-contained adds cluster-config plus the extended-container slack")
}

func TestSfdiskAppendScript(t *testing.T) {
	// A representative first-safe-append offset: 512 MiB expressed in 512-byte sectors.
	const startSector = int64(512) * mib / mbrSectorSize

	t.Run("install-only is two primaries with the first pinned to the append offset", func(t *testing.T) {
		script, err := sfdiskAppendScript(partitionPlan(ModeInstallOnly), startSector)
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("start=%d, size=%dM, type=83\nsize=%dM, type=83\n",
			startSector, p2ReservationBytes/mib, p4ReservationBytes/mib), script)
	})

	t.Run("self-contained adds an extended container for the logicals", func(t *testing.T) {
		script, err := sfdiskAppendScript(partitionPlan(ModeSelfContained), startSector)
		require.NoError(t, err)
		extMB := (p3ReservationBytes + p4ReservationBytes + extSlackBytes) / mib
		assert.Equal(t, fmt.Sprintf(
			"start=%d, size=%dM, type=83\nsize=%dM, type=5\nsize=%dM, type=83\nsize=%dM, type=83\n",
			startSector, p2ReservationBytes/mib, extMB, p3ReservationBytes/mib, p4ReservationBytes/mib), script)
	})

	t.Run("only the first partition carries an explicit start", func(t *testing.T) {
		// The remaining partitions must have no start= so sfdisk lays them out contiguously after
		// the pinned first one; exactly one start= line proves the data partitions stay packed.
		script, err := sfdiskAppendScript(partitionPlan(ModeSelfContained), startSector)
		require.NoError(t, err)
		assert.Equal(t, 1, strings.Count(script, "start="),
			"only the first appended partition is pinned; the rest follow contiguously")
	})

	t.Run("a non-positive start sector is rejected", func(t *testing.T) {
		// A zero/negative start would place the first data partition at or before the ISO's own
		// bytes, so the script builder refuses it rather than emitting a clobbering layout.
		_, err := sfdiskAppendScript(partitionPlan(ModeInstallOnly), 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid append start sector")
	})
}

func TestParseSfdiskDump(t *testing.T) {
	// A representative `sfdisk --dump` output: the label/label-id/device/unit header, a blank
	// line, then the ISO's two partitions and three appended ones (one extended container).
	out := `label: dos
label-id: 0x12345678
device: /dev/loop0
unit: sectors

/dev/loop0p1 : start=           0, size=     2764800, type=0
/dev/loop0p2 : start=        2760, size=        8192, type=ef
/dev/loop0p3 : start=     2764800, size=    16777216, type=83
/dev/loop0p4 : start=    19542016, size=      688128, type=5
/dev/loop0p5 : start=    19544064, size=      524288, type=83
`
	entries := parseSfdiskDump(out)
	require.Len(t, entries, 5)
	assert.Equal(t, "/dev/loop0p1", entries[0].node)
	assert.Equal(t, "0", entries[0].typeCode)
	assert.Equal(t, "ef", entries[1].typeCode)
	assert.Equal(t, "83", entries[2].typeCode)
	assert.Equal(t, "5", entries[3].typeCode)
	assert.Equal(t, "/dev/loop0p5", entries[4].node)
}

// TestMkfsCommand covers the per-filesystem formatter selection: ext4 and xfs use their own force
// flags (no cross-type reuse) and an unsupported type is a hard error.
func TestMkfsCommand(t *testing.T) {
	t.Run("ext4 uses -F", func(t *testing.T) {
		args, err := mkfsCommand(fsTypeExt4, labelIBIImages, "/dev/loop0p3")
		require.NoError(t, err)
		assert.Equal(t, []string{"mkfs.ext4", "-F", "-L", labelIBIImages, "/dev/loop0p3"}, args)
	})
	t.Run("xfs uses -f", func(t *testing.T) {
		args, err := mkfsCommand(fsTypeXFS, labelIBIImages, "/dev/loop0p3")
		require.NoError(t, err)
		assert.Equal(t, []string{"mkfs.xfs", "-f", "-L", labelIBIImages, "/dev/loop0p3"}, args)
	})
	t.Run("unknown type is a hard error", func(t *testing.T) {
		args, err := mkfsCommand("btrfs", labelIBIImages, "/dev/loop0p3")
		require.Error(t, err)
		assert.Nil(t, args)
		assert.ErrorContains(t, err, "unsupported filesystem type")
	})
}

// TestAssembleMediaInstallOnly covers the happy path: the ISO is written to the front, the loop is
// attached, the ISO partitions are snapshotted, the data partitions are appended and then formatted
// in creation order, and (install-only having no cluster-config) nothing is mounted.
func TestAssembleMediaInstallOnly(t *testing.T) {
	img := filepath.Join(t.TempDir(), "out.img")
	iso := writeTempISOResult(t, 4096)
	fp := &fakePartitionOps{
		attachDev: "/dev/loop9",
		snapshot:  []string{"/dev/loop9p1", "/dev/loop9p2"},
		dataNodes: []string{"/dev/loop9p3", "/dev/loop9p4"},
	}
	img2 := &fakeImagePopulator{}

	handle, err := assembleMedia(context.Background(), testLogger(), validInstallOnly(), iso,
		BuildOptions{OutputPath: img}, mediaBuildDeps{partition: fp, images: img2})
	require.NoError(t, err)
	require.NotNil(t, handle)

	wantPlan := partitionPlan(ModeInstallOnly)
	assert.Equal(t, img, handle.ImagePath)
	assert.Equal(t, "/dev/loop9", handle.LoopDevice)
	assert.True(t, handle.IsImage)
	assert.Equal(t, wantPlan, handle.Plan)
	assert.Equal(t, []string{"/dev/loop9p1", "/dev/loop9p2"}, handle.ISOParts)

	// The seam was driven: attach, snapshot the ISO parts, append the data partitions, resolve the
	// appended nodes, format each (ibi-images, ibi-status), then populate ibi-images (resolve by
	// label, mount, stage, unmount). install-only builds no durability store and has no
	// cluster-config partition, so there is only the one (ibi-images) populate mount.
	assert.Equal(t, []string{"attach", "snapshot", "append", "dataNodes", "format", "format",
		"resolve", "mount", "umount"}, fp.calls)
	assert.Equal(t, []string{labelIBIImages}, fp.resolvedLabels, "only ibi-images is resolved/mounted for install-only")
	assert.Equal(t, []string{"buildLayout"}, img2.calls, "install-only builds the OCI layout but no durability store")
	assert.Equal(t, []string{"/dev/loop9p3", "/dev/loop9p4"}, fp.formattedDevs)
	assert.Equal(t, []string{labelIBIImages, labelIBIStatus}, fp.formattedLabels)
	assert.Equal(t, []PartitionPlan{wantPlan}, fp.appendedPlans)

	// The data partitions are appended starting at the first safe append offset (the MiB-aligned
	// end of the ISO, in 512-byte sectors), never at sfdisk's first-unassigned sector, so a data
	// partition can never land inside the copied ISO bytes.
	isoFI, statErr := os.Stat(iso.ISOPath)
	require.NoError(t, statErr)
	wantStart := alignUpToMiB(isoFI.Size()) / mbrSectorSize
	assert.Equal(t, []int64{wantStart}, fp.appendedStartSector)

	// The image was created at the full container size, and success leaves it with the loop still
	// attached (the caller owns teardown).
	total, err := containerSize(iso.ISOPath, wantPlan)
	require.NoError(t, err)
	fi, statErr := os.Stat(img)
	require.NoError(t, statErr)
	assert.Equal(t, total, fi.Size())
	assert.NotContains(t, fp.calls, "detach", "success must not tear the handle down")

	// Close is the caller's single teardown entry point.
	require.NoError(t, handle.Close())
	assert.Equal(t, []string{"/dev/loop9"}, fp.detached)
	_, statErr = os.Stat(img)
	assert.True(t, os.IsNotExist(statErr), "Close must remove the discarded image")
}

// TestAssembleMediaSelfContainedPopulates covers the self-contained path: three data partitions are
// formatted, then the cluster-config partition is resolved by label, mounted, populated, and
// unmounted.
func TestAssembleMediaSelfContainedPopulates(t *testing.T) {
	img := filepath.Join(t.TempDir(), "out.img")
	iso := writeTempISOResult(t, 4096)
	p3Root := writeTree(t, t.TempDir(), map[string]string{
		filepath.Join("cluster-configuration", "manifest.json"): `{"apiVersion":1}`,
	})
	fp := &fakePartitionOps{
		attachDev: "/dev/loop9",
		snapshot:  []string{"/dev/loop9p1", "/dev/loop9p2"},
		dataNodes: []string{"/dev/loop9p3", "/dev/loop9p5", "/dev/loop9p6"},
		resolveByLabel: map[string]string{
			labelIBIImages:     "/dev/loop9p3",
			labelClusterConfig: "/dev/loop9p5",
		},
	}
	img2 := &fakeImagePopulator{}

	handle, err := assembleMedia(context.Background(), testLogger(), validSelfContained(), iso,
		BuildOptions{OutputPath: img, P3Root: p3Root}, mediaBuildDeps{partition: fp, images: img2})
	require.NoError(t, err)
	require.NotNil(t, handle)

	assert.Equal(t, []string{
		"attach", "snapshot", "append", "dataNodes",
		"format", "format", "format", // ibi-images, cluster-config, ibi-status
		"resolve", "mount", "umount", // populate ibi-images (OCI layout + lca-cli + store)
		"resolve", "mount", "umount", // populate cluster-config
	}, fp.calls)
	assert.Equal(t, []string{labelIBIImages, labelClusterConfig}, fp.resolvedLabels)
	assert.Equal(t, []string{"buildLayout", "buildStore"}, img2.calls, "self-contained builds both the OCI layout and the durability store")
	assert.Equal(t, []string{labelIBIImages, labelClusterConfig, labelIBIStatus}, fp.formattedLabels)
	assert.Equal(t, "/dev/loop9p5", fp.mountedDev, "cluster-config is mounted on its resolved node")
	assert.Equal(t, snapshotTree(p3Root), fp.mountSnapshot, "the config tree is populated byte-for-byte")

	require.NoError(t, handle.Close())
}

func TestAssembleMediaCleanupOnError(t *testing.T) {
	iso := writeTempISOResult(t, 4096)

	t.Run("attach failure removes the image and detaches nothing", func(t *testing.T) {
		img := filepath.Join(t.TempDir(), "out.img")
		fp := &fakePartitionOps{attachErr: fmt.Errorf("losetup boom")}

		handle, err := assembleMedia(context.Background(), testLogger(), validInstallOnly(), iso,
			BuildOptions{OutputPath: img}, mediaBuildDeps{partition: fp})
		require.Error(t, err)
		assert.Nil(t, handle)
		assert.Equal(t, []string{"attach"}, fp.calls, "no detach when nothing was attached")
		assert.Empty(t, fp.detached)
		_, statErr := os.Stat(img)
		assert.True(t, os.IsNotExist(statErr), "the partial image must be removed on failure")
	})

	t.Run("append failure detaches the loop and removes the image", func(t *testing.T) {
		img := filepath.Join(t.TempDir(), "out.img")
		fp := &fakePartitionOps{attachDev: "/dev/loop5", appendErr: fmt.Errorf("sfdisk boom")}

		handle, err := assembleMedia(context.Background(), testLogger(), validInstallOnly(), iso,
			BuildOptions{OutputPath: img}, mediaBuildDeps{partition: fp})
		require.Error(t, err)
		assert.Nil(t, handle)
		assert.ErrorContains(t, err, "sfdisk boom")
		assert.Equal(t, []string{"attach", "snapshot", "append", "detach"}, fp.calls)
		assert.Equal(t, []string{"/dev/loop5"}, fp.detached)
		_, statErr := os.Stat(img)
		assert.True(t, os.IsNotExist(statErr), "the partial image must be removed on failure")
	})

	t.Run("empty ISO partition snapshot fails loudly", func(t *testing.T) {
		img := filepath.Join(t.TempDir(), "out.img")
		fp := &fakePartitionOps{attachDev: "/dev/loop5", snapshot: []string{}}

		_, err := assembleMedia(context.Background(), testLogger(), validInstallOnly(), iso,
			BuildOptions{OutputPath: img}, mediaBuildDeps{partition: fp})
		require.Error(t, err)
		assert.ErrorContains(t, err, "isohybrid live ISO")
		assert.Equal(t, []string{"attach", "snapshot", "detach"}, fp.calls)
	})

	t.Run("wrong appended-partition count fails loudly", func(t *testing.T) {
		img := filepath.Join(t.TempDir(), "out.img")
		fp := &fakePartitionOps{attachDev: "/dev/loop5", dataNodes: []string{"/dev/loop5p3"}} // only one, want two

		_, err := assembleMedia(context.Background(), testLogger(), validInstallOnly(), iso,
			BuildOptions{OutputPath: img}, mediaBuildDeps{partition: fp})
		require.Error(t, err)
		assert.ErrorContains(t, err, "expected 2 appended data partitions")
		assert.Equal(t, []string{"attach", "snapshot", "append", "dataNodes", "detach"}, fp.calls)
	})

	t.Run("detach failure preserves the image and reports both failures", func(t *testing.T) {
		img := filepath.Join(t.TempDir(), "out.img")
		fp := &fakePartitionOps{attachDev: "/dev/loop5", appendErr: fmt.Errorf("sfdisk boom"), detachErr: fmt.Errorf("losetup -d boom")}

		_, err := assembleMedia(context.Background(), testLogger(), validInstallOnly(), iso,
			BuildOptions{OutputPath: img}, mediaBuildDeps{partition: fp})
		require.Error(t, err)
		assert.ErrorContains(t, err, "sfdisk boom")
		assert.ErrorContains(t, err, "losetup -d boom")
		assert.FileExists(t, img)
	})
}

// TestAssembleMediaPreservesImageWhenClusterConfigUnmountFails covers the recovery guard: when the
// cluster-config unmount fails, that filesystem is still mounted over the loop device, so the
// rollback must NOT detach the loop or remove the backing image. The loop stays attached and the
// image stays on disk.
func TestAssembleMediaPreservesImageWhenClusterConfigUnmountFails(t *testing.T) {
	img := filepath.Join(t.TempDir(), "out.img")
	iso := writeTempISOResult(t, 4096)
	p3Root := writeTree(t, t.TempDir(), map[string]string{
		filepath.Join("cluster-configuration", "manifest.json"): `{"apiVersion":1}`,
	})
	fp := &fakePartitionOps{
		attachDev:      "/dev/loop8",
		dataNodes:      []string{"/dev/loop8p3", "/dev/loop8p5", "/dev/loop8p6"},
		umountErr:      fmt.Errorf("umount boom"),
		umountErrMatch: "create-usb-p3-", // fail only the cluster-config unmount, not the ibi-images one
	}

	_, err := assembleMedia(context.Background(), testLogger(), validSelfContained(), iso,
		BuildOptions{OutputPath: img, P3Root: p3Root}, mediaBuildDeps{partition: fp, images: &fakeImagePopulator{}})
	require.Error(t, err)
	assert.ErrorContains(t, err, "umount boom")
	assert.ErrorIs(t, err, errP3Mounted)

	assert.NotContains(t, fp.calls, "detach", "a still-mounted partition must not have its loop detached")
	assert.Empty(t, fp.detached)
	assert.FileExists(t, img, "a still-mounted partition's backing image must be preserved for recovery")
}

// TestAssembleMediaRefusesToOverwriteExistingOutput covers the destructive-overwrite guard: when
// the --output path already exists, assembleMedia must fail without touching the existing file and
// without reaching any privileged op.
func TestAssembleMediaRefusesToOverwriteExistingOutput(t *testing.T) {
	img := filepath.Join(t.TempDir(), "out.img")
	iso := writeTempISOResult(t, 4096)
	const existing = "pre-existing image contents"
	require.NoError(t, os.WriteFile(img, []byte(existing), 0o600))

	fp := &fakePartitionOps{attachDev: "/dev/loop1"}
	handle, err := assembleMedia(context.Background(), testLogger(), validInstallOnly(), iso,
		BuildOptions{OutputPath: img}, mediaBuildDeps{partition: fp})
	require.Error(t, err)
	assert.Nil(t, handle)
	assert.Contains(t, err.Error(), "already exists")

	got, readErr := os.ReadFile(img)
	require.NoError(t, readErr)
	assert.Equal(t, existing, string(got), "an existing output file must not be modified or removed")
	assert.Empty(t, fp.calls, "no privileged op may run when the output path already exists")
}

// TestCloseDetachFailurePreservesAndRetries covers Close's behavior when the detach fails: it must
// keep LoopDevice set and leave the backing image in place, and a later Close must detach and
// remove once the cause clears.
func TestCloseDetachFailurePreservesAndRetries(t *testing.T) {
	img := filepath.Join(t.TempDir(), "out.img")
	require.NoError(t, os.WriteFile(img, []byte("backing"), 0o600))
	fp := &fakePartitionOps{detachErr: fmt.Errorf("losetup -d boom")}
	h := &MediaHandle{ImagePath: img, LoopDevice: "/dev/loop7", IsImage: true, partition: fp}

	err := h.Close()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "detaching loop device")
	assert.Equal(t, "/dev/loop7", h.LoopDevice, "LoopDevice must be preserved for a retry when detach fails")
	assert.FileExists(t, img, "the backing image must not be removed while the loop may still be attached")

	fp.detachErr = nil
	require.NoError(t, h.Close())
	assert.Empty(t, h.LoopDevice)
	_, statErr := os.Stat(img)
	assert.True(t, os.IsNotExist(statErr), "a successful retry must remove the image")
	assert.Equal(t, []string{"detach", "detach"}, fp.calls, "detach is attempted again on retry")
}

// TestFormatDataPartitions covers the shared format stage directly: it maps the appended nodes to
// the plan in creation order and formats each with its filesystem type and label, and a wrong node
// count fails loudly.
func TestFormatDataPartitions(t *testing.T) {
	t.Run("formats each appended partition in plan order", func(t *testing.T) {
		fp := &fakePartitionOps{dataNodes: []string{"/dev/loop0p3", "/dev/loop0p4"}}
		h := &MediaHandle{LoopDevice: "/dev/loop0", Plan: partitionPlan(ModeInstallOnly), partition: fp}
		require.NoError(t, formatDataPartitions(testLogger(), mediaBuildDeps{partition: fp}, h))
		assert.Equal(t, []string{"/dev/loop0p3", "/dev/loop0p4"}, fp.formattedDevs)
		assert.Equal(t, []string{labelIBIImages, labelIBIStatus}, fp.formattedLabels)
		assert.Equal(t, []string{ibiImagesFSType, ibiStatusFSType}, fp.formattedFSTypes)
	})

	t.Run("wrong node count is a hard error", func(t *testing.T) {
		fp := &fakePartitionOps{dataNodes: []string{"/dev/loop0p3"}}
		h := &MediaHandle{LoopDevice: "/dev/loop0", Plan: partitionPlan(ModeInstallOnly), partition: fp}
		err := formatDataPartitions(testLogger(), mediaBuildDeps{partition: fp}, h)
		require.Error(t, err)
		assert.ErrorContains(t, err, "expected 2 appended data partitions")
	})

	t.Run("format error is surfaced", func(t *testing.T) {
		fp := &fakePartitionOps{dataNodes: []string{"/dev/loop0p3", "/dev/loop0p4"}, formatErr: fmt.Errorf("mkfs boom")}
		h := &MediaHandle{LoopDevice: "/dev/loop0", Plan: partitionPlan(ModeInstallOnly), partition: fp}
		err := formatDataPartitions(testLogger(), mediaBuildDeps{partition: fp}, h)
		require.ErrorContains(t, err, "mkfs boom")
	})
}

// TestValidateUSBSizeBudget covers the --usb-size budget guard against the final apparent image
// size: a zero budget disables it, a total within budget passes, and a total over budget fails with
// a clear error.
func TestValidateUSBSizeBudget(t *testing.T) {
	t.Run("zero budget disables the check", func(t *testing.T) {
		require.NoError(t, validateUSBSizeBudget(0, 1<<40))
	})
	t.Run("within budget passes", func(t *testing.T) {
		require.NoError(t, validateUSBSizeBudget(16*mib, 8*mib))
	})
	t.Run("over budget fails with a clear error", func(t *testing.T) {
		err := validateUSBSizeBudget(8*mib, 16*mib)
		require.Error(t, err)
		assert.ErrorContains(t, err, "exceeding the --usb-size budget")
	})
}

// TestAssembleMediaFailsISOPreconditionBeforeModification covers the precondition gate: an ISO that
// does not leave enough free MBR primary slots for the mode fails before the output image is written
// and before any privileged op runs.
func TestAssembleMediaFailsISOPreconditionBeforeModification(t *testing.T) {
	img := filepath.Join(t.TempDir(), "out.img")
	// An ISO advertising four used primaries leaves no free slot for the appended data partitions.
	iso := ISOBuildResult{ISOPath: filepath.Join(t.TempDir(), "iso"), LCABinaryPath: filepath.Join(t.TempDir(), "bin")}
	require.NoError(t, os.WriteFile(iso.ISOPath, mbrISOBytes(4096, 4), 0o600))
	require.NoError(t, os.WriteFile(iso.LCABinaryPath, []byte("b"), 0o600))
	fp := &fakePartitionOps{}

	_, err := assembleMedia(context.Background(), testLogger(), validInstallOnly(), iso,
		BuildOptions{OutputPath: img}, mediaBuildDeps{partition: fp, images: &fakeImagePopulator{}})
	require.Error(t, err)
	assert.ErrorContains(t, err, "does not leave enough free primary slots")
	_, statErr := os.Stat(img)
	assert.True(t, os.IsNotExist(statErr), "no image is written when the ISO-layout precondition fails")
	assert.Empty(t, fp.calls, "no privileged op runs when the precondition fails")
}

// TestAssembleMediaFailsOverBudgetBeforeModification covers the size-budget gate: a --usb-size
// smaller than the assembled media fails before the output image is written.
func TestAssembleMediaFailsOverBudgetBeforeModification(t *testing.T) {
	img := filepath.Join(t.TempDir(), "out.img")
	iso := writeTempISOResult(t, 4096)
	fp := &fakePartitionOps{}

	_, err := assembleMedia(context.Background(), testLogger(), validInstallOnly(), iso,
		BuildOptions{OutputPath: img, USBSizeBytes: 1 << 20}, // 1 MiB budget, far below the data partitions
		mediaBuildDeps{partition: fp, images: &fakeImagePopulator{}})
	require.Error(t, err)
	assert.ErrorContains(t, err, "exceeding the --usb-size budget")
	_, statErr := os.Stat(img)
	assert.True(t, os.IsNotExist(statErr), "no image is written when the media exceeds the budget")
	assert.Empty(t, fp.calls, "no privileged op runs when the budget is exceeded")
}

// TestPopulateIBIImagesStagesLayoutBinaryAndStore covers the ibi-images populate stage directly:
// install-only builds the OCI layout and stages the binary (no store); self-contained also builds
// the durability store with the build host architecture and the on-node store path.
func TestPopulateIBIImagesStagesLayoutAndStore(t *testing.T) {
	t.Run("install-only: layout + binary, no store", func(t *testing.T) {
		fp := &fakePartitionOps{resolveDev: "/dev/loop0p3"}
		images := &fakeImagePopulator{}
		h := &MediaHandle{LoopDevice: "/dev/loop0", Plan: partitionPlan(ModeInstallOnly), partition: fp}
		iso := writeTempISOResult(t, 4096)

		err := populateIBIImages(context.Background(), testLogger(), mediaBuildDeps{partition: fp, images: images},
			h, validInstallOnly(), iso, BuildOptions{OutputPath: "/tmp/out.img"}, "")
		require.NoError(t, err)
		assert.Equal(t, []string{"buildLayout"}, images.calls)
		assert.Equal(t, []string{"resolve", "mount", "umount"}, fp.calls)
	})

	t.Run("self-contained: layout + binary + store", func(t *testing.T) {
		fp := &fakePartitionOps{resolveDev: "/dev/loop0p3"}
		images := &fakeImagePopulator{}
		h := &MediaHandle{LoopDevice: "/dev/loop0", Plan: partitionPlan(ModeSelfContained), partition: fp}
		iso := writeTempISOResult(t, 4096)

		err := populateIBIImages(context.Background(), testLogger(), mediaBuildDeps{partition: fp, images: images},
			h, validSelfContained(), iso, BuildOptions{OutputPath: "/tmp/out.img"}, t.TempDir())
		require.NoError(t, err)
		assert.Equal(t, []string{"buildLayout", "buildStore"}, images.calls)
		assert.NotEmpty(t, images.gotArch, "the durability store is built for an explicit target architecture")
	})

	t.Run("a BuildLayout failure is surfaced and the partition is unmounted", func(t *testing.T) {
		fp := &fakePartitionOps{resolveDev: "/dev/loop0p3"}
		images := &fakeImagePopulator{layoutErr: fmt.Errorf("mirror boom")}
		h := &MediaHandle{LoopDevice: "/dev/loop0", Plan: partitionPlan(ModeInstallOnly), partition: fp}
		iso := writeTempISOResult(t, 4096)

		err := populateIBIImages(context.Background(), testLogger(), mediaBuildDeps{partition: fp, images: images},
			h, validInstallOnly(), iso, BuildOptions{OutputPath: "/tmp/out.img"}, "")
		require.ErrorContains(t, err, "mirror boom")
		assert.Equal(t, []string{"resolve", "mount", "umount"}, fp.calls, "the partition is unmounted even on a populate failure")
	})
}
