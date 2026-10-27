package ibi_preparation

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/openshift-kni/lifecycle-agent/api/ibiconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

const (
	// prepareResultPhase is the phase recorded in every prepare result.
	prepareResultPhase = "prepare"

	resultSuccess = "success"
	resultFailure = "failure"

	reentryNone    = "none"
	reentryBlocked = "blocked"

	// pivotNone / pivotFailed record the self-contained profile's pivot outcome, distinct
	// from the re-entry sub-status: pivotNone is the default, pivotFailed is amended onto the
	// result when the on-USB pivot into the installed disk did not take (see pivotReboot).
	pivotNone   = "none"
	pivotFailed = "failed"

	// prepareLogBundleName is the compact diagnostic log bundle written under the
	// ibi-status logs directory (a gzip-compressed journal capture).
	prepareLogBundleName = "prepare-logs.txt.gz"

	// prepareLogBundleCap bounds the journal capture so a runaway journal can never fill the
	// p4 partition or be buffered whole in memory during failure handling. The capture is
	// streamed through `tail -c` (see writeLogBundle), so only the most recent output -- the
	// tail, where a failure's cause is recorded -- is ever read back.
	prepareLogBundleCap = 16 << 20 // 16 MiB

	// emergencyTarget halts the node powered-on (and reachable for a technician) rather
	// than powering it off, so a failed or re-entered prepare never emits the
	// install-only power-off success signal.
	emergencyTarget = "emergency.target"

	// fsTypeExt4 is the only filesystem the writable ibi-status (p4) partition is ever
	// created with (see create-usb). Requiring it rejects a device that merely carries the
	// ibi-status label but uses another filesystem (e.g. a hand-altered or mislabeled
	// writable vfat stick) before it can be adopted as the status partition.
	fsTypeExt4 = "ext4"

	// efiFirmwareDir exists only when the node booted via UEFI firmware (not legacy BIOS).
	// Only UEFI exposes the one-time BootNext selection efibootmgr drives, so its presence
	// gates the firmware boot-target selection the self-contained pivot relies on.
	efiFirmwareDir = "/sys/firmware/efi"

	// efiSystemPartType is the GPT partition type GUID of an EFI System Partition. The pivot's
	// one-time boot target points at the installed disk's partition carrying this type.
	efiSystemPartType = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"

	// installedDiskBootLabel is the efibootmgr boot-entry label created for the installed disk
	// when no existing entry already points at its ESP.
	installedDiskBootLabel = "lca-ibi-installed-disk"
)

// errReentryBlocked is the terminal outcome when a boot re-enters a completed install:
// the destructive prepare is deliberately refused rather than wiping the disk.
var errReentryBlocked = errors.New(
	"destructive prepare refused: ibi-status records a completed install (re-entry blocked)")

// statusPartitionMountDir is the mount point for the writable ibi-status (p4) partition.
// It is a package var (not a const) so unit tests can redirect the writability probe,
// re-entry guard, and result write-back to a temporary directory instead of a real mount.
var statusPartitionMountDir = common.IBIStatusMountPath

// collectStatusLogs gathers a best-effort diagnostic log bundle onto the mounted
// ibi-status partition. It is a package var so unit tests can stub it out; the default
// captures the boot journal (capped, gzip-compressed). A failure here never blocks the
// result write-back or the terminal action and never changes the prepare result.
var collectStatusLogs = (*IBIPrepare).writeLogBundle

// espMountDir is the mount point used to verify the installed disk's EFI System Partition
// carries a bootloader before a boot entry is created for it. It is a package var so unit tests
// can redirect the verification to a temporary directory instead of a real mount.
var espMountDir = "/var/mnt/ibi-esp"

// espBootloaders are the bootloaders a created boot entry may load, in preference order. Each
// is checked for existence on the mounted ESP (fsPath) before an entry that loads it (efiPath,
// the backslash-separated path efibootmgr expects) is created, so an entry is only ever created
// for a verified, present bootloader. shim is preferred over the removable-media fallback.
var espBootloaders = []struct {
	fsPath  string
	efiPath string
}{
	{filepath.Join("EFI", "redhat", "shimx64.efi"), `\EFI\redhat\shimx64.efi`},
	{filepath.Join("EFI", "BOOT", "BOOTX64.EFI"), `\EFI\BOOT\BOOTX64.EFI`},
}

// isUEFIBooted reports whether the node booted via UEFI firmware. It is a package var so unit
// tests can force the UEFI and legacy-BIOS branches of the pivot boot-target selection.
var isUEFIBooted = func() bool {
	_, err := os.Stat(efiFirmwareDir)
	return err == nil
}

// selectPivotBootTarget points the firmware at the installed disk for the next boot before
// pivotReboot issues the reboot. It is a package var so unit tests can stub it; the default
// (selectInstalledDiskBootTarget) sets and verifies a one-time UEFI BootNext.
var selectPivotBootTarget = (*IBIPrepare).selectInstalledDiskBootTarget

