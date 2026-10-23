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

// writeTree creates the given rel-path->content files (with any parent directories) under root
// and returns root, for building a representative cluster-config tree in tests.
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
// both by the fake seam (to capture the populated mountpoint before a simulated unmount) and by
// tests to build the expected tree from a source directory.
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

// clearDir removes every entry inside dir (but not dir itself), simulating that an unmount leaves
// the mountpoint directory empty.
func clearDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// selfContainedHandle returns a MediaHandle wired to fp with a self-contained plan (which includes
// the cluster-config partition), as assembleMedia would hand to the populate stage.
func selfContainedHandle(fp *fakePartitionOps) *MediaHandle {
	return &MediaHandle{
		ImagePath:  "/tmp/media.img",
		LoopDevice: "/dev/loopfake",
		Plan:       partitionPlan(ModeSelfContained),
		IsImage:    true,
		partition:  fp,
	}
}

// TestPopulateClusterConfig_SelfContained drives the full populate stage with a fake seam: the
// cluster-config partition is resolved by filesystem label, mounted, and the mountpoint receives
// the config tree byte-for-byte; the partition is unmounted even on the success path.
func TestPopulateClusterConfig_SelfContained(t *testing.T) {
	p3Root := writeTree(t, t.TempDir(), map[string]string{
		filepath.Join("cluster-configuration", "manifest.json"):       `{"apiVersion":1}`,
		filepath.Join("cluster-configuration", "manifests", "a.yaml"): "kind: A\n",
		filepath.Join("cluster-configuration", "manifests", "b.yaml"): "kind: B\n",
		filepath.Join("extra-manifests", "extra.yaml"):                "kind: Extra\n",
	})

	fp := &fakePartitionOps{resolveDev: "/dev/loopfakep5"}
	handle := selfContainedHandle(fp)

	err := populateClusterConfig(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, handle, p3Root)
	require.NoError(t, err)

	assert.Equal(t, []string{"resolve", "mount", "umount"}, fp.calls)
	assert.Equal(t, "/dev/loopfakep5", fp.mountedDev, "cluster-config is mounted on its resolved node")
	assert.Equal(t, snapshotTree(p3Root), fp.mountSnapshot)
}

// TestPopulateClusterConfig_InstallOnlySkipped verifies the stage is a no-op for a plan that has no
// cluster-config partition (install-only): no privileged op runs and it returns nil.
func TestPopulateClusterConfig_InstallOnlySkipped(t *testing.T) {
	fp := &fakePartitionOps{}
	handle := &MediaHandle{
		LoopDevice: "/dev/loopfake",
		Plan:       partitionPlan(ModeInstallOnly),
		IsImage:    true,
		partition:  fp,
	}

	// A p3Root is supplied to prove the skip is driven by the plan, not by a missing tree.
	p3Root := writeTree(t, t.TempDir(), map[string]string{"cluster-configuration/manifest.json": "{}"})

	err := populateClusterConfig(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, handle, p3Root)
	require.NoError(t, err)
	assert.Empty(t, fp.calls, "install-only must not touch the partition seam")
}

// TestPopulateClusterConfig_NoTreeLeavesEmpty verifies that self-contained media with no config
// tree keeps the formatted-but-empty cluster-config partition without mounting it.
func TestPopulateClusterConfig_NoTreeLeavesEmpty(t *testing.T) {
	fp := &fakePartitionOps{}
	handle := selfContainedHandle(fp)

	err := populateClusterConfig(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, handle, "")
	require.NoError(t, err)
	assert.Empty(t, fp.calls, "an empty tree needs no resolve/mount/populate")
}

