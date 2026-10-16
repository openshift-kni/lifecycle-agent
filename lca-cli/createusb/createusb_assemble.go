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

// This file coordinates the create-usb media-assembly pipeline: allocate the sparse
// image, attach a loop device, run the p1/p2/p3/p4 stages, and manage cleanup. The
// placeholder p1 and base GPT creation live in createusb_p1.go; later work writes
// the real live ISO into that reserved region.
//
// # Privileged-ops seam
//
// Partitioning needs privileged host ops (losetup, sgdisk) that unit tests cannot run.
// They are hidden behind the partitionOps interface + mediaBuildDeps struct (mirroring
// imageBuildDeps/defaultImageBuildDeps in createusb_images.go), so the pure logic
// (size/offset/alignment math, mode→partition-set) is unit-testable with fakes and only
// the real device path is exercised by a root-gated integration test.
//
// # --write-device is deferred
//
// Writing to a real block device is destructive and is rejected up front by Build while
// the assembly pipeline is incomplete: the only target this story ever writes is a
// sparse .img. ops.CreateExtraPartition (ops.go) is node-install-specific (sfdisk append,
// xfs, grows root) and is a reference pattern only -- this seam owns its own GPT/offset
// logic.
//
// # Placeholder output
//
// Build keeps the sparse .img with a GPT and all planned partition slots. p1 reserves
// space for the live ISO; p2/p4 and, in self-contained mode, p3 are empty ext4
// filesystems. The image is not bootable until ISO and payload population are wired in.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

const (
	// sectorSize is the logical sector size assumed for the loopback-attached container.
	// A loop device over a regular file exposes 512-byte logical sectors.
	sectorSize = int64(512)

	// alignmentBytes is the 1 MiB partition alignment used for every partition start, the
	// conventional alignment sgdisk uses by default.
	alignmentBytes = int64(1) << 20

	// gptFrontReservedBytes is the space reserved at the front of the container for the
	// protective MBR and the primary GPT (header + partition-entry array) before the first
	// partition. 1 MiB comfortably covers it and keeps p1 1 MiB-aligned.
	gptFrontReservedBytes = int64(1) << 20

	// gptBackupReservedBytes is the space reserved at the tail of the container for the
	// backup GPT header + partition-entry array. Allocating it up front means the backup
	// GPT lands at the true device end at creation time -- no post-hoc relocation needed.
	gptBackupReservedBytes = int64(1) << 20

	// placeholderTypeCode is the GPT type code assigned to the reserved partitions
	// (0x8300, Linux filesystem). It is a reservation marker; CNF-27168 and the p2/p3/p4
	// stories set the final type/content when they populate their partitions.
	placeholderTypeCode = "8300"
)

// Provisional reservation sizes. They are placeholders driven by the expected on-media
// content, not measured artifacts; the exact values (notably the p1 live-ISO reservation)
// are confirmed or changed by the CNF-27158 assembly-approach gate. The container is a
// sparse .img, so reserving full size costs no real bytes until content is written.
const (
	// p2ReservationBytes reserves the ibi-images OCI layout region (CNF-28668).
	p2ReservationBytes = int64(8) << 30
	// p3ReservationBytes reserves the self-contained cluster-config region (CNF-28669).
	p3ReservationBytes = int64(256) << 20
	// p4ReservationBytes reserves the ibi-status region (CNF-28670).
	p4ReservationBytes = int64(64) << 20
)

// GPT partition names (labels) for the remaining reserved partitions. p1's label is
// defined with its stage in createusb_p1.go.
const (
	labelIBIImages     = "ibi-images"
	labelClusterConfig = "cluster-config"
	labelIBIStatus     = "ibi-status"
)

// Supported filesystem types for FormatPartition. Each takes different mkfs options, so the
// correct formatter + flags are selected per type (mkfsCommand) rather than reusing one flag set.
const (
	fsTypeExt4 = "ext4"
	fsTypeXFS  = "xfs"
)

const (
	// partitionNodePollInterval is how often waitForPartitionNode polls for the new partition
	// device node to appear after refreshing the kernel partition table.
	partitionNodePollInterval = 100 * time.Millisecond
	// partitionNodeWaitTimeout bounds how long waitForPartitionNode waits for the partition
	// device node to appear before giving up.
	partitionNodeWaitTimeout = 5 * time.Second
)