// PrepareResult is the machine-readable per-phase result written to prepare.json at the
// root of the ibi-status (p4) partition on both the success and failure paths. A
// technician (or a later phase) reads it off-box to learn how the prepare phase ended.
type PrepareResult struct {
	Phase       string         `json:"phase"`
	Profile     string         `json:"profile"`
	Result      string         `json:"result"`
	Timestamp   string         `json:"timestamp"`
	SeedVersion string         `json:"seedVersion"`
	Checks      *PrepareChecks `json:"checks,omitempty"`
	Error       *string        `json:"error"`
	Reentry     string         `json:"reentry"`
	// Pivot is the self-contained pivot sub-status (pivotNone / pivotFailed). It is
	// pivotNone on every result and amended to pivotFailed only when the on-USB pivot into
	// the installed disk does not take; install-only media never pivots, so it stays
	// pivotNone there.
	Pivot string `json:"pivot"`
}

// PrepareChecks records the image-coverage counts observed during a successful prepare.
// Richer per-image self-check counts are owned by CNF-27162.
type PrepareChecks struct {
	ImagesExpected int `json:"imagesExpected"`
	ImagesPresent  int `json:"imagesPresent"`
}

// statusPartition is a mounted, writable ibi-status (p4) partition.
type statusPartition struct {
	deviceName string
	mountDir   string
	unmount    func() error
}

// mountStatusPartition discovers the writable ibi-status (p4) partition by its
// filesystem label, requires exactly one such device using the ext4 filesystem, mounts it
// read-write, and confirms it is writable. It is a hard precondition for both media
// profiles: a disconnected prepare runs it before any destructive disk operation, so 0 or
// >1 devices, a non-ext4 filesystem, a mount failure, or a read-only filesystem all fail
// here while the installation disk is still intact. It returns an idempotent unmount
// closure.
func (i *IBIPrepare) mountStatusPartition() (*statusPartition, error) {
	blockDevices, err := i.ops.ListBlockDevices()
	if err != nil {
		return nil, fmt.Errorf("failed to list block devices: %w", err)
	}

	matches := make([]ops.BlockDevice, 0, 1)
	for _, bd := range blockDevices {
		if bd.Label == common.IBIStatusFSLabel {
			matches = append(matches, bd)
		}
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("expected exactly one block device labelled %q, found %d",
			common.IBIStatusFSLabel, len(matches))
	}
	// The p4 partition is always ext4 (see create-usb). Requiring the filesystem type -- not
	// just the label and writability -- rejects a hand-altered or mislabeled writable device
	// (e.g. a vfat stick labelled "ibi-status") before it can be adopted as the status
	// partition, while the installation disk is still intact.
	if matches[0].FSType != fsTypeExt4 {
		return nil, fmt.Errorf("ibi-status device %s must use the %q filesystem, found %q",
			matches[0].Name, fsTypeExt4, matches[0].FSType)
	}
	deviceName := matches[0].Name

	if err := i.ops.Mount(deviceName, statusPartitionMountDir); err != nil {
		return nil, fmt.Errorf("failed to mount ibi-status device %s: %w", deviceName, err)
	}

	var unmounted bool
	unmount := func() error {
		if unmounted {
			return nil
		}
		if err := i.ops.Umount(deviceName); err != nil {
			i.log.Warnf("failed to unmount ibi-status device %s: %v", deviceName, err)
			return fmt.Errorf("failed to unmount ibi-status device %s: %w", deviceName, err)
		}
		unmounted = true
		return nil
	}

	// A read-only p4 must fail closed before any wipe, not only surface when the result
	// write-back later fails; probe writability now, while the install disk is intact.
	if err := probeWritable(statusPartitionMountDir); err != nil {
		_ = unmount()
		return nil, fmt.Errorf("ibi-status device %s is not writable: %w", deviceName, err)
	}

	return &statusPartition{deviceName: deviceName, mountDir: statusPartitionMountDir, unmount: unmount}, nil
}

// probeWritable confirms dir is writable by creating, writing, and removing a probe file.
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".ibi-status-probe-")
	if err != nil {
		return fmt.Errorf("creating writability probe: %w", err)
	}
	name := f.Name()
	_, writeErr := f.Write([]byte("ok"))
	closeErr := f.Close()
	_ = os.Remove(name)
	if writeErr != nil {
		return fmt.Errorf("writing writability probe: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing writability probe: %w", closeErr)
	}
	return nil
}

