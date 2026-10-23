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

// Integration coverage for the metadata-preserving copier (copyStagedTree) that writes the staged
// ibi-images payload onto the formatted ext4 partition (developer-of-record directive on PR #10013).
// The faked-seam unit tests copy a regular-file tree into a plain temp dir and cannot establish that
// the real durability store -- a containers/storage overlay store carrying symlinks, hardlinks,
// numeric ownership, xattrs and special mode bits -- survives the copy onto a real ext4 filesystem.
//
// Two gates, matching the rest of the package's integration tests:
//   - metadata fidelity is proven directly, by staging each tricky entry type and asserting it on the
//     ext4 destination (a symlink-only fixture is insufficient); and
//   - a real overlay durability store is built, copied onto ext4 with copyStagedTree, remounted
//     read-only, and opened as a read-only additional image store -- the node's consumption path --
//     proving the copied store is not merely present but usable.
//
// Both are root-gated (losetup + mkfs.ext4 + mount, and the overlay layer chown) and target only a
// loopback-attached sparse .img, never a real block device. TestMain (reexec.Init) is defined in
// createusb_images_integration_test.go for the whole package.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"go.podman.io/image/v5/oci/layout"
	istorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	cstorage "go.podman.io/storage"
	"go.podman.io/storage/pkg/system"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/localimages"
)

// mountExt4Loop creates a sparse image, attaches it to a loop device, formats it ext4, and mounts it,
// registering cleanups (umount then detach) with t.Cleanup. It returns the mountpoint. It requires
// root and the loop/mkfs/mount tools (callers gate with requireAssemblyTools).
func mountExt4Loop(t *testing.T) string {
	t.Helper()
	img := filepath.Join(t.TempDir(), "ext4.img")
	f, err := os.Create(img) //nolint:gosec // test-only path under a temp dir
	require.NoError(t, err)
	require.NoError(t, f.Truncate(64<<20)) // 64 MiB is ample for the staged fixtures / small store
	require.NoError(t, f.Close())

	out, err := exec.Command("losetup", "--find", "--show", img).CombinedOutput() //nolint:gosec // test-only path
	require.NoError(t, err, string(out))
	loopDev := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("losetup", "-d", loopDev).Run() }) //nolint:gosec // fixed device path

	mkfs, err := exec.Command("mkfs.ext4", "-F", "-q", loopDev).CombinedOutput() //nolint:gosec // fixed device path
	require.NoError(t, err, string(mkfs))

	mnt := t.TempDir()
	mout, err := exec.Command("mount", loopDev, mnt).CombinedOutput() //nolint:gosec // fixed device + temp mountpoint
	require.NoError(t, err, string(mout))
	t.Cleanup(func() { _ = exec.Command("umount", mnt).Run() }) //nolint:gosec // fixed temp mountpoint
	return mnt
}

// inodeOf returns the inode number of path (via lstat, so symlinks are not followed).
func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Lstat(path)
	require.NoError(t, err)
	st, ok := fi.Sys().(*syscall.Stat_t)
	require.True(t, ok, "expected *syscall.Stat_t for %q", path)
	return st.Ino
}

// lstatOf returns the raw stat of path (via lstat).
func lstatOf(t *testing.T, path string) *syscall.Stat_t {
	t.Helper()
	fi, err := os.Lstat(path)
	require.NoError(t, err)
	st, ok := fi.Sys().(*syscall.Stat_t)
	require.True(t, ok, "expected *syscall.Stat_t for %q", path)
	return st
}

