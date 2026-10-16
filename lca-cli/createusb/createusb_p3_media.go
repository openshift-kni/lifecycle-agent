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

// This file implements the self-contained p3 cluster-config media stage (USB-based IBI
// proposal §6.4 step 5, split from CNF-27171 as CNF-28669). It is the media/partition side
// of p3; the config-tree content is rendered and digest-pinned separately by
// createusb_p3.go (RenderP3Tree) + createusb_images.go (CNF-27169).
//
// The reserved p3 slot (number 3, label cluster-config, 256 MiB) is laid out by the p1 seam
// (CNF-28667) in partitionPlan; install-only carries no p3 entry. This stage fills that slot:
// it creates the GPT entry, settles the kernel partition node, formats ext4, sets the FS
// label, mounts the partition, and populates it content-verbatim from the rendered p3 tree.
//
// # Privileged-ops seam
//
// Every privileged step (create entry, settle, mkfs, mount, umount) goes through the
// partitionOps seam so the ordering/lifecycle logic is unit-testable with a fake and only the
// real device path is exercised by the root-gated integration test. The pure populate (tree
// copy) is plain filesystem code.
//
// # Teardown
//
// MediaHandle.Close detaches the loop device but does NOT unmount, so this stage owns its own
// unmount: it defers an error-aggregated umount that always runs before the loop is detached.
//
// # Source-tree handoff
//
// p3Root is consumed read-only. Threading the rendered tree from the command into the assemble
// flow is the end-to-end join CNF-27171 wires; until then only the integration test supplies a
// p3Root, so an empty p3Root leaves the reserved slot unpopulated (logged, not an error). A
// p3Root that IS supplied but missing/empty fails loudly.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"
)

const (
	// p3DirMode is the directory mode used for directories created while populating p3.
	p3DirMode = 0o750

	// clusterConfigFSType is the filesystem created on the p3 (cluster-config) partition. ext4
	// holds the 14-char cluster-config label (vfat's 11-char limit could not) and post-pivot
	// locates p3 by label with kernel FS auto-detection, imposing no FS-type requirement.
	clusterConfigFSType = fsTypeExt4
)

// errP3Mounted signals that the cluster-config (p3) partition could not be unmounted, so its
// filesystem is still mounted over the loop device. The caller's rollback must NOT detach the
// loop or remove the backing image in this case: while the filesystem is mounted, tearing the
// loop/image down would discard the only path to recover or retry the unmount. assembleMedia's
// rollback checks for this via errors.Is and preserves the image + loop handle instead.
var errP3Mounted = errors.New("cluster-config (p3) partition is still mounted")

// createP3Partition fills the reserved self-contained p3 cluster-config slot on the media
// handle: it creates the GPT entry, settles the kernel node, formats an ext4 filesystem,
// labels it cluster-config, mounts it, and populates it content-verbatim from p3Root.
//
// It is a no-op when the plan carries no p3 entry (install-only). When the plan has a p3 entry
// but p3Root is empty, it skips with a warning: the end-to-end render/thread join is CNF-27171
// and nothing supplies the tree in the live command path yet. A p3Root that IS supplied but
// missing, not a directory, empty, oversized for the reserved region, or containing a
// non-regular file fails loudly.
func createP3Partition(ctx context.Context, log *logrus.Logger, deps mediaBuildDeps, media *MediaHandle, p3Root string) (err error) {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("p3 partition creation canceled: %w", cerr)
	}

	p3, ok := plannedPartition(media.Plan, labelClusterConfig)
	if !ok {
		// install-only: the plan reserves no cluster-config slot, so there is nothing to create.
		log.Infof("create-usb: no cluster-config (p3) partition in the plan (mode=%s); skipping p3 stage", media.Plan.Mode)
		return nil
	}

	if p3Root == "" {
		// Self-contained, but no config tree was threaded in. The render + handoff is CNF-27171;
		// skip loudly so the reserved slot is simply left unfilled rather than silently ignored.
		log.Warnf("create-usb: cluster-config (p3) slot is reserved but no config tree was provided; " +
			"leaving p3 unpopulated (end-to-end tree handoff is CNF-27171)")
		return nil
	}

	// Validate the source tree and enforce the file policy + 256 MiB budget before touching the
	// disk, so a bad tree fails before the partition is created/formatted.
	if err := validateP3Source(p3Root, p3.Size); err != nil {
		return err
	}

	// Create, settle, and format the partition through the shared create -> settle -> format
	// sequence p2 and p3 both use (createusb_assemble.go). p3 then mounts the returned device
	// node and populates it -- the mount/populate/unmount steps below are p3-specific.
	partDev, err := createAndFormatPartition(deps.partition, media.LoopDevice, p3, clusterConfigFSType)
	if err != nil {
		return fmt.Errorf("creating cluster-config (p3) partition: %w", err)
	}

	mountpoint, err := os.MkdirTemp("", "create-usb-p3-")
	if err != nil {
		return fmt.Errorf("creating cluster-config (p3) mountpoint: %w", err)
	}
	// Remove the (empty, post-unmount) mountpoint directory last. Deferred before the mount so
	// it runs after the unmount below (defers run LIFO).
	defer func() {
		if rerr := os.Remove(mountpoint); rerr != nil && !os.IsNotExist(rerr) {
			log.Warnf("create-usb: removing cluster-config (p3) mountpoint %s: %v", mountpoint, rerr)
		}
	}()

	if err := deps.partition.MountPartition(partDev, mountpoint); err != nil {
		return fmt.Errorf("mounting cluster-config (p3) partition %s at %s: %w", partDev, mountpoint, err)
	}
	// Always unmount before the loop device is detached (Close does not unmount). Aggregate a
	// cleanup failure into the returned error rather than swallowing it, and tag it with
	// errP3Mounted so the caller's rollback preserves the still-mounted image + loop handle
	// (detaching/removing them while mounted would discard the recovery path).
	defer func() {
		if uerr := deps.partition.UnmountPartition(mountpoint); uerr != nil {
			err = errors.Join(err, fmt.Errorf("unmounting cluster-config (p3) partition at %s: %w",
				mountpoint, errors.Join(errP3Mounted, uerr)))
		}
	}()

	if err := copyTreeContentVerbatim(p3Root, mountpoint); err != nil {
		return fmt.Errorf("populating cluster-config (p3) partition from %s: %w", p3Root, err)
	}

	log.Infof("create-usb: cluster-config (p3) partition created, formatted ext4, labeled %q, and populated from %s",
		labelClusterConfig, p3Root)
	return nil
}

