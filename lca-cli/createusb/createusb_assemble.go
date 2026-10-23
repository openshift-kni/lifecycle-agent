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

// This file coordinates the create-usb media-assembly pipeline. It implements the boot
// model proven by the CNF-27197 testing-workflow spike (hack/usb-ibi-assemble-media.sh):
// "append after the live ISO", using the ISO's own DOS/MBR table.
//
//   1. Build the customized RHCOS live ISO (createusb_p1_content.go: fetch+verify, embed
//      the ignition that auto-runs `lca-cli ibi`, embed the shipping lca-cli binary).
//   2. Copy that ISO byte-for-byte to the FRONT of the output image (LBA 0) and grow the
//      image to the full media size (createusb_p1.go: writeLiveISOToFront). The byte-for-byte
//      copy preserves the ISO's DOS/MBR table and the EFI entries that make it bootable.
//   3. Attach the image to a loop device (--partscan) and snapshot the ISO's own partitions.
//   4. Append the data partitions into the free MBR slots AFTER the ISO with `sfdisk --append`
//      (the ISO's partitions are never touched), then re-read the table.
//   5. Format each appended data partition and set its filesystem label to the on-node
//      discovery contract (ibi-images / cluster-config / ibi-status). Self-contained then
//      populates cluster-config from the rendered config tree.
//
// # Why MBR, not GPT (CNF-27197, Developer of Record)
//
// A real RHCOS isohybrid live ISO has a hybrid DOS/GPT layout on which
// `sgdisk --move-second-header` fails ("Invalid partition data!"), so a GPT append path
// cannot assemble media. The earlier "GPT-first, ISO dd'd into a reserved p1 offset" model
// did not boot (the ISO's protective MBR/ESP never land at LBA 0). This pipeline uses the
// ISO's existing DOS/MBR table and appends with sfdisk, which is the layout the CNF-27197
// spike booted under Secure Boot.
//
// # Discovery is by filesystem label, never by partition number
//
// The on-node code discovers data partitions by filesystem label (prepare finds ibi-images;
// post-pivot's waitForConfiguration scans for cluster-config; the runtime writer finds
// ibi-status). The partition numbers sfdisk assigns after the ISO are an implementation
// detail; everything downstream keys off the label set here.
//
// # Privileged-ops seam
//
// Partitioning needs privileged host ops (losetup, sfdisk, mkfs, blkid) that unit tests
// cannot run. They are hidden behind the partitionOps interface + mediaBuildDeps struct, so
// the pure assembly logic (size math, plan, ordering, rollback) is unit-testable with fakes
// and only the real device path is exercised by a root-gated integration test. The live-ISO
// build is behind the liveISOBuilder seam for the same reason (no network/podman in unit tests).
//
// # --write-device is deferred
//
// Writing to a real block device is destructive and is rejected up front by Build: the only
// target this pipeline writes is a sparse .img.

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
	// mib is one mebibyte; partition sizes are expressed to sfdisk in whole MiB.
	mib = int64(1) << 20
)

// Data-partition reservation sizes. The container is a sparse .img, so reserving full size
// costs no real bytes until content is written. ibi-images is sized to its measured payload
// (see sizeIBIImagesPartition) with p2MinReservationBytes as the floor; ibi-status ships
// formatted, labelled, and empty.
const (
	// p2MinReservationBytes is the floor for the ibi-images partition. The partition is sized
	// to the measured on-media payload (OCI layout + digests.json + the staged lca-cli binary +
	// the self-contained durability store) plus ext4 metadata and free-space headroom, but never
	// below this minimum, so a small payload still gets a reasonable working size and the common
	// case keeps the familiar 8 GiB reservation. Deriving the reservation from the measured
	// payload (rather than fixing it at 8 GiB) is what lets validateUSBSizeBudget reflect an
	// oversized payload and fail at planning time instead of during population.
	p2MinReservationBytes = int64(8) << 30
	// p3ReservationBytes reserves the self-contained cluster-config region.
	p3ReservationBytes = int64(256) << 20
	// p4ReservationBytes reserves the ibi-status region.
	p4ReservationBytes = int64(64) << 20

	// ibiImagesMetadataOverheadBytes is a fixed allowance added above the measured payload for the
	// ext4 filesystem's own metadata (superblocks, inode tables, and the journal) on the
	// ibi-images partition, which mkfs reserves before any file is written.
	ibiImagesMetadataOverheadBytes = int64(256) << 20
	// ibiImagesHeadroomNum / ibiImagesHeadroomDen express the free-space headroom kept above the
	// measured payload (110% => 10% headroom) so the partition is not sized to exactly full.
	ibiImagesHeadroomNum = int64(110)
	ibiImagesHeadroomDen = int64(100)

	// slackBytes pads the whole container so the appended partitions and their MBR/EBR
	// alignment overhead fit after the ISO without landing on the very last sector.
	slackBytes = int64(64) << 20
	// extSlackBytes pads the self-contained MBR extended container so it holds both logical
	// partitions plus their per-logical EBR + 1 MiB alignment overhead.
	extSlackBytes = int64(16) << 20
)

// GPT/MBR partition type codes used in the sfdisk append script.
const (
	// mbrTypeLinux (0x83) marks a Linux-filesystem data partition.
	mbrTypeLinux = "83"
	// mbrTypeExtended (0x05) marks the MBR extended container that holds the self-contained
	// logical partitions (cluster-config, ibi-status). It is never formatted or labelled.
	mbrTypeExtended = "5"
)