// TestCopyStagedTreePreservesMetadataIntegration stages one of each tricky entry type the durability
// store can carry -- a symlink, a hardlink pair, a setuid file, a user xattr, numeric ownership, and a
// FIFO special file -- copies the tree onto a real ext4 partition with copyStagedTree, and asserts
// every attribute survives. This is the direct proof that the metadata-preserving copier replaced the
// regular-files-only copier correctly; a symlink-only fixture would not establish it.
func TestCopyStagedTreePreservesMetadataIntegration(t *testing.T) {
	requireAssemblyTools(t) // root + losetup/mkfs.ext4/mount/umount
	mnt := mountExt4Loop(t)

	src := t.TempDir()
	// A regular executable file (the staged lca-cli binary analogue).
	require.NoError(t, os.WriteFile(filepath.Join(src, "exec"), []byte("#!/bin/true\n"), 0o755))
	// A setuid file: the special mode bit must survive the copy.
	suid := filepath.Join(src, "suid")
	require.NoError(t, os.WriteFile(suid, []byte("suid-bytes"), 0o600))
	require.NoError(t, os.Chmod(suid, 0o600|os.ModeSetuid))
	// A symlink (the overlay layer link farm is symlinks).
	require.NoError(t, os.Symlink("exec", filepath.Join(src, "link")))
	// A hardlink pair sharing one inode.
	orig := filepath.Join(src, "orig")
	require.NoError(t, os.WriteFile(orig, []byte("hardlinked-content"), 0o640))
	require.NoError(t, os.Link(orig, filepath.Join(src, "hard")))
	// A user xattr (capabilities/IMA/user.* are the xattrs the copier carries).
	require.NoError(t, system.Lsetxattr(orig, "user.lca.test", []byte("xattr-value"), 0))
	// Numeric ownership (set last so the chown does not clear anything we set above).
	require.NoError(t, os.Lchown(orig, 12345, 12345))
	// A FIFO special file.
	require.NoError(t, syscall.Mkfifo(filepath.Join(src, "fifo"), 0o644)) //nolint:gosec // test-only fixture

	require.NoError(t, copyStagedTree(src, mnt))

	// Symlink preserved (not dereferenced), pointing at the same target.
	linkInfo, err := os.Lstat(filepath.Join(mnt, "link"))
	require.NoError(t, err)
	assert.NotZero(t, linkInfo.Mode()&os.ModeSymlink, "symlink must be copied as a symlink, not dereferenced")
	tgt, err := os.Readlink(filepath.Join(mnt, "link"))
	require.NoError(t, err)
	assert.Equal(t, "exec", tgt)

	// Executable bit preserved.
	execInfo, err := os.Lstat(filepath.Join(mnt, "exec"))
	require.NoError(t, err)
	assert.NotZero(t, execInfo.Mode()&0o111, "executable mode must be preserved")

	// Setuid bit preserved.
	suidInfo, err := os.Lstat(filepath.Join(mnt, "suid"))
	require.NoError(t, err)
	assert.NotZero(t, suidInfo.Mode()&os.ModeSetuid, "setuid special mode bit must be preserved")

	// Hardlink preserved: the two names share one inode on the destination.
	assert.Equal(t, inodeOf(t, filepath.Join(mnt, "orig")), inodeOf(t, filepath.Join(mnt, "hard")),
		"hardlinked files must share an inode after the copy")

	// Numeric ownership preserved.
	ownSt := lstatOf(t, filepath.Join(mnt, "orig"))
	assert.Equal(t, uint32(12345), ownSt.Uid, "numeric uid must be preserved")
	assert.Equal(t, uint32(12345), ownSt.Gid, "numeric gid must be preserved")

	// user.* xattr preserved.
	gotXattr, err := system.Lgetxattr(filepath.Join(mnt, "orig"), "user.lca.test")
	require.NoError(t, err)
	assert.Equal(t, []byte("xattr-value"), gotXattr, "user.* xattr must be preserved")

	// FIFO special file preserved.
	fifoInfo, err := os.Lstat(filepath.Join(mnt, "fifo"))
	require.NoError(t, err)
	assert.NotZero(t, fifoInfo.Mode()&os.ModeNamedPipe, "FIFO special file must be preserved")
}

