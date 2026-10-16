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
// # Shared create/format sequence
//
// p2 creates and formats its partition through createAndFormatPartition (createusb_assemble.go) --
// the single shared create -> settle -> format sequence p2 and p3 both use (GPT creation,
// partition-table refresh, bounded device-node resolution, and ext4 formatting all live there).
// p2 needs no mount: it ships an empty filesystem, so it stops after formatting; p3 continues on
// to mount + populate.

import (
	"fmt"

	"github.com/sirupsen/logrus"
)

// ibiImagesFSType is the filesystem created on the p2 (ibi-images) partition. ext4 is used
// because ibi-images is new create-usb media with no prepare/runtime filesystem constraint
// (nothing mounts or opens p2 by a required type); it is the conservative default. The
// mkfs.xfs precedent elsewhere in the tree (ops.CreateExtraPartition) is the
// installation-disk container-storage path, a different storage path, so it does not
// dictate the media p2 type.
const ibiImagesFSType = fsTypeExt4

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
	p2, ok := plannedPartition(h.Plan, labelIBIImages)
	if !ok {
		return fmt.Errorf("partition %q not found in the media layout", labelIBIImages)
	}
	log.Infof("create-usb: creating and formatting p%d (%s, %d bytes) as %s",
		p2.Number, p2.Label, p2.Size, ibiImagesFSType)
	// p2 ships an empty filesystem, so it stops after the shared create -> settle -> format
	// sequence and never mounts; the returned device node is unused here.
	if _, err := createAndFormatPartition(deps.partition, h.LoopDevice, p2, ibiImagesFSType); err != nil {
		return fmt.Errorf("creating p%d (%s): %w", p2.Number, p2.Label, err)
	}
	return nil
}