// BuildOptions carries the resolved output target for a create-usb media build. Exactly
// one of OutputPath / WriteDevice is set (enforced by the command's validateOutputTarget).
type BuildOptions struct {
	// OutputPath is the path of the sparse .img to create (--output).
	OutputPath string
	// WriteDevice is the block device to write to (--write-device). It is deferred: Build
	// rejects it while the assembly pipeline is incomplete.
	WriteDevice string
	// P3Root is the rendered + digest-pinned self-contained config tree consumed read-only
	// by the lower-level p3 stage. The public Build does not yet run that stage.
	P3Root string
}

// PlannedPartition is one entry in the computed media layout: its GPT partition number,
// label, 1 MiB-aligned byte offset within the container, and reserved byte size.
type PlannedPartition struct {
	Number int
	Label  string
	Offset int64
	Size   int64
}

// PartitionPlan is the ordered media layout for a mode. This story creates only the base
// GPT + the placeholder p1 entry; the p2/p3/p4 entries are populated by the sibling
// stories into the offsets reserved here.
type PartitionPlan struct {
	Mode       Mode
	Partitions []PlannedPartition
}

// PartitionEntry is a parsed on-disk GPT partition-table entry, returned by
// ReadPartitionTable so tests can assert the actual table rather than match a string.
type PartitionEntry struct {
	Number int
	// Start and End are the inclusive first/last logical sectors.
	Start int64
	End   int64
	// Size is the entry's byte size, derived from the sector range.
	Size int64
	// Type is the GPT type code reported by sgdisk (e.g. "8300").
	Type string
	Name string
}

// partitionOps is the privileged host-ops seam the media build depends on. The p2, p3, and p4
// stages (CNF-28668/CNF-28669/CNF-28670) share the same create/settle/format primitives; p3
// adds mount/unmount on top for its populate step. p2 and p4 both create an empty, unmounted
// filesystem and stop after formatting.
type partitionOps interface {
	// AttachLoopback attaches imgPath to a loop device (losetup --find --show -P) and
	// returns the device path (e.g. /dev/loop0).
	AttachLoopback(imgPath string) (loopDev string, err error)
	// DetachLoopback detaches loopDev (losetup -d). It is idempotent: an already-detached
	// device is not an error.
	DetachLoopback(loopDev string) error
	// CreateBaseLayout initializes the GPT and placeholder p1; the p1 stage in
	// createusb_p1.go owns this operation.
	CreateBaseLayout(dev string, plan PartitionPlan) error
	// ReadPartitionTable reads dev's GPT and returns the parsed entries.
	ReadPartitionTable(dev string) ([]PartitionEntry, error)

	// The create -> settle -> format primitives below are the single shared implementation
	// every partition stage uses (the createAndFormatPartition helper composes them). p2
	// (ibi-images), p3 (cluster-config), and p4 (ibi-status) all drive them so there is exactly
	// one copy of the GPT-creation, partition-table-refresh, bounded device-node-resolution, and
	// ext4-format command sequences.

	// CreatePartition adds a single GPT entry to dev at the number, 1 MiB-aligned offset, and
	// reserved size the plan carries for part (sgdisk --new/--change-name/--typecode),
	// using the same GPT-entry operation as p1. It does not format the partition.
	CreatePartition(dev string, part PlannedPartition) error
	// SettlePartition makes the kernel partition node for partition number on dev available
	// after its GPT entry was added (re-read the table + a bounded wait for the device node)
	// and returns the partition device path (e.g. /dev/loop0p3).
	SettlePartition(dev string, number int) (partDev string, err error)
	// FormatPartition creates a fsType filesystem on partDev and sets its FS label. fsType is
	// validated (ext4/xfs) before mkfs runs.
	FormatPartition(partDev, fsType, label string) error

	// MountPartition and UnmountPartition back p3's populate step; p2 creates an empty
	// partition and never mounts.

	// MountPartition mounts partDev at mountpoint.
	MountPartition(partDev, mountpoint string) error
	// UnmountPartition unmounts mountpoint.
	UnmountPartition(mountpoint string) error
}