// TestCopiedDurabilityStoreReadableAsAdditionalImageStoreIntegration is the developer-of-record's
// exact directive for the store copy: build a real overlay durability store, copy it onto a mounted
// ext4 partition with copyStagedTree, remount the partition read-only, and open the copied store as a
// read-only additional image store -- the node's consumption path. It proves the metadata-preserving
// copier yields a store that is not merely present on ext4 but genuinely usable after the copy.
// TestDurabilityStoreReadableAsAdditionalImageStoreIntegration covers the same consumer gate from the
// build location; this covers it from the copied, read-only ext4 location.
func TestCopiedDurabilityStoreReadableAsAdditionalImageStoreIntegration(t *testing.T) {
	requireAssemblyTools(t) // root + losetup/mkfs.ext4/mount/umount for the ext4 destination

	// Stage a real OCI layout + digests.json under the staging root, as BuildImageLayout does.
	staging := t.TempDir()
	srcDir := buildMultiArchSourceLayout(t, []string{runtime.GOARCH}, "src")
	srcRef, err := layout.NewReference(srcDir, "src")
	require.NoError(t, err)

	destLayout := filepath.Join(staging, integrationLayoutDir)
	mirrored, err := copyIntoLayout(context.Background(), testLogger(), srcRef, destLayout, integrationOCITag, "")
	require.NoError(t, err)
	recordedDigest := mirrored.digest

	entry := localimages.ImageEntry{Name: integrationImageName, Digest: recordedDigest, OCITag: integrationOCITag}
	manifest := localimages.DigestManifest{Version: digestManifestVersion, OCILayoutPath: integrationLayoutDir, Images: []localimages.ImageEntry{entry}}
	writeJSON(t, filepath.Join(staging, digestManifestFileName), manifest)

	// Probe overlay availability up front, so a later build/read error is a genuine regression.
	requireOverlayStore(t)

	// The store will finally live at <mount>/store once copied, so render the drop-in against that
	// path -- the absolute path the node mounts p2 at and opens the store from.
	mnt := mountExt4Loop(t)
	runtimeStorePath := filepath.Join(mnt, durabilityStoreDirName)

	// Build the store under the staging root (committed to <staging>/store); with overlay confirmed
	// available, a build error is a real regression.
	res, err := BuildDurabilityStore(context.Background(), testLogger(), &Manifest{Mode: ModeSelfContained}, staging, runtimeStorePath, runtime.GOARCH)
	require.NoError(t, err, "overlay is available, so a build error is a real regression")
	require.NotNil(t, res)
	require.Equal(t, filepath.Join(staging, durabilityStoreDirName), res.StorePath)

	// Copy the staged tree (OCI layout + digest manifest + store) onto the ext4 partition with the
	// metadata-preserving copier under test, so the store lands at <mount>/store.
	require.NoError(t, copyStagedTree(staging, mnt))
	require.DirExists(t, runtimeStorePath)

	// Remount the partition read-only, exactly as the node mounts the ibi-images partition.
	remount, err := exec.Command("mount", "-o", "remount,ro", mnt).CombinedOutput() //nolint:gosec // fixed temp mountpoint
	require.NoError(t, err, string(remount))

	// The rendered drop-in registers the copied store at its final (read-only) path.
	driver, additionalStores := parseRenderedStorageConf(t, res.StorageConfPath)
	require.Equal(t, durabilityStoreDriver, driver)
	require.Equal(t, []string{runtimeStorePath}, additionalStores, "the drop-in must register the store at its final copied path")

	// Open a throwaway, empty primary overlay store with the copied, read-only store attached as a
	// read-only additional image store -- how the node consumes the media.
	store, err := cstorage.GetStore(cstorage.StoreOptions{
		GraphDriverName:    driver,
		GraphRoot:          filepath.Join(t.TempDir(), "node-graph"),
		RunRoot:            filepath.Join(t.TempDir(), "node-run"),
		GraphDriverOptions: []string{"imagestore=" + additionalStores[0]},
	})
	require.NoError(t, err, "opening the copied store as a read-only additional image store is the consumer-side gate")
	defer func() {
		if _, err := store.Shutdown(false); err != nil {
			t.Logf("shutting down read-only store: %v", err)
		}
	}()

	// The imported image resolves from the copied read-only additional store under its canonical
	// name@digest -- the exact reference the node asks for offline.
	canonical := integrationImageName + "@" + recordedDigest
	storeRef, err := istorage.Transport.ParseStoreReference(store, canonical)
	require.NoError(t, err)
	_, storedImage, err := istorage.ResolveReference(storeRef)
	require.NoError(t, err, "canonical name@digest must resolve from the copied read-only additional image store")
	require.NotNil(t, storedImage)

	// ...and its image is genuinely readable through that store, not just indexed.
	img, err := storeRef.NewImage(context.Background(), &types.SystemContext{})
	require.NoError(t, err, "the imported image must be openable from the copied read-only additional image store")
	defer img.Close()
	manifestBytes, _, err := img.Manifest(context.Background())
	require.NoError(t, err, "the imported image's manifest must be readable after the copy")
	require.NotEmpty(t, manifestBytes)
	configBlob, err := img.ConfigBlob(context.Background())
	require.NoError(t, err, "the imported image's config must be readable after the copy")
	require.NotEmpty(t, configBlob)
}
