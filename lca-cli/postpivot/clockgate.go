package postpivot

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/openshift-kni/lifecycle-agent/api/ibiconfig"
	clusterconfig_api "github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ibistatus"
	"github.com/openshift-kni/lifecycle-agent/internal/recert"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedclusterinfo"
)

const (
	// chronySyncMaxTries bounds `chronyc waitsync` so a configured-but-unreachable time source
	// (the offline case) fails fast and the gate falls back to the RTC window instead of
	// stalling the pre-recert path. At a 1s interval this is a bounded 30s acquisition window,
	// long enough for a reachable NTP source to be selected and synchronized, while still
	// keeping the offline path from waiting indefinitely.
	chronySyncMaxTries = "30"

	// chronySyncMaxCorrection is the maximum remaining clock correction (in seconds) that
	// `chronyc waitsync` will accept as synchronized. It must be non-zero: a value of "0" tells
	// waitsync not to check the correction at all, so a selected source could report
	// synchronized while the clock still has a large uncorrected offset, letting the trusted
	// path return before the clock is actually accurate. One second is tight enough to keep
	// recert's certificate validity window sane while tolerating normal residual slew.
	chronySyncMaxCorrection = "1"

	// chronyLocalModeRefIDPrefix is the reference-ID prefix chronyd reports when it is running
	// in local mode -- i.e. it is "synchronized" to its own clock with no real external source
	// selected (the NTP local reference-clock refid family, 7F7F01xx). chrony documents that
	// local mode can appear synchronized without a real time source, so confirming `waitsync`
	// alone is not enough: a mixed config (a reachable-looking `server` plus a `local stratum`
	// fallback) whose server is offline can satisfy waitsync through local mode and bypass the
	// seed window. The runtime tracking check rejects this refid so those configs fall through
	// to the RTC plausibility window.
	chronyLocalModeRefIDPrefix = "7F7F01"
)

// nowUTC returns the current system clock in UTC. It is a package var so unit tests can
// control "now" without touching the real clock. On boot the system clock is seeded from the
// RTC, so this is the value the gate judges for plausibility.
var nowUTC = func() time.Time { return time.Now().UTC() }

// chronyConfigFilePath is the on-node chrony configuration file written during the gate's
// early chrony bring-up. It is a package var so unit tests can redirect the write to a temp
// file.
var chronyConfigFilePath = common.ChronyConfig

// seedCertLowerBound derives a lower bound for the system clock from local seed material for
// seeds that predate SeedCreationTime. It is a package var so unit tests can inject a bound.
var seedCertLowerBound = defaultSeedCertLowerBound

// writeReconfigureFailureAndHalt records a terminal reconfigure failure on p4 and halts the
// node powered-on. It is a package var so unit tests can assert the gate reaches the terminal
// path without performing a real mount/halt.
var writeReconfigureFailureAndHalt = ibistatus.WriteFailureAndHalt

// controlPlaneCertFiles are the node's control-plane certificate files the degraded old-seed
// lower-bound check reads. Before recert regenerates them these are the seed's certificates,
// so their earliest NotBefore is a conservative lower bound for a plausible clock.
var controlPlaneCertFiles = []string{
	"/var/lib/kubelet/pki/kubelet-client-current.pem",
	"/var/lib/kubelet/pki/kubelet-server-current.pem",
}