// createAndFormatPartition is the shared create -> settle -> format sequence every partition
// stage runs: it creates part's GPT entry at its reserved offset on dev, settles its kernel
// device node (partition-table refresh + a bounded wait), formats it as fsType with part.Label
// as the FS label, and returns the partition device node. p2 (ibi-images) and p4 (ibi-status)
// call it and are done; p3 (cluster-config) calls it and then mounts the returned node to
// populate the partition.
func createAndFormatPartition(p partitionOps, dev string, part PlannedPartition, fsType string) (string, error) {
	if err := p.CreatePartition(dev, part); err != nil {
		return "", fmt.Errorf("creating p%d (%s) entry on %s: %w", part.Number, part.Label, dev, err)
	}
	partDev, err := p.SettlePartition(dev, part.Number)
	if err != nil {
		return "", fmt.Errorf("settling p%d (%s) partition node: %w", part.Number, part.Label, err)
	}
	if err := p.FormatPartition(partDev, fsType, part.Label); err != nil {
		return "", fmt.Errorf("formatting p%d (%s) as %s: %w", part.Number, part.Label, fsType, err)
	}
	return partDev, nil
}

// mediaBuildDeps bundles the injectable seam(s). It has a single field today; the
// p2/p3/p4 stories add format/mount ops to partitionOps rather than new fields.
type mediaBuildDeps struct {
	partition partitionOps
}

// defaultMediaBuildDeps returns the production seam backed by the host ops helpers,
// exactly as defaultImageBuildDeps wires ops.NewOps in createusb_images.go.
func defaultMediaBuildDeps(log *logrus.Logger) mediaBuildDeps {
	return mediaBuildDeps{
		partition: &opsPartitionOps{
			ops: ops.NewOps(log, ops.NewRegularExecutor(log, false)),
			log: log,
		},
	}
}

// MediaHandle is the live media container the assembly pipeline threads through: the
// image path, the attached loop device, the computed plan, and whether the target is an
// image file. The sibling p2/p3/p4 stories receive it from assembleMedia and own
// teardown; Close is the single teardown entry point.
type MediaHandle struct {
	ImagePath  string
	LoopDevice string
	Plan       PartitionPlan
	IsImage    bool

	partition partitionOps
}

// Close discards a partial media image after detaching its loop device.
//
// If the detach fails, Close returns the error and leaves LoopDevice set and the backing
// image in place: the loop device may still be attached to the image, so removing the
// image would orphan an attachment and lose the only handle a caller has to retry the
// detach. The caller can call Close again to retry once the underlying cause is cleared.
func (h *MediaHandle) Close() error {
	if err := h.detach(); err != nil {
		return err
	}
	if h.IsImage && h.ImagePath != "" {
		if err := os.Remove(h.ImagePath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing media image %s: %w", h.ImagePath, err)
		}
		h.ImagePath = ""
	}
	return nil
}

