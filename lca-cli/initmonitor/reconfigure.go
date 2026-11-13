package initmonitor

import (
	"context"
	"fmt"
	"os"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sirupsen/logrus"

	seedreconfig "github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/healthcheck"
	"github.com/openshift-kni/lifecycle-agent/internal/ibistatus"
	"github.com/openshift-kni/lifecycle-agent/internal/reconfigure"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

// This file implements the self-contained USB-based IBI reconfigure result-writer
// (CNF-27173, proposal §7.1/§8.H). It runs as a dedicated oneshot systemd unit
// (lca-reconfigure-result-writer.service), NOT inside post-pivot, so the long "operators
// Available" wait never blocks the post-pivot flow. It is the sole writer of the
// reconfigure.json convergence verdict:
//
//   - Success: node Ready AND all cluster operators converged => ibistatus.WriteSuccess
//     (no halt) -> best-effort self-disable. The node keeps running.
//   - Failure: the criterion is not met by the deadline (anchored at the write-once
//     post-pivot start) => best-effort ibistatus.WriteFailure -> best-effort self-disable ->
//     the node ALWAYS halts powered-on via emergency.target.
//
// Both verdicts are idempotent across reboots (enforced inside the ibistatus writers): an
// existing terminal verdict is never overwritten. The loop observes only live cluster state,
// so it polls across installation-configuration.service restarts without racing them.

// Package-var seams so unit tests can control time, the cluster observation, the kube client,
// the ibistatus writers/reader, the halt, and the unit-disable without a real node. They
// mirror the clock gate's seams in lca-cli/postpivot/clockgate.go.
var (
	// reconfigureNowUTC returns the current time; a package var so tests control "now".
	reconfigureNowUTC = func() time.Time { return time.Now().UTC() }

	// reconfigurePollInterval bounds how often the convergence criterion is re-evaluated.
	reconfigurePollInterval = 30 * time.Second

	// reconfigureSleep waits between polls; a seam so tests advance a fake clock instead of
	// sleeping for real.
	reconfigureSleep = func(d time.Duration) { time.Sleep(d) }

	// reconfigureHandoffFilePath is the durable handoff path; a seam for tests.
	reconfigureHandoffFilePath = common.ReconfigureHandoffFile

	// reconfigureCompletedFilePath is the post-pivot completion marker path; a seam for tests.
	reconfigureCompletedFilePath = common.ReconfigureCompletedFile

	// reconfigureSuccessPendingFilePath is the pending-success marker path; a seam for tests.
	reconfigureSuccessPendingFilePath = common.ReconfigureSuccessPendingFile

	// reconfigureCompletionPresent reports whether post-pivot reconfiguration has finished (the
	// completion marker exists). A seam so tests can simulate a writer that runs before
	// reconfiguration completed.
	reconfigureCompletionPresent = defaultReconfigureCompletionPresent

	// reconfigurePendingSuccessPresent reports whether a prior boot already observed convergence and
	// durably recorded a pending-success decision (the pending-success marker exists). A seam so
	// tests can simulate a success write that failed on an earlier boot.
	reconfigurePendingSuccessPresent = defaultReconfigurePendingSuccessPresent

	// writeReconfigurePendingSuccess durably records the pending-success decision before the p4
	// verdict write; a seam for tests.
	writeReconfigurePendingSuccess = reconfigure.WritePendingSuccessMarker

	// readReconfigureHandoff/readReconfigureResult read the durable handoff and the p4 verdict.
	readReconfigureHandoff = reconfigure.ReadHandoff
	readReconfigureResult  = ibistatus.ReadReconfigureResult

	// writeReconfigureSuccess/writeReconfigureFailure record the terminal verdict (write-only,
	// idempotent, no halt). haltReconfigurePoweredOn is the unconditional terminal action.
	writeReconfigureSuccess  = ibistatus.WriteSuccess
	writeReconfigureFailure  = ibistatus.WriteFailure
	haltReconfigurePoweredOn = ibistatus.HaltPoweredOn

	// reconfigureClientCallTimeout bounds a single kube API call so a connected-but-unresponsive
	// apiserver (a hung response read, which a bare client does not time out) cannot block a
	// convergence check indefinitely and defer the mandatory deadline halt. With it, the loop's
	// total wait is bounded to the convergence deadline plus at most one call timeout.
	reconfigureClientCallTimeout = 30 * time.Second

	// reconfigureBuildClient builds the kube client used to observe convergence; a seam so
	// tests inject a fake client with Node/ClusterOperator/Infrastructure objects.
	reconfigureBuildClient = func(m *InitMonitor) (runtimeclient.Client, error) {
		return buildReconfigureKubeClient(m.scheme, common.KubeconfigFile, reconfigureClientCallTimeout)
	}

	// reconfigureConvergenceMet evaluates the convergence criterion against the live client:
	// node Ready AND all cluster operators converged. It reuses the canonical healthcheck
	// predicates and is a seam so tests can force a deterministic sequence.
	reconfigureConvergenceMet = defaultReconfigureConvergenceMet

	// reconfigureDisableUnit self-disables the result-writer unit (best-effort); a seam.
	reconfigureDisableUnit = func(o ops.Ops, log *logrus.Logger) error {
		if _, err := o.SystemctlAction("disable", common.ReconfigureResultWriterService); err != nil {
			return fmt.Errorf("failed to disable %s: %w", common.ReconfigureResultWriterService, err)
		}
		return nil
	}
)

// buildReconfigureKubeClient builds the kube client used to observe convergence, with a
// per-call timeout so no single API call can hang past the convergence deadline. It mirrors
// utils.CreateKubeClient but sets config.Timeout.
func buildReconfigureKubeClient(scheme *runtime.Scheme, kubeconfig string, callTimeout time.Duration) (runtimeclient.Client, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to build config from flags for kube client: %w", err)
	}
	config.Timeout = callTimeout
	config.WarningHandler = rest.NoWarnings{}
	rc, err := runtimeclient.New(config, runtimeclient.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create runtimeclient for kube: %w", err)
	}
	return rc, nil
}

