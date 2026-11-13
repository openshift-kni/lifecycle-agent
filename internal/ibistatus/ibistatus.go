// Package ibistatus provides the shared machine-readable result record and the durable
// write-back + powered-on halt helpers for the post-pivot reconfigure phase of a USB-based
// IBI. It mirrors the per-phase result contract established for the prepare phase in
// lca-cli/ibi-preparation (PrepareResult, writeFileDurable, haltPoweredOn), applied to the
// reconfigure.json file written at the root of the writable ibi-status (p4) partition.
//
// Today this is consumed only by the post-pivot clock gate (CNF-27172), which runs before
// recert and, on an implausible system clock, writes a terminal failure record here and
// halts the node powered-on so recert never mints certificates with a bad validity window.
// The reconfigure result-writer (CNF-27173) is expected to reuse this schema and writer for
// the success record. The prepare-phase helpers in lca-cli/ibi-preparation remain in place
// and un-migrated to keep this change's blast radius off the critical destructive prepare
// path; folding both phases onto these shared helpers is deferred follow-up work.
package ibistatus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

const (
	// PhaseReconfigure is the phase recorded in every reconfigure result.
	PhaseReconfigure = "reconfigure"

	// ResultSuccess / ResultFailure are the terminal result values, matching the prepare
	// phase vocabulary so a technician reads prepare.json and reconfigure.json the same way.
	ResultSuccess = "success"
	ResultFailure = "failure"

	// emergencyTarget halts the node powered-on (and reachable for a technician) rather than
	// powering it off, exactly as the prepare phase does on failure.
	emergencyTarget = "emergency.target"

	// fsTypeExt4 is the only filesystem the writable ibi-status (p4) partition is created
	// with (see create-usb); requiring it rejects a mislabeled device.
	fsTypeExt4 = "ext4"
)

// Clock-gate decision values recorded in ClockGateResult.Decision, naming which branch of
// the gate produced the result.
const (
	DecisionSyncedSource   = "synced-source"
	DecisionRTCWindow      = "rtc-window"
	DecisionCertLowerBound = "cert-lower-bound"
	DecisionConfigError    = "config-error"
)

// statusMountDir is the mount point for the writable ibi-status (p4) partition. It is a
// package var (not a const) so unit tests can redirect the result write-back to a temporary
// directory instead of a real mount.
var statusMountDir = common.IBIStatusMountPath

// ReconfigureResult is the machine-readable result written to reconfigure.json at the root
// of the ibi-status (p4) partition. It is structurally parallel to the prepare phase's
// PrepareResult so both phases are read off-box the same way.
type ReconfigureResult struct {
	Phase       string           `json:"phase"`
	Profile     string           `json:"profile"`
	Result      string           `json:"result"`
	Timestamp   string           `json:"timestamp"`
	SeedVersion string           `json:"seedVersion"`
	Error       *string          `json:"error"`
	Clock       *ClockGateResult `json:"clock,omitempty"`
}

// ClockGateResult carries the clock-gate specifics of a reconfigure failure: the time the
// gate observed, the plausible window it checked against (when known), and which branch of
// the gate decided. It lets a technician see exactly why the node halted before recert.
type ClockGateResult struct {
	ObservedTime string `json:"observedTime"`
	WindowStart  string `json:"windowStart,omitempty"`
	WindowEnd    string `json:"windowEnd,omitempty"`
	Decision     string `json:"decision"`
}

// statusPartition is a mounted ibi-status (p4) partition.
type statusPartition struct {
	deviceName string
	mountDir   string
	unmount    func() error
}

// StatusPartitionPresent reports whether a valid writable ibi-status (p4) partition is attached.
// The post-pivot flow uses this to detect that it is running the self-contained USB-based IBI
// profile. A listing error, multiple labelled devices, or a mislabeled non-ext4 device is
// returned as an error so it is not silently read as "absent"; a clean absence returns false.
// Invalid labelled media fails closed even on otherwise non-USB profiles: a label collision
// means the environment or media is malformed and must not silently disable status handling.
func StatusPartitionPresent(o ops.Ops) (bool, error) {
	blockDevices, err := o.ListBlockDevices()
	if err != nil {
		return false, fmt.Errorf("failed to list block devices: %w", err)
	}
	_, present, err := findStatusPartition(blockDevices)
	if err != nil {
		return false, err
	}
	return present, nil
}

// findStatusPartition returns the unique ibi-status device, whether one is present, and any
// validation error. Zero matches are a clean absence; more than one match or a non-ext4
// filesystem is an error because the label is ambiguous or invalid.
func findStatusPartition(blockDevices []ops.BlockDevice) (ops.BlockDevice, bool, error) {
	matches := make([]ops.BlockDevice, 0, 1)
	for _, bd := range blockDevices {
		if bd.Label == common.IBIStatusFSLabel {
			matches = append(matches, bd)
		}
	}
	if len(matches) == 0 {
		return ops.BlockDevice{}, false, nil
	}
	if len(matches) != 1 {
		return ops.BlockDevice{}, false, fmt.Errorf("expected exactly one block device labelled %q, found %d",
			common.IBIStatusFSLabel, len(matches))
	}
	if matches[0].FSType != fsTypeExt4 {
		return ops.BlockDevice{}, false, fmt.Errorf("ibi-status device %s must use the %q filesystem, found %q",
			matches[0].Name, fsTypeExt4, matches[0].FSType)
	}
	return matches[0], true, nil
}