// Finalize releases the loop device and keeps the placeholder image for the caller.
// A detach failure leaves both the loop and image available for recovery.
func (h *MediaHandle) Finalize() error {
	if err := h.detach(); err != nil {
		return err
	}
	f, err := os.OpenFile(h.ImagePath, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("opening media image %s for sync: %w", h.ImagePath, err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing media image %s: %w", h.ImagePath, err)
	}
	return nil
}

func (h *MediaHandle) detach() error {
	if h.LoopDevice == "" {
		return nil
	}
	if err := h.partition.DetachLoopback(h.LoopDevice); err != nil {
		return fmt.Errorf("detaching loop device %s: %w", h.LoopDevice, err)
	}
	h.LoopDevice = ""
	return nil
}

// Build creates a placeholder .img with GPT and formatted partition slots. The image is
// not bootable until ISO writing and content population are wired in.
func Build(ctx context.Context, log *logrus.Logger, m *Manifest, opts BuildOptions) error {
	return build(ctx, log, m, opts, defaultMediaBuildDeps(log))
}

// build is the dependency-injected core of Build (mirroring BuildImageLayout →
// buildImageLayout). It is unexported so tests exercise the public behavior with a fake
// seam while production Build injects defaultMediaBuildDeps.
func build(ctx context.Context, log *logrus.Logger, m *Manifest, opts BuildOptions, deps mediaBuildDeps) error {
	// Reject the destructive device path before any attach/write while assembly is
	// incomplete, so a real block device is never overwritten with incomplete media.
	if opts.WriteDevice != "" {
		return fmt.Errorf("--write-device is not yet supported for placeholder media (CNF-27158)")
	}

	handle, err := assembleMedia(ctx, log, m, opts, deps)
	if err != nil {
		return err
	}

	// Add and format the empty p2 (ibi-images) partition into the region the base layout
	// reserved. On failure, tear the handle down (detach loop + remove the partial .img)
	// before returning so a failed build never leaks a loop device or partial image.
	if perr := createP2Partition(log, handle, deps); perr != nil {
		if cerr := handle.Close(); cerr != nil {
			return fmt.Errorf("%w; additionally failed to clean up the partial media: %w", perr, cerr)
		}
		return perr
	}

	// Add and format the empty p4 (ibi-status) partition into its reserved region (both modes).
	// It uses the same shared create -> settle -> format sequence as p2 and ships empty and
	// unmounted; the runtime writer (CNF-27163) populates it later. On failure, tear the handle
	// down before returning, exactly as for p2.
	if perr := createP4Partition(log, handle, deps); perr != nil {
		if cerr := handle.Close(); cerr != nil {
			return fmt.Errorf("%w; additionally failed to clean up the partial media: %w", perr, cerr)
		}
		return perr
	}

	if err := handle.Finalize(); err != nil {
		return fmt.Errorf("finalizing placeholder media %s: %w", handle.ImagePath, err)
	}
	log.Warnf("create-usb: created placeholder image %s; it has no live ISO or image payload and is not bootable", handle.ImagePath)
	return nil
}

// assembleMedia is the DI core that creates the media container, attaches the loopback,
// establishes the base GPT + placeholder p1, and returns a live *MediaHandle for the
// sibling stories to extend.
//
// It owns its own lifecycle on failure: on any internal error it detaches the loop device
// (if attached) and removes the partial .img before returning (nil, err), via defer-based
// rollback keyed on success. On success it returns a live handle with the loop still
// attached; the caller owns teardown via MediaHandle.Close.
func assembleMedia(ctx context.Context, log *logrus.Logger, m *Manifest, opts BuildOptions, deps mediaBuildDeps) (handle *MediaHandle, err error) {
	if cerr := ctx.Err(); cerr != nil {
		return nil, fmt.Errorf("media assembly canceled: %w", cerr)
	}
	if opts.OutputPath == "" {
		return nil, fmt.Errorf("an output .img path is required")
	}

	plan := partitionPlan(m.Mode, defaultP1ReservationBytes)
	total := containerSize(plan)
	imgPath := opts.OutputPath

	log.Infof("create-usb: establishing base media layout (mode=%s, container=%d bytes, partitions=%d)",
		m.Mode, total, len(plan.Partitions))

	if err = allocateSparseImage(imgPath, total); err != nil {
		return nil, fmt.Errorf("allocating media image %s: %w", imgPath, err)
	}

	// Roll back everything this call allocated unless it completes successfully. On
	// success the caller owns teardown, so success clears the rollback.
	var loopDev string
	success := false
	defer func() {
		if success {
			return
		}
		// If p3 could not be unmounted, its filesystem is still mounted over the loop device.
		// Detaching the loop and removing the backing image would discard the only path to
		// recover or retry the unmount, so preserve both and surface where things stand.
		if errors.Is(err, errP3Mounted) {
			log.Warnf("create-usb: cluster-config (p3) partition is still mounted; preserving loop device %s "+
				"and media image %s for recovery (unmount it, then detach %s and remove the image)",
				loopDev, imgPath, loopDev)
			return
		}
		if loopDev != "" {
			if derr := deps.partition.DetachLoopback(loopDev); derr != nil {
				log.Warnf("create-usb: detaching loop device %s during rollback: %v", loopDev, derr)
			}
		}
		if rerr := os.Remove(imgPath); rerr != nil && !os.IsNotExist(rerr) {
			log.Warnf("create-usb: removing partial media image %s during rollback: %v", imgPath, rerr)
		}
	}()

	loopDev, err = deps.partition.AttachLoopback(imgPath)
	if err != nil {
		return nil, fmt.Errorf("attaching loop device for %s: %w", imgPath, err)
	}

	if err = createP1Partition(log, plan, loopDev, deps); err != nil {
		return nil, err
	}

	handle = &MediaHandle{
		ImagePath:  imgPath,
		LoopDevice: loopDev,
		Plan:       plan,
		IsImage:    true,
		partition:  deps.partition,
	}

	// Self-contained: fill the reserved p3 cluster-config slot (create entry, mkfs.ext4,
	// label, mount, populate). It runs after the base layout and before success, so a p3
	// failure triggers the rollback above (p3 unmounts itself first, then the rollback
	// detaches the loop and removes the partial image). install-only has no p3 entry in the
	// plan, so the stage is a no-op there.
	if err = createP3Partition(ctx, log, deps, handle, opts.P3Root); err != nil {
		return nil, fmt.Errorf("creating cluster-config (p3) partition on %s: %w", loopDev, err)
	}

	success = true
	return handle, nil
}

// partitionPlan computes the ordered media layout for a mode, with every partition start
// 1 MiB-aligned and offsets derived from the placeholder p1 size: p1+p2+p4 for
// install-only, p1+p2+p3+p4 for self-contained. The p2/p3/p4 entries are populated by the
// sibling stories into the offsets reserved here.
//
// Each role has a *fixed* GPT partition number so a partition keeps the same number across
// modes: live-iso=1, ibi-images=2, cluster-config=3, ibi-status=4. In install-only mode
// cluster-config (p3) is omitted, so ibi-status stays p4 (the layout is p1/p2/p4, not a
// renumbered p1/p2/p3) -- the number is not derived from the slice position.
func partitionPlan(mode Mode, p1Size int64) PartitionPlan {
	specs := []struct {
		number int
		label  string
		size   int64
	}{
		{1, labelLiveISO, p1Size},
		{2, labelIBIImages, p2ReservationBytes},
	}
	if mode == ModeSelfContained {
		specs = append(specs, struct {
			number int
			label  string
			size   int64
		}{3, labelClusterConfig, p3ReservationBytes})
	}
	specs = append(specs, struct {
		number int
		label  string
		size   int64
	}{4, labelIBIStatus, p4ReservationBytes})

	parts := make([]PlannedPartition, 0, len(specs))
	offset := gptFrontReservedBytes
	for _, s := range specs {
		offset = alignUp(offset, alignmentBytes)
		parts = append(parts, PlannedPartition{
			Number: s.number,
			Label:  s.label,
			Offset: offset,
			Size:   s.size,
		})
		offset += s.size
	}
	return PartitionPlan{Mode: mode, Partitions: parts}
}

// containerSize is the total container size for a plan: the aligned end of the last
// partition plus the tail backup-GPT reservation, so the backup GPT lands at the true
// device end at creation time.
func containerSize(plan PartitionPlan) int64 {
	if len(plan.Partitions) == 0 {
		return alignUp(gptFrontReservedBytes, alignmentBytes) + gptBackupReservedBytes
	}
	last := plan.Partitions[len(plan.Partitions)-1]
	end := alignUp(last.Offset+last.Size, alignmentBytes)
	return end + gptBackupReservedBytes
}

// alignUp rounds v up to the next multiple of align (align must be > 0).
func alignUp(v, align int64) int64 {
	return ((v + align - 1) / align) * align
}

// allocateSparseImage creates a new sparse file of exactly size bytes. The backing bytes
// are not written, so a multi-GiB container costs no real disk until content is written
// into it.
//
// It creates the file exclusively (O_EXCL): if the output path already exists it fails
// without touching the existing file, rather than truncating it. This prevents a
// placeholder build from destroying an existing image at a reused output path.
func allocateSparseImage(path string, size int64) error {
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // output path is supplied by the operator at build time
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("output path %s already exists; refusing to overwrite it (remove it or choose a different --output path): %w", path, err)
		}
		return fmt.Errorf("creating image file: %w", err)
	}
	if terr := f.Truncate(size); terr != nil {
		_ = f.Close()
		return fmt.Errorf("truncating image file to %d bytes: %w", size, terr)
	}
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("closing image file: %w", cerr)
	}
	return nil
}