// GPT partition names (filesystem labels) for the data partitions. p1's live-ISO label is
// defined with its stage in createusb_p1.go. These strings are the on-node discovery
// contract and must not change.
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

// BuildOptions carries the resolved inputs for a create-usb media build. Exactly one of
// OutputPath / WriteDevice is set (enforced by the command's validateOutputTarget).
type BuildOptions struct {
	// OutputPath is the path of the .img to create (--output).
	OutputPath string
	// WriteDevice is the block device to write to (--write-device). It is deferred: Build
	// rejects it.
	WriteDevice string
	// LCAImage is the lca-cli pull-spec (--lca-image); the exact shipping binary is extracted
	// from it and embedded in the live ISO. Required for assembly.
	LCAImage string
	// AuthFile is the workstation --auth-file used to pull the lca-cli image (build-time only;
	// never written to the media).
	AuthFile string
	// RecertImage is the recert pull-spec mirrored into the on-media OCI image layout. Required
	// for the image-layout population stage.
	RecertImage string
	// USBSizeBytes is the --usb-size budget in bytes. When > 0 the build fails if the resulting
	// whole-stick (final apparent) image size exceeds it. Zero disables the check.
	USBSizeBytes int64
	// P3Root is a pre-rendered + digest-pinned self-contained config tree populated into the
	// cluster-config partition. When empty, the image-layout stage renders the tree itself.
	P3Root string
}

// PlannedPartition is one data partition appended after the live ISO: its filesystem label,
// filesystem type, and reserved byte size. The partition number sfdisk assigns is not
// recorded here -- the media is addressed by filesystem label (see the package doc).
type PlannedPartition struct {
	Label  string
	FSType string
	Size   int64
}

// PartitionPlan is the ordered set of data partitions a mode appends after the live ISO:
// ibi-images + ibi-status for install-only, ibi-images + cluster-config + ibi-status for
// self-contained. The order is the creation order the sfdisk append and the post-append
// device-node mapping both rely on.
type PartitionPlan struct {
	Mode       Mode
	Partitions []PlannedPartition
}

// partitionOps is the privileged host-ops seam the media build depends on: attach/detach the
// loopback, snapshot the ISO's own partitions, append the data partitions into the free MBR
// slots after the ISO, re-read the table, map the appended partitions to device nodes, format
// them, resolve them by filesystem label, and mount/unmount (self-contained populate only).
type partitionOps interface {
	// AttachLoopback attaches imgPath to a loop device (losetup --find --show --partscan) and
	// returns the device path (e.g. /dev/loop0).
	AttachLoopback(imgPath string) (loopDev string, err error)
	// DetachLoopback detaches loopDev (losetup -d). It is idempotent: an already-detached
	// device is not an error.
	DetachLoopback(loopDev string) error

	// SnapshotPartitions returns the device nodes of every partition currently in dev's table.
	// It is called after the ISO is written to the front and before the data partitions are
	// appended, so it captures the ISO's own partitions -- which must never be formatted.
	SnapshotPartitions(dev string) (nodes []string, err error)
	// AppendDataPartitions appends plan's data partitions into the free MBR slots after the ISO
	// (sfdisk --append) and re-reads the kernel partition table. The ISO's own partitions are
	// left untouched. startSector pins the first appended partition's start to the first safe
	// append offset (the MiB-aligned end of the ISO) so a data partition can never land inside the
	// copied ISO bytes, regardless of where the ISO's own partitions end.
	AppendDataPartitions(dev string, plan PartitionPlan, startSector int64) error
	// DataPartitionNodes returns the device nodes of the data partitions appended after the ISO,
	// in creation order (ibi-images, [cluster-config,] ibi-status). It selects the Linux
	// (type 83) partitions that are not part of isoParts and are not the extended container, so
	// the mapping never depends on a fixed partition number and can never pick an ISO partition.
	DataPartitionNodes(dev string, isoParts []string) (nodes []string, err error)
	// FormatPartition creates a fsType filesystem on partDev and sets its FS label. fsType is
	// validated (ext4/xfs) before mkfs runs.
	FormatPartition(partDev, fsType, label string) error
	// ResolvePartitionByFSLabel returns the device node on dev whose filesystem label matches
	// label. It scans only dev's own partitions (not a global blkid -L) so it can never resolve
	// a same-labelled partition on another attached disk.
	ResolvePartitionByFSLabel(dev, label string) (partDev string, err error)

	// MountPartition and UnmountPartition back the self-contained cluster-config populate step;
	// ibi-images and ibi-status ship empty and are never mounted here.
	MountPartition(partDev, mountpoint string) error
	UnmountPartition(mountpoint string) error
}

// liveISOBuilder is the seam that produces the customized bootable RHCOS live ISO. It is an
// interface so unit tests inject a fake that writes a stand-in ISO file (no network, no podman,
// no real RHCOS image); production wraps BuildLiveISO. The result carries both the customized
// ISO (written to the front of the media) and the extracted shipping lca-cli binary (copied onto
// the ibi-images partition so the embedded unit's ExecStart resolves on boot).
type liveISOBuilder interface {
	Build(ctx context.Context, log *logrus.Logger, m *Manifest, opts ISOBuildOptions) (ISOBuildResult, error)
}

// defaultLiveISOBuilder wraps the production BuildLiveISO.
type defaultLiveISOBuilder struct{}

