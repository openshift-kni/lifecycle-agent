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
// # Populate stage (CNF-27171)
//
// populateIBIImages fills the formatted ibi-images partition: it resolves it by filesystem label,
// mounts it, and stages (1) the build-unique OCI image layout + on-media digests.json via
// BuildImageLayout (createusb_images.go, CNF-27167/CNF-27169), (2) the exact shipping lca-cli
// binary the embedded ignition's lca-ibi.service execs on boot (common.LocalImagesLCABinaryPath),
// and (3) for self-contained media, the read-only containers/storage durability store via
// BuildDurabilityStore (createusb_p2_content.go, CNF-27170). It then unmounts before the caller
// detaches the loop.
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
	"os"
	"path/filepath"
	"runtime"

	"github.com/sirupsen/logrus"

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

// populateIBIImages fills the formatted ibi-images partition on the media handle: it resolves the
// partition by filesystem label, mounts it, and stages the OCI image layout + digests.json, the
// shipping lca-cli binary, and (self-contained) the read-only durability store, then unmounts.
//
// It runs before assembleMedia marks success, so any error here triggers the caller's rollback. An
// unmount failure is tagged errP2Mounted so the rollback preserves the still-mounted image + loop
// handle instead of tearing them down (matching populateClusterConfig / errP3Mounted).
func populateIBIImages(ctx context.Context, log *logrus.Logger, deps mediaBuildDeps, media *MediaHandle, m *Manifest, isoResult ISOBuildResult, opts BuildOptions, p3Root string) (err error) {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("ibi-images populate canceled: %w", cerr)
	}

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

	// 1. Mirror the OCI image layout + commit digests.json into the partition (and, for
	//    self-contained, render the digest-pinned p3 config tree under p3Root), and write the
	//    <output>.digests.json sidecar beside the output image.
	layout, err := deps.images.BuildLayout(ctx, log, m, ImageSetOptions{
		AuthFile:    opts.AuthFile,
		RecertImage: opts.RecertImage,
		LCAImage:    opts.LCAImage,
	}, opts.OutputPath, mountpoint, p3Root)
	if err != nil {
		return fmt.Errorf("building the on-media OCI image layout on ibi-images: %w", err)
	}
	log.Infof("create-usb: staged %d images (%d bytes) and digests.json into ibi-images",
		len(layout.Images), layout.Size.StagedBytes)

	// 2. Stage the exact shipping lca-cli binary the embedded unit execs on boot.
	if err := stageLCABinary(log, isoResult.LCABinaryPath, mountpoint); err != nil {
		return err
	}

	// 3. Self-contained only: build the read-only durability store under <p2>/store from the same
	//    digest-pinned closure. install-only is a no-op.
	//
	//    The store is built for the build host's architecture (runtime.GOARCH): the manifest carries
	//    no target-node architecture, so cross-architecture media (e.g. an amd64 workstation building
	//    for an arm64 node) is not supported here — selecting an explicit target architecture is
	//    follow-up work. Same-architecture builds (the common case) are correct.
	if m.Mode == ModeSelfContained {
		store, serr := deps.images.BuildStore(ctx, log, m, mountpoint, common.LocalImagesStorePath, runtime.GOARCH)
		if serr != nil {
			return fmt.Errorf("building the on-media durability store on ibi-images: %w", serr)
		}
		if store != nil {
			log.Infof("create-usb: staged durability store (%d images, %d bytes) at %s",
				store.ImportedImages, store.StoreBytes, store.StorePath)
		}
	}

	return nil
}

// stageLCABinary copies the extracted shipping lca-cli binary onto the mounted ibi-images partition
// at common.LocalImagesLCABinaryPath (i.e. <p2Root>/lca-cli) with an executable mode, so the
// embedded lca-ibi.service's ExecStart resolves and runs on boot.
func stageLCABinary(log *logrus.Logger, srcPath, p2Root string) error {
	if srcPath == "" {
		return errors.New("the live-ISO build did not return an extracted lca-cli binary to stage on ibi-images")
	}
	dst := filepath.Join(p2Root, filepath.Base(common.LocalImagesLCABinaryPath))
	if err := copyFileExecutable(srcPath, dst); err != nil {
		return fmt.Errorf("staging lca-cli binary onto ibi-images at %s: %w", dst, err)
	}
	log.Infof("create-usb: staged lca-cli binary at %s (mode %#o)", dst, lcaBinaryMode)
	return nil
}

// copyFileExecutable streams src to dst with an executable mode (lcaBinaryMode). It streams rather
// than ReadFile/WriteFile because the lca-cli binary is large (~90 MB), so loading it entirely into
// memory would be wasteful.
func copyFileExecutable(src, dst string) (err error) {
	in, err := os.Open(filepath.Clean(src))
	if err != nil {
		return fmt.Errorf("opening %q: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	//nolint:gosec // dst is a tool-controlled mountpoint joined with a fixed basename; mode is required for exec on boot
	out, err := os.OpenFile(filepath.Clean(dst), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, lcaBinaryMode)
	if err != nil {
		return fmt.Errorf("creating %q: %w", dst, err)
	}
	if _, err = io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copying %q to %q: %w", src, dst, err)
	}
	// Ensure the executable mode survives any umask applied at create time.
	if cerr := out.Chmod(lcaBinaryMode); cerr != nil {
		_ = out.Close()
		return fmt.Errorf("setting executable mode on %q: %w", dst, cerr)
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