// ---- production seam implementation ----

// opsPartitionOps is the production partitionOps backed by the host podman/util ops
// (losetup, sgdisk) run in the host namespace, as opsSeedClosureReader does in
// createusb_images.go.
type opsPartitionOps struct {
	ops ops.Ops
	log *logrus.Logger
}

// AttachLoopback attaches imgPath to a loop device and returns the device path.
func (p *opsPartitionOps) AttachLoopback(imgPath string) (string, error) {
	out, err := p.ops.RunInHostNamespace("losetup", "--find", "--show", "-P", imgPath)
	if err != nil {
		return "", fmt.Errorf("running losetup for %s: %w", imgPath, err)
	}
	dev := strings.TrimSpace(out)
	if dev == "" {
		return "", fmt.Errorf("losetup returned no device for %s", imgPath)
	}
	return dev, nil
}

// DetachLoopback detaches loopDev. It tolerates an already-detached device so teardown is
// idempotent.
func (p *opsPartitionOps) DetachLoopback(loopDev string) error {
	if loopDev == "" {
		return nil
	}
	if _, err := p.ops.RunInHostNamespace("losetup", "-d", loopDev); err != nil {
		if isAlreadyDetached(err) {
			return nil
		}
		return fmt.Errorf("running losetup -d %s: %w", loopDev, err)
	}
	return nil
}

