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

// This file owns the p1 live-ISO placement stage. In the CNF-27197-proven DOS/MBR layout the
// customized RHCOS live ISO owns the front of the media (LBA 0): it is copied byte-for-byte to
// the output image so its own DOS/MBR partition table and EFI entries -- which make it bootable
// under UEFI/Secure Boot -- stay at sector 0. The image is then grown to the full media size so
// the data partitions have room to be appended after the ISO (createusb_assemble.go).
//
// The customized ISO itself (fetch+verify, embedded ignition, embedded lca-cli binary) is built
// by createusb_p1_content.go.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"
)

// labelLiveISO names the live-ISO role in logs. The live ISO carries its own ISO9660/EFI
// partition table (it is not a create-usb-formatted partition), so this is a role label only,
// not a filesystem label set by create-usb.
const labelLiveISO = "live-iso"

// MBR (DOS) partition-table geometry used to inspect the ISO's layout as a precondition. An MBR
// lives in the first 512-byte sector: the partition table is four 16-byte entries at offset 446,
// each carrying its partition type at byte 4 (0x00 = unused), and the sector ends with the
// 0x55AA boot signature at offset 510.
const (
	mbrSectorSize           = 512
	mbrPartitionTableOffset = 446
	mbrPartitionEntrySize   = 16
	mbrPartitionEntryCount  = 4
	mbrPartitionTypeOffset  = 4
	mbrSignatureOffset      = 510
)

// requiredFreePrimarySlots is the number of top-level MBR slots assembleMedia appends after the
// ISO for either mode: install-only appends two primaries (ibi-images, ibi-status); self-contained
// appends ibi-images as a primary plus one MBR extended container (holding the cluster-config and
// ibi-status logicals). Both modes therefore need two free primary slots.
const requiredFreePrimarySlots = 2

// validateISOLayout inspects the customized live ISO's actual MBR partition table before the image
// is grown or appended to, and fails with a clear, actionable error when the ISO does not leave
// enough free primary slots for the selected mode. MBR has only four primary slots; a real RHCOS
// isohybrid live ISO uses two (ISO9660 + ESP), leaving the two the append needs. This is verified,
// not assumed: the append after the ISO would otherwise fail opaquely mid-build.
func validateISOLayout(log *logrus.Logger, isoPath string, plan PartitionPlan) error {
	used, err := countISOPrimaryPartitions(isoPath)
	if err != nil {
		return err
	}
	free := mbrPartitionEntryCount - used
	log.Infof("create-usb: live ISO MBR layout: %d of %d primary slots used, %d free (mode=%s needs %d free)",
		used, mbrPartitionEntryCount, free, plan.Mode, requiredFreePrimarySlots)
	if free < requiredFreePrimarySlots {
		return fmt.Errorf("live ISO uses %d of %d MBR primary slots, leaving only %d free, but %s media must append %d partitions after the ISO; the ISO does not leave enough free primary slots to assemble %s media",
			used, mbrPartitionEntryCount, free, plan.Mode, requiredFreePrimarySlots, plan.Mode)
	}
	return nil
}

// countISOPrimaryPartitions reads the ISO's MBR (first sector), validates the 0x55AA boot
// signature, and returns the number of non-empty primary partition entries (type byte != 0). A
// missing signature means the source is not a bootable DOS/MBR (isohybrid) image and is rejected.
func countISOPrimaryPartitions(isoPath string) (int, error) {
	f, err := os.Open(filepath.Clean(isoPath))
	if err != nil {
		return 0, fmt.Errorf("opening live ISO %s: %w", isoPath, err)
	}
	defer func() { _ = f.Close() }()

	sector := make([]byte, mbrSectorSize)
	if _, err := io.ReadFull(f, sector); err != nil {
		return 0, fmt.Errorf("reading the live ISO %s MBR sector: %w", isoPath, err)
	}
	if sector[mbrSignatureOffset] != 0x55 || sector[mbrSignatureOffset+1] != 0xAA {
		return 0, fmt.Errorf("live ISO %s has no MBR boot signature (0x55AA); is the source a RHCOS isohybrid live ISO?", isoPath)
	}

	used := 0
	for i := 0; i < mbrPartitionEntryCount; i++ {
		typeByte := sector[mbrPartitionTableOffset+i*mbrPartitionEntrySize+mbrPartitionTypeOffset]
		if typeByte != 0x00 {
			used++
		}
	}
	return used, nil
}

// WriteLiveISOToFront copies a built live ISO to the front of the media image at imgPath and
// grows the image to totalBytes. It is a standalone building block wired into the media-assembly
// pipeline by assembleMedia.
func WriteLiveISOToFront(log *logrus.Logger, imgPath, isoPath string, totalBytes int64) error {
	log.Infof("create-usb: writing live ISO (%s) to the front of %s and growing to %d bytes", labelLiveISO, imgPath, totalBytes)
	return writeLiveISOToFront(imgPath, isoPath, totalBytes)
}

// writeLiveISOToFront copies the customized live ISO byte-for-byte to the front of a freshly
// created media image and truncates the image up to totalBytes so the data partitions can be
// appended after the ISO. The byte-for-byte copy preserves the ISO's DOS/MBR table and the EFI
// entries that make it bootable, exactly as the CNF-27197 spike's `dd if=ISO of=OUTPUT` +
// `truncate` does.
//
// The image is created exclusively (O_EXCL): if the output path already exists it fails without
// touching the existing file, so a build never destroys an existing image at a reused output path.
func writeLiveISOToFront(imgPath, isoPath string, totalBytes int64) (err error) {
	iso, err := os.Open(filepath.Clean(isoPath))
	if err != nil {
		return fmt.Errorf("opening live ISO %s: %w", isoPath, err)
	}
	defer func() { _ = iso.Close() }()
	isoInfo, err := iso.Stat()
	if err != nil {
		return fmt.Errorf("stat live ISO %s: %w", isoPath, err)
	}
	if isoInfo.Size() > totalBytes {
		return fmt.Errorf("live ISO (%d bytes) is larger than the planned media size (%d bytes)", isoInfo.Size(), totalBytes)
	}

	out, err := os.OpenFile(filepath.Clean(imgPath), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // output path is supplied by the operator at build time
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("output path %s already exists; refusing to overwrite it (remove it or choose a different --output path): %w", imgPath, err)
		}
		return fmt.Errorf("creating media image %s: %w", imgPath, err)
	}
	// On any failure after creation, remove the partial image so a reused output path stays
	// available and no truncated media is left behind.
	defer func() {
		if err != nil {
			if removeErr := os.Remove(imgPath); removeErr != nil && !os.IsNotExist(removeErr) {
				err = fmt.Errorf("%w; additionally failed to remove the partial media image %s: %w", err, imgPath, removeErr)
			}
		}
	}()

	if _, err = io.Copy(out, iso); err != nil {
		_ = out.Close()
		return fmt.Errorf("copying live ISO %s to %s: %w", isoPath, imgPath, err)
	}
	if terr := out.Truncate(totalBytes); terr != nil {
		_ = out.Close()
		return fmt.Errorf("growing media image %s to %d bytes: %w", imgPath, totalBytes, terr)
	}
	if serr := out.Sync(); serr != nil {
		_ = out.Close()
		return fmt.Errorf("syncing media image %s: %w", imgPath, serr)
	}
	if cerr := out.Close(); cerr != nil {
		return fmt.Errorf("closing media image %s: %w", imgPath, cerr)
	}
	return nil
}