// guardReentry inspects an existing prepare result on the ibi-status partition before any
// destructive disk operation. On a first boot (no result file) it returns blocked=false
// and the prepare proceeds. If a completed install is found (result: success) this boot is
// a re-entry over that install: it records reentry: blocked while preserving the success
// result (never rewriting it), returns blocked=true, and the caller halts the node
// powered-on without wiping. A result file that exists but cannot be read or parsed fails
// closed (returns an error): an unreadable marker is never treated as "safe to wipe".
func (i *IBIPrepare) guardReentry(sp *statusPartition) (blocked bool, err error) {
	resultPath := filepath.Join(sp.mountDir, common.PrepareResultFileName)
	data, err := os.ReadFile(resultPath) //nolint:gosec // resultPath is the fixed result file under the mounted p4 root
	if err != nil {
		if os.IsNotExist(err) {
			// First boot: no completed install recorded, proceed to prepare.
			return false, nil
		}
		return false, fmt.Errorf("failed to read existing prepare result %s: %w", resultPath, err)
	}

	var existing PrepareResult
	if err := json.Unmarshal(data, &existing); err != nil {
		return false, fmt.Errorf("failed to parse existing prepare result %s: %w", resultPath, err)
	}

	switch existing.Result {
	case resultSuccess:
		// Re-entry over a completed install: handled below.
	case resultFailure:
		// A previous prepare explicitly failed; a retry may wipe and reinstall.
		return false, nil
	default:
		// A parseable marker with an empty or unrecognized result is never treated as safe
		// to wipe: fail closed, exactly like a malformed marker.
		return false, fmt.Errorf("existing prepare result %s has an unrecognized result %q; refusing to wipe over an uncertain marker",
			resultPath, existing.Result)
	}

	// Re-entry over a completed install: refuse to wipe. Amend only the re-entry marker,
	// preserving result: success so every subsequent boot still refuses to wipe. This path
	// never flows through the generic failure writer, which could otherwise overwrite the
	// success marker and reopen the re-wipe hole.
	i.log.Warnf("ibi-status records a completed install (result: %s); refusing to re-run destructive prepare", existing.Result)
	existing.Reentry = reentryBlocked
	if err := i.writeResult(sp, &existing); err != nil {
		// Even if the blocked re-entry cannot be recorded, a completed install must never
		// be wiped: report blocked and let the caller halt powered-on.
		i.log.Errorf("failed to record blocked re-entry (completed install preserved, not wiping): %v", err)
	}
	return true, nil
}

// finalizeDisconnected records the prepare outcome on the ibi-status partition and
// performs the terminal action. prepErr is the result of the core prepare (nil on
// success). The fixed order is: write prepare.json (success or failure) and a best-effort
// log bundle -> durability sync -> unmount p4 -> terminal action. On the success path any
// of a failed result write, a failed durability sync, or a failed p4 unmount downgrades to
// halt-powered-on: a successful prepare must not emit the power-off success signal when its
// required record could not be committed, the durability sequence did not complete, or the
// removable status filesystem is still mounted.
func (i *IBIPrepare) finalizeDisconnected(sp *statusPartition, imagesExpected int, prepErr error) error {
	result := i.buildResult(imagesExpected, prepErr)

	writeErr := i.writeResultPreservingSuccess(sp, result)
	if writeErr != nil {
		i.log.Errorf("failed to write prepare result to ibi-status partition: %v", writeErr)
	}

	// Best-effort diagnostics; never blocks the result or the terminal action.
	if err := collectStatusLogs(i, sp); err != nil {
		i.log.Warnf("failed to collect prepare log bundle (best-effort): %v", err)
	}

	// Durability: flush the whole filesystem so the record survives a power loss, then
	// unmount before the terminal action. A sync or unmount failure on the success path
	// means the documented durability sequence did not complete (prepare.json may be stale
	// or unavailable after power loss, or p4 may still be mounted when the stick is pulled),
	// so both are captured and downgrade the success power-off to a powered-on halt, exactly
	// like a failed result write.
	var syncErr error
	if _, err := i.ops.RunInHostNamespace("sync"); err != nil {
		syncErr = fmt.Errorf("failed to sync filesystems before terminal action: %w", err)
		i.log.Warnf("%v", syncErr)
	}
	unmountErr := sp.unmount()
	if unmountErr != nil {
		i.log.Warnf("failed to unmount ibi-status partition before terminal action: %v", unmountErr)
	}

	switch {
	case prepErr != nil:
		// Failure: halt powered-on, never power off.
		return i.haltPoweredOn(prepErr)
	case writeErr != nil:
		// Success path, but the required record could not be committed: do not signal
		// success via power-off; halt powered-on instead.
		return i.haltPoweredOn(fmt.Errorf("prepare succeeded but recording the result failed: %w", writeErr))
	case syncErr != nil:
		// Success path, but the whole-filesystem durability flush failed: the record may not
		// survive a power loss, so halt powered-on rather than signalling success.
		return i.haltPoweredOn(fmt.Errorf("prepare succeeded but the final durability sync failed: %w", syncErr))
	case unmountErr != nil:
		// Success path, but p4 could not be unmounted: do not power off with the removable
		// status filesystem still mounted; halt powered-on instead.
		return i.haltPoweredOn(fmt.Errorf("prepare succeeded but unmounting the ibi-status partition failed: %w", unmountErr))
	default:
		// Success: take the profile's mutually-exclusive terminal action. install-only powers
		// the node off (the warehouse-shutdown success signal); self-contained reboots off the
		// live ISO into the freshly installed disk (the pivot) so on-USB reconfigure can run.
		// The success record was already written and flushed to p4 above, so the pivot is
		// necessarily issued after prepare.json: success is durable.
		if i.config.Profile == ibiconfig.ProfileSelfContained {
			return i.pivotReboot(sp)
		}
		return i.shutdownNode()
	}
}