// ReadPartitionTable reads dev's GPT (sgdisk -p) and returns the parsed entries.
func (p *opsPartitionOps) ReadPartitionTable(dev string) ([]PartitionEntry, error) {
	out, err := p.ops.RunInHostNamespace("sgdisk", "-p", dev)
	if err != nil {
		return nil, fmt.Errorf("reading partition table on %s: %w", dev, err)
	}
	return parseSgdiskTable(out), nil
}

// CreatePartition adds the single GPT entry part reserves to dev, using the same sector math
// and sgdisk invocation as the p1 stage (--new/--change-name/--typecode).
// The placeholder type code is kept: the cluster-config partition is located by FS label at
// post-pivot, not by GPT type.
func (p *opsPartitionOps) CreatePartition(dev string, part PlannedPartition) error {
	startSector := part.Offset / sectorSize
	endSector := (part.Offset+part.Size)/sectorSize - 1
	if _, err := p.ops.RunInHostNamespace("sgdisk",
		fmt.Sprintf("--new=%d:%d:%d", part.Number, startSector, endSector),
		fmt.Sprintf("--change-name=%d:%s", part.Number, part.Label),
		fmt.Sprintf("--typecode=%d:%s", part.Number, placeholderTypeCode),
		dev); err != nil {
		return fmt.Errorf("creating partition %d (%s) on %s: %w", part.Number, part.Label, dev, err)
	}
	return nil
}

// SettlePartition re-reads dev's partition table (partprobe) and waits for the new partition
// node to be created under /dev, then returns its device path. Without this the <dev>pN node
// may not exist yet when mkfs runs. The wait is bounded (see waitForPartitionNode) so a slow or
// stuck udev cannot hang the settle indefinitely.
func (p *opsPartitionOps) SettlePartition(dev string, number int) (string, error) {
	if _, err := p.ops.RunInHostNamespace("partprobe", dev); err != nil {
		return "", fmt.Errorf("re-reading partition table on %s: %w", dev, err)
	}
	partDev := partitionDevicePath(dev, number)
	if err := p.waitForPartitionNode(partDev); err != nil {
		return "", err
	}
	return partDev, nil
}

// FormatPartition creates a fsType filesystem on partDev with the given FS label. The formatter
// and its force flag are selected per type by mkfsCommand (ext4 -F, xfs -f), and an unsupported
// type fails before any mkfs runs. ext4 is the media default because it holds the 14-char
// cluster-config label (vfat's 11-char limit could not) and post-pivot locates p3 by label with
// kernel FS auto-detection, imposing no FS-type requirement.
func (p *opsPartitionOps) FormatPartition(partDev, fsType, label string) error {
	mkfs, err := mkfsCommand(fsType, label, partDev)
	if err != nil {
		return err
	}
	if _, err := p.ops.RunInHostNamespace(mkfs[0], mkfs[1:]...); err != nil {
		return fmt.Errorf("running %s -L %s on %s: %w", mkfs[0], label, partDev, err)
	}
	return nil
}

// waitForPartitionNode waits for node to appear after a partition-table refresh: it asks udev to
// settle (best effort) and then polls for the node, returning a clear error if the node never
// materializes.
//
// A single deadline bounds the whole wait. udevadm settle is given an explicit timeout derived
// from that deadline so a slow or stuck settle cannot push the overall wait past
// partitionNodeWaitTimeout -- without it, the timer below would only start once settle returned,
// so the method could block far longer than its reported limit.
func (p *opsPartitionOps) waitForPartitionNode(node string) error {
	deadline := time.Now().Add(partitionNodeWaitTimeout)

	// Bound settle by the remaining time to the shared deadline (rounded up to whole seconds,
	// the granularity udevadm --timeout accepts). settle is best effort: a failure is non-fatal
	// because the existence poll below is the real guard.
	settleTimeout := int(time.Until(deadline).Seconds()) + 1
	if _, err := p.ops.RunInHostNamespace("udevadm", "settle", fmt.Sprintf("--timeout=%d", settleTimeout)); err != nil {
		p.log.Warnf("create-usb: udevadm settle before waiting for partition node %s failed: %v", node, err)
	}

	for {
		_, err := os.Stat(node)
		if err == nil {
			return nil
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("checking for partition node %s: %w", node, err)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("partition node %s did not appear within %s after refreshing the partition table",
				node, partitionNodeWaitTimeout)
		}
		time.Sleep(partitionNodePollInterval)
	}
}