// clockGateWithChronyConfig is the pre-recert clock/RTC plausibility gate (USB-based IBI
// proposal §7.1). It runs in PostPivotConfiguration immediately before recert so certificates
// are never minted with a bad validity window when the node is personalized offline.
//
// It applies only to the self-contained USB-based IBI profile, detected by the presence of
// the writable ibi-status (p4) partition -- install-only media and IBU personalize without
// a p4 partition and are skipped. It is also a strict no-op once recert has already completed,
// so a later reboot of a converged node is never halted for a clock that now looks implausible.
//
// When it does apply, it decides plausibility in three tiers:
//  1. A trusted local time source -- a genuine external/on-link chrony source that confirms
//     synchronization before recert -- makes the clock authoritative, so the gate passes.
//  2. Otherwise, for a seed carrying SeedCreationTime, the system clock must fall within
//     [seedCreationTime, seedCreationTime + maxSeedAge]; outside that window it fails.
//  3. Otherwise (an older seed without SeedCreationTime) a degraded lower-bound-only check
//     against the seed's control-plane certificate NotBefore applies: a grossly-past clock
//     fails, otherwise the gate proceeds with a warning. This is a documented limitation and
//     is not the full plausibility gate.
//
// It returns true only when synchronization to a trusted source succeeded and the gate's
// temporary makestep configuration is active. On failure it writes a terminal
// reconfigure.json failure record to p4 and halts the node powered-on via emergency.target;
// recert never runs.
func (p *PostPivot) clockGateWithChronyConfig(seedReconfiguration *clusterconfig_api.SeedReconfiguration,
	seedClusterInfo *seedclusterinfo.SeedClusterInfo) (bool, error) {
	// Idempotent across reboots: once recert has completed, the clock is no longer gated --
	// the node is converged and must not be halted even if its clock now looks implausible.
	if p.recertAlreadyCompleted() {
		p.log.Info("Clock gate: recert already completed, skipping the pre-recert clock check")
		return false, nil
	}

	// Scope to the self-contained USB-based IBI profile: without a p4 partition this is
	// install-only or IBU, which have no rescue medium to record a failure on and must not be
	// halted here.
	present, err := ibistatus.StatusPartitionPresent(p.ops)
	if err != nil {
		return false, fmt.Errorf("clock gate: failed to detect the ibi-status partition: %w", err)
	}
	if !present {
		p.log.Info("Clock gate: no ibi-status (p4) partition present; not the self-contained USB IBI profile, skipping the clock gate")
		return false, nil
	}

	maxSeedAge, err := clusterconfig_api.ParseMaxSeedAge(seedReconfiguration.MaxSeedAge)
	if err != nil {
		// A malformed maxSeedAge is an operator configuration error in a tuning knob, not an
		// implausible clock. Fail the reconfigure with a plain error (which aborts before recert,
		// so no certificates are minted) rather than halting the node powered-on: the operator
		// fixes the value and reboots. recert still never runs.
		return false, fmt.Errorf("invalid maxSeedAge configuration: %w", err)
	}

	// Tier 1: a trusted, confirmed-synced local time source makes the clock authoritative.
	// Presence of a chrony config alone is never enough -- the source must be a genuine
	// external/on-link source and must actually confirm synchronization before recert.
	if clusterconfig_api.ChronyConfigHasTrustedSource(seedReconfiguration.ChronyConfig) {
		if syncErr := p.bringChronyUpAndConfirmSync(seedReconfiguration.ChronyConfig); syncErr != nil {
			var restorationErr *chronyConfigRestorationError
			if errors.As(syncErr, &restorationErr) {
				return false, p.failClockGate(seedClusterInfo,
					&ibistatus.ClockGateResult{
						ObservedTime: nowUTC().Format(time.RFC3339),
						Decision:     ibistatus.DecisionConfigError,
					},
					fmt.Errorf("failed to restore the original chrony configuration: %w", syncErr))
			}
			p.log.Warnf("Clock gate: a trusted chrony source is configured but synchronization was not confirmed (%v); falling back to the RTC plausibility window", syncErr)
		} else {
			p.log.Info("Clock gate: chrony synchronized to a trusted source before recert; the system clock is authoritative")
			return true, nil
		}
	}

	now := nowUTC()

	// Tier 2: full window gate for seeds carrying a creation timestamp.
	if seedClusterInfo.SeedCreationTime != "" {
		seedCreation, perr := time.Parse(time.RFC3339, seedClusterInfo.SeedCreationTime)
		if perr != nil {
			return false, p.failClockGate(seedClusterInfo,
				&ibistatus.ClockGateResult{ObservedTime: now.Format(time.RFC3339), Decision: ibistatus.DecisionConfigError},
				fmt.Errorf("failed to parse seed creation time %q: %w", seedClusterInfo.SeedCreationTime, perr))
		}
		windowStart := seedCreation.UTC()
		windowEnd := windowStart.Add(maxSeedAge)
		clk := &ibistatus.ClockGateResult{
			ObservedTime: now.Format(time.RFC3339),
			WindowStart:  windowStart.Format(time.RFC3339),
			WindowEnd:    windowEnd.Format(time.RFC3339),
			Decision:     ibistatus.DecisionRTCWindow,
		}
		switch {
		case now.Before(windowStart):
			return false, p.failClockGate(seedClusterInfo, clk,
				fmt.Errorf("system clock %s is earlier than the seed creation time %s; a node cannot be personalized before its seed existed",
					now.Format(time.RFC3339), windowStart.Format(time.RFC3339)))
		case now.After(windowEnd):
			return false, p.failClockGate(seedClusterInfo, clk,
				fmt.Errorf("system clock %s is later than the plausibility ceiling %s (seed creation + %s); configure a trusted local time source or widen maxSeedAge",
					now.Format(time.RFC3339), windowEnd.Format(time.RFC3339), maxSeedAge))
		default:
			p.log.Infof("Clock gate: system clock %s is within the plausible window [%s, %s]; proceeding to recert",
				now.Format(time.RFC3339), windowStart.Format(time.RFC3339), windowEnd.Format(time.RFC3339))
			return false, nil
		}
	}

	// Tier 3: older seed without SeedCreationTime -- degraded lower-bound-only check.
	lower, ok := seedCertLowerBound(p.log)
	if !ok {
		p.log.Warn("Clock gate: seed carries no creation time and no control-plane certificate lower bound could be derived; proceeding without the full plausibility gate (documented limitation for pre-SeedCreationTime seeds)")
		return false, nil
	}
	if now.Before(lower) {
		clk := &ibistatus.ClockGateResult{
			ObservedTime: now.Format(time.RFC3339),
			WindowStart:  lower.Format(time.RFC3339),
			Decision:     ibistatus.DecisionCertLowerBound,
		}
		return false, p.failClockGate(seedClusterInfo, clk,
			fmt.Errorf("system clock %s is earlier than the earliest seed control-plane certificate validity %s; the clock is grossly implausible",
				now.Format(time.RFC3339), lower.Format(time.RFC3339)))
	}
	p.log.Warnf("Clock gate: seed carries no creation time; applied a degraded lower-bound-only check against %s (not the full plausibility gate); proceeding to recert",
		lower.Format(time.RFC3339))
	return false, nil
}

