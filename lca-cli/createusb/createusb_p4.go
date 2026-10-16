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

// This file implements the p4 (ibi-status) slice of the create-usb media-assembly pipeline
// (USB-based IBI proposal §6.4 step 5, §7.4): add the GPT entry for the ibi-status partition
// into the region the base layout (CNF-28667) already reserved, make a filesystem on it, and
// set its FS label to ibi-status, leaving it empty, writable, and unmounted. CNF-27163 populates
// p4 at runtime (prepare.json / reconfigure.json / /logs); this story only creates the empty slot.
//
// # Shared create/format sequence
//
// p4 creates and formats its partition through createAndFormatPartition (createusb_assemble.go) --
// the single shared create -> settle -> format sequence p2, p3, and p4 all use (GPT creation,
// partition-table refresh, bounded device-node resolution -- including a bounded udevadm settle --
// and ext4 formatting all live there). Like p2, p4 needs no mount: it ships an empty filesystem,
// so it stops after formatting. This file carries only p4-specific policy (the fixed-ext4
// invariant guard and the plan lookup), not its own copy of the partition primitives.
//
// # ext4 is a fixed invariant, not an operator knob
//
// There is no filesystem-type field in the create-usb manifest schema, so no operator- or
// API-facing path can ever supply a filesystem type: p4 is always ext4. validateIBIStatusFSType
// is a narrow internal invariant guard that fails the build loudly (naming vfat explicitly) if the
// fixed ibiStatusFSType constant is ever regressed away from a journaled filesystem. Durability is
// the runtime writer's (CNF-27163) responsibility -- it must implement and test the
// temp -> fsync -> rename -> fsync(dir) -> sync sequence; p4's only job is to provide a
// correctly-formatted, empty, writable ext4 substrate.

import (
	"fmt"

	"github.com/sirupsen/logrus"
)

// ibiStatusFSType is the filesystem created on the p4 (ibi-status) partition. p4 must be a
// journaled filesystem (ext4) because the runtime writer (CNF-27163) targets it as its durable
// substrate; see validateIBIStatusFSType. A non-journaled type such as vfat is not supported.
const ibiStatusFSType = fsTypeExt4

// validateIBIStatusFSType enforces p4's fixed-filesystem-type invariant: p4 must be ext4. The
// create-usb manifest has no filesystem-type field, so this is not an operator-facing validation
// -- it is an internal guard against a code regression that changes ibiStatusFSType away from a
// journaled filesystem (e.g. to vfat). It names vfat explicitly and states the
// journaled-filesystem requirement so the failure is self-explanatory.
func validateIBIStatusFSType(fsType string) error {
	if fsType != ibiStatusFSType {
		return fmt.Errorf("p4 (ibi-status) filesystem type must be %q, got %q: p4 must be a journaled "+
			"filesystem so the runtime writer (CNF-27163) has a durable substrate; a non-journaled type "+
			"such as vfat is not supported", ibiStatusFSType, fsType)
	}
	return nil
}

// createP4Partition adds and formats the empty p4 (ibi-status) partition in the region the base
// layout already reserved: it creates the GPT entry at p4's planned offset, makes an ext4
// filesystem on it, and sets its FS label to ibi-status. p4 is present in both modes' plans
// (fixed number 4), so this runs unconditionally. It copies no content and never mounts -- p4
// ships empty, writable, and unmounted; the runtime writer (CNF-27163) populates it later.
//
// It is a separate step from assembleMedia so the p1 foundation and its tests stay unchanged;
// build threads the live *MediaHandle from assembleMedia into it and owns teardown on failure.
func createP4Partition(log *logrus.Logger, h *MediaHandle, deps mediaBuildDeps) error {
	// Invariant guard: p4 is always ext4 (no operator input); fail loudly on a regression before
	// touching the disk.
	if err := validateIBIStatusFSType(ibiStatusFSType); err != nil {
		return err
	}

	p4, ok := plannedPartition(h.Plan, labelIBIStatus)
	if !ok {
		return fmt.Errorf("partition %q not found in the media layout", labelIBIStatus)
	}
	log.Infof("create-usb: creating and formatting p%d (%s, %d bytes) as %s",
		p4.Number, p4.Label, p4.Size, ibiStatusFSType)
	// p4 ships an empty filesystem, so it stops after the shared create -> settle -> format
	// sequence and never mounts; the returned device node is unused here.
	if _, err := createAndFormatPartition(deps.partition, h.LoopDevice, p4, ibiStatusFSType); err != nil {
		return fmt.Errorf("creating p%d (%s): %w", p4.Number, p4.Label, err)
	}
	return nil
}