// mkfsCommand returns the argv that makes fsType on node with the given filesystem label.
// mkfs.ext4 and mkfs.xfs take different options (ext4 force is -F, xfs force is -f), so the
// formatter + flags are selected per type; an unsupported type is a hard error rather than a
// silently-wrong flag set.
func mkfsCommand(fsType, label, node string) ([]string, error) {
	switch fsType {
	case fsTypeExt4:
		return []string{"mkfs.ext4", "-F", "-L", label, node}, nil
	case fsTypeXFS:
		return []string{"mkfs.xfs", "-f", "-L", label, node}, nil
	default:
		return nil, fmt.Errorf("unsupported filesystem type %q for %s", fsType, label)
	}
}

// plannedPartition returns the planned partition with the given label, and whether the plan has
// such an entry. Callers that require the partition (p2, always present) turn a false into an
// error; callers for which it is optional (p3 cluster-config, absent in install-only) skip.
func plannedPartition(plan PartitionPlan, label string) (PlannedPartition, bool) {
	for _, part := range plan.Partitions {
		if part.Label == label {
			return part, true
		}
	}
	return PlannedPartition{}, false
}

// MountPartition mounts partDev at mountpoint (which must already exist).
func (p *opsPartitionOps) MountPartition(partDev, mountpoint string) error {
	if _, err := p.ops.RunInHostNamespace("mount", partDev, mountpoint); err != nil {
		return fmt.Errorf("mounting %s at %s: %w", partDev, mountpoint, err)
	}
	return nil
}

// UnmountPartition unmounts mountpoint. It must run before the loop device is detached
// (MediaHandle.Close detaches but does not unmount).
func (p *opsPartitionOps) UnmountPartition(mountpoint string) error {
	if _, err := p.ops.RunInHostNamespace("umount", mountpoint); err != nil {
		return fmt.Errorf("unmounting %s: %w", mountpoint, err)
	}
	return nil
}

// partitionDevicePath returns the kernel device node for partition number on dev. Devices
// whose name ends in a digit (loop0, nvme0n1, mmcblk0) use a "p" separator (loop0p3); others
// (sda) append the number directly (sda3). create-usb always drives a loopback device, so the
// "p" form is the one used in practice.
func partitionDevicePath(dev string, number int) string {
	sep := ""
	if n := len(dev); n > 0 && dev[n-1] >= '0' && dev[n-1] <= '9' {
		sep = "p"
	}
	return fmt.Sprintf("%s%s%d", dev, sep, number)
}

// parseSgdiskTable parses the partition rows of `sgdisk -p` output into structured
// entries. Each data row is "Number Start End <Size value> <Size unit> Code Name...";
// the byte Size is computed from the exact sector range rather than the human-readable
// Size column. Non-data lines (header, totals) are skipped.
func parseSgdiskTable(out string) []PartitionEntry {
	var entries []PartitionEntry
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		// A data row has at least: Number Start End sizeValue sizeUnit Code [Name...].
		if len(fields) < 6 {
			continue
		}
		var number int
		if _, err := fmt.Sscanf(fields[0], "%d", &number); err != nil {
			continue
		}
		var start, end int64
		if _, err := fmt.Sscanf(fields[1], "%d", &start); err != nil {
			continue
		}
		if _, err := fmt.Sscanf(fields[2], "%d", &end); err != nil {
			continue
		}
		name := ""
		if len(fields) > 6 {
			name = strings.Join(fields[6:], " ")
		}
		entries = append(entries, PartitionEntry{
			Number: number,
			Start:  start,
			End:    end,
			Size:   (end - start + 1) * sectorSize,
			Type:   fields[5],
			Name:   name,
		})
	}
	return entries
}

// isAlreadyDetached reports whether err from `losetup -d` indicates the device was
// already detached, which teardown treats as success.
func isAlreadyDetached(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such device") || strings.Contains(msg, "no such file")
}
