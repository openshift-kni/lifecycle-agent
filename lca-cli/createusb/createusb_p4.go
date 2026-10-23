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

// This file carries the p4 (ibi-status) filesystem policy for the create-usb media-assembly
// pipeline (USB-based IBI proposal §6.4 step 5, §7.4). In the DOS/MBR append-after-ISO layout the
// ibi-status partition is appended after the live ISO (for self-contained, as a logical inside the
// MBR extended container) and formatted + labelled by the shared format stage
// (formatDataPartitions in createusb_assemble.go), leaving it empty, writable, and unmounted.
// CNF-27163 populates it at runtime (prepare.json / reconfigure.json / /logs).
//
// # ext4 is a fixed invariant, not an operator knob
//
// There is no filesystem-type field in the create-usb manifest schema, so no operator- or
// API-facing path can ever supply a filesystem type: ibi-status is always ext4.
// validateIBIStatusFSType is a narrow internal invariant guard that fails the build loudly
// (naming vfat explicitly) if the fixed ibiStatusFSType constant is ever regressed away from a
// journaled filesystem. Durability is the runtime writer's (CNF-27163) responsibility; this
// pipeline's only job is to provide a correctly-formatted, empty, writable ext4 substrate.

import "fmt"

// ibiStatusFSType is the filesystem created on the ibi-status partition. It must be a journaled
// filesystem (ext4) because the runtime writer (CNF-27163) targets it as its durable substrate;
// see validateIBIStatusFSType. A non-journaled type such as vfat is not supported.
const ibiStatusFSType = fsTypeExt4

// validateIBIStatusFSType enforces ibi-status's fixed-filesystem-type invariant: it must be ext4.
// The create-usb manifest has no filesystem-type field, so this is not an operator-facing
// validation -- it is an internal guard against a code regression that changes ibiStatusFSType
// away from a journaled filesystem (e.g. to vfat). It names vfat explicitly and states the
// journaled-filesystem requirement so the failure is self-explanatory.
func validateIBIStatusFSType(fsType string) error {
	if fsType != ibiStatusFSType {
		return fmt.Errorf("ibi-status filesystem type must be %q, got %q: ibi-status must be a journaled "+
			"filesystem so the runtime writer (CNF-27163) has a durable substrate; a non-journaled type "+
			"such as vfat is not supported", ibiStatusFSType, fsType)
	}
	return nil
}