// haltPoweredOn transitions the node to the emergency target, leaving it powered on and
// reachable rather than powering it off, and returns cause. It is the terminal action for
// every disconnected failure and for a blocked re-entry.
func (i *IBIPrepare) haltPoweredOn(cause error) error {
	i.log.Errorf("halting node powered-on (%s): %v", emergencyTarget, cause)
	if _, err := i.ops.SystemctlAction("isolate", emergencyTarget); err != nil {
		return fmt.Errorf("%w; additionally failed to halt powered-on via %s: %s", cause, emergencyTarget, err.Error())
	}
	return cause
}

// pivotRebootTimeout bounds how long pivotReboot waits for a successfully-issued reboot to
// take the node down before concluding the pivot did not take. A real reboot never returns
// control, so this only elapses when the reboot silently failed to proceed; it is a package
// var (not a const) so unit tests can set it to 0 and not block on the real wait.
var pivotRebootTimeout = 5 * time.Minute

// pivotReboot is the self-contained profile's success terminal action: instead of powering
// the node off, it reboots it off the live ISO into the freshly installed disk so the on-USB
// reconfigure (post-pivot) can run. It is reached from finalizeDisconnected only after
// prepare.json has been written with result: success and flushed to the ibi-status partition,
// so the pivot is necessarily issued after the success record is durable on p4.
//
// A plain reboot does not change the boot device, so before issuing the reboot it selects the
// installed disk as the next boot target (selectPivotBootTarget): the self-contained USB stays
// inserted (p3 is consumed by on-USB reconfigure after the pivot), so if the firmware still
// prioritizes the removable device the node would re-boot the live ISO, guardReentry would block
// the destructive prepare, and post-pivot work would never start. On UEFI this sets and verifies
// a one-time BootNext into the installed disk; a target that cannot be selected or verified is a
// pivot failure, so the node never issues a reboot that would silently land back in the live ISO.
//
// A successful reboot never returns control to this process. So if the boot target cannot be
// selected, the reboot command errors, or it is issued but the grace period elapses with the
// process still alive, the pivot did not take -- the node is still in the live ISO -- and
// handlePivotFailure records pivot: failed and halts the node powered-on rather than leaving it
// idling in the live ISO. This keeps the coarse power-state signal honest: off = install-only
// success; still powered on = a failure or a pivot that did not take. The reboot is issued via a
// transient systemd unit (systemd-run), mirroring internal/reboot, so it survives this prepare
// service being torn down as the node goes down.
func (i *IBIPrepare) pivotReboot(sp *statusPartition) error {
	i.log.Info("Self-contained prepare succeeded; selecting the installed disk as the next boot target")
	if err := selectPivotBootTarget(i); err != nil {
		return i.handlePivotFailure(sp, fmt.Errorf(
			"failed to select the installed disk as the next boot target: %w", err))
	}

	i.log.Info("Rebooting off the live ISO into the installed disk (pivot)")
	if _, err := i.ops.RunInHostNamespace("systemd-run", "--unit", "lca-ibi-pivot",
		"--description", "\"lifecycle-agent: pivot into the installed disk\"",
		"systemctl", "reboot"); err != nil {
		return i.handlePivotFailure(sp, fmt.Errorf("failed to issue pivot reboot into the installed disk: %w", err))
	}

	// A successful reboot never returns; wait for the kernel to take the node down. If this
	// grace period elapses the process is still alive, so the reboot did not take.
	i.log.Infof("Pivot reboot issued; waiting up to %s for the node to go down", pivotRebootTimeout)
	time.Sleep(pivotRebootTimeout)

	return i.handlePivotFailure(sp, fmt.Errorf(
		"node did not reboot into the installed disk within %s of issuing the pivot", pivotRebootTimeout))
}

