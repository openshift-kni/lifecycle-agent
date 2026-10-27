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

// This file carries the p2 (ibi-images) filesystem policy and content-populate stage for the
// create-usb media-assembly pipeline (USB-based IBI proposal §6.4 step 5, CNF-28668 + CNF-27171).
// In the DOS/MBR append-after-ISO layout the ibi-images partition is appended after the live ISO
// and formatted + labelled by the shared format stage (formatDataPartitions in
// createusb_assemble.go); this file owns its filesystem type and the populate stage.
//
// # Stage-then-copy (CNF-27171)
//
// The ibi-images payload is staged into scratch first and copied onto the partition second, so the
// partition can be sized to the measured payload rather than a fixed reservation (developer-of-record
// directive):
//
//   - stageIBIImagesPayload builds the payload into a scratch directory: (1) the build-unique OCI
//     image layout + on-media digests.json via BuildImageLayout (createusb_images.go,
//     CNF-27167/CNF-27169), (2) the exact shipping lca-cli binary the embedded ignition's
//     lca-ibi.service execs on boot (common.LocalImagesLCABinaryPath), and (3) for self-contained
//     media the read-only containers/storage durability store via BuildDurabilityStore
//     (createusb_p2_content.go, CNF-27170). It returns the measured byte size so assembleMedia can
//     size the ibi-images partition (sizeIBIImagesPartition) before it is created.
//   - copyStagedIBIImages resolves the formatted partition by filesystem label, mounts it, verifies
//     the sized filesystem has room for the payload, copies the staged tree onto it (preserving file
//     modes so the staged lca-cli binary stays executable), and unmounts before the caller detaches
//     the loop.
//
// # Privileged-ops seam
//
// Resolve-by-label, mount, and umount go through the partitionOps seam, and the OCI-layout /
// durability-store builds go through the imagePopulator seam, so the ordering/lifecycle logic is
// unit-testable with fakes and only the real device + container tooling path is exercised by the
// root-gated integration tests.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/sirupsen/logrus"
	"go.podman.io/storage/pkg/archive"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
)

// ibiImagesFSType is the filesystem created on the ibi-images partition. ext4 is used because
// ibi-images is new create-usb media with no prepare/runtime filesystem constraint (prepare
// discovers it by filesystem label with kernel FS auto-detection, imposing no type requirement);
// it is the conservative default. The mkfs.xfs choice in the CNF-27197 throwaway spike script is
// not a type requirement, so the p2 story's ext4 default stands. It is formatted exec-capable (not
// noexec) so init_t can exec the staged lca-cli binary on boot.
const ibiImagesFSType = fsTypeExt4

// lcaBinaryMode is the file mode of the lca-cli binary staged on ibi-images. It must be executable:
// the embedded lca-ibi.service execs it directly from the mounted partition, so a non-exec mode (or
// a noexec mount) would fail ExecStart on boot.
const lcaBinaryMode = 0o755

// errP2Mounted signals that the ibi-images partition could not be unmounted, so its filesystem is
// still mounted over the loop device. Like errP3Mounted, the caller's rollback must NOT detach the
// loop or remove the backing image in this case -- doing so would discard the only path to recover
// or retry the unmount.
var errP2Mounted = errors.New("ibi-images (p2) partition is still mounted")

// stageIBIImagesPayload builds the ibi-images payload into a scratch directory (stagingRoot), not the
// media partition, so the partition can be sized to the measured payload before it is created. It
// stages the OCI image layout + digests.json (and, for self-contained, renders the digest-pinned p3
// config tree under p3Root), the shipping lca-cli binary, and (self-contained) the read-only
// durability store. It returns the measured on-media byte size of the staged tree. No partition op
// runs here; the staged tree is copied onto the formatted partition by copyStagedIBIImages.
func stageIBIImagesPayload(ctx context.Context, log *logrus.Logger, deps mediaBuildDeps, m *Manifest, isoResult ISOBuildResult, opts BuildOptions, stagingRoot, p3Root string) (int64, error) {
	if cerr := ctx.Err(); cerr != nil {
		return 0, fmt.Errorf("ibi-images staging canceled: %w", cerr)
	}

	// 1. Mirror the OCI image layout + commit digests.json into the staging root (and, for
	//    self-contained, render the digest-pinned p3 config tree under p3Root), and write the
	//    <output>.digests.json sidecar beside the output image.
	layout, err := deps.images.BuildLayout(ctx, log, m, ImageSetOptions{
		AuthFile:    opts.AuthFile,
		RecertImage: opts.RecertImage,
		LCAImage:    opts.LCAImage,
	}, opts.OutputPath, stagingRoot, p3Root)
	if err != nil {
		return 0, fmt.Errorf("building the on-media OCI image layout for ibi-images: %w", err)
	}
	log.Infof("create-usb: staged %d images (%d bytes) and digests.json for ibi-images",
		len(layout.Images), layout.Size.StagedBytes)

	// 2. Stage the exact shipping lca-cli binary the embedded unit execs on boot.
	if err := stageLCABinary(log, isoResult.LCABinaryPath, stagingRoot); err != nil {
		return 0, err
	}

	// 3. Self-contained only: build the read-only durability store under <stagingRoot>/store from the
	//    same digest-pinned closure. install-only is a no-op. The store's additionalimagestores point
	//    at the on-node runtime path (common.LocalImagesStorePath), not the staging location, so
	//    staging in scratch does not change the baked on-node reference.
	//
	//    The store is built for the build host's architecture (runtime.GOARCH): the manifest carries
	//    no target-node architecture, so cross-architecture media (e.g. an amd64 workstation building
	//    for an arm64 node) is not supported here — selecting an explicit target architecture is
	//    follow-up work. Same-architecture builds (the common case) are correct.
	if m.Mode == ModeSelfContained {
		store, serr := deps.images.BuildStore(ctx, log, m, stagingRoot, common.LocalImagesStorePath, runtime.GOARCH)
		if serr != nil {
			return 0, fmt.Errorf("building the on-media durability store for ibi-images: %w", serr)
		}
		if store != nil {
			log.Infof("create-usb: staged durability store (%d images, %d bytes) at %s",
				store.ImportedImages, store.StoreBytes, store.StorePath)
		}
	}

	payloadBytes, err := measureTree(stagingRoot)
	if err != nil {
		return 0, err
	}
	return payloadBytes, nil
}