// recertAlreadyCompleted reports whether recert has already run, in which case the clock gate
// is a no-op. RunOnce writes <name>.done in the working directory on success, so recert's
// marker is recert.done; the recert summary file is a second, independent signal that recert
// already ran (recert() itself refuses to re-run when it exists).
func (p *PostPivot) recertAlreadyCompleted() bool {
	if _, err := os.Stat(filepath.Join(p.workingDir, "recert.done")); err == nil {
		return true
	}
	if _, err := os.Stat(recert.SummaryFile); err == nil {
		return true
	}
	return false
}

// failClockGate assembles the terminal reconfigure failure record and hands off to the shared
// write-back + powered-on halt. The returned error aborts PostPivotConfiguration before recert
// even in the (logged) case where the halt itself could not be issued.
func (p *PostPivot) failClockGate(seedClusterInfo *seedclusterinfo.SeedClusterInfo,
	clk *ibistatus.ClockGateResult, cause error) error {
	p.log.Errorf("Clock gate failed; the node will halt powered-on before recert: %v", cause)
	msg := cause.Error()
	result := &ibistatus.ReconfigureResult{
		Phase:       ibistatus.PhaseReconfigure,
		Profile:     ibiconfig.ProfileSelfContained,
		Result:      ibistatus.ResultFailure,
		Timestamp:   nowUTC().Format(time.RFC3339),
		SeedVersion: seedClusterInfo.SeedClusterOCPVersion,
		Error:       &msg,
		Clock:       clk,
	}
	return writeReconfigureFailureAndHalt(p.ops, p.log, result, fmt.Errorf("pre-recert clock gate failed: %w", cause))
}

