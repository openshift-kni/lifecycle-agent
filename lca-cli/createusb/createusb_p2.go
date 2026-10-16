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

// This file implements the p2 (ibi-images) slice of the create-usb media-assembly pipeline
// (USB-based IBI proposal §6.4 step 5, CNF-28668): add the GPT entry for the ibi-images
// partition into the region the base layout (CNF-28667) already reserved, make a filesystem
// on it, and set its FS label to ibi-images (prepare discovers p2 by this label).
//
// # Empty partition
//
// Per the agreed scope this story creates an *empty* partition only -- it copies no content.
// The build-unique OCI image layout + digests.json (staged into p2Root by CNF-27167's
// BuildImageLayout) and the read-only durability store/ are written onto p2 by the later
// populate stories (CNF-27170/CNF-27171).
//
// # Seam extension
//
// It extends the partitionOps seam with a single generic AddPartition method (create GPT
// entry -> refresh table -> wait for the node -> format) so the p3/p4 stories
// (CNF-28669/CNF-28670) reuse it. Mounting is deliberately *not* added here: it has no
// consumer until the populate story, matching how CNF-28667 deferred the real device write.

import (
	"fmt"
	"os"
	"time"

	"github.com/sirupsen/logrus"
)

// ibiImagesFSType is the filesystem created on the p2 (ibi-images) partition. ext4 is used
// because ibi-images is new create-usb media with no prepare/runtime filesystem constraint
// (nothing mounts or opens p2 by a required type); it is the conservative default. The
// mkfs.xfs precedent elsewhere in the tree (ops.CreateExtraPartition) is the
// installation-disk container-storage path, a different storage path, so it does not
// dictate the media p2 type.
const ibiImagesFSType = fsTypeExt4

// Supported filesystem types for AddPartition. Each takes different mkfs options, so the
// correct formatter + flags are selected per type rather than reusing one flag set.
const (
	fsTypeExt4 = "ext4"
	fsTypeXFS  = "xfs"
)

const (
	// partitionNodePollInterval is how often AddPartition polls for the new partition device
	// node to appear after refreshing the kernel partition table.
	partitionNodePollInterval = 100 * time.Millisecond
	// partitionNodeWaitTimeout bounds how long AddPartition waits for the partition device
	// node to appear before giving up.
	partitionNodeWaitTimeout = 5 * time.Second
)

// createP2Partition adds and formats the empty p2 (ibi-images) partition in the region the
// base layout already reserved: it creates the GPT entry at p2's planned offset, makes an
// ext4 filesystem on it, and sets its FS label to ibi-images (prepare discovers p2 by this
// label). It copies no content -- the OCI image layout + digests.json and the durability
// store/ are populated by later stories (CNF-27170/CNF-27171).
//
// It is a separate step from assembleMedia so the p1 foundation and its tests stay
// unchanged; build threads the live *MediaHandle from assembleMedia into it and owns
// teardown on failure.
func createP2Partition(log *logrus.Logger, h *MediaHandle, deps mediaBuildDeps) error {
	p2, err := partitionByLabel(h.Plan, labelIBIImages)
	if err != nil {
		return fmt.Errorf("locating the %s partition: %w", labelIBIImages, err)
	}
	log.Infof("create-usb: creating and formatting p%d (%s, %d bytes) as %s",
		p2.Number, p2.Label, p2.Size, ibiImagesFSType)
	if err := deps.partition.AddPartition(h.LoopDevice, p2, ibiImagesFSType); err != nil {
		return fmt.Errorf("creating p%d (%s): %w", p2.Number, p2.Label, err)
	}
	return nil
}

// partitionByLabel returns the planned partition with the given label, or an error if the
// plan has no such entry (e.g. a mode that omits that partition).
func partitionByLabel(plan PartitionPlan, label string) (PlannedPartition, error) {
	for _, part := range plan.Partitions {
		if part.Label == label {
			return part, nil
		}
	}
	return PlannedPartition{}, fmt.Errorf("partition %q not found in the media layout", label)
}