// defaultReconfigureConvergenceMet reports whether the node is Ready and all cluster operators
// are converged, using the canonical healthcheck predicates (healthcheck.IsNodeReady is a
// no-op on non-SingleReplica topologies; healthcheck.AreClusterOperatorsReady requires every
// operator Available, not Progressing, not Degraded). Any predicate error means "not yet
// converged".
func defaultReconfigureConvergenceMet(ctx context.Context, c runtimeclient.Client) bool {
	l := zap.New(zap.UseFlagOptions(&zap.Options{Development: true}))
	if err := healthcheck.IsNodeReady(ctx, c, l); err != nil {
		l.Info("reconfigure result-writer: node not yet ready", "error", err.Error())
		return false
	}
	if err := healthcheck.AreClusterOperatorsReady(ctx, c, l); err != nil {
		l.Info("reconfigure result-writer: cluster operators not yet converged", "error", err.Error())
		return false
	}
	return true
}

// statMarkerPresent reports whether the marker file at path exists. A clean absence returns
// (false, nil); only a genuine stat IO error is returned (wrapped with desc) so the caller can
// fail safe rather than acting on an uncertain state.
func statMarkerPresent(path, desc string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("failed to stat %s %s: %w", desc, path, err)
}

// defaultReconfigureCompletionPresent reports whether the post-pivot completion marker exists.
func defaultReconfigureCompletionPresent(path string) (bool, error) {
	return statMarkerPresent(path, "post-pivot completion marker")
}

// defaultReconfigurePendingSuccessPresent reports whether the pending-success marker exists.
func defaultReconfigurePendingSuccessPresent(path string) (bool, error) {
	return statMarkerPresent(path, "reconfigure pending-success marker")
}