// validateP3Source checks that p3Root is a non-empty directory whose tree is a legal p3
// payload: regular files and directories only (symlinks and other special files are rejected,
// matching the content-verbatim copy below), and a total size within the reserved budget.
//
// budget is the reserved partition size (p3ReservationBytes). The usable ext4 space is
// somewhat smaller due to filesystem metadata, so this is an upper-bound guard that fails
// early for an obviously-too-big tree; a tree that squeaks under the budget but exceeds the
// usable space still fails loudly during the copy (ENOSPC).
func validateP3Source(p3Root string, budget int64) error {
	info, err := os.Stat(p3Root)
	if err != nil {
		return fmt.Errorf("accessing p3 config tree %q: %w", p3Root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("p3 config tree %q is not a directory", p3Root)
	}
	entries, err := os.ReadDir(p3Root)
	if err != nil {
		return fmt.Errorf("reading p3 config tree %q: %w", p3Root, err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("p3 config tree %q is empty; self-contained media requires a rendered cluster-config tree", p3Root)
	}

	total, err := p3SourceSize(p3Root)
	if err != nil {
		return err
	}
	if total > budget {
		return fmt.Errorf("p3 config tree %q is %d bytes, exceeding the reserved cluster-config partition size of %d bytes",
			p3Root, total, budget)
	}
	return nil
}

// p3SourceSize walks p3Root, enforces the file policy (regular files and directories only;
// symlinks and other non-regular files are rejected loudly), and returns the total byte size
// of the regular files. WalkDir lstats entries and does not follow symlinks, so a symlinked
// file or directory is reported here and rejected rather than silently dereferenced.
func p3SourceSize(p3Root string) (int64, error) {
	var total int64
	walkErr := filepath.WalkDir(p3Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("p3 config tree entry %q is not a regular file (mode %s); "+
				"self-contained media may ship only regular files and directories", path, d.Type())
		}
		fi, ierr := d.Info()
		if ierr != nil {
			return fmt.Errorf("stating p3 config tree entry %q: %w", path, ierr)
		}
		total += fi.Size()
		return nil
	})
	if walkErr != nil {
		return 0, fmt.Errorf("scanning p3 config tree %q: %w", p3Root, walkErr)
	}
	return total, nil
}

// copyTreeContentVerbatim recursively copies srcRoot into dstRoot, preserving the relative
// layout. It is "content-verbatim", not metadata-preserving: file contents and tree layout are
// identical, but modes are normalized (directories p3DirMode, files 0o600 via
// copyFileVerbatim). It enforces the same file policy as p3SourceSize (regular files and
// directories only) as defense-in-depth on the copy path.
func copyTreeContentVerbatim(srcRoot, dstRoot string) error {
	walkErr := filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(srcRoot, path)
		if rerr != nil {
			return fmt.Errorf("computing relative path for %q under %q: %w", path, srcRoot, rerr)
		}
		dst := filepath.Join(dstRoot, rel)
		if d.IsDir() {
			if err := os.MkdirAll(dst, p3DirMode); err != nil {
				return fmt.Errorf("creating directory %q: %w", dst, err)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("p3 config tree entry %q is not a regular file (mode %s); refusing to copy", path, d.Type())
		}
		return copyFileVerbatim(path, dst)
	})
	if walkErr != nil {
		return fmt.Errorf("copying p3 config tree %q to %q: %w", srcRoot, dstRoot, walkErr)
	}
	return nil
}