// selectInstalledDiskBootTarget points the firmware at the installed disk for the next boot.
//
// On UEFI it sets and verifies a one-time efibootmgr BootNext to the installed disk's ESP (see
// setUEFIBootNextToInstalledDisk): a one-time override is used so the firmware's persistent
// BootOrder is left untouched, and the selection is verified so an override that did not take is
// caught here (reported as a pivot failure by the caller) rather than silently re-booting the
// live ISO.
//
// On legacy BIOS there is no BootNext equivalent, so the pivot cannot *guarantee* the firmware
// boots the installed disk -- that depends on a documented, hardware-tested boot-order
// arrangement (the installed disk ordered ahead of the USB, or the USB removed). Rather than
// claiming a guarantee it cannot make, it logs the dependency and proceeds with the reboot.
func (i *IBIPrepare) selectInstalledDiskBootTarget() error {
	if !isUEFIBooted() {
		i.log.Warnf("node is not UEFI-booted: cannot set a one-time firmware boot target for the pivot; "+
			"booting the installed disk %s relies on a documented, hardware-tested boot-order arrangement "+
			"for legacy BIOS systems (installed disk ordered ahead of the USB, or the USB removed)",
			i.config.InstallationDisk)
		return nil
	}
	return i.setUEFIBootNextToInstalledDisk()
}

// setUEFIBootNextToInstalledDisk sets and verifies a one-time UEFI boot into the installed disk.
// It locates the installed disk's EFI System Partition, finds an existing efibootmgr entry whose
// device path points at that partition (creating one for the partition's verified bootloader if
// none exists), sets it as BootNext, and verifies BootNext reads back as that entry. Any step
// that cannot complete -- no ESP, no verified bootloader, or an unverifiable BootNext -- returns
// an error so the caller records a pivot failure and halts powered-on rather than issuing a
// reboot that could land back in the live ISO.
func (i *IBIPrepare) setUEFIBootNextToInstalledDisk() error {
	esp, err := i.installedDiskESP()
	if err != nil {
		return err
	}

	bootNum, err := i.findBootEntryForESP(esp)
	if err != nil {
		return err
	}
	if bootNum == "" {
		i.log.Infof("no existing UEFI boot entry points at the installed disk ESP %s; creating one", esp.device)
		bootNum, err = i.createBootEntryForESP(esp)
		if err != nil {
			return err
		}
	}

	i.log.Infof("setting one-time UEFI BootNext to boot entry %s (installed disk ESP %s)", bootNum, esp.device)
	if _, err := i.ops.RunInHostNamespace("efibootmgr", "--bootnext", bootNum); err != nil {
		return fmt.Errorf("failed to set UEFI BootNext to boot entry %s: %w", bootNum, err)
	}
	return i.verifyBootNext(bootNum)
}

// installedESP identifies the installed disk's EFI System Partition: its partition device, the
// parent disk, the partition number, and the GPT partition UUID (lowercased) used to match an
// efibootmgr entry's device path.
type installedESP struct {
	device   string
	disk     string
	partNum  string
	partUUID string
}

// installedDiskESP finds the EFI System Partition on the configured installation disk by its GPT
// partition type, returning the fields needed to match or create a boot entry for it. It errors
// if no ESP is found (e.g. a BIOS-only layout on a UEFI-booted node), which the caller treats as
// an unselectable boot target (a pivot failure).
func (i *IBIPrepare) installedDiskESP() (*installedESP, error) {
	out, err := i.ops.RunInHostNamespace("lsblk", "--pairs", "--paths",
		"--output", "NAME,PARTTYPE,PARTUUID,PARTN,PKNAME", i.config.InstallationDisk)
	if err != nil {
		return nil, fmt.Errorf("failed to list partitions of installation disk %s: %w", i.config.InstallationDisk, err)
	}
	for _, line := range strings.Split(out, "\n") {
		fields := parseLsblkPairs(line)
		if !strings.EqualFold(fields["PARTTYPE"], efiSystemPartType) {
			continue
		}
		esp := &installedESP{
			device:   fields["NAME"],
			partNum:  fields["PARTN"],
			partUUID: strings.ToLower(fields["PARTUUID"]),
			disk:     deviceNodePath(fields["PKNAME"]),
		}
		if esp.partNum == "" {
			esp.partNum = trailingDigits(esp.device)
		}
		if esp.device == "" || esp.partNum == "" || esp.partUUID == "" || esp.disk == "" {
			return nil, fmt.Errorf("installation disk %s EFI System Partition is missing identifying fields "+
				"(device %q, part %q, partuuid %q, disk %q)",
				i.config.InstallationDisk, esp.device, esp.partNum, esp.partUUID, esp.disk)
		}
		return esp, nil
	}
	return nil, fmt.Errorf("no EFI System Partition (GPT type %s) found on installation disk %s",
		efiSystemPartType, i.config.InstallationDisk)
}

// bootEntryRE matches an efibootmgr boot entry line, capturing the four-hex-digit boot number:
//
//	Boot0001* Red Hat Enterprise Linux	HD(1,GPT,<partuuid>,...)/File(\EFI\redhat\shimx64.efi)
var bootEntryRE = regexp.MustCompile(`^Boot([0-9A-Fa-f]{4})\*?\s`)