// chronyConfigWithMakestep returns chronyConfig with the gate's transient makestep policy.
// A bad RTC can leave the clock far enough off that chronyd would otherwise slew slowly;
// makestep 1 -1 lets chronyd step the clock as soon as a trusted source is selected. Any
// existing makestep directive is replaced so the gate's transient config always uses the
// unrestricted-step policy needed before recert.
func chronyConfigWithMakestep(chronyConfig string) string {
	if strings.TrimSpace(chronyConfig) == "" {
		return "makestep 1 -1\n"
	}
	lines := strings.Split(chronyConfig, "\n")
	kept := make([]string, 0, len(lines)+1)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			fields := strings.Fields(trimmed)
			if len(fields) > 0 && strings.EqualFold(fields[0], "makestep") {
				continue
			}
		}
		kept = append(kept, line)
	}
	joined := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	if joined != "" {
		joined += "\n"
	}
	return joined + "makestep 1 -1\n"
}

// bringChronyUpAndConfirmSync places the user's chrony configuration and (re)starts chronyd
// before recert, then waits a bounded time for synchronization. Today chronyd is only
// restarted after recert, too late to correct the clock before certs are minted; bringing it
// up here lets a reachable trusted source step the clock first. recert later writes its own
// processed /etc/chrony.conf and restartChronydService reloads it, so this early write is
// transient. A configured-but-unreachable source (offline) times out quickly and surfaces as
// an error so the caller falls back to the RTC window.
func (p *PostPivot) bringChronyUpAndConfirmSync(chronyConfig string) error {
	gateConfig := chronyConfigWithMakestep(chronyConfig)
	//nolint:gosec // chrony.conf is a non-secret config normally world-readable; this is a transient pre-recert write recert overwrites.
	if err := os.WriteFile(chronyConfigFilePath, []byte(gateConfig), 0o644); err != nil {
		return fmt.Errorf("failed to write chrony config %s: %w", chronyConfigFilePath, err)
	}
	if _, err := p.ops.SystemctlAction("restart", "chronyd"); err != nil {
		return p.restoreChronyConfigAfterGateError(chronyConfig,
			fmt.Errorf("failed to restart chronyd: %w", err))
	}
	// `chronyc waitsync <max-tries> <max-correction> <max-skew> <interval>`: the bounded
	// acquisition window gives a reachable source time to be selected and corrected, while a
	// bounded max-correction (non-zero) ensures the remaining clock correction is actually
	// small before we treat the source as authoritative — "0" would disable that check entirely.
	if _, err := p.ops.RunInHostNamespace("chronyc", "waitsync", chronySyncMaxTries, chronySyncMaxCorrection, "0", "1"); err != nil {
		return p.restoreChronyConfigAfterGateError(chronyConfig,
			fmt.Errorf("chrony did not confirm synchronization: %w", err))
	}
	// waitsync confirms the clock is synchronized but not *which* source was selected. chronyd
	// can report synchronized while running in local mode (its own clock, no real external
	// source) -- e.g. a mixed config whose only reachable-looking `server` is offline falling
	// back to `local stratum`. Verify the runtime-selected reference and reject local mode so
	// such configs do not bypass the seed window via the very RTC the gate exists to distrust.
	if err := p.confirmChronyReferenceNotLocal(); err != nil {
		return p.restoreChronyConfigAfterGateError(chronyConfig, err)
	}
	return nil
}

// restoreChronyConfigAfterGate writes the operator's original Chrony configuration and restarts
// chronyd. PostPivotConfiguration calls it when setup or recert fails after the gate installed
// its temporary unrestricted makestep policy but before recert has replaced the configuration.
func (p *PostPivot) restoreChronyConfigAfterGate(chronyConfig string) error {
	//nolint:gosec // chrony.conf is a non-secret config normally world-readable.
	if err := os.WriteFile(chronyConfigFilePath, []byte(chronyConfig), 0o644); err != nil {
		return fmt.Errorf("failed to restore chrony config %s: %w", chronyConfigFilePath, err)
	}
	if _, err := p.ops.SystemctlAction("restart", "chronyd"); err != nil {
		return fmt.Errorf("failed to restart chronyd after restoring its config: %w", err)
	}
	return nil
}

