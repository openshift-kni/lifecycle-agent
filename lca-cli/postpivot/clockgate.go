package postpivot

import (
	"crypto/x509"
	"encoding/pem"
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
	// defaultMaxSeedAge is the default plausibility ceiling for the system clock relative to
	// the seed's creation time: a conservative warehouse->ship->field shelf life that catches
	// gross RTC errors (an unset/battery-dead clock landing in 1970/2000, or a wildly future
	// date) without tripping on realistic logistics. It is configurable per deployment via
	// SeedReconfiguration.MaxSeedAge. 180 days was confirmed as a reasonable starting default.
	defaultMaxSeedAge = 180 * 24 * time.Hour

	// maxAllowedSeedAge caps a configured maxSeedAge so an absurd or overflowing value is
	// rejected with a clear config error rather than silently widening the window to meaningless.
	maxAllowedSeedAge = 10 * 365 * 24 * time.Hour

	// chronySyncMaxTries bounds `chronyc waitsync` so a configured-but-unreachable time source
	// (the offline case) fails fast and the gate falls back to the RTC window instead of
	// stalling the pre-recert path. At a 1s interval this is a few seconds at most.
	chronySyncMaxTries = "3"

	// chronySyncMaxCorrection is the maximum remaining clock correction (in seconds) that
	// `chronyc waitsync` will accept as synchronized. It must be non-zero: a value of "0" tells
	// waitsync not to check the correction at all, so a selected source could report
	// synchronized while the clock still has a large uncorrected offset, letting the trusted
	// path return before the clock is actually accurate. One second is tight enough to keep
	// recert's certificate validity window sane while tolerating normal residual slew.
	chronySyncMaxCorrection = "1"
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

// clockGate is the pre-recert clock/RTC plausibility gate (USB-based IBI proposal §7.1). It
// runs in PostPivotConfiguration immediately before recert so certificates are never minted
// with a bad validity window when the node is personalized offline.
//
// It applies only to the self-contained USB-based IBI profile, detected by the presence of
// the writable ibi-status (p4) partition -- install-only media and IBU personalize without a
// p4 partition and are skipped. It is also a strict no-op once recert has already completed,
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
// On failure it writes a terminal reconfigure.json failure record to p4 and halts the node
// powered-on via emergency.target; recert never runs.
func (p *PostPivot) clockGate(seedReconfiguration *clusterconfig_api.SeedReconfiguration,
	seedClusterInfo *seedclusterinfo.SeedClusterInfo) error {
	// Idempotent across reboots: once recert has completed, the clock is no longer gated --
	// the node is converged and must not be halted even if its clock now looks implausible.
	if p.recertAlreadyCompleted() {
		p.log.Info("Clock gate: recert already completed, skipping the pre-recert clock check")
		return nil
	}

	// Scope to the self-contained USB-based IBI profile: without a p4 partition this is
	// install-only or IBU, which have no rescue medium to record a failure on and must not be
	// halted here.
	present, err := ibistatus.StatusPartitionPresent(p.ops)
	if err != nil {
		return fmt.Errorf("clock gate: failed to detect the ibi-status partition: %w", err)
	}
	if !present {
		p.log.Info("Clock gate: no ibi-status (p4) partition present; not the self-contained USB IBI profile, skipping the clock gate")
		return nil
	}

	maxSeedAge, err := resolveMaxSeedAge(seedReconfiguration.MaxSeedAge)
	if err != nil {
		// A malformed maxSeedAge is an operator configuration error in a tuning knob, not an
		// implausible clock. Fail the reconfigure with a plain error (which aborts before recert,
		// so no certificates are minted) rather than halting the node powered-on: the operator
		// fixes the value and reboots. recert still never runs.
		return fmt.Errorf("invalid maxSeedAge configuration: %w", err)
	}

	// Tier 1: a trusted, confirmed-synced local time source makes the clock authoritative.
	// Presence of a chrony config alone is never enough -- the source must be a genuine
	// external/on-link source and must actually confirm synchronization before recert.
	if chronyConfigHasTrustedSource(seedReconfiguration.ChronyConfig) {
		if syncErr := p.bringChronyUpAndConfirmSync(seedReconfiguration.ChronyConfig); syncErr != nil {
			p.log.Warnf("Clock gate: a trusted chrony source is configured but synchronization was not confirmed (%v); falling back to the RTC plausibility window", syncErr)
		} else {
			p.log.Info("Clock gate: chrony synchronized to a trusted source before recert; the system clock is authoritative")
			return nil
		}
	}

	now := nowUTC()

	// Tier 2: full window gate for seeds carrying a creation timestamp.
	if seedClusterInfo.SeedCreationTime != "" {
		seedCreation, perr := time.Parse(time.RFC3339, seedClusterInfo.SeedCreationTime)
		if perr != nil {
			return p.failClockGate(seedClusterInfo,
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
			return p.failClockGate(seedClusterInfo, clk,
				fmt.Errorf("system clock %s is earlier than the seed creation time %s; a node cannot be personalized before its seed existed",
					now.Format(time.RFC3339), windowStart.Format(time.RFC3339)))
		case now.After(windowEnd):
			return p.failClockGate(seedClusterInfo, clk,
				fmt.Errorf("system clock %s is later than the plausibility ceiling %s (seed creation + %s); configure a trusted local time source or widen maxSeedAge",
					now.Format(time.RFC3339), windowEnd.Format(time.RFC3339), maxSeedAge))
		default:
			p.log.Infof("Clock gate: system clock %s is within the plausible window [%s, %s]; proceeding to recert",
				now.Format(time.RFC3339), windowStart.Format(time.RFC3339), windowEnd.Format(time.RFC3339))
			return nil
		}
	}

	// Tier 3: older seed without SeedCreationTime -- degraded lower-bound-only check.
	lower, ok := seedCertLowerBound(p.log)
	if !ok {
		p.log.Warn("Clock gate: seed carries no creation time and no control-plane certificate lower bound could be derived; proceeding without the full plausibility gate (documented limitation for pre-SeedCreationTime seeds)")
		return nil
	}
	if now.Before(lower) {
		clk := &ibistatus.ClockGateResult{
			ObservedTime: now.Format(time.RFC3339),
			WindowStart:  lower.Format(time.RFC3339),
			Decision:     ibistatus.DecisionCertLowerBound,
		}
		return p.failClockGate(seedClusterInfo, clk,
			fmt.Errorf("system clock %s is earlier than the earliest seed control-plane certificate validity %s; the clock is grossly implausible",
				now.Format(time.RFC3339), lower.Format(time.RFC3339)))
	}
	p.log.Warnf("Clock gate: seed carries no creation time; applied a degraded lower-bound-only check against %s (not the full plausibility gate); proceeding to recert",
		lower.Format(time.RFC3339))
	return nil
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

// resolveMaxSeedAge validates and resolves the configured maxSeedAge. An empty value uses the
// default (180 days); a value must be a positive Go duration string no larger than
// maxAllowedSeedAge, otherwise a config error is returned so the gate fails closed rather than
// silently accepting an implausible window.
func resolveMaxSeedAge(configured string) (time.Duration, error) {
	if strings.TrimSpace(configured) == "" {
		return defaultMaxSeedAge, nil
	}
	d, err := time.ParseDuration(configured)
	if err != nil {
		return 0, fmt.Errorf("maxSeedAge %q is not a valid duration: %w", configured, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("maxSeedAge %q must be positive", configured)
	}
	if d > maxAllowedSeedAge {
		return 0, fmt.Errorf("maxSeedAge %q exceeds the maximum allowed (%s)", configured, maxAllowedSeedAge)
	}
	return d, nil
}

// chronyConfigHasTrustedSource reports whether a chrony configuration declares at least one
// genuine external or on-link time source that could actually step the clock -- a server,
// pool, or peer pointing at a real address, or a hardware reference clock (refclock). It
// deliberately excludes sources backed by the same unreliable RTC the gate exists to distrust:
// the NTP local-clock pseudo-address family (127.127.x.x), loopback servers, and the
// `local stratum` fallback directive. A config carrying only those is treated as having no
// trusted source, so the gate falls through to the RTC plausibility window instead of being
// bypassed by the very clock it is meant to check.
func chronyConfigHasTrustedSource(chronyConfig string) bool {
	for _, raw := range strings.Split(chronyConfig, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || isChronyCommentLine(line) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToLower(fields[0]) {
		case "server", "pool", "peer":
			if len(fields) < 2 || isLocalClockAddress(fields[1]) {
				continue
			}
			return true
		case "refclock":
			// A hardware reference clock (PHC/PPS/GPS) is a genuine on-link time source.
			return true
		}
		// `local stratum N`, `rtcsync`, `rtcfile`, and other non-source directives are ignored.
	}
	return false
}

// isChronyCommentLine reports whether a trimmed chrony config line is a comment. chrony treats
// lines beginning with '#', '%', '!', or ';' as comments.
func isChronyCommentLine(line string) bool {
	if line == "" {
		return false
	}
	switch line[0] {
	case '#', '%', '!', ';':
		return true
	}
	return false
}

// isLocalClockAddress reports whether addr is the local/RTC-backed pseudo-source the gate must
// not trust: the NTP reference-clock pseudo-IP family (127.127.x.x) or a loopback address.
func isLocalClockAddress(addr string) bool {
	a := strings.ToLower(strings.TrimSpace(addr))
	if strings.HasPrefix(a, "127.127.") {
		return true
	}
	switch a {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// bringChronyUpAndConfirmSync places the user's chrony configuration and (re)starts chronyd
// before recert, then waits a bounded time for synchronization. Today chronyd is only
// restarted after recert, too late to correct the clock before certs are minted; bringing it
// up here lets a reachable trusted source step the clock first. recert later writes its own
// processed /etc/chrony.conf and restartChronydService reloads it, so this early write is
// transient. A configured-but-unreachable source (offline) times out quickly and surfaces as
// an error so the caller falls back to the RTC window.
func (p *PostPivot) bringChronyUpAndConfirmSync(chronyConfig string) error {
	//nolint:gosec // chrony.conf is a non-secret config normally world-readable; this is a transient pre-recert write recert overwrites.
	if err := os.WriteFile(chronyConfigFilePath, []byte(chronyConfig), 0o644); err != nil {
		return fmt.Errorf("failed to write chrony config %s: %w", chronyConfigFilePath, err)
	}
	if _, err := p.ops.SystemctlAction("restart", "chronyd"); err != nil {
		return fmt.Errorf("failed to restart chronyd: %w", err)
	}
	// `chronyc waitsync <max-tries> <max-correction> <max-skew> <interval>`: a small max-tries
	// at a 1s interval bounds the wait so an unreachable source does not stall the gate, and a
	// bounded max-correction (non-zero) ensures the remaining clock correction is actually small
	// before we treat the source as authoritative — "0" would disable that check entirely.
	if _, err := p.ops.RunInHostNamespace("chronyc", "waitsync", chronySyncMaxTries, chronySyncMaxCorrection, "0", "1"); err != nil {
		return fmt.Errorf("chrony did not confirm synchronization: %w", err)
	}
	return nil
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
