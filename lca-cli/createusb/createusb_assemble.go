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

// This file implements the first slice of the create-usb media-assembly pipeline
// (USB-based IBI proposal §6.4 step 5, §10 candidate (a)): create the output media
// container (a sparse .img), establish the base GPT, and add a *placeholder p1
// partition* that reserves the live-ISO region at the front. CNF-27168 later writes the
// real bootable RHCOS isohybrid ISO into p1; this story only reserves and lays out the
// region so the p2/p3/p4 stories (CNF-28668/CNF-28669/CNF-28670) have a stable base to
// append into.
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
// # Honesty
//
// Because p2/p3/p4 are not implemented yet (and the mechanics stay provisional until the
// CNF-27158 bootability gate passes), public Build never claims to have produced complete
// media: it runs the container + placeholder-p1 foundation, tears it back down (detach
// loop device, remove the partial .img), and returns an explicit incomplete-assembly
// error. The internal assembleMedia core returns a live *MediaHandle for the sibling
// stories to thread through; a half-built handle can never escape the public surface.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	// defaultP1ReservationBytes reserves the live-ISO region at the front. A RHCOS live
	// ISO is ~1.2-1.5 GiB; 2 GiB leaves headroom. CNF-27168 writes the real ISO here.
	defaultP1ReservationBytes = int64(2) << 30
	// p2ReservationBytes reserves the ibi-images OCI layout region (CNF-28668).
	p2ReservationBytes = int64(8) << 30
	// p3ReservationBytes reserves the self-contained cluster-config region (CNF-28669).
	p3ReservationBytes = int64(256) << 20
	// p4ReservationBytes reserves the ibi-status region (CNF-28670).
	p4ReservationBytes = int64(64) << 20
)

// GPT partition names (labels) for the reserved partitions, matching the roles the
// sibling stories populate.
const (
	labelLiveISO       = "live-iso"
	labelIBIImages     = "ibi-images"
	labelClusterConfig = "cluster-config"
	labelIBIStatus     = "ibi-status"
)

// errAssemblyIncomplete is returned by Build after the container + placeholder-p1
// foundation runs: the media is not complete (p2/p3/p4 are not implemented yet), so the
// scaffold fails loudly rather than claim to have produced usable media.
var errAssemblyIncomplete = errors.New(
	"media assembly incomplete: partitions p2/p3/p4 not yet implemented (CNF-28668/CNF-28669/CNF-28670)")