func (defaultLiveISOBuilder) Build(ctx context.Context, log *logrus.Logger, m *Manifest, opts ISOBuildOptions) (ISOBuildResult, error) {
	return BuildLiveISO(ctx, log, m, opts)
}

// imagePopulator is the seam that populates the ibi-images partition with the OCI image layout +
// digests.json (and renders the self-contained p3 config tree) and the read-only durability
// store. It is an interface so the assembly orchestration is unit-testable with a fake (no
// network, no podman, no seed image); production wraps BuildImageLayout / BuildDurabilityStore.
type imagePopulator interface {
	// BuildLayout mirrors the manifest's image set into a build-unique OCI layout + digests.json
	// under p2Root, renders the self-contained p3 config tree under p3Root (install-only renders
	// none), and writes the <output>.digests.json sidecar beside outputPath.
	BuildLayout(ctx context.Context, log *logrus.Logger, m *Manifest, opts ImageSetOptions, outputPath, p2Root, p3Root string) (*LayoutBuildResult, error)
	// BuildStore builds the read-only containers/storage durability store under p2Root for
	// self-contained media. runtimeStorePath/targetArch are empty to use the build defaults.
	BuildStore(ctx context.Context, log *logrus.Logger, m *Manifest, p2Root, runtimeStorePath, targetArch string) (*StoreBuildResult, error)
}

// defaultImagePopulator wraps the production BuildImageLayout / BuildDurabilityStore.
type defaultImagePopulator struct{}

func (defaultImagePopulator) BuildLayout(ctx context.Context, log *logrus.Logger, m *Manifest, opts ImageSetOptions, outputPath, p2Root, p3Root string) (*LayoutBuildResult, error) {
	return BuildImageLayout(ctx, log, m, opts, outputPath, p2Root, p3Root)
}

func (defaultImagePopulator) BuildStore(ctx context.Context, log *logrus.Logger, m *Manifest, p2Root, runtimeStorePath, targetArch string) (*StoreBuildResult, error) {
	return BuildDurabilityStore(ctx, log, m, p2Root, runtimeStorePath, targetArch)
}

// mediaBuildDeps bundles the injectable seams: the privileged partition ops, the live-ISO
// builder, and the ibi-images content populator.
type mediaBuildDeps struct {
	partition partitionOps
	iso       liveISOBuilder
	images    imagePopulator
}

// defaultMediaBuildDeps returns the production seams backed by the host ops helpers, the real
// live-ISO build, and the real OCI image-layout / durability-store population.
func defaultMediaBuildDeps(log *logrus.Logger) mediaBuildDeps {
	return mediaBuildDeps{
		partition: &opsPartitionOps{
			ops: ops.NewOps(log, ops.NewRegularExecutor(log, false)),
			log: log,
		},
		iso:    defaultLiveISOBuilder{},
		images: defaultImagePopulator{},
	}
}

// MediaHandle is the live media container the assembly pipeline threads through: the image
// path, the attached loop device, the computed plan, the snapshot of the ISO's own partitions
// (never touched), and whether the target is an image file. Close is the single teardown
// entry point.
type MediaHandle struct {
	ImagePath  string
	LoopDevice string
	Plan       PartitionPlan
	ISOParts   []string
	IsImage    bool

	partition partitionOps
}

// Close discards a partial media image after detaching its loop device.
//
// If the detach fails, Close returns the error and leaves LoopDevice set and the backing image
// in place: the loop device may still be attached to the image, so removing the image would
// orphan an attachment and lose the only handle a caller has to retry the detach. The caller
// can call Close again to retry once the underlying cause is cleared.
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