// RunReconfigureResultWriter is the entry point for the result-writer unit. It gates on the
// explicit self-contained profile (via the durable handoff), honours an already-recorded terminal
// verdict (idempotence), re-publishes a prior boot's pending-success decision if one exists, then
// observes the success criterion (post-pivot completion AND cluster convergence) until met or the
// deadline, and writes the terminal reconfigure.json verdict: success keeps the node running, a
// timeout writes a failure and always halts the node powered-on. The deadline is enforced
// independently of the completion marker, so a stalled or failed post-pivot cannot defer the halt.
func (m *InitMonitor) RunReconfigureResultWriter() error {
	log := m.log

	// 1. Explicit profile gate: only the self-contained post-pivot writes the durable handoff.
	// ConditionPathExists on the unit usually prevents it from even starting otherwise; this
	// is belt-and-suspenders and also covers a profile mismatch.
	handoff, err := readReconfigureHandoff(reconfigureHandoffFilePath)
	if err != nil {
		log.Warnf("reconfigure result-writer: could not read the durable handoff (%v); nothing to do", err)
		return nil
	}
	if handoff == nil {
		log.Info("reconfigure result-writer: no durable handoff present; not the self-contained profile, nothing to do")
		return nil
	}
	if handoff.Profile != seedreconfig.ProfileSelfContained {
		log.Infof("reconfigure result-writer: profile %q is not %q; nothing to do", handoff.Profile, seedreconfig.ProfileSelfContained)
		return nil
	}

	// NOTE: there is deliberately no completion gate on the writer's STARTUP. The completion marker
	// gates only the SUCCESS verdict (see successCriterionMet), not whether the writer runs at all.
	// Gating startup on completion would let a stalled or failed post-pivot -- which never writes the
	// marker -- defer the convergence deadline indefinitely, so the node would hang instead of
	// halting at the deadline. The writer therefore always runs for the self-contained profile and
	// enforces the deadline; it simply cannot record SUCCESS until reconfiguration has finished.

	// 2. Idempotence: never re-decide once a terminal verdict exists.
	if existing, rerr := readReconfigureResult(m.ops, log); rerr != nil {
		log.Warnf("reconfigure result-writer: could not read the existing reconfigure result (%v); continuing", rerr)
	} else if existing.IsTerminal() {
		log.Infof("reconfigure result-writer: reconfigure.json already records a terminal %q verdict; nothing to do", existing.Result)
		return nil
	}

	// 2b. Pending-success retry: if a prior boot already observed convergence and durably recorded a
	// pending-success decision (its p4 write failed and the node rebooted), re-publish that decision
	// now instead of re-deciding against live state -- which, past the write-once deadline, could
	// record a false failure for a node that already converged. This takes precedence over the
	// deadline and the poll loop.
	if pending, perr := reconfigurePendingSuccessPresent(reconfigureSuccessPendingFilePath); perr != nil {
		log.Warnf("reconfigure result-writer: could not check the pending-success marker (%v); continuing", perr)
	} else if pending {
		log.Info("reconfigure result-writer: a prior boot recorded a pending-success decision; re-publishing success")
		return m.publishReconfigureSuccess(handoff)
	}

	// 3. Resolve the convergence deadline from the write-once post-pivot-start anchor.
	timeout, terr := seedreconfig.ParseReconfigureConvergenceTimeout(handoff.ConvergenceTimeout)
	if terr != nil {
		log.Warnf("reconfigure result-writer: invalid convergence timeout %q (%v); using the default %s",
			handoff.ConvergenceTimeout, terr, seedreconfig.DefaultReconfigureConvergenceTimeout)
		timeout = seedreconfig.DefaultReconfigureConvergenceTimeout
	}
	start, perr := time.Parse(time.RFC3339, handoff.PostPivotStartTime)
	if perr != nil {
		// An unparseable handoff anchor is a fail-safe no-op: without a trustworthy anchor the
		// writer cannot soundly decide a timeout, so it must not halt the node.
		log.Warnf("reconfigure result-writer: unparseable post-pivot start time %q (%v); nothing to do (fail safe)",
			handoff.PostPivotStartTime, perr)
		return nil
	}
	deadline := start.UTC().Add(timeout)
	log.Infof("reconfigure result-writer: convergence deadline is %s (anchor %s + %s)",
		deadline.Format(time.RFC3339), start.UTC().Format(time.RFC3339), timeout)

	// 4. Observe convergence until the criterion is met or the deadline elapses. A writer that
	// starts after the deadline, or that cannot build a kube client before it, goes straight to
	// the failure path.
	converged := false
	switch {
	case !reconfigureNowUTC().Before(deadline):
		log.Warnf("reconfigure result-writer: started after the convergence deadline %s; recording failure", deadline.Format(time.RFC3339))
	default:
		client, cerr := m.buildReconfigureClient(deadline)
		if cerr != nil {
			log.Errorf("reconfigure result-writer: %v; recording failure", cerr)
		} else {
			converged = m.pollUntilConvergedOrDeadline(client, deadline)
		}
	}

	// 4b. Final convergence recheck before any failure verdict. Polling in step 4 stops at the
	// deadline, and a writer that starts after the deadline skips polling entirely. In both cases
	// the node may already satisfy the criterion -- most importantly when a prior boot observed
	// convergence but its success write failed and this boot started after the original deadline.
	// One live recheck here means a terminal failure (and the halt that follows) is only ever
	// recorded for a node that is genuinely not converged, never for one that already converged.
	if !converged {
		converged = m.finalConvergenceRecheck()
	}

	// 5. Success: durably record the success DECISION before the verdict write, then publish it.
	// Persisting the pending-success marker first means a failed p4 write is re-published as success
	// on a later boot (step 2b) rather than re-decided against possibly-transient live state past the
	// deadline, which could otherwise record a false failure for a node that already converged.
	if converged {
		if merr := writeReconfigurePendingSuccess(reconfigureSuccessPendingFilePath); merr != nil {
			// Best-effort: if the decision cannot be persisted, still attempt the verdict write below.
			// The worst case degrades to the pre-marker behaviour (a later boot re-decides via the
			// final recheck), never to a wrong verdict on this boot.
			log.Errorf("reconfigure result-writer: failed to persist the pending-success decision (%v); continuing to the verdict write", merr)
		}
		return m.publishReconfigureSuccess(handoff)
	}

	// 6. Failure: ordered best-effort write -> best-effort disable -> ALWAYS halt powered-on.
	// Neither a write failure, an idempotence-guard no-op, nor a disable failure may skip the
	// halt.
	cause := fmt.Errorf("reconfigure convergence criterion not met within %s", timeout)
	msg := cause.Error()
	if werr := writeReconfigureFailure(m.ops, log, m.reconfigureResult(handoff, ibistatus.ResultFailure, &msg)); werr != nil {
		log.Errorf("reconfigure result-writer: failed to record the failure verdict (halting powered-on regardless): %v", werr)
	}
	if derr := reconfigureDisableUnit(m.ops, log); derr != nil {
		log.Errorf("reconfigure result-writer: failed to disable the unit before halt (halting powered-on regardless): %v", derr)
	}
	return haltReconfigurePoweredOn(m.ops, log, cause)
}