// IsTerminal reports whether a result records a terminal reconfigure verdict (success or
// failure). The idempotence guard never overwrites such a record.
func (r *ReconfigureResult) IsTerminal() bool {
	return r != nil && (r.Result == ResultSuccess || r.Result == ResultFailure)
}

// WriteSuccess records a terminal reconfigure success on the ibi-status partition and does
// NOT halt. It is the reconfigure result-writer's success path: the node keeps running once
// the convergence criterion is met. Like WriteFailure it is guarded by the write-only
// idempotence check, so it never overwrites an existing terminal verdict.
func WriteSuccess(o ops.Ops, log *logrus.Logger, result *ReconfigureResult) error {
	return writeTerminalResult(o, log, result)
}

// WriteFailure records a terminal reconfigure failure on the ibi-status partition and does
// NOT halt. It is the write-only half of WriteFailureAndHalt: the clock gate pairs it with an
// unconditional halt, while the result-writer orders it before a best-effort unit-disable and
// an unconditional halt. It returns any write/mount error (or nil when the idempotence guard
// no-ops the write).
func WriteFailure(o ops.Ops, log *logrus.Logger, result *ReconfigureResult) error {
	return writeTerminalResult(o, log, result)
}

// writeTerminalResult mounts the ibi-status (p4) partition read-write and durably writes the
// terminal reconfigure.json record (mount -> durable write -> sync -> unmount), with no halt.
//
// Idempotence guard (global, write-only): if a terminal verdict (success or failure) is
// already recorded it skips the write and returns nil, so neither verdict is ever overwritten
// across reboots. The read happens under the same mount as the write, so there is no
// read-then-write race. The guard lives here, inside the writers, so the clock gate (via
// WriteFailureAndHalt) inherits it for free; crucially it suppresses only the p4 write and
// never a terminal action (WriteFailureAndHalt halts regardless of what this returns).
func writeTerminalResult(o ops.Ops, log *logrus.Logger, result *ReconfigureResult) error {
	sp, err := mountStatusPartition(o, false)
	if err != nil {
		return err
	}
	defer func() {
		if uerr := sp.unmount(); uerr != nil {
			log.Warnf("reconfigure: failed to unmount ibi-status partition: %v", uerr)
		}
	}()

	if existing, rerr := readReconfigureResultAt(log, sp.mountDir); rerr != nil {
		log.Warnf("reconfigure: could not read the existing reconfigure result (proceeding to write): %v", rerr)
	} else if existing.IsTerminal() {
		log.Infof("reconfigure: reconfigure.json already records a terminal %q verdict; not overwriting", existing.Result)
		return nil
	}

	if werr := sp.writeResult(result); werr != nil {
		return werr
	}
	// Flush the whole filesystem so the record survives a power loss. Best-effort: a sync
	// failure is logged but does not fail the write that already landed.
	if _, serr := o.RunInHostNamespace("sync"); serr != nil {
		log.Warnf("reconfigure: failed to sync filesystems after writing the result: %v", serr)
	}
	return nil
}

// WriteFailureAndHalt records a terminal reconfigure failure on the ibi-status partition and
// then ALWAYS halts the node powered-on. It is the clock gate's single caller: on a bad clock
// the invariant is that recert must never run, so the write is best-effort while the halt is
// mandatory.
//
// Explicitly NOT the anti-pattern `if err := WriteFailure(...); err != nil { return err }` --
// that would let a p4 mount/write/sync/unmount failure, or an idempotence-guard no-op, skip
// the halt and let the node proceed to recert on a bad clock. The returned error is cause
// (wrapped if the isolate itself fails); the caller returns it from the post-pivot flow so the
// process aborts before recert even if the isolate did not take.
func WriteFailureAndHalt(o ops.Ops, log *logrus.Logger, result *ReconfigureResult, cause error) error {
	if err := WriteFailure(o, log, result); err != nil {
		// Logged but NEVER returned early -- safety invariant: halt regardless.
		log.Errorf("clock gate: failed to record reconfigure failure (halting powered-on regardless): %v", err)
	}
	return HaltPoweredOn(o, log, cause)
}

// ReadReconfigureResult mounts the ibi-status (p4) partition read-only, reads reconfigure.json
// if present, and unmounts. It returns the parsed result, or (nil, nil) when there is no valid
// terminal verdict to honour: an absent file is a clean not-found, and a malformed/corrupt
// file is treated as "no valid terminal verdict" (logged, not an error) so a bad file never
// wedges the node. A genuine mount/IO failure is returned as an error.
func ReadReconfigureResult(o ops.Ops, log *logrus.Logger) (*ReconfigureResult, error) {
	sp, err := mountStatusPartition(o, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		if uerr := sp.unmount(); uerr != nil {
			log.Warnf("reconfigure: failed to unmount ibi-status partition after read: %v", uerr)
		}
	}()
	return readReconfigureResultAt(log, sp.mountDir)
}