// findBootEntryForESP returns the boot number of an existing efibootmgr entry whose device path
// points at esp (matched by the ESP's GPT partition UUID in the entry's HD() device path), or ""
// if none exists. efibootmgr -v prints each entry's device path, which embeds the target
// partition's PARTUUID.
func (i *IBIPrepare) findBootEntryForESP(esp *installedESP) (string, error) {
	out, err := i.ops.RunInHostNamespace("efibootmgr", "-v")
	if err != nil {
		return "", fmt.Errorf("failed to read UEFI boot entries: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		m := bootEntryRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if strings.Contains(strings.ToLower(line), esp.partUUID) {
			return strings.ToUpper(m[1]), nil
		}
	}
	return "", nil
}

// createBootEntryForESP creates a UEFI boot entry for the installed disk's ESP and returns its
// boot number. It first verifies the ESP actually carries a bootloader (mounting it read-only and
// checking the known loader paths) so an entry is only ever created for a present, verified
// bootloader; an ESP with no recognized loader is an unselectable target (an error). After
// creating the entry it re-scans efibootmgr to resolve the new entry's boot number.
func (i *IBIPrepare) createBootEntryForESP(esp *installedESP) (string, error) {
	loader, err := i.verifiedESPBootloader(esp)
	if err != nil {
		return "", err
	}
	i.log.Infof("creating UEFI boot entry %q for installed disk %s part %s loader %s",
		installedDiskBootLabel, esp.disk, esp.partNum, loader)
	if _, err := i.ops.RunInHostNamespace("efibootmgr", "--create",
		"--disk", esp.disk, "--part", esp.partNum,
		"--label", installedDiskBootLabel, "--loader", loader); err != nil {
		return "", fmt.Errorf("failed to create UEFI boot entry for installed disk ESP %s: %w", esp.device, err)
	}
	bootNum, err := i.findBootEntryForESP(esp)
	if err != nil {
		return "", err
	}
	if bootNum == "" {
		return "", fmt.Errorf("created a UEFI boot entry for installed disk ESP %s but could not resolve its boot number", esp.device)
	}
	return bootNum, nil
}

// verifiedESPBootloader mounts the installed disk's ESP read-only and returns the efibootmgr
// --loader path of the first known bootloader present on it, so a created boot entry always
// targets a real, present bootloader. It errors if the ESP cannot be mounted or carries none of
// the known loaders.
func (i *IBIPrepare) verifiedESPBootloader(esp *installedESP) (string, error) {
	deviceName := strings.TrimPrefix(esp.device, "/dev/")
	if err := i.ops.MountReadOnly(deviceName, espMountDir); err != nil {
		return "", fmt.Errorf("failed to mount installed disk ESP %s to verify its bootloader: %w", esp.device, err)
	}
	defer func() {
		if err := i.ops.Umount(deviceName); err != nil {
			i.log.Warnf("failed to unmount installed disk ESP %s after bootloader verification: %v", esp.device, err)
		}
	}()

	for _, bl := range espBootloaders {
		if _, err := os.Stat(filepath.Join(espMountDir, bl.fsPath)); err == nil {
			return bl.efiPath, nil
		}
	}
	return "", fmt.Errorf("no known bootloader found on installed disk ESP %s", esp.device)
}

// bootNextRE captures the boot number from efibootmgr's "BootNext: XXXX" line.
var bootNextRE = regexp.MustCompile(`(?m)^BootNext:\s*([0-9A-Fa-f]{4})\s*$`)

// verifyBootNext confirms the firmware's BootNext reads back as want, so a BootNext the firmware
// silently rejected is caught before the reboot (an unverifiable target is a pivot failure)
// rather than letting the node re-boot the live ISO.
func (i *IBIPrepare) verifyBootNext(want string) error {
	out, err := i.ops.RunInHostNamespace("efibootmgr")
	if err != nil {
		return fmt.Errorf("failed to read back UEFI BootNext for verification: %w", err)
	}
	m := bootNextRE.FindStringSubmatch(out)
	if m == nil {
		return fmt.Errorf("UEFI BootNext was not set after selecting boot entry %s", strings.ToUpper(want))
	}
	if got := strings.ToUpper(m[1]); got != strings.ToUpper(want) {
		return fmt.Errorf("UEFI BootNext verification failed: expected boot entry %s, firmware reports %s",
			strings.ToUpper(want), got)
	}
	return nil
}

// lsblkPairRE matches one KEY="value" token of `lsblk --pairs` output.
var lsblkPairRE = regexp.MustCompile(`([A-Z]+)="([^"]*)"`)

// parseLsblkPairs parses one line of `lsblk --pairs` output (KEY="value" ...) into a map. It
// tolerates empty values, which pairs output quotes explicitly (unlike raw/column output).
func parseLsblkPairs(line string) map[string]string {
	fields := map[string]string{}
	for _, m := range lsblkPairRE.FindAllStringSubmatch(line, -1) {
		fields[m[1]] = m[2]
	}
	return fields
}

// trailingDigitsRE captures the trailing run of digits of a string.
var trailingDigitsRE = regexp.MustCompile(`(\d+)$`)

