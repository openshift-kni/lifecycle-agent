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
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTree creates the given rel-path->content files (with any parent directories) under
// root and returns root, for building a representative p3 config tree in tests.
func writeTree(t *testing.T, root string, files map[string]string) string {
	t.Helper()
	for rel, content := range files {
		dst := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o750))
		require.NoError(t, os.WriteFile(dst, []byte(content), 0o600))
	}
	return root
}

// snapshotTree returns the rel-path->content map of every regular file under root. It is used
// both by the fake seam (to capture the populated mountpoint before a simulated unmount) and
// by tests to build the expected tree from a source directory.
func snapshotTree(root string) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		data, rerr := os.ReadFile(path) //nolint:gosec // test-controlled path under a temp dir
		if rerr != nil {
			return rerr
		}
		out[rel] = string(data)
		return nil
	})
	return out
}

// clearDir removes every entry inside dir (but not dir itself), simulating that an unmount
// leaves the mountpoint directory empty.
func clearDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// selfContainedHandle returns a MediaHandle wired to fp with a self-contained plan (which
// reserves the p3 cluster-config slot), as assembleMedia would hand to the p3 stage.
func selfContainedHandle(fp *fakePartitionOps) *MediaHandle {
	plan := partitionPlan(ModeSelfContained, defaultP1ReservationBytes)
	return &MediaHandle{
		ImagePath:  "/tmp/media.img",
		LoopDevice: "/dev/loopfake",
		Plan:       plan,
		IsImage:    true,
		partition:  fp,
	}
}

// TestCreateP3Partition_SelfContained drives the full p3 stage with a fake seam: the reserved
// p3 entry is created, settled, formatted ext4 + labeled cluster-config, mounted, and the
// mountpoint receives the p3Root tree byte-for-byte; the partition is unmounted even on the
// success path.
func TestCreateP3Partition_SelfContained(t *testing.T) {
	p3Root := writeTree(t, t.TempDir(), map[string]string{
		filepath.Join("cluster-configuration", "manifest.json"):       `{"apiVersion":1}`,
		filepath.Join("cluster-configuration", "manifests", "a.yaml"): "kind: A\n",
		filepath.Join("cluster-configuration", "manifests", "b.yaml"): "kind: B\n",
		filepath.Join("extra-manifests", "extra.yaml"):                "kind: Extra\n",
	})

	fp := &fakePartitionOps{}
	handle := selfContainedHandle(fp)

	err := createP3Partition(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, handle, p3Root)
	require.NoError(t, err)

	// The seam was driven in the required order, and the partition was unmounted on success.
	assert.Equal(t, []string{"createPart", "settle", "format", "mount", "umount"}, fp.calls)

	// The reserved p3 slot (number 3, label cluster-config) was the entry created.
	require.Len(t, fp.createdParts, 1)
	wantP3, ok := plannedPartition(handle.Plan, labelClusterConfig)
	require.True(t, ok)
	assert.Equal(t, wantP3, fp.createdParts[0])
	assert.Equal(t, 3, fp.createdParts[0].Number)

	// The filesystem was ext4-labeled cluster-config on the settled partition device
	// (the fake returns <dev>p<number>, matching the loopback partition-node form).
	assert.Equal(t, "/dev/loopfakep3", fp.formattedDev)
	assert.Equal(t, labelClusterConfig, fp.formatLabel)
	assert.Equal(t, "/dev/loopfakep3", fp.mountedDev)

	// The populated tree matches the source byte-for-byte with identical relative layout.
	assert.Equal(t, snapshotTree(p3Root), fp.mountSnapshot)
}

// TestCreateP3Partition_InstallOnlySkipped verifies the stage is a no-op for a plan that
// reserves no p3 entry (install-only): no privileged op runs and it returns nil.
func TestCreateP3Partition_InstallOnlySkipped(t *testing.T) {
	fp := &fakePartitionOps{}
	handle := &MediaHandle{
		LoopDevice: "/dev/loopfake",
		Plan:       partitionPlan(ModeInstallOnly, defaultP1ReservationBytes),
		IsImage:    true,
		partition:  fp,
	}

	// A p3Root is supplied to prove the skip is driven by the plan, not by a missing tree.
	p3Root := writeTree(t, t.TempDir(), map[string]string{"cluster-configuration/manifest.json": "{}"})

	err := createP3Partition(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, handle, p3Root)
	require.NoError(t, err)
	assert.Empty(t, fp.calls, "install-only must not touch the partition seam")
}

// TestCreateP3Partition_NoTreeCreatesPlaceholder verifies that self-contained media
// includes a formatted p3 slot even before its config tree is available.
func TestCreateP3Partition_NoTreeCreatesPlaceholder(t *testing.T) {
	fp := &fakePartitionOps{}
	handle := selfContainedHandle(fp)

	err := createP3Partition(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, handle, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"createPart", "settle", "format"}, fp.calls)
	assert.Equal(t, labelClusterConfig, fp.formatLabel)
	assert.NotContains(t, fp.calls, "mount")
}