// copyStagedIBIImages copies the staged ibi-images payload onto the formatted partition: it resolves
// the partition by filesystem label, mounts it, verifies the sized filesystem has room for the
// payload, copies the staged tree onto it with the metadata-preserving copier (so the staged lca-cli
// binary stays executable and the durability store's symlinks/ownership/xattrs survive), and unmounts.
//
// It runs before assembleMedia marks success, so any error here triggers the caller's rollback. An
// unmount failure is tagged errP2Mounted so the rollback preserves the still-mounted image + loop
// handle instead of tearing them down (matching populateClusterConfig / errP3Mounted).
func copyStagedIBIImages(log *logrus.Logger, deps mediaBuildDeps, media *MediaHandle, stagingRoot string, payloadBytes int64) (err error) {
	partDev, err := deps.partition.ResolvePartitionByFSLabel(media.LoopDevice, labelIBIImages)
	if err != nil {
		return fmt.Errorf("resolving ibi-images partition by label: %w", err)
	}

	mountpoint, err := os.MkdirTemp("", "create-usb-p2-")
	if err != nil {
		return fmt.Errorf("creating ibi-images mountpoint: %w", err)
	}
	// Remove the (empty, post-unmount) mountpoint directory last (defers run LIFO, after unmount).
	defer func() {
		if rerr := os.Remove(mountpoint); rerr != nil && !os.IsNotExist(rerr) {
			log.Warnf("create-usb: removing ibi-images mountpoint %s: %v", mountpoint, rerr)
		}
	}()

	if err := deps.partition.MountPartition(partDev, mountpoint); err != nil {
		return fmt.Errorf("mounting ibi-images partition %s at %s: %w", partDev, mountpoint, err)
	}
	// Always unmount before the loop device is detached (Close does not unmount). Aggregate a
	// cleanup failure into the returned error and tag it with errP2Mounted so the caller preserves
	// the still-mounted image + loop handle.
	defer func() {
		if uerr := deps.partition.UnmountPartition(mountpoint); uerr != nil {
			err = errors.Join(err, fmt.Errorf("unmounting ibi-images partition at %s: %w",
				mountpoint, errors.Join(errP2Mounted, uerr)))
		}
	}()

	// Verify the sized, formatted filesystem has room for the payload before copying, so an
	// undersized partition fails with a clear size error rather than a mid-copy ENOSPC.
	free, err := freeBytes(mountpoint)
	if err != nil {
		return err
	}
	if free < payloadBytes {
		return fmt.Errorf("ibi-images partition has %d bytes free but the staged payload is %d bytes; "+
			"the partition is undersized for the payload", free, payloadBytes)
	}

	if err := copyStagedTree(stagingRoot, mountpoint); err != nil {
		return fmt.Errorf("copying the staged ibi-images payload onto %s: %w", partDev, err)
	}
	log.Infof("create-usb: copied the ibi-images payload (%d bytes) onto %s", payloadBytes, partDev)
	return nil
}