// trailingDigits returns the trailing run of digits of s, used to derive a partition number
// from a partition device node (e.g. /dev/sda2 -> "2", /dev/nvme0n1p2 -> "2") when lsblk did
// not report PARTN.
func trailingDigits(s string) string {
	return trailingDigitsRE.FindString(s)
}

// deviceNodePath returns the absolute /dev node for a (possibly bare) kernel device name, so an
// lsblk PKNAME reported either as "sda" or "/dev/sda" yields "/dev/sda".
func deviceNodePath(name string) string {
	switch {
	case name == "":
		return ""
	case strings.HasPrefix(name, "/"):
		return name
	default:
		return filepath.Join("/dev", name)
	}
}

// handlePivotFailure is the terminal path when the self-contained pivot did not take. It
// records a pivot: failed sub-status on the ibi-status partition -- distinct from
// reentry: blocked -- and halts the node powered-on (never powered off) so the still-running
// node is itself the failure signal. Recording the marker is best-effort: a failure to write
// it never prevents the powered-on halt.
func (i *IBIPrepare) handlePivotFailure(sp *statusPartition, cause error) error {
	i.log.Errorf("pivot into the installed disk did not take: %v", cause)
	if err := i.markPivotFailed(sp); err != nil {
		i.log.Errorf("failed to record pivot: failed on the ibi-status partition (halting powered-on regardless): %v", err)
	}
	return i.haltPoweredOn(cause)
}

// markPivotFailed amends the prepare result on the ibi-status partition with pivot: failed,
// preserving the existing result: success exactly as guardReentry preserves it when it records
// reentry: blocked, and refreshes the diagnostic log bundle so it covers the pivot attempt.
// finalizeDisconnected unmounts p4 before the terminal action (so the removable status filesystem
// is clean when the node reboots or powers off), so the partition is re-mounted on its known
// device for this amend and unmounted again afterwards. The marker write is atomic and durable
// (writeResult -> writeFileDurable), so it survives a power loss without a separate
// whole-filesystem sync. Refreshing the log bundle is best-effort: a capture failure never
// prevents the pivot: failed marker or (via handlePivotFailure) the powered-on halt.
func (i *IBIPrepare) markPivotFailed(sp *statusPartition) error {
	if err := i.ops.Mount(sp.deviceName, sp.mountDir); err != nil {
		return fmt.Errorf("failed to re-mount ibi-status device %s to record pivot failure: %w", sp.deviceName, err)
	}
	defer func() {
		if err := i.ops.Umount(sp.deviceName); err != nil {
			i.log.Warnf("failed to unmount ibi-status device %s after recording pivot failure: %v", sp.deviceName, err)
		}
	}()

	existing, err := i.readResult(sp)
	if err != nil {
		return fmt.Errorf("failed to read prepare result to amend pivot failure: %w", err)
	}
	existing.Pivot = pivotFailed
	writeErr := i.writeResult(sp, existing)
	if writeErr != nil {
		i.log.Errorf("failed to write pivot failure marker: %v", writeErr)
	}

	// Refresh the diagnostic log bundle while p4 is still mounted so it covers the pivot attempt
	// and the grace wait: finalizeDisconnected captured the bundle *before* the terminal action,
	// so without this refresh the bundle the troubleshooting guide points technicians to for the
	// pivot cause would predate the pivot failure. Best-effort by contract: a capture failure must
	// not prevent the (already-attempted) pivot: failed marker or the powered-on halt.
	if err := collectStatusLogs(i, sp); err != nil {
		i.log.Warnf("failed to refresh prepare log bundle after pivot failure (best-effort): %v", err)
	}

	if writeErr != nil {
		return fmt.Errorf("failed to write pivot failure marker: %w", writeErr)
	}
	return nil
}

// buildResult assembles the per-phase result from the config and the core prepare
// outcome. On success the full on-media closure is imported and precache-verified, so the
// imagesExpected images are reported as fully present; the counts are omitted on failure
// (presence is unknown) and richer per-image self-check counts are owned by CNF-27162.
func (i *IBIPrepare) buildResult(imagesExpected int, prepErr error) *PrepareResult {
	r := &PrepareResult{
		Phase:       prepareResultPhase,
		Profile:     i.config.Profile,
		Timestamp:   nowRFC3339(),
		SeedVersion: i.config.SeedVersion,
		Reentry:     reentryNone,
		Pivot:       pivotNone,
	}
	if prepErr != nil {
		r.Result = resultFailure
		msg := prepErr.Error()
		r.Error = &msg
		return r
	}
	r.Result = resultSuccess
	if imagesExpected > 0 {
		r.Checks = &PrepareChecks{ImagesExpected: imagesExpected, ImagesPresent: imagesExpected}
	}
	return r
}