// chronyConfigRestorationError marks a failure to restore the gate's temporary Chrony
// configuration. It wraps both the triggering failure and the restoration failure.
type chronyConfigRestorationError struct {
	cause      error
	restoreErr error
}

func (e *chronyConfigRestorationError) Error() string {
	return fmt.Sprintf("%v; additionally failed to restore the original chrony configuration: %v", e.cause, e.restoreErr)
}

func (e *chronyConfigRestorationError) Unwrap() []error {
	return []error{e.cause, e.restoreErr}
}

// restoreChronyConfigAfterGateError restores the original Chrony configuration after a failure
// that occurred while the gate's temporary configuration was active. The original cause remains
// the primary wrapped error; a restoration failure is appended as a second wrapped error.
func (p *PostPivot) restoreChronyConfigAfterGateError(chronyConfig string, cause error) error {
	if restoreErr := p.restoreChronyConfigAfterGate(chronyConfig); restoreErr != nil {
		return &chronyConfigRestorationError{cause: cause, restoreErr: restoreErr}
	}
	return cause
}

// confirmChronyReferenceNotLocal inspects the reference chronyd actually selected (via
// `chronyc tracking`) and rejects local mode. It is deliberately strict: if the selected
// reference cannot be determined, it returns an error so the caller falls back to the seed
// plausibility window rather than treating an unverified clock as authoritative.
func (p *PostPivot) confirmChronyReferenceNotLocal() error {
	// `-n` disables reverse-DNS lookups so the check does not depend on name resolution offline.
	out, err := p.ops.RunInHostNamespace("chronyc", "-n", "tracking")
	if err != nil {
		return fmt.Errorf("failed to read chrony tracking state to verify the selected source: %w", err)
	}
	refID, ok := parseChronyReferenceID(out)
	if !ok {
		return fmt.Errorf("could not determine the synchronized chrony reference from tracking output")
	}
	if strings.HasPrefix(strings.ToUpper(refID), chronyLocalModeRefIDPrefix) {
		return fmt.Errorf("chrony is synchronized in local mode (reference ID %s); no real external time source was selected", refID)
	}
	return nil
}

// parseChronyReferenceID extracts the hexadecimal reference ID from `chronyc tracking` output.
// The tracking report begins with a line of the form `Reference ID    : 7F7F0101 (name)`.
func parseChronyReferenceID(trackingOutput string) (string, bool) {
	for _, raw := range strings.Split(trackingOutput, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "Reference ID") {
			continue
		}
		_, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		return fields[0], true
	}
	return "", false
}

// defaultSeedCertLowerBound returns the earliest NotBefore among the node's control-plane
// certificates, used only as a degraded lower bound for seeds that predate SeedCreationTime.
// It is best-effort: if no certificate can be read and parsed it returns ok=false and the gate
// proceeds with a warning (the documented pre-SeedCreationTime limitation).
func defaultSeedCertLowerBound(log *logrus.Logger) (time.Time, bool) {
	earliest := time.Time{}
	found := false
	for _, certFile := range controlPlaneCertFiles {
		data, err := os.ReadFile(certFile) //nolint:gosec // fixed control-plane cert paths read read-only for a NotBefore lower bound
		if err != nil {
			log.Warnf("Clock gate: could not read control-plane certificate %s for the lower-bound check: %v", certFile, err)
			continue
		}
		for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
			if block.Type != "CERTIFICATE" {
				continue
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				continue
			}
			if !found || cert.NotBefore.Before(earliest) {
				earliest = cert.NotBefore
				found = true
			}
		}
	}
	if !found {
		return time.Time{}, false
	}
	return earliest.UTC(), true
}