// readReconfigureResultAt reads and parses reconfigure.json from an already-mounted p4 root.
// A missing file returns (nil, nil); a malformed file is logged and also returns (nil, nil) so
// a corrupt record never wedges the node. Only an IO error other than not-exist is returned.
func readReconfigureResultAt(log *logrus.Logger, mountDir string) (*ReconfigureResult, error) {
	resultPath := filepath.Join(mountDir, common.ReconfigureResultFileName)
	data, err := os.ReadFile(resultPath) //nolint:gosec // resultPath is the mounted p4 root the caller controls
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read reconfigure result %s: %w", resultPath, err)
	}
	var result ReconfigureResult
	if err := json.Unmarshal(data, &result); err != nil {
		log.Warnf("reconfigure: ignoring malformed reconfigure result %s (treated as no verdict): %v", resultPath, err)
		return nil, nil
	}
	return &result, nil
}

// mountStatusPartition discovers the writable ibi-status (p4) partition by its filesystem
// label, requires exactly one such ext4 device, and mounts it. When readOnly is set it mounts
// read-only (used by the result reader); otherwise it mounts read-write (used by the writers).
// It returns an idempotent unmount closure.
func mountStatusPartition(o ops.Ops, readOnly bool) (*statusPartition, error) {
	blockDevices, err := o.ListBlockDevices()
	if err != nil {
		return nil, fmt.Errorf("failed to list block devices: %w", err)
	}

	device, present, err := findStatusPartition(blockDevices)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, fmt.Errorf("no block device labelled %q was found", common.IBIStatusFSLabel)
	}
	deviceName := device.Name

	mount := o.Mount
	if readOnly {
		mount = o.MountReadOnly
	}
	if err := mount(deviceName, statusMountDir); err != nil {
		return nil, fmt.Errorf("failed to mount ibi-status device %s: %w", deviceName, err)
	}

	var unmounted bool
	unmount := func() error {
		if unmounted {
			return nil
		}
		if err := o.Umount(deviceName); err != nil {
			return fmt.Errorf("failed to unmount ibi-status device %s: %w", deviceName, err)
		}
		unmounted = true
		return nil
	}

	return &statusPartition{deviceName: deviceName, mountDir: statusMountDir, unmount: unmount}, nil
}

// HaltPoweredOn transitions the node to the emergency target, leaving it powered on and
// reachable rather than powering it off, and returns cause. It is the shared terminal action
// for both the clock gate (via WriteFailureAndHalt) and the reconfigure result-writer's
// timeout path (write -> disable -> halt).
func HaltPoweredOn(o ops.Ops, log *logrus.Logger, cause error) error {
	log.Errorf("reconfigure: halting node powered-on (%s): %v", emergencyTarget, cause)
	if _, err := o.SystemctlAction("isolate", emergencyTarget); err != nil {
		return fmt.Errorf("%w; additionally failed to halt powered-on via %s: %s", cause, emergencyTarget, err.Error())
	}
	return cause
}

// writeResult marshals result and writes it to reconfigure.json at the root of the
// ibi-status partition using the atomic durable sequence (temp file -> fsync -> rename ->
// directory fsync), so a reader never observes a half-written file and the record survives a
// power loss before the terminal action.
//
// This is the unconditional low-level write: it always overwrites. The terminal-verdict
// idempotence guard lives one level up, in writeTerminalResult, so an existing success or
// failure is never overwritten across reboots. Recovery tooling that must force a rewrite
// deletes reconfigure.json first (which makes the guard a clean no-op) and lets the next boot
// record a fresh verdict.
func (sp *statusPartition) writeResult(result *ReconfigureResult) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal reconfigure result: %w", err)
	}
	data = append(data, '\n')
	resultPath := filepath.Join(sp.mountDir, common.ReconfigureResultFileName)
	if err := writeFileDurable(resultPath, data); err != nil {
		return fmt.Errorf("failed to write reconfigure result %s: %w", resultPath, err)
	}
	return nil
}

// writeFileDurable atomically and durably writes data to path: it writes a temp file in the
// same directory, fsyncs it, renames it into place, and fsyncs the parent directory so the
// published file survives a power loss (not merely a process exit). It mirrors the durable
// publish sequence used by the prepare phase (writeFileDurable in lca-cli/ibi-preparation).
func writeFileDurable(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".reconfigure-result-")
	if err != nil {
		return fmt.Errorf("creating temp file for %q: %w", path, err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
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
	f, err := os.Open(dir) //nolint:gosec // dir is the mounted p4 root the caller controls
	if err != nil {
		return fmt.Errorf("opening %q to sync: %w", dir, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing %q: %w", dir, err)
	}
	return nil
}
