package ibi_preparation

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// A successful reboot never returns control to this process. So if the reboot command errors,
// or it is issued but the grace period elapses with the process still alive, the pivot did not
// take -- the node is still in the live ISO -- and handlePivotFailure records pivot: failed
// and halts the node powered-on rather than leaving it idling in the live ISO. This keeps the
// coarse power-state signal honest: off = install-only success; still powered on = a failure or
// a pivot that did not take. The reboot is issued via a transient systemd unit (systemd-run),
// mirroring internal/reboot, so it survives this prepare service being torn down as the node
// goes down.
func (i *IBIPrepare) pivotReboot(sp *statusPartition) error {
	i.log.Info("Self-contained prepare succeeded; rebooting into the installed disk (pivot)")
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
// reentry: blocked. finalizeDisconnected unmounts p4 before the terminal action (so the
// removable status filesystem is clean when the node reboots or powers off), so the partition
// is re-mounted on its known device for this amend and unmounted again afterwards. The write
// is atomic and durable (writeResult -> writeFileDurable), so the marker survives a power loss
// without a separate whole-filesystem sync.
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
	if err := i.writeResult(sp, existing); err != nil {
		return fmt.Errorf("failed to write pivot failure marker: %w", err)
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