// BuildOptions carries the resolved output target for a create-usb media build. Exactly
// one of OutputPath / WriteDevice is set (enforced by the command's validateOutputTarget).
type BuildOptions struct {
	// OutputPath is the path of the sparse .img to create (--output).
	OutputPath string
	// WriteDevice is the block device to write to (--write-device). It is deferred: Build
	// rejects it while the assembly pipeline is incomplete.
	WriteDevice string
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

// partitionOps is the privileged host-ops seam the media build depends on. The p2/p3/p4
// stories extend this same interface with format/mount methods.
type partitionOps interface {
	// AttachLoopback attaches imgPath to a loop device (losetup --find --show -P) and
	// returns the device path (e.g. /dev/loop0).
	AttachLoopback(imgPath string) (loopDev string, err error)
	// DetachLoopback detaches loopDev (losetup -d). It is idempotent: an already-detached
	// device is not an error.
	DetachLoopback(loopDev string) error
	// CreateBaseLayout creates the GPT and the placeholder p1 entry on dev, reserving the
	// live-ISO region and the p2/p3/p4 offsets. This is the operation that establishes the
	// base layout.
	CreateBaseLayout(dev string, plan PartitionPlan) error
	// ReadPartitionTable reads dev's GPT and returns the parsed entries.
	ReadPartitionTable(dev string) ([]PartitionEntry, error)
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

// Close tears the media handle down: it detaches the loop device and removes the image
// file. It is the discard path for a run that produced no completed media (this story
// always discards, because p2/p3/p4 are not implemented yet). It is safe to call once and
// clears the fields it releases so a second call is a no-op. Keeping completed media is a
// sibling-story concern that will introduce a finalize path.
//
// If the detach fails, Close returns the error and leaves LoopDevice set and the backing
// image in place: the loop device may still be attached to the image, so removing the
// image would orphan an attachment and lose the only handle a caller has to retry the
// detach. The caller can call Close again to retry once the underlying cause is cleared.
func (h *MediaHandle) Close() error {
	if h.LoopDevice != "" {
		if err := h.partition.DetachLoopback(h.LoopDevice); err != nil {
			// Keep LoopDevice set (so a later Close can retry the detach) and do not remove
			// the backing image while a loop device may still be attached to it.
			return fmt.Errorf("detaching loop device %s: %w", h.LoopDevice, err)
		}
		h.LoopDevice = ""
	}
	if h.IsImage && h.ImagePath != "" {
		if err := os.Remove(h.ImagePath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing media image %s: %w", h.ImagePath, err)
		}
		h.ImagePath = ""
	}
	return nil
}

// Build assembles the USB media from a validated manifest.
//
// This story implements the container + placeholder-p1 foundation of the media-assembly
// pipeline: for --output it creates a sparse .img, establishes the base GPT, and adds a
// placeholder p1 partition reserving the live-ISO region (CNF-27168 writes the real ISO).
// --write-device is deferred (destructive) and is rejected before any write while the
// pipeline is incomplete.
//
// Build returns an error only -- never a handle -- so a half-built media handle can never
// escape a failed call. Because p2/p3/p4 (CNF-28668/CNF-28669/CNF-28670) are not
// implemented yet, Build runs the foundation, tears it back down (detach loop, remove the
// partial .img), and returns an explicit incomplete-assembly error so the scaffold never
// claims to have produced complete, bootable media.
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
		return fmt.Errorf("media assembly incomplete: --write-device is not yet supported until media assembly is complete (CNF-28668/CNF-28669/CNF-28670)")
	}

	handle, err := assembleMedia(ctx, log, m, opts, deps)
	if err != nil {
		return err
	}

	// There is no next assembly stage yet, so the foundation produced no usable media:
	// discard the handle (detach loop + remove the partial .img) and fail loudly.
	if cerr := handle.Close(); cerr != nil {
		return fmt.Errorf("%w; additionally failed to clean up the partial media: %w", errAssemblyIncomplete, cerr)
	}
	return errAssemblyIncomplete
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

	if err = deps.partition.CreateBaseLayout(loopDev, plan); err != nil {
		return nil, fmt.Errorf("creating base GPT + placeholder p1 on %s: %w", loopDev, err)
	}

	handle = &MediaHandle{
		ImagePath:  imgPath,
		LoopDevice: loopDev,
		Plan:       plan,
		IsImage:    true,
		partition:  deps.partition,
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
// without touching the existing file, rather than truncating it. A build never completes
// in this story (it always returns the incomplete-assembly error and discards its own
// freshly created image), so clobbering a path the operator reused -- e.g. a previous
// real image -- would destroy that file for a command that then reports failure.
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

// CreateBaseLayout zeroes any existing table, creates a fresh GPT at the full container
// size, and adds the placeholder p1 entry reserving the live-ISO region. Only p1 is
// created here; the p2/p3/p4 entries are added by the sibling stories into the offsets
// the plan reserves.
func (p *opsPartitionOps) CreateBaseLayout(dev string, plan PartitionPlan) error {
	if len(plan.Partitions) == 0 {
		return fmt.Errorf("empty partition plan")
	}
	p1 := plan.Partitions[0]
	startSector := p1.Offset / sectorSize
	endSector := (p1.Offset+p1.Size)/sectorSize - 1

	// Zap any residual table so a fresh, valid GPT is written (and the backup GPT lands at
	// the true device end, since the loop device spans the full container).
	if _, err := p.ops.RunInHostNamespace("sgdisk", "--zap-all", dev); err != nil {
		return fmt.Errorf("zapping partition table on %s: %w", dev, err)
	}
	if _, err := p.ops.RunInHostNamespace("sgdisk",
		fmt.Sprintf("--new=%d:%d:%d", p1.Number, startSector, endSector),
		fmt.Sprintf("--change-name=%d:%s", p1.Number, p1.Label),
		fmt.Sprintf("--typecode=%d:%s", p1.Number, placeholderTypeCode),
		dev); err != nil {
		return fmt.Errorf("creating placeholder p1 on %s: %w", dev, err)
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