// Finalize releases the loop device and keeps the assembled image for the caller. A detach
// failure leaves both the loop and image available for recovery.
func (h *MediaHandle) Finalize() error {
	if err := h.detach(); err != nil {
		return err
	}
	f, err := os.OpenFile(h.ImagePath, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("opening media image %s for sync: %w", h.ImagePath, err)
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return fmt.Errorf("syncing and closing media image %s: %w", h.ImagePath, err)
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

// Build assembles bootable USB-based IBI media: it builds the customized live ISO, writes it to
// the front of the output image (LBA 0), and appends the formatted, labelled data partitions
// after it following the CNF-27197-proven DOS/MBR layout.
func Build(ctx context.Context, log *logrus.Logger, m *Manifest, opts BuildOptions) error {
	return build(ctx, log, m, opts, defaultMediaBuildDeps(log))
}

// build is the dependency-injected core of Build. It is unexported so tests exercise the public
// behavior with fake seams while production Build injects defaultMediaBuildDeps.
func build(ctx context.Context, log *logrus.Logger, m *Manifest, opts BuildOptions, deps mediaBuildDeps) error {
	// Reject the destructive device path before any ISO build or attach: the only target this
	// pipeline writes is a sparse .img, so a real block device is never overwritten.
	if opts.WriteDevice != "" {
		return fmt.Errorf("--write-device is not yet supported for create-usb media")
	}
	if opts.OutputPath == "" {
		return fmt.Errorf("an output .img path is required")
	}

	// Build the customized bootable live ISO under a scratch work directory. The ISO is copied
	// into the output image during assembly, so the work directory is removed afterwards.
	workDir, err := os.MkdirTemp("", "create-usb-iso-")
	if err != nil {
		return fmt.Errorf("creating live-ISO work directory: %w", err)
	}
	defer func() {
		if rerr := os.RemoveAll(workDir); rerr != nil {
			log.Warnf("create-usb: removing live-ISO work directory %s: %v", workDir, rerr)
		}
	}()

	log.Infof("create-usb: building customized live ISO (mode=%s)", m.Mode)
	isoResult, err := deps.iso.Build(ctx, log, m, ISOBuildOptions{
		LCAImage: opts.LCAImage,
		AuthFile: opts.AuthFile,
		WorkDir:  workDir,
	})
	if err != nil {
		return fmt.Errorf("building live ISO: %w", err)
	}

	handle, err := assembleMedia(ctx, log, m, isoResult, opts, deps)
	if err != nil {
		return err
	}

	if err := handle.Finalize(); err != nil {
		return fmt.Errorf("finalizing media %s: %w", handle.ImagePath, err)
	}
	log.Infof("create-usb: assembled bootable %s media at %s (live ISO at LBA 0 + appended data partitions)",
		m.Mode, handle.ImagePath)
	return nil
}

// assembleMedia writes the customized live ISO to the front of the output image, attaches the
// loopback, appends the data partitions into the free MBR slots after the ISO, formats and
// labels them, and (self-contained) populates cluster-config. It returns a live *MediaHandle
// with the loop still attached; the caller owns teardown via MediaHandle.Finalize/Close.
//
// It owns its own lifecycle on failure: on any internal error it detaches the loop device (if
// attached) and removes the partial .img before returning (nil, err), via defer-based rollback
// keyed on success. The one exception is a cluster-config unmount failure, where the filesystem
// is still mounted over the loop device: the rollback preserves the image + loop handle instead
// (see errP3Mounted).
func assembleMedia(ctx context.Context, log *logrus.Logger, m *Manifest, isoResult ISOBuildResult, opts BuildOptions, deps mediaBuildDeps) (handle *MediaHandle, err error) {
	if cerr := ctx.Err(); cerr != nil {
		return nil, fmt.Errorf("media assembly canceled: %w", cerr)
	}

	// Invariant guard: ibi-status is always ext4 (no operator input); fail loudly on a regression
	// before touching the disk.
	if verr := validateIBIStatusFSType(ibiStatusFSType); verr != nil {
		return nil, verr
	}

	plan := partitionPlan(m.Mode)
	imgPath := opts.OutputPath
	isoPath := isoResult.ISOPath

	// Fail fast if the output image already exists, before the expensive payload staging below.
	// writeLiveISOToFront is the authoritative guard (O_EXCL); this early check only avoids staging
	// the whole payload just to discard it when --output already exists.
	if err = ensureOutputAbsent(imgPath); err != nil {
		return nil, err
	}

	// Validate the ISO layout as a precondition before any expensive staging or image write:
	// inspect the ISO's actual MBR entries and confirm it leaves enough free primary slots for the
	// mode. The free-slot count depends only on the number of data partitions, not their sizes, so
	// the provisional (floor-sized) plan is sufficient here and the check fails fast on a bad ISO
	// before the payload is staged.
	if err = validateISOLayout(log, isoPath, plan); err != nil {
		return nil, err
	}

	// For self-contained media the image-layout stage renders the digest-pinned p3 config tree,
	// which the cluster-config populate stage then copies onto its partition. Share one scratch
	// render directory between the two stages. A pre-rendered opts.P3Root overrides it (used
	// directly by populateClusterConfig). install-only renders no tree.
	p3Root := opts.P3Root
	if m.Mode == ModeSelfContained && p3Root == "" {
		renderDir, derr := os.MkdirTemp("", "create-usb-p3-render-")
		if derr != nil {
			return nil, fmt.Errorf("creating p3 config-tree render directory: %w", derr)
		}
		defer func() {
			if rerr := os.RemoveAll(renderDir); rerr != nil {
				log.Warnf("create-usb: removing p3 config-tree render directory %s: %v", renderDir, rerr)
			}
		}()
		p3Root = renderDir
	}

	// Stage the ibi-images payload into scratch BEFORE partition planning so the partition is sized
	// to the actual payload (the OCI layout + digests.json + the staged lca-cli binary + the
	// self-contained durability store), not a fixed reservation. The staged tree is copied onto the
	// formatted partition later by copyStagedIBIImages.
	//
	// Stage beside the output image rather than in os.TempDir(): the payload is multiple GiB (the
	// partition floor alone is 8 GiB), and on hosts where /tmp is a RAM-backed tmpfs (the Fedora
	// default) staging there can fail with ENOSPC or pressure memory. The output volume is where the
	// user already expects the large (sparse) image to be written.
	stagingRoot, err := os.MkdirTemp(filepath.Dir(imgPath), "create-usb-ibi-images-staging-")
	if err != nil {
		return nil, fmt.Errorf("creating ibi-images staging directory: %w", err)
	}
	defer func() {
		if rerr := os.RemoveAll(stagingRoot); rerr != nil {
			log.Warnf("create-usb: removing ibi-images staging directory %s: %v", stagingRoot, rerr)
		}
	}()

	payloadBytes, err := stageIBIImagesPayload(ctx, log, deps, m, isoResult, opts, stagingRoot, p3Root)
	if err != nil {
		return nil, err
	}

	// Size the ibi-images partition to the measured payload (floored at p2MinReservationBytes) and
	// recompute the container size, then enforce the USB-size budget against the resulting final
	// image size. An oversized payload fails here, at planning time, with a clear size error --
	// before anything is written to the stick.
	ibiImagesSize := sizeIBIImagesPartition(payloadBytes)
	setIBIImagesSize(plan, ibiImagesSize)
	log.Infof("create-usb: ibi-images payload is %d bytes; sized the ibi-images partition to %d bytes (minimum %d)",
		payloadBytes, ibiImagesSize, p2MinReservationBytes)

	total, err := containerSize(isoPath, plan)
	if err != nil {
		return nil, err
	}

	log.Infof("create-usb: assembling media layout (mode=%s, container=%d bytes, data partitions=%d)",
		m.Mode, total, len(plan.Partitions))

	// Enforce the USB-size budget against the final apparent image size (ISO + data partitions +
	// extended-container/alignment overhead), before anything is written.
	if err = validateUSBSizeBudget(opts.USBSizeBytes, total); err != nil {
		return nil, err
	}

	// Copy the ISO byte-for-byte to the front of the image (preserving its DOS/MBR + EFI
	// entries) and grow the image to the full media size. This refuses to overwrite an existing
	// --output path (O_EXCL).
	if err = writeLiveISOToFront(imgPath, isoPath, total); err != nil {
		return nil, err
	}

	// Roll back everything this call allocated unless it completes successfully. On success the
	// caller owns teardown, so success clears the rollback.
	var loopDev string
	success := false
	defer func() {
		if success {
			return
		}
		// If a populated partition (ibi-images or cluster-config) could not be unmounted, its
		// filesystem is still mounted over the loop device. Detaching the loop and removing the
		// backing image would discard the only path to recover or retry the unmount, so preserve
		// both and surface where things stand.
		if errors.Is(err, errP2Mounted) || errors.Is(err, errP3Mounted) {
			log.Warnf("create-usb: a populated partition is still mounted; preserving loop device %s "+
				"and media image %s for recovery (unmount it, then detach %s and remove the image)",
				loopDev, imgPath, loopDev)
			return
		}
		if loopDev != "" {
			if derr := deps.partition.DetachLoopback(loopDev); derr != nil {
				err = errors.Join(err, fmt.Errorf("detaching loop device %s during rollback: %w; preserving media image %s for recovery", loopDev, derr, imgPath))
				return
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

	// Snapshot the ISO's own partitions before appending, so the data partitions can be
	// identified by exclusion and the ISO's ISO9660/EFI partitions are never formatted.
	isoParts, err := deps.partition.SnapshotPartitions(loopDev)
	if err != nil {
		return nil, fmt.Errorf("snapshotting the live ISO partitions on %s: %w", loopDev, err)
	}
	if len(isoParts) == 0 {
		return nil, fmt.Errorf("no partitions found on %s after writing the live ISO; is the source a RHCOS isohybrid live ISO?", loopDev)
	}

	// Pin the first appended partition to the first safe append offset (the MiB-aligned end of the
	// ISO), the same boundary containerSize sizes the image around. Without an explicit start,
	// `sfdisk --append` uses the first unassigned sector, which for an ISO whose last occupied
	// partition ends before that offset would land the data partition inside the copied ISO bytes
	// and let mkfs overwrite them.
	isoInfo, serr := os.Stat(isoPath)
	if serr != nil {
		return nil, fmt.Errorf("stat live ISO %s: %w", isoPath, serr)
	}
	appendStartSector := alignUpToMiB(isoInfo.Size()) / mbrSectorSize
	if err = deps.partition.AppendDataPartitions(loopDev, plan, appendStartSector); err != nil {
		return nil, fmt.Errorf("appending data partitions on %s: %w", loopDev, err)
	}

	handle = &MediaHandle{
		ImagePath:  imgPath,
		LoopDevice: loopDev,
		Plan:       plan,
		ISOParts:   isoParts,
		IsImage:    true,
		partition:  deps.partition,
	}

	if err = formatDataPartitions(log, deps, handle); err != nil {
		return nil, err
	}

	// Copy the staged ibi-images payload (OCI image layout + on-media digests.json, the staged
	// lca-cli binary the embedded unit execs on boot, and the self-contained durability store) onto
	// the formatted partition, verifying it fits the sized filesystem. Runs before success so any
	// failure triggers the rollback above; an unmount failure is tagged errP2Mounted so the rollback
	// preserves the still-mounted image + loop handle.
	if err = copyStagedIBIImages(log, deps, handle, stagingRoot, payloadBytes); err != nil {
		return nil, err
	}

	// Self-contained: populate the cluster-config partition from the rendered config tree. It
	// runs before success, so a populate/unmount failure triggers the rollback above (an unmount
	// failure preserves the image + loop handle). install-only has no cluster-config partition,
	// so the stage is a no-op there.
	if err = populateClusterConfig(ctx, log, deps, handle, p3Root); err != nil {
		return nil, err
	}

	success = true
	return handle, nil
}

// ensureOutputAbsent fails if imgPath already exists, so a build fails fast before staging the
// payload rather than discarding the staged work when writeLiveISOToFront's O_EXCL later refuses to
// overwrite. A stat error other than not-exist is surfaced too.
func ensureOutputAbsent(imgPath string) error {
	_, statErr := os.Stat(imgPath)
	if statErr == nil {
		return fmt.Errorf("output image %s already exists; refusing to overwrite", imgPath)
	}
	if !os.IsNotExist(statErr) {
		return fmt.Errorf("checking output image %s: %w", imgPath, statErr)
	}
	return nil
}

// formatDataPartitions maps the appended data partitions to their device nodes (by exclusion of
// the ISO's own partitions) and formats each with its planned filesystem type and label, in the
// plan's creation order. It copies no content; ibi-images and ibi-status ship empty, and
// cluster-config is populated by populateClusterConfig.
func formatDataPartitions(log *logrus.Logger, deps mediaBuildDeps, h *MediaHandle) error {
	nodes, err := deps.partition.DataPartitionNodes(h.LoopDevice, h.ISOParts)
	if err != nil {
		return fmt.Errorf("resolving appended data partition nodes on %s: %w", h.LoopDevice, err)
	}
	if len(nodes) != len(h.Plan.Partitions) {
		return fmt.Errorf("expected %d appended data partitions on %s, found %d: %v",
			len(h.Plan.Partitions), h.LoopDevice, len(nodes), nodes)
	}
	for i, part := range h.Plan.Partitions {
		log.Infof("create-usb: formatting %s (%s, %d bytes) as %s", nodes[i], part.Label, part.Size, part.FSType)
		if err := deps.partition.FormatPartition(nodes[i], part.FSType, part.Label); err != nil {
			return fmt.Errorf("formatting %s (%s) as %s: %w", nodes[i], part.Label, part.FSType, err)
		}
	}
	return nil
}

// partitionPlan returns the ordered data partitions a mode appends after the live ISO:
// ibi-images + ibi-status for install-only, ibi-images + cluster-config + ibi-status for
// self-contained. The filesystem types come from the per-partition stage files (all three are
// ext4: ibiImagesFSType / clusterConfigFSType / ibiStatusFSType). The order is the creation order
// the sfdisk append and the device-node mapping rely on.
func partitionPlan(mode Mode) PartitionPlan {
	parts := []PlannedPartition{
		{Label: labelIBIImages, FSType: ibiImagesFSType, Size: p2MinReservationBytes},
	}
	if mode == ModeSelfContained {
		parts = append(parts, PlannedPartition{Label: labelClusterConfig, FSType: clusterConfigFSType, Size: p3ReservationBytes})
	}
	parts = append(parts, PlannedPartition{Label: labelIBIStatus, FSType: ibiStatusFSType, Size: p4ReservationBytes})
	return PartitionPlan{Mode: mode, Partitions: parts}
}

// sizeIBIImagesPartition derives the ibi-images partition reservation from the measured on-media
// payload: the payload plus free-space headroom and a fixed ext4-metadata allowance, rounded up to
// a whole MiB, floored at p2MinReservationBytes. Sizing the partition to the payload (rather than
// a fixed 8 GiB) is what makes an oversized payload fail the --usb-size budget at planning time
// with a clear size error instead of overrunning a fixed partition during population.
func sizeIBIImagesPartition(payloadBytes int64) int64 {
	sized := alignUpToMiB(payloadBytes*ibiImagesHeadroomNum/ibiImagesHeadroomDen + ibiImagesMetadataOverheadBytes)
	if sized < p2MinReservationBytes {
		return p2MinReservationBytes
	}
	return sized
}

// setIBIImagesSize resizes the ibi-images partition in plan to sizeBytes (the payload-derived
// reservation). partitionPlan returns a freshly-allocated slice each call, so mutating it in place
// is safe. The other partitions are fixed-size and left unchanged.
func setIBIImagesSize(plan PartitionPlan, sizeBytes int64) {
	for i := range plan.Partitions {
		if plan.Partitions[i].Label == labelIBIImages {
			plan.Partitions[i].Size = sizeBytes
			return
		}
	}
}

// containerSize is the total image size: the live ISO (rounded up to a whole MiB so the first
// appended partition starts MiB-aligned) plus every data partition, the self-contained extended
// container overhead, and the whole-container slack. The result is MiB-aligned.
func containerSize(isoPath string, plan PartitionPlan) (int64, error) {
	info, err := os.Stat(isoPath)
	if err != nil {
		return 0, fmt.Errorf("stat live ISO %s: %w", isoPath, err)
	}
	total := alignUpToMiB(info.Size())
	for _, p := range plan.Partitions {
		total += p.Size
	}
	if plan.Mode == ModeSelfContained {
		total += extSlackBytes
	}
	total += slackBytes
	return alignUpToMiB(total), nil
}

// alignUpToMiB rounds v up to the next whole mebibyte -- the alignment the media layout uses for
// the ISO end and every appended partition boundary.
func alignUpToMiB(v int64) int64 {
	return ((v + mib - 1) / mib) * mib
}

// validateUSBSizeBudget fails the build when the resulting whole-stick size exceeds the --usb-size
// budget. total is the final apparent image size computed by containerSize -- the live ISO plus
// every data partition and the extended-container/alignment overhead -- so the check reflects what
// is actually written to the stick, not the staged-byte subtotal (SizeReport.StagedBytes, which
// deliberately excludes that overhead). A zero/negative budget disables the check.
func validateUSBSizeBudget(budgetBytes, total int64) error {
	if budgetBytes <= 0 {
		return nil
	}
	if total > budgetBytes {
		return fmt.Errorf("assembled media is %d bytes, exceeding the --usb-size budget of %d bytes by %d bytes; use a larger USB stick (--usb-size) or reduce the on-media payload",
			total, budgetBytes, total-budgetBytes)
	}
	return nil
}

// plannedPartition returns the planned data partition with the given label, and whether the plan
// has such an entry. Callers that require the partition turn a false into an error; callers for
// which it is optional (cluster-config, absent in install-only) skip.
func plannedPartition(plan PartitionPlan, label string) (PlannedPartition, bool) {
	for _, part := range plan.Partitions {
		if part.Label == label {
			return part, true
		}
	}
	return PlannedPartition{}, false
}

// ---- production seam implementation ----

// opsPartitionOps is the production partitionOps backed by the host util ops (losetup, sfdisk,
// blockdev, udevadm, mkfs, blkid, lsblk) run in the host namespace.
type opsPartitionOps struct {
	ops ops.Ops
	log *logrus.Logger
}

// AttachLoopback attaches imgPath to a loop device (with --partscan so the ISO's own partitions
// are exposed) and returns the device path.
func (p *opsPartitionOps) AttachLoopback(imgPath string) (string, error) {
	out, err := p.ops.RunInHostNamespace("losetup", "--find", "--show", "--partscan", imgPath)
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

// SnapshotPartitions returns every partition device node currently in dev's table (sfdisk
// --dump), used to capture the ISO's own partitions before the data partitions are appended.
func (p *opsPartitionOps) SnapshotPartitions(dev string) ([]string, error) {
	out, err := p.ops.RunInHostNamespace("sfdisk", "--dump", dev)
	if err != nil {
		return nil, fmt.Errorf("dumping partition table on %s: %w", dev, err)
	}
	nodes := make([]string, 0)
	for _, e := range parseSfdiskDump(out) {
		nodes = append(nodes, e.node)
	}
	return nodes, nil
}

// AppendDataPartitions appends plan's data partitions into the free MBR slots after the ISO with
// sfdisk --append, then re-reads the kernel partition table. For self-contained it adds an MBR
// extended container to hold the cluster-config and ibi-status logical partitions (MBR allows
// only 4 primaries and the ISO already uses two). The sfdisk script is fed via a temp file
// redirected on stdin because the host-ops executor has no stdin channel.
func (p *opsPartitionOps) AppendDataPartitions(dev string, plan PartitionPlan, startSector int64) error {
	script, err := sfdiskAppendScript(plan, startSector)
	if err != nil {
		return err
	}
	scriptFile, err := os.CreateTemp("", "create-usb-sfdisk-")
	if err != nil {
		return fmt.Errorf("creating sfdisk append script: %w", err)
	}
	defer func() {
		if rerr := os.Remove(scriptFile.Name()); rerr != nil && !os.IsNotExist(rerr) {
			p.log.Warnf("create-usb: removing sfdisk append script %s: %v", scriptFile.Name(), rerr)
		}
	}()
	if _, werr := scriptFile.WriteString(script); werr != nil {
		_ = scriptFile.Close()
		return fmt.Errorf("writing sfdisk append script: %w", werr)
	}
	if cerr := scriptFile.Close(); cerr != nil {
		return fmt.Errorf("closing sfdisk append script: %w", cerr)
	}

	if _, err := p.ops.RunBashInHostNamespace("sfdisk", "--append", dev, "<", scriptFile.Name()); err != nil {
		return fmt.Errorf("appending data partitions on %s: %w", dev, err)
	}
	return p.rereadPartitionTable(dev)
}

// rereadPartitionTable asks the kernel to re-read dev's partition table and waits for the new
// device nodes to settle. partprobe is intentionally not used (it is absent on the RHCOS base
// OS, per the CNF-27197 spike); the supported reread is `blockdev --rereadpt`.
func (p *opsPartitionOps) rereadPartitionTable(dev string) error {
	if _, err := p.ops.RunInHostNamespace("blockdev", "--rereadpt", dev); err != nil {
		return fmt.Errorf("re-reading partition table on %s: %w", dev, err)
	}
	// udevadm settle is best effort: the node resolution below is the real guard.
	if _, err := p.ops.RunInHostNamespace("udevadm", "settle"); err != nil {
		p.log.Warnf("create-usb: udevadm settle after re-reading the partition table on %s failed: %v", dev, err)
	}
	return nil
}

// DataPartitionNodes returns the appended data partition nodes in creation order: the Linux
// (type 83) partitions in dev's table that are not part of isoParts (the ISO's own partitions)
// and are not the extended container (type 5).
func (p *opsPartitionOps) DataPartitionNodes(dev string, isoParts []string) ([]string, error) {
	out, err := p.ops.RunInHostNamespace("sfdisk", "--dump", dev)
	if err != nil {
		return nil, fmt.Errorf("dumping partition table on %s: %w", dev, err)
	}
	iso := make(map[string]struct{}, len(isoParts))
	for _, n := range isoParts {
		iso[n] = struct{}{}
	}
	nodes := make([]string, 0)
	for _, e := range parseSfdiskDump(out) {
		if e.typeCode != mbrTypeLinux {
			continue
		}
		if _, ok := iso[e.node]; ok {
			continue
		}
		nodes = append(nodes, e.node)
	}
	return nodes, nil
}

// FormatPartition creates a fsType filesystem on partDev with the given FS label. The formatter
// and its force flag are selected per type by mkfsCommand (ext4 -F, xfs -f), and an unsupported
// type fails before any mkfs runs.
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

// ResolvePartitionByFSLabel returns the device node on dev whose filesystem label matches label.
// It enumerates only dev's own partitions (lsblk) and checks each one's filesystem label (blkid),
// never a global `blkid -L`, so it cannot resolve a same-labelled partition on another disk.
func (p *opsPartitionOps) ResolvePartitionByFSLabel(dev, label string) (string, error) {
	out, err := p.ops.RunInHostNamespace("lsblk", "-rno", "NAME,TYPE", dev)
	if err != nil {
		return "", fmt.Errorf("listing partitions on %s: %w", dev, err)
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != "part" {
			continue
		}
		node := "/dev/" + fields[0]
		fsLabel, err := p.ops.RunInHostNamespace("blkid", "-s", "LABEL", "-o", "value", node)
		if err != nil {
			// blkid exits non-zero for a partition with no filesystem label; that is not the one
			// we want, so skip it rather than failing the whole resolution.
			continue
		}
		if strings.TrimSpace(fsLabel) == label {
			return node, nil
		}
	}
	return "", fmt.Errorf("could not resolve a partition by filesystem label %q on %s", label, dev)
}

// MountPartition mounts partDev at mountpoint (which must already exist).
func (p *opsPartitionOps) MountPartition(partDev, mountpoint string) error {
	if _, err := p.ops.RunInHostNamespace("mount", partDev, mountpoint); err != nil {
		return fmt.Errorf("mounting %s at %s: %w", partDev, mountpoint, err)
	}
	return nil
}

// UnmountPartition unmounts mountpoint. It must run before the loop device is detached.
func (p *opsPartitionOps) UnmountPartition(mountpoint string) error {
	if _, err := p.ops.RunInHostNamespace("umount", mountpoint); err != nil {
		return fmt.Errorf("unmounting %s: %w", mountpoint, err)
	}
	return nil
}

// sfdiskDumpEntry is one parsed `sfdisk --dump` partition row: its device node and MBR type code.
type sfdiskDumpEntry struct {
	node     string
	typeCode string
}

// parseSfdiskDump parses the partition rows of `sfdisk --dump` output. Each data row looks like
//
//	/dev/loop0p3 : start=     2764800, size=  16777216, type=83
//
// The leading device node and the type= field are extracted; non-partition lines (the label/
// label-id/device/unit header and blank lines) are skipped.
func parseSfdiskDump(out string) []sfdiskDumpEntry {
	var entries []sfdiskDumpEntry
	for _, line := range strings.Split(out, "\n") {
		node, _, ok := strings.Cut(line, " : ")
		if !ok {
			continue
		}
		node = strings.TrimSpace(node)
		if !strings.HasPrefix(node, "/dev/") {
			continue
		}
		typeCode := ""
		for _, field := range strings.Split(line, ",") {
			field = strings.TrimSpace(field)
			if v, found := strings.CutPrefix(field, "type="); found {
				typeCode = strings.TrimSpace(v)
			}
		}
		entries = append(entries, sfdiskDumpEntry{node: node, typeCode: typeCode})
	}
	return entries
}

// sfdiskAppendScript builds the `sfdisk --append` script for plan's data partitions. For
// install-only it appends two primaries (ibi-images, ibi-status); for self-contained it appends
// ibi-images as a primary, then an extended container sized to hold the cluster-config and
// ibi-status logicals plus their EBR/alignment overhead.
//
// The first (ibi-images) partition is pinned to startSector -- the first safe append offset, the
// MiB-aligned end of the ISO. sfdisk otherwise places an appended partition at the first
// unassigned sector, which for an ISO whose last occupied partition ends before that offset would
// start the data partition inside the copied ISO bytes and let mkfs overwrite them. The remaining
// partitions carry no explicit start, so sfdisk lays them out contiguously after the first.
func sfdiskAppendScript(plan PartitionPlan, startSector int64) (string, error) {
	if startSector <= 0 {
		return "", fmt.Errorf("invalid append start sector %d: the first data partition must start after the ISO", startSector)
	}
	images, ok := plannedPartition(plan, labelIBIImages)
	if !ok {
		return "", fmt.Errorf("media layout is missing the %q partition", labelIBIImages)
	}
	status, ok := plannedPartition(plan, labelIBIStatus)
	if !ok {
		return "", fmt.Errorf("media layout is missing the %q partition", labelIBIStatus)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "start=%d, size=%dM, type=%s\n", startSector, images.Size/mib, mbrTypeLinux)
	if plan.Mode == ModeSelfContained {
		config, ok := plannedPartition(plan, labelClusterConfig)
		if !ok {
			return "", fmt.Errorf("self-contained media layout is missing the %q partition", labelClusterConfig)
		}
		extMB := (config.Size + status.Size + extSlackBytes) / mib
		fmt.Fprintf(&b, "size=%dM, type=%s\n", extMB, mbrTypeExtended)
		fmt.Fprintf(&b, "size=%dM, type=%s\n", config.Size/mib, mbrTypeLinux)
		fmt.Fprintf(&b, "size=%dM, type=%s\n", status.Size/mib, mbrTypeLinux)
	} else {
		fmt.Fprintf(&b, "size=%dM, type=%s\n", status.Size/mib, mbrTypeLinux)
	}
	return b.String(), nil
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

// isAlreadyDetached reports whether err from `losetup -d` indicates the device was already
// detached, which teardown treats as success.
func isAlreadyDetached(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such device") || strings.Contains(msg, "no such file")
}