// partitionDevice returns the device node for partition number on loopDev (e.g.
// /dev/loop0 + 2 -> /dev/loop0p2). This "<dev>pN" form is correct for the
// loopback-attached container this phase writes; the real block-device path (which may use
// a different partition-node suffix) is deferred with --write-device.
func partitionDevice(loopDev string, number int) string {
	return fmt.Sprintf("%sp%d", loopDev, number)
}

// mkfsCommand returns the argv that makes fsType on node with the given filesystem label.
// mkfs.ext4 and mkfs.xfs take different options (ext4 force is -F, xfs force is -f), so the
// formatter + flags are selected per type; an unsupported type is a hard error rather than
// a silently-wrong flag set.
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

// AddPartition creates the GPT entry for part at its reserved offset on dev, refreshes the
// kernel partition table, waits for the new partition device node to appear, and formats it
// as fsType with part.Label as the filesystem label.
//
// The filesystem type is validated before any GPT entry is written, so an unsupported type
// fails without mutating the device. losetup attaches with -P and partprobe refreshes the
// table, but the <dev>pN node is created asynchronously by udev, so the method waits for the
// node (udevadm settle + a bounded existence poll) before mkfs to avoid racing it.
func (p *opsPartitionOps) AddPartition(dev string, part PlannedPartition, fsType string) error {
	node := partitionDevice(dev, part.Number)

	// Validate the filesystem type up front so an unsupported type fails before any GPT
	// entry is written to the device.
	mkfs, err := mkfsCommand(fsType, part.Label, node)
	if err != nil {
		return err
	}

	// Create the GPT entry at the reserved offset, computing start/end sectors exactly as
	// CreateBaseLayout does for p1 and reusing the placeholder type code (CNF-27168 and the
	// populate stories set final types/content when they write their partitions).
	startSector := part.Offset / sectorSize
	endSector := (part.Offset+part.Size)/sectorSize - 1
	if _, err := p.ops.RunInHostNamespace("sgdisk",
		fmt.Sprintf("--new=%d:%d:%d", part.Number, startSector, endSector),
		fmt.Sprintf("--change-name=%d:%s", part.Number, part.Label),
		fmt.Sprintf("--typecode=%d:%s", part.Number, placeholderTypeCode),
		dev); err != nil {
		return fmt.Errorf("creating partition %d (%s) on %s: %w", part.Number, part.Label, dev, err)
	}

	// Refresh the kernel partition table so the new node is created, then wait for it before
	// formatting (the node appears asynchronously, so mkfs can otherwise race it).
	if _, err := p.ops.RunInHostNamespace("partprobe", dev); err != nil {
		return fmt.Errorf("refreshing partition table on %s: %w", dev, err)
	}
	if err := p.waitForPartitionNode(node); err != nil {
		return err
	}

	if _, err := p.ops.RunInHostNamespace(mkfs[0], mkfs[1:]...); err != nil {
		return fmt.Errorf("formatting %s as %s: %w", node, fsType, err)
	}
	return nil
}

// waitForPartitionNode waits for node to appear after a partition-table refresh: it asks
// udev to settle (best effort) and then polls for the node with a bounded backoff, returning
// a clear error if the node never materializes.
func (p *opsPartitionOps) waitForPartitionNode(node string) error {
	if _, err := p.ops.RunInHostNamespace("udevadm", "settle"); err != nil {
		// Non-fatal: settle is best effort; the existence poll below is the real guard.
		p.log.Warnf("create-usb: udevadm settle before waiting for partition node %s failed: %v", node, err)
	}

	attempts := int(partitionNodeWaitTimeout / partitionNodePollInterval)
	for i := 0; ; i++ {
		_, err := os.Stat(node)
		if err == nil {
			return nil
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("checking for partition node %s: %w", node, err)
		}
		if i >= attempts {
			return fmt.Errorf("partition node %s did not appear within %s after refreshing the partition table",
				node, partitionNodeWaitTimeout)
		}
		time.Sleep(partitionNodePollInterval)
	}
}