// TestCreateP3Partition_PopulateFilePolicy covers the source-tree validation: a regular-file
// tree is accepted and copied with identical layout/content, while a symlink, an oversized
// tree, a missing p3Root, and an empty p3Root are each rejected loudly before any partition is
// created.
func TestCreateP3Partition_PopulateFilePolicy(t *testing.T) {
	t.Run("regular-file tree is accepted and copied verbatim", func(t *testing.T) {
		p3Root := writeTree(t, t.TempDir(), map[string]string{
			filepath.Join("cluster-configuration", "manifest.json"): `{"apiVersion":1}`,
			filepath.Join("extra-manifests", "nested", "x.yaml"):    "kind: X\n",
		})
		fp := &fakePartitionOps{}
		err := createP3Partition(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), p3Root)
		require.NoError(t, err)
		assert.Equal(t, snapshotTree(p3Root), fp.mountSnapshot)
	})

	t.Run("rejects a symlink in the tree", func(t *testing.T) {
		p3Root := writeTree(t, t.TempDir(), map[string]string{
			filepath.Join("cluster-configuration", "manifest.json"): "{}",
		})
		require.NoError(t, os.Symlink("/etc/passwd", filepath.Join(p3Root, "link")))
		fp := &fakePartitionOps{}
		err := createP3Partition(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), p3Root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a regular file")
		assert.Empty(t, fp.calls, "the file policy must be enforced before any partition op")
	})

	t.Run("rejects a tree larger than the reserved partition", func(t *testing.T) {
		p3Root := t.TempDir()
		// A sparse file whose logical size exceeds p3ReservationBytes: os.Truncate reports the
		// size without consuming the bytes, so the budget guard trips without a real 256 MiB file.
		big := filepath.Join(p3Root, "big.bin")
		f, err := os.Create(big) //nolint:gosec // test-controlled path under a temp dir
		require.NoError(t, err)
		require.NoError(t, f.Truncate(p3ReservationBytes+1))
		require.NoError(t, f.Close())

		fp := &fakePartitionOps{}
		err = createP3Partition(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), p3Root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeding the reserved")
		assert.Empty(t, fp.calls)
	})

	t.Run("rejects a missing p3Root", func(t *testing.T) {
		fp := &fakePartitionOps{}
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		err := createP3Partition(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), missing)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "accessing p3 config tree")
		assert.Empty(t, fp.calls)
	})

	t.Run("rejects an empty p3Root directory", func(t *testing.T) {
		fp := &fakePartitionOps{}
		err := createP3Partition(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is empty")
		assert.Empty(t, fp.calls)
	})
}

// TestCreateP3Partition_StepFailuresAndCleanup verifies that a failure injected at each
// privileged step surfaces the original error, that the partition is unmounted whenever it was
// mounted (and only then), and that an unmount (cleanup) failure is aggregated rather than
// swallowed.
func TestCreateP3Partition_StepFailuresAndCleanup(t *testing.T) {
	newTree := func(t *testing.T) string {
		t.Helper()
		return writeTree(t, t.TempDir(), map[string]string{
			filepath.Join("cluster-configuration", "manifest.json"): "{}",
		})
	}

	t.Run("create-entry failure surfaces and never mounts", func(t *testing.T) {
		fp := &fakePartitionOps{createPartErr: fmt.Errorf("sgdisk boom")}
		err := createP3Partition(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), newTree(t))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sgdisk boom")
		assert.Equal(t, []string{"createPart"}, fp.calls)
	})

	t.Run("settle failure surfaces and never mounts", func(t *testing.T) {
		fp := &fakePartitionOps{settleErr: fmt.Errorf("partprobe boom")}
		err := createP3Partition(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), newTree(t))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "partprobe boom")
		assert.Equal(t, []string{"createPart", "settle"}, fp.calls)
	})

	t.Run("format failure surfaces and never mounts", func(t *testing.T) {
		fp := &fakePartitionOps{formatErr: fmt.Errorf("mkfs boom")}
		err := createP3Partition(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), newTree(t))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mkfs boom")
		assert.Equal(t, []string{"createPart", "settle", "format"}, fp.calls)
	})

	t.Run("mount failure surfaces and does not unmount", func(t *testing.T) {
		fp := &fakePartitionOps{mountErr: fmt.Errorf("mount boom")}
		err := createP3Partition(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), newTree(t))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mount boom")
		assert.Equal(t, []string{"createPart", "settle", "format", "mount"}, fp.calls)
		assert.NotContains(t, fp.calls, "umount", "a partition that never mounted must not be unmounted")
	})

	t.Run("populate failure still unmounts and surfaces the original error", func(t *testing.T) {
		// A file in the source clashes with a directory the fake pre-creates in the mountpoint on
		// mount, so copyFileVerbatim fails writing to a directory path -- a deterministic populate
		// failure after a successful mount, regardless of the test's uid.
		p3Root := writeTree(t, t.TempDir(), map[string]string{"clash": "data"})
		fp := &fakePartitionOps{
			onMount: func(mountpoint string) {
				_ = os.MkdirAll(filepath.Join(mountpoint, "clash"), 0o750)
			},
		}
		err := createP3Partition(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), p3Root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "populating cluster-config (p3) partition")
		assert.Equal(t, []string{"createPart", "settle", "format", "mount", "umount"}, fp.calls,
			"a mounted partition must be unmounted even when populate fails")
	})

	t.Run("unmount failure is aggregated with a successful populate", func(t *testing.T) {
		fp := &fakePartitionOps{umountErr: fmt.Errorf("umount boom")}
		err := createP3Partition(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), newTree(t))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "umount boom")
		assert.Contains(t, err.Error(), "unmounting cluster-config (p3) partition")
		assert.Equal(t, []string{"createPart", "settle", "format", "mount", "umount"}, fp.calls)
	})
}

// TestCreateP3Partition_ContextCanceled verifies the stage honors a canceled context before
// doing any work.
func TestCreateP3Partition_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fp := &fakePartitionOps{}
	err := createP3Partition(ctx, testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "canceled")
	assert.Empty(t, fp.calls)
}