// TestPopulateClusterConfig_FilePolicy covers the source-tree validation: a regular-file tree is
// accepted and copied with identical layout/content, while a symlink, an oversized tree, a missing
// p3Root, and an empty p3Root are each rejected loudly before the partition is resolved or mounted.
func TestPopulateClusterConfig_FilePolicy(t *testing.T) {
	t.Run("regular-file tree is accepted and copied verbatim", func(t *testing.T) {
		p3Root := writeTree(t, t.TempDir(), map[string]string{
			filepath.Join("cluster-configuration", "manifest.json"): `{"apiVersion":1}`,
			filepath.Join("extra-manifests", "nested", "x.yaml"):    "kind: X\n",
		})
		fp := &fakePartitionOps{}
		err := populateClusterConfig(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), p3Root)
		require.NoError(t, err)
		assert.Equal(t, snapshotTree(p3Root), fp.mountSnapshot)
	})

	t.Run("rejects a symlink in the tree", func(t *testing.T) {
		p3Root := writeTree(t, t.TempDir(), map[string]string{
			filepath.Join("cluster-configuration", "manifest.json"): "{}",
		})
		require.NoError(t, os.Symlink("/etc/passwd", filepath.Join(p3Root, "link")))
		fp := &fakePartitionOps{}
		err := populateClusterConfig(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), p3Root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a regular file")
		assert.Empty(t, fp.calls, "the file policy must be enforced before any partition op")
	})

	t.Run("rejects a tree larger than the reserved partition", func(t *testing.T) {
		p3Root := t.TempDir()
		big := filepath.Join(p3Root, "big.bin")
		f, err := os.Create(big) //nolint:gosec // test-controlled path under a temp dir
		require.NoError(t, err)
		require.NoError(t, f.Truncate(p3ReservationBytes+1))
		require.NoError(t, f.Close())

		fp := &fakePartitionOps{}
		err = populateClusterConfig(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), p3Root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeding the reserved")
		assert.Empty(t, fp.calls)
	})

	t.Run("rejects a missing p3Root", func(t *testing.T) {
		fp := &fakePartitionOps{}
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		err := populateClusterConfig(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), missing)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "accessing p3 config tree")
		assert.Empty(t, fp.calls)
	})

	t.Run("rejects an empty p3Root directory", func(t *testing.T) {
		fp := &fakePartitionOps{}
		err := populateClusterConfig(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is empty")
		assert.Empty(t, fp.calls)
	})
}

// TestPopulateClusterConfig_StepFailuresAndCleanup verifies that a failure injected at each
// privileged step surfaces the original error, that the partition is unmounted whenever it was
// mounted (and only then), and that an unmount (cleanup) failure is aggregated (tagged errP3Mounted)
// rather than swallowed.
func TestPopulateClusterConfig_StepFailuresAndCleanup(t *testing.T) {
	newTree := func(t *testing.T) string {
		t.Helper()
		return writeTree(t, t.TempDir(), map[string]string{
			filepath.Join("cluster-configuration", "manifest.json"): "{}",
		})
	}

	t.Run("resolve failure surfaces and never mounts", func(t *testing.T) {
		fp := &fakePartitionOps{resolveErr: fmt.Errorf("blkid boom")}
		err := populateClusterConfig(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), newTree(t))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "blkid boom")
		assert.Equal(t, []string{"resolve"}, fp.calls)
	})

	t.Run("mount failure surfaces and does not unmount", func(t *testing.T) {
		fp := &fakePartitionOps{mountErr: fmt.Errorf("mount boom")}
		err := populateClusterConfig(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), newTree(t))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mount boom")
		assert.Equal(t, []string{"resolve", "mount"}, fp.calls)
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
		err := populateClusterConfig(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), p3Root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "populating cluster-config partition")
		assert.Equal(t, []string{"resolve", "mount", "umount"}, fp.calls,
			"a mounted partition must be unmounted even when populate fails")
	})

	t.Run("unmount failure is aggregated with a successful populate", func(t *testing.T) {
		fp := &fakePartitionOps{umountErr: fmt.Errorf("umount boom")}
		err := populateClusterConfig(context.Background(), testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), newTree(t))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "umount boom")
		assert.Contains(t, err.Error(), "unmounting cluster-config partition")
		assert.ErrorIs(t, err, errP3Mounted)
		assert.Equal(t, []string{"resolve", "mount", "umount"}, fp.calls)
	})
}

// TestPopulateClusterConfig_ContextCanceled verifies the stage honors a canceled context before
// doing any work.
func TestPopulateClusterConfig_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fp := &fakePartitionOps{}
	err := populateClusterConfig(ctx, testLogger(), mediaBuildDeps{partition: fp}, selfContainedHandle(fp), t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "canceled")
	assert.Empty(t, fp.calls)
}
