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

// StatusPartitionPresent reports whether a writable ibi-status (p4) partition is attached.
// The post-pivot flow uses this to detect that it is running the self-contained USB-based IBI
// profile -- install-only media and IBU personalize the node without a p4 partition, so the
// pre-recert clock gate must not apply (and must never try to halt powered-on) there. A
// listing error is returned so a transient failure is not silently read as "absent".
func StatusPartitionPresent(o ops.Ops) (bool, error) {
	blockDevices, err := o.ListBlockDevices()
	if err != nil {
		return false, fmt.Errorf("failed to list block devices: %w", err)
	}
	for _, bd := range blockDevices {
		if bd.Label == common.IBIStatusFSLabel {
			return true, nil
		}
	}
	return false, nil
}

// WriteFailureAndHalt records a terminal reconfigure failure on the ibi-status partition and
// halts the node powered-on. The fixed order mirrors the prepare phase's finalizeDisconnected:
// mount p4 -> durable write of reconfigure.json -> sync -> unmount -> isolate emergency.target.
//
// Safety takes precedence over recording: on a bad clock the invariant is that recert must
// never run, so a failure to mount, write, sync, or unmount is logged but never prevents the
// powered-on halt. The returned error is cause (wrapped if the isolate itself fails); the
// caller returns it from the post-pivot flow so the process aborts before recert even if the
// isolate did not take.
func WriteFailureAndHalt(o ops.Ops, log *logrus.Logger, result *ReconfigureResult, cause error) error {
	sp, err := mountStatusPartition(o)
	if err != nil {
		// p4 is unavailable; the failure cannot be recorded, but the node must still not
		// proceed to recert: halt powered-on with the original cause.
		log.Errorf("clock gate: failed to mount ibi-status partition to record the failure (halting powered-on regardless): %v", err)
		return haltPoweredOn(o, log, cause)
	}

	if werr := sp.writeResult(result); werr != nil {
		log.Errorf("clock gate: failed to write reconfigure failure record (halting powered-on regardless): %v", werr)
	}

	// Flush the whole filesystem so the record survives a power loss, then unmount before the
	// terminal action. Both are best-effort: a failure here never prevents the halt.
	if _, err := o.RunInHostNamespace("sync"); err != nil {
		log.Warnf("clock gate: failed to sync filesystems before halt: %v", err)
	}
	if err := sp.unmount(); err != nil {
		log.Warnf("clock gate: failed to unmount ibi-status partition before halt: %v", err)
	}

	return haltPoweredOn(o, log, cause)
}

// mountStatusPartition discovers the writable ibi-status (p4) partition by its filesystem
// label, requires exactly one such ext4 device, and mounts it read-write. It returns an
// idempotent unmount closure.
func mountStatusPartition(o ops.Ops) (*statusPartition, error) {
	blockDevices, err := o.ListBlockDevices()
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
	if matches[0].FSType != fsTypeExt4 {
		return nil, fmt.Errorf("ibi-status device %s must use the %q filesystem, found %q",
			matches[0].Name, fsTypeExt4, matches[0].FSType)
	}
	deviceName := matches[0].Name

	if err := o.Mount(deviceName, statusMountDir); err != nil {
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

// haltPoweredOn transitions the node to the emergency target, leaving it powered on and
// reachable rather than powering it off, and returns cause.
func haltPoweredOn(o ops.Ops, log *logrus.Logger, cause error) error {
	log.Errorf("clock gate: halting node powered-on (%s): %v", emergencyTarget, cause)
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
// Unlike prepare.json, reconfigure.json has no "preserve success" guard: a reconfigure
// failure is a recoverable condition (an operator fixes the clock or adds a trusted time
// source and reboots), so the file always reflects the most recent attempt and a later
// recovering boot overwrites a failure.
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