// buildReconfigureClient builds the kube client, retrying until it succeeds or the deadline
// elapses (the kubeconfig may not exist yet at early boot). It never consults
// installation-configuration.service state, so it polls across its restarts without racing.
func (m *InitMonitor) buildReconfigureClient(deadline time.Time) (runtimeclient.Client, error) {
	var lastErr error
	for {
		client, err := reconfigureBuildClient(m)
		if err == nil {
			return client, nil
		}
		lastErr = err
		if !reconfigureNowUTC().Before(deadline) {
			return nil, fmt.Errorf("could not build a kube client before the convergence deadline: %w", lastErr)
		}
		m.log.Warnf("reconfigure result-writer: kube client not ready yet (%v); retrying", err)
		reconfigureSleep(reconfigurePollInterval)
	}
}

// successCriterionMet reports whether a SUCCESS verdict may be recorded: post-pivot reconfiguration
// must have finished (the completion marker exists) AND the cluster must have converged (node Ready
// and all cluster operators settled). Gating success on completion prevents recording a false
// success against the still-healthy seed cluster when post-pivot has not finished; the convergence
// deadline is enforced separately (by the caller), so a stalled or failed post-pivot that never
// writes the completion marker still cannot defer the deadline -- the writer simply records failure
// at the deadline rather than success. A completion-marker check that errors or reports "not yet
// complete" is treated as "cannot succeed" (never as success).
func (m *InitMonitor) successCriterionMet(ctx context.Context, client runtimeclient.Client) bool {
	if done, cerr := reconfigureCompletionPresent(reconfigureCompletedFilePath); cerr != nil {
		m.log.Warnf("reconfigure result-writer: could not check the post-pivot completion marker (%v); treating as not complete", cerr)
		return false
	} else if !done {
		m.log.Info("reconfigure result-writer: post-pivot reconfiguration has not completed yet (no completion marker); not recording success")
		return false
	}
	return reconfigureConvergenceMet(ctx, client)
}