// writeResultPreservingSuccess writes result, except it never overwrites an existing
// result: success marker with a failure result. The re-entry guard already blocks a boot
// that finds a completed install before this writer runs, so this is defense-in-depth: it
// guarantees the generic failure writer can never downgrade a completed install to failure
// (which would reopen the re-wipe hole) even if the ordering is changed in the future. A
// success result is always written.
func (i *IBIPrepare) writeResultPreservingSuccess(sp *statusPartition, result *PrepareResult) error {
	if result.Result != resultSuccess {
		if existing, err := i.readResult(sp); err == nil && existing.Result == resultSuccess {
			i.log.Warnf("preserving existing completed-install marker (result: success); not overwriting it with a %s result", result.Result)
			return nil
		}
	}
	return i.writeResult(sp, result)
}

// readResult reads and parses the prepare result currently stored on the ibi-status
// partition. A missing or malformed file returns an error.
func (i *IBIPrepare) readResult(sp *statusPartition) (*PrepareResult, error) {
	path := filepath.Join(sp.mountDir, common.PrepareResultFileName)
	data, err := os.ReadFile(path) //nolint:gosec // path is the fixed result file under the mounted p4 root
	if err != nil {
		return nil, fmt.Errorf("failed to read prepare result %s: %w", path, err)
	}
	var result PrepareResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("failed to parse prepare result %s: %w", path, err)
	}
	return &result, nil
}

// writeResult marshals result and writes it to prepare.json at the root of the ibi-status
// partition using the atomic durable sequence (temp file -> fsync -> rename -> directory
// fsync), so a reader never observes a half-written file and the record survives a power
// loss before the terminal action.
func (i *IBIPrepare) writeResult(sp *statusPartition, result *PrepareResult) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal prepare result: %w", err)
	}
	data = append(data, '\n')
	path := filepath.Join(sp.mountDir, common.PrepareResultFileName)
	if err := writeFileDurable(path, data); err != nil {
		return fmt.Errorf("failed to write prepare result %s: %w", path, err)
	}
	return nil
}

// writeLogBundle captures a compact, size-capped diagnostic journal onto the ibi-status
// partition under the logs directory. It is best-effort: callers log and ignore any
// error. It is the default implementation behind the collectStatusLogs seam.
func (i *IBIPrepare) writeLogBundle(sp *statusPartition) error {
	logsDir := filepath.Join(sp.mountDir, common.IBIStatusLogsDir)
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		return fmt.Errorf("failed to create logs directory %s: %w", logsDir, err)
	}

	// Bound the captured journal to prepareLogBundleCap bytes by streaming it through
	// `tail -c` in the host namespace, so the full (potentially huge) boot journal is never
	// buffered in memory during failure handling -- only its most recent cap bytes (the tail,
	// where a failure's cause is recorded) are read back. pipefail makes a journalctl failure
	// surface as an error instead of being masked by tail's success exit status.
	cmd := fmt.Sprintf("set -o pipefail; journalctl --no-pager -b | tail -c %d", prepareLogBundleCap)
	out, err := i.ops.RunBashInHostNamespace(cmd)
	if err != nil {
		return fmt.Errorf("failed to collect journal: %w", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(out)); err != nil {
		_ = gz.Close()
		return fmt.Errorf("failed to compress journal: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("failed to finalize journal bundle: %w", err)
	}

	bundlePath := filepath.Join(logsDir, prepareLogBundleName)
	if err := writeFileDurable(bundlePath, buf.Bytes()); err != nil {
		return fmt.Errorf("failed to write log bundle %s: %w", bundlePath, err)
	}
	return nil
}

// writeFileDurable atomically and durably writes data to path: it writes a temp file in
// the same directory, fsyncs it, renames it into place, and fsyncs the parent directory so
// the published file survives a power loss (not merely a process exit). It mirrors the
// durable publish sequence used by the create-usb media builder (publishFile).
func writeFileDurable(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".prepare-result-")
	if err != nil {
		return fmt.Errorf("creating temp file for %q: %w", path, err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		// Only the pre-rename temp file is ours to remove; once the rename succeeds the
		// temp name no longer exists and the published file must be left in place.
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %q: %w", path, err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing temp file for %q: %w", path, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file for %q: %w", path, err)
	}
	if err = os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("setting permissions on %q: %w", path, err)
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publishing %q: %w", path, err)
	}
	renamed = true
	if err = fsyncDir(dir); err != nil {
		return fmt.Errorf("syncing directory for %q: %w", path, err)
	}
	return nil
}

// fsyncDir opens dir and fsyncs it, flushing its entries (the rename commit) to stable
// storage.
func fsyncDir(dir string) error {
	f, err := os.Open(dir) //nolint:gosec // dir is the mounted p4 root/logs directory the caller controls
	if err != nil {
		return fmt.Errorf("opening %q to sync: %w", dir, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing %q: %w", dir, err)
	}
	return nil
}

// nowRFC3339 returns the current UTC time formatted per RFC 3339, the timestamp recorded
// in the prepare result.
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