// stageLCABinary copies the extracted shipping lca-cli binary into the ibi-images staging root at
// common.LocalImagesLCABinaryPath's basename (i.e. <stagingRoot>/lca-cli) with an executable mode, so
// that once the staged tree is copied onto the partition the embedded lca-ibi.service's ExecStart
// resolves and runs on boot.
func stageLCABinary(log *logrus.Logger, srcPath, stagingRoot string) error {
	if srcPath == "" {
		return errors.New("the live-ISO build did not return an extracted lca-cli binary to stage on ibi-images")
	}
	dst := filepath.Join(stagingRoot, filepath.Base(common.LocalImagesLCABinaryPath))
	if err := copyFilePreservingMode(srcPath, dst, lcaBinaryMode); err != nil {
		return fmt.Errorf("staging lca-cli binary onto ibi-images at %s: %w", dst, err)
	}
	log.Infof("create-usb: staged lca-cli binary at %s (mode %#o)", dst, lcaBinaryMode)
	return nil
}

// copyFilePreservingMode streams src to dst, creating dst with the given permission bits and syncing
// it to disk. It streams rather than ReadFile/WriteFile because some staged files (the lca-cli binary
// at ~90 MB, image-layout blobs) are large, so loading them entirely into memory would be wasteful.
func copyFilePreservingMode(src, dst string, perm os.FileMode) (err error) {
	in, err := os.Open(filepath.Clean(src))
	if err != nil {
		return fmt.Errorf("opening %q: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	//nolint:gosec // dst is a tool-controlled mountpoint/staging path joined with a staged-tree-relative path
	out, err := os.OpenFile(filepath.Clean(dst), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("creating %q: %w", dst, err)
	}
	if _, err = io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copying %q to %q: %w", src, dst, err)
	}
	// Ensure the mode survives any umask applied at create time (the lca-cli binary must stay
	// executable so init_t can exec it on boot).
	if cerr := out.Chmod(perm); cerr != nil {
		_ = out.Close()
		return fmt.Errorf("setting mode on %q: %w", dst, cerr)
	}
	if serr := out.Sync(); serr != nil {
		_ = out.Close()
		return fmt.Errorf("syncing %q: %w", dst, serr)
	}
	if cerr := out.Close(); cerr != nil {
		return fmt.Errorf("closing %q: %w", dst, cerr)
	}
	return nil
}

// measureTree returns the total apparent byte size of every non-directory entry under root --
// regular files by their content size and symlinks/special files by their (small) apparent size.
// It is used to size the ibi-images partition to its payload and to verify the formatted filesystem
// has room for the copy. The self-contained durability store is a containers/storage overlay store
// whose on-disk form carries symlinks (the overlay layer link farm) and may carry special files, so
// skipping non-regular entries would undercount the payload the metadata-preserving copier writes
// and risk a mid-copy ENOSPC; counting them keeps the size/free-space guards conservative.
func measureTree(root string) (int64, error) {
	var total int64
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// WalkDir does not follow symlinks, so d.Info() is the lstat of the entry itself: a symlink
		// reports the length of its target path, a special file reports zero.
		info, ierr := d.Info()
		if ierr != nil {
			return fmt.Errorf("stating staged payload entry %q: %w", path, ierr)
		}
		total += info.Size()
		return nil
	})
	if walkErr != nil {
		return 0, fmt.Errorf("measuring staged payload under %s: %w", root, walkErr)
	}
	return total, nil
}

// copyStagedTree copies the staged ibi-images payload under srcRoot onto dstRoot with a
// metadata-preserving archive copier (go.podman.io/storage/pkg/archive). The self-contained
// durability store is a containers/storage overlay store whose on-disk form includes symlinks
// (the overlay layer link farm), hardlinks, numeric ownership, xattrs and special mode bits; the
// earlier regular-files-only copier rejected those entries and would fail the copy of a real store.
// CopyWithTar streams the tree through a tar pipe, preserving symlinks (without following them),
// hardlinks, numeric ownership, capability/IMA/user.* xattrs, special mode bits
// (setuid/setgid/sticky) and device/FIFO nodes, and returns an error if a required attribute cannot
// be restored -- so a store that cannot be copied faithfully fails the build rather than shipping a
// subtly-broken store. It equally handles the OCI layout and the staged lca-cli binary (plain
// files/dirs, executable mode preserved).
//
// The in-process archiver (not the chroot variant) is used deliberately: the source is the tool's
// own staged tree, not untrusted input, so the chroot hardening is unnecessary and its
// CAP_SYS_CHROOT requirement would needlessly narrow where the copy can run.
func copyStagedTree(srcRoot, dstRoot string) error {
	if err := archive.NewDefaultArchiver().CopyWithTar(srcRoot, dstRoot); err != nil {
		return fmt.Errorf("copying staged ibi-images payload %q to %q (metadata-preserving): %w", srcRoot, dstRoot, err)
	}
	return nil
}

// freeBytes returns the number of bytes available on the filesystem that contains path (statfs
// f_bavail * f_bsize). It is used to verify the formatted ibi-images partition has room for the
// staged payload before copying, so an undersized partition fails with a clear size error.
func freeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("checking free space on %s: %w", path, err)
	}
	//nolint:gosec,unconvert // f_bavail/f_bsize are non-negative sizes; int64 keeps the product correct across arches
	return int64(st.Bavail) * int64(st.Bsize), nil
}