// pollUntilConvergedOrDeadline polls the success criterion until it is met (returns true) or the
// deadline elapses (returns false). It queries only the live cluster and the local completion
// marker, never the post-pivot service state, so it polls safely across
// installation-configuration.service restarts.
func (m *InitMonitor) pollUntilConvergedOrDeadline(client runtimeclient.Client, deadline time.Time) bool {
	ctx := context.Background()
	for {
		if !reconfigureNowUTC().Before(deadline) {
			return false
		}
		if m.successCriterionMet(ctx, client) {
			return true
		}
		m.log.Infof("reconfigure result-writer: criterion not yet met; re-checking in %s (deadline %s)",
			reconfigurePollInterval, deadline.Format(time.RFC3339))
		reconfigureSleep(reconfigurePollInterval)
	}
}

// finalConvergenceRecheck performs a single live success-criterion check before a deadline failure
// is recorded. The deadline has already elapsed, so it does not retry the client build: it makes one
// best-effort attempt and reports whether the criterion (completion AND convergence) is currently
// met. Any client-build error, or an unmet criterion, is treated as "not converged" so the caller
// proceeds to the failure path. It is the safety net for a node that already satisfies the criterion
// past the deadline -- notably a first run that started after the deadline because post-pivot took
// longer than the timeout but ultimately succeeded.
func (m *InitMonitor) finalConvergenceRecheck() bool {
	client, err := reconfigureBuildClient(m)
	if err != nil {
		m.log.Warnf("reconfigure result-writer: final convergence recheck could not build a kube client (%v); treating as not converged", err)
		return false
	}
	if m.successCriterionMet(context.Background(), client) {
		m.log.Info("reconfigure result-writer: success criterion met on the final recheck after the deadline; recording success instead of failure")
		return true
	}
	m.log.Info("reconfigure result-writer: success criterion still not met on the final recheck; recording failure")
	return false
}

// publishReconfigureSuccess records the terminal success verdict, then self-disables the unit
// (best-effort, ONLY after the verdict is persisted -- disabling before the verdict lands would
// defeat the next-boot retry). If the verdict write fails the unit is left enabled and the error is
// surfaced so systemd re-runs it on the next boot; the durable pending-success marker (written by
// the caller before this) then re-publishes the decision via step 2b. The idempotence guard inside
// the writer protects an already-recorded verdict.
func (m *InitMonitor) publishReconfigureSuccess(handoff *reconfigure.Handoff) error {
	log := m.log
	log.Info("reconfigure result-writer: convergence criterion met; recording success")
	if werr := writeReconfigureSuccess(m.ops, log, m.reconfigureResult(handoff, ibistatus.ResultSuccess, nil)); werr != nil {
		log.Errorf("reconfigure result-writer: failed to record the success verdict (unit left enabled for a next-boot retry): %v", werr)
		return fmt.Errorf("failed to record reconfigure success: %w", werr)
	}
	if derr := reconfigureDisableUnit(m.ops, log); derr != nil {
		// A failed self-disable never demotes the success; the idempotence guard protects a later
		// boot that re-runs the unit.
		log.Errorf("reconfigure result-writer: failed to disable the unit after success (success stands): %v", derr)
	}
	return nil
}

// reconfigureResult assembles the terminal reconfigure.json record from the handoff metadata.
func (m *InitMonitor) reconfigureResult(handoff *reconfigure.Handoff, result string, errMsg *string) *ibistatus.ReconfigureResult {
	return &ibistatus.ReconfigureResult{
		Phase:       ibistatus.PhaseReconfigure,
		Profile:     handoff.Profile,
		Result:      result,
		Timestamp:   reconfigureNowUTC().Format(time.RFC3339),
		SeedVersion: handoff.SeedVersion,
		Error:       errMsg,
	}
}
