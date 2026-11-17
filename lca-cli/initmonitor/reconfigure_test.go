package initmonitor

import (
	"context"
	"fmt"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openshift-kni/lifecycle-agent/internal/ibistatus"
	"github.com/openshift-kni/lifecycle-agent/internal/reconfigure"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

const testAnchor = "2026-01-01T00:00:00Z"

// reconfigureStub captures what the result-writer did through the package-var seams.
type reconfigureStub struct {
	successResult *ibistatus.ReconfigureResult
	failureResult *ibistatus.ReconfigureResult
	disableCalls  int
	disableErr    error
	writeFailErr  error
	writeSuccErr  error
	halted        bool
	haltCause     error
	polls         int
	now           time.Time
	// pendingSuccessPresent is what the pending-success marker check reports; pendingSuccessWrites
	// counts how many times the writer persisted the pending-success decision.
	pendingSuccessPresent bool
	pendingSuccessWrites  int
	// events records the terminal actions in the exact order they happened, so tests can assert
	// the real write -> disable -> halt sequence rather than merely that each action occurred.
	events []string
}

// Event names recorded in reconfigureStub.events, one per terminal action seam.
const (
	eventWriteSuccess = "write-success"
	eventWriteFailure = "write-failure"
	eventDisable      = "disable"
	eventHalt         = "halt"
)

// stubReconfigureSeams installs recording/deterministic seams and restores the originals.
// By default: no handoff on disk unless set, no existing result, convergence never met,
// time starts at the anchor and advances one poll interval per sleep.
func stubReconfigureSeams(t *testing.T) *reconfigureStub {
	t.Helper()
	s := &reconfigureStub{now: mustParse(testAnchor)}

	orig := struct {
		now               func() time.Time
		sleep             func(time.Duration)
		interval          time.Duration
		readHandoff       func(string) (*reconfigure.Handoff, error)
		readResult        func(ops.Ops, *logrus.Logger) (*ibistatus.ReconfigureResult, error)
		writeSucc         func(ops.Ops, *logrus.Logger, *ibistatus.ReconfigureResult) error
		writeFail         func(ops.Ops, *logrus.Logger, *ibistatus.ReconfigureResult) error
		halt              func(ops.Ops, *logrus.Logger, error) error
		build             func(*InitMonitor) (runtimeclient.Client, error)
		converged         func(context.Context, runtimeclient.Client) bool
		disable           func(ops.Ops, *logrus.Logger) error
		handoffPath       string
		completionPresent func(string) (bool, error)
		completedPath     string
		pendingPresent    func(string) (bool, error)
		writePending      func(string) error
		pendingPath       string
	}{
		reconfigureNowUTC, reconfigureSleep, reconfigurePollInterval, readReconfigureHandoff,
		readReconfigureResult, writeReconfigureSuccess, writeReconfigureFailure,
		haltReconfigurePoweredOn, reconfigureBuildClient, reconfigureConvergenceMet,
		reconfigureDisableUnit, reconfigureHandoffFilePath,
		reconfigureCompletionPresent, reconfigureCompletedFilePath,
		reconfigurePendingSuccessPresent, writeReconfigurePendingSuccess, reconfigureSuccessPendingFilePath,
	}
	t.Cleanup(func() {
		reconfigureNowUTC = orig.now
		reconfigureSleep = orig.sleep
		reconfigurePollInterval = orig.interval
		readReconfigureHandoff = orig.readHandoff
		readReconfigureResult = orig.readResult
		writeReconfigureSuccess = orig.writeSucc
		writeReconfigureFailure = orig.writeFail
		haltReconfigurePoweredOn = orig.halt
		reconfigureBuildClient = orig.build
		reconfigureConvergenceMet = orig.converged
		reconfigureDisableUnit = orig.disable
		reconfigureHandoffFilePath = orig.handoffPath
		reconfigureCompletionPresent = orig.completionPresent
		reconfigureCompletedFilePath = orig.completedPath
		reconfigurePendingSuccessPresent = orig.pendingPresent
		writeReconfigurePendingSuccess = orig.writePending
		reconfigureSuccessPendingFilePath = orig.pendingPath
	})

	reconfigurePollInterval = time.Minute
	reconfigureNowUTC = func() time.Time { return s.now }
	reconfigureSleep = func(d time.Duration) { s.now = s.now.Add(d) }
	readReconfigureHandoff = func(string) (*reconfigure.Handoff, error) {
		return &reconfigure.Handoff{Profile: "self-contained", PostPivotStartTime: testAnchor, SeedVersion: "4.18.3"}, nil
	}
	readReconfigureResult = func(ops.Ops, *logrus.Logger) (*ibistatus.ReconfigureResult, error) { return nil, nil }
	writeReconfigureSuccess = func(_ ops.Ops, _ *logrus.Logger, r *ibistatus.ReconfigureResult) error {
		s.successResult = r
		s.events = append(s.events, eventWriteSuccess)
		return s.writeSuccErr
	}
	writeReconfigureFailure = func(_ ops.Ops, _ *logrus.Logger, r *ibistatus.ReconfigureResult) error {
		s.failureResult = r
		s.events = append(s.events, eventWriteFailure)
		return s.writeFailErr
	}
	haltReconfigurePoweredOn = func(_ ops.Ops, _ *logrus.Logger, cause error) error {
		s.halted = true
		s.haltCause = cause
		s.events = append(s.events, eventHalt)
		return cause
	}
	reconfigureBuildClient = func(*InitMonitor) (runtimeclient.Client, error) { return nil, nil }
	reconfigureConvergenceMet = func(context.Context, runtimeclient.Client) bool { s.polls++; return false }
	reconfigureDisableUnit = func(ops.Ops, *logrus.Logger) error {
		s.disableCalls++
		s.events = append(s.events, eventDisable)
		return s.disableErr
	}
	// By default post-pivot reconfiguration has completed, so the success criterion's completion
	// requirement is satisfied.
	reconfigureCompletionPresent = func(string) (bool, error) { return true, nil }
	// By default no prior boot recorded a pending-success decision.
	reconfigurePendingSuccessPresent = func(string) (bool, error) { return s.pendingSuccessPresent, nil }
	writeReconfigurePendingSuccess = func(string) error { s.pendingSuccessWrites++; return nil }
	return s
}

func mustParse(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func newTestMonitor() *InitMonitor {
	return &InitMonitor{log: logrus.New()}
}

// convergedClient returns a fake client whose cluster satisfies the convergence criterion:
// SingleReplica infrastructure and a Ready node with the SNO role labels (and no cluster
// operators, which the health check reads as "all ready").
func convergedClient(t *testing.T) runtimeclient.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, configv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	infra := &configv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Status:     configv1.InfrastructureStatus{InfrastructureTopology: configv1.SingleReplicaTopologyMode},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "sno", Labels: map[string]string{
			"node-role.kubernetes.io/control-plane": "",
			"node-role.kubernetes.io/master":        "",
			"node-role.kubernetes.io/worker":        "",
		}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(infra, node).Build()
}

// TestReconfigureResultWriter_Success proves that when the real convergence predicate is
// satisfied the writer records success (no halt) and self-disables.
func TestReconfigureResultWriter_Success(t *testing.T) {
	s := stubReconfigureSeams(t)
	// Use the real predicate against a converged fake cluster to exercise the criterion wiring.
	reconfigureConvergenceMet = defaultReconfigureConvergenceMet
	reconfigureBuildClient = func(*InitMonitor) (runtimeclient.Client, error) { return convergedClient(t), nil }

	require.NoError(t, newTestMonitor().RunReconfigureResultWriter())
	require.NotNil(t, s.successResult)
	assert.Equal(t, ibistatus.ResultSuccess, s.successResult.Result)
	assert.Equal(t, "self-contained", s.successResult.Profile)
	assert.Equal(t, "4.18.3", s.successResult.SeedVersion)
	assert.Equal(t, 1, s.disableCalls)
	assert.False(t, s.halted)
	assert.Nil(t, s.failureResult)
	// The verdict must be persisted before the unit self-disables, and no halt occurs on success.
	assert.Equal(t, []string{eventWriteSuccess, eventDisable}, s.events)
}

// TestReconfigureResultWriter_SuccessWriteFailure_KeepsUnitEnabled proves that when the success
// verdict cannot be persisted the writer does NOT disable the unit (so the next boot retries)
// and returns the write error (CodeRabbit/codex: disable only after the verdict is persisted).
func TestReconfigureResultWriter_SuccessWriteFailure_KeepsUnitEnabled(t *testing.T) {
	s := stubReconfigureSeams(t)
	s.writeSuccErr = fmt.Errorf("p4 write failed")
	reconfigureConvergenceMet = func(context.Context, runtimeclient.Client) bool { return true }

	err := newTestMonitor().RunReconfigureResultWriter()
	require.Error(t, err)
	assert.ErrorIs(t, err, s.writeSuccErr)
	require.NotNil(t, s.successResult)
	assert.Zero(t, s.disableCalls, "unit must stay enabled so the next boot can retry the success write")
	assert.False(t, s.halted, "a failed success write must never halt the node")
	// Only the attempted write happened: no disable, no halt.
	assert.Equal(t, []string{eventWriteSuccess}, s.events)
}

// TestReconfigureResultWriter_Timeout proves that when the criterion is never met the writer
// records a failure, self-disables, and ALWAYS halts, in that order.
func TestReconfigureResultWriter_Timeout(t *testing.T) {
	s := stubReconfigureSeams(t)
	readReconfigureHandoff = func(string) (*reconfigure.Handoff, error) {
		return &reconfigure.Handoff{Profile: "self-contained", PostPivotStartTime: testAnchor, ConvergenceTimeout: "3m"}, nil
	}

	err := newTestMonitor().RunReconfigureResultWriter()
	require.Error(t, err)
	require.NotNil(t, s.failureResult)
	assert.Equal(t, ibistatus.ResultFailure, s.failureResult.Result)
	require.NotNil(t, s.failureResult.Error)
	assert.Equal(t, 1, s.disableCalls)
	assert.True(t, s.halted)
	assert.ErrorIs(t, err, s.haltCause)
	assert.Contains(t, err.Error(), "3m")
	assert.Positive(t, s.polls, "should have polled before the deadline")
	// The failure path must run strictly in order: record the verdict, self-disable, then halt.
	assert.Equal(t, []string{eventWriteFailure, eventDisable, eventHalt}, s.events)
}

// TestReconfigureResultWriter_WriteFailureStillHalts proves a failed failure-write never
// prevents the self-disable and the halt (Steven required #1).
func TestReconfigureResultWriter_WriteFailureStillHalts(t *testing.T) {
	s := stubReconfigureSeams(t)
	s.writeFailErr = fmt.Errorf("p4 write failed")
	readReconfigureHandoff = func(string) (*reconfigure.Handoff, error) {
		return &reconfigure.Handoff{Profile: "self-contained", PostPivotStartTime: testAnchor, ConvergenceTimeout: "2m"}, nil
	}

	err := newTestMonitor().RunReconfigureResultWriter()
	require.Error(t, err)
	assert.Equal(t, 1, s.disableCalls)
	assert.True(t, s.halted)
	// A failed failure-write still keeps the ordered write -> disable -> halt sequence.
	assert.Equal(t, []string{eventWriteFailure, eventDisable, eventHalt}, s.events)
}

// TestReconfigureResultWriter_DisableFailureStillHalts proves a failed self-disable never
// prevents the halt on the failure path.
func TestReconfigureResultWriter_DisableFailureStillHalts(t *testing.T) {
	s := stubReconfigureSeams(t)
	s.disableErr = fmt.Errorf("disable failed")
	readReconfigureHandoff = func(string) (*reconfigure.Handoff, error) {
		return &reconfigure.Handoff{Profile: "self-contained", PostPivotStartTime: testAnchor, ConvergenceTimeout: "2m"}, nil
	}

	err := newTestMonitor().RunReconfigureResultWriter()
	require.Error(t, err)
	assert.True(t, s.halted)
}

// TestReconfigureResultWriter_SuccessDisableFailure_VerdictStands proves a failed self-disable
// after success does not demote the success and never halts (Steven minor).
func TestReconfigureResultWriter_SuccessDisableFailure_VerdictStands(t *testing.T) {
	s := stubReconfigureSeams(t)
	s.disableErr = fmt.Errorf("disable failed")
	reconfigureConvergenceMet = func(context.Context, runtimeclient.Client) bool { return true }

	require.NoError(t, newTestMonitor().RunReconfigureResultWriter())
	require.NotNil(t, s.successResult)
	assert.Equal(t, 1, s.disableCalls)
	assert.False(t, s.halted)
}

// TestReconfigureResultWriter_PollsAcrossRestarts proves the loop re-evaluates only the live
// cluster, converging after several polls without consulting any service state.
func TestReconfigureResultWriter_PollsAcrossRestarts(t *testing.T) {
	s := stubReconfigureSeams(t)
	reconfigureConvergenceMet = func(context.Context, runtimeclient.Client) bool {
		s.polls++
		return s.polls >= 3
	}

	require.NoError(t, newTestMonitor().RunReconfigureResultWriter())
	assert.GreaterOrEqual(t, s.polls, 3)
	require.NotNil(t, s.successResult)
	assert.False(t, s.halted)
}

// TestReconfigureResultWriter_NotSelfContained_NoOp and _NoHandoff_NoOp prove the explicit
// profile gate (req #3).
func TestReconfigureResultWriter_NotSelfContained_NoOp(t *testing.T) {
	s := stubReconfigureSeams(t)
	readReconfigureHandoff = func(string) (*reconfigure.Handoff, error) {
		return &reconfigure.Handoff{Profile: "install-only", PostPivotStartTime: testAnchor}, nil
	}
	require.NoError(t, newTestMonitor().RunReconfigureResultWriter())
	assert.Nil(t, s.successResult)
	assert.Nil(t, s.failureResult)
	assert.False(t, s.halted)
	assert.Zero(t, s.disableCalls)
}

func TestReconfigureResultWriter_NoHandoff_NoOp(t *testing.T) {
	s := stubReconfigureSeams(t)
	readReconfigureHandoff = func(string) (*reconfigure.Handoff, error) { return nil, nil }
	require.NoError(t, newTestMonitor().RunReconfigureResultWriter())
	assert.Nil(t, s.successResult)
	assert.Nil(t, s.failureResult)
	assert.False(t, s.halted)
}

// TestReconfigureResultWriter_AlreadyTerminal_Idempotent proves an existing terminal verdict
// short-circuits the writer without polling or rewriting (req #1).
func TestReconfigureResultWriter_AlreadyTerminal_Idempotent(t *testing.T) {
	for _, verdict := range []string{ibistatus.ResultSuccess, ibistatus.ResultFailure} {
		t.Run(verdict, func(t *testing.T) {
			s := stubReconfigureSeams(t)
			readReconfigureResult = func(ops.Ops, *logrus.Logger) (*ibistatus.ReconfigureResult, error) {
				return &ibistatus.ReconfigureResult{Result: verdict}, nil
			}
			require.NoError(t, newTestMonitor().RunReconfigureResultWriter())
			assert.Nil(t, s.successResult)
			assert.Nil(t, s.failureResult)
			assert.False(t, s.halted)
			assert.Zero(t, s.polls)
		})
	}
}

// TestReconfigureResultWriter_StartAfterDeadline proves a writer that starts after the deadline
// does not enter the polling loop and, when the final recheck confirms the node is still not
// converged, goes to the failure path (req #5). The single convergence call is the final recheck,
// not the deadline-bounded poll loop.
func TestReconfigureResultWriter_StartAfterDeadline(t *testing.T) {
	s := stubReconfigureSeams(t)
	readReconfigureHandoff = func(string) (*reconfigure.Handoff, error) {
		return &reconfigure.Handoff{Profile: "self-contained", PostPivotStartTime: testAnchor, ConvergenceTimeout: "10m"}, nil
	}
	// now is already well past anchor + 10m.
	s.now = mustParse(testAnchor).Add(2 * time.Hour)

	err := newTestMonitor().RunReconfigureResultWriter()
	require.Error(t, err)
	assert.True(t, s.halted)
	require.NotNil(t, s.failureResult)
	// Exactly one convergence check — the final recheck — and no deadline-bounded poll loop (the
	// stub's reconfigureSleep would advance the clock if the loop ran; it never runs here).
	assert.Equal(t, 1, s.polls, "started-after-deadline must do one final recheck, not the poll loop")
}

// TestReconfigureResultWriter_FalseFailureAfterConvergence proves the fix for the DoR's "false
// failure after successful convergence": a writer that starts after the original deadline (e.g. a
// prior boot observed convergence but its success write failed, then the node rebooted past the
// deadline) must NOT record a failure or halt if the node already satisfies the criterion. The
// final recheck records success instead.
func TestReconfigureResultWriter_FalseFailureAfterConvergence(t *testing.T) {
	s := stubReconfigureSeams(t)
	readReconfigureHandoff = func(string) (*reconfigure.Handoff, error) {
		return &reconfigure.Handoff{Profile: "self-contained", PostPivotStartTime: testAnchor, ConvergenceTimeout: "10m", SeedVersion: "4.18.3"}, nil
	}
	// Started well past the deadline, but the node is converged now.
	s.now = mustParse(testAnchor).Add(2 * time.Hour)
	reconfigureConvergenceMet = func(context.Context, runtimeclient.Client) bool { return true }

	require.NoError(t, newTestMonitor().RunReconfigureResultWriter())
	require.NotNil(t, s.successResult)
	assert.Equal(t, ibistatus.ResultSuccess, s.successResult.Result)
	assert.Nil(t, s.failureResult, "a node that already converged must not be recorded as a failure")
	assert.False(t, s.halted, "a converged node must never be halted")
	assert.Equal(t, 1, s.disableCalls)
	assert.Equal(t, []string{eventWriteSuccess, eventDisable}, s.events)
}

// TestReconfigureResultWriter_NoCompletionMarker_EnforcesDeadline proves the completion marker gates
// only the SUCCESS verdict, not the writer's startup: with no completion marker the writer never
// records success (even against a converged cluster) but still enforces the deadline, recording a
// failure and halting. This is the fix for the DoR's "writer gated on reconfigure-completed cannot
// enforce the convergence deadline while post-pivot is still running or stalled".
func TestReconfigureResultWriter_NoCompletionMarker_EnforcesDeadline(t *testing.T) {
	s := stubReconfigureSeams(t)
	reconfigureCompletionPresent = func(string) (bool, error) { return false, nil }
	// Even a converged cluster must not produce a success while reconfiguration is unfinished.
	reconfigureConvergenceMet = func(context.Context, runtimeclient.Client) bool { return true }
	readReconfigureHandoff = func(string) (*reconfigure.Handoff, error) {
		return &reconfigure.Handoff{Profile: "self-contained", PostPivotStartTime: testAnchor, ConvergenceTimeout: "3m"}, nil
	}

	err := newTestMonitor().RunReconfigureResultWriter()
	require.Error(t, err)
	assert.Nil(t, s.successResult, "must not record success before reconfiguration completes")
	require.NotNil(t, s.failureResult, "the deadline must be enforced even without a completion marker")
	assert.True(t, s.halted)
	assert.Zero(t, s.pendingSuccessWrites, "no success decision may be recorded without completion")
	assert.Equal(t, []string{eventWriteFailure, eventDisable, eventHalt}, s.events)
}

// TestReconfigureResultWriter_CompletionCheckError_EnforcesDeadline proves a completion-marker check
// that errors is treated as "cannot succeed" (never a false success) while the deadline is still
// enforced: the writer records a failure and halts rather than hanging on an uncertain state.
func TestReconfigureResultWriter_CompletionCheckError_EnforcesDeadline(t *testing.T) {
	s := stubReconfigureSeams(t)
	reconfigureCompletionPresent = func(string) (bool, error) { return false, fmt.Errorf("stat failed") }
	reconfigureConvergenceMet = func(context.Context, runtimeclient.Client) bool { return true }
	readReconfigureHandoff = func(string) (*reconfigure.Handoff, error) {
		return &reconfigure.Handoff{Profile: "self-contained", PostPivotStartTime: testAnchor, ConvergenceTimeout: "3m"}, nil
	}

	err := newTestMonitor().RunReconfigureResultWriter()
	require.Error(t, err)
	assert.Nil(t, s.successResult)
	require.NotNil(t, s.failureResult)
	assert.True(t, s.halted)
}

// TestReconfigureResultWriter_Success_PersistsPendingDecision proves the writer durably records the
// pending-success decision BEFORE it writes the success verdict, so a failed verdict write can be
// re-published as success on a later boot.
func TestReconfigureResultWriter_Success_PersistsPendingDecision(t *testing.T) {
	s := stubReconfigureSeams(t)
	reconfigureConvergenceMet = func(context.Context, runtimeclient.Client) bool { return true }

	require.NoError(t, newTestMonitor().RunReconfigureResultWriter())
	require.NotNil(t, s.successResult)
	assert.Equal(t, 1, s.pendingSuccessWrites, "the success decision must be persisted before the verdict write")
}

// TestReconfigureResultWriter_PendingSuccess_Republished proves the pending-success retry: if a prior
// boot observed convergence and recorded a pending-success decision but its p4 write failed, a later
// boot -- even one that starts past the original deadline with the cluster momentarily not converged
// -- re-publishes success from the marker instead of re-deciding and recording a false failure.
func TestReconfigureResultWriter_PendingSuccess_Republished(t *testing.T) {
	s := stubReconfigureSeams(t)
	s.pendingSuccessPresent = true
	readReconfigureHandoff = func(string) (*reconfigure.Handoff, error) {
		return &reconfigure.Handoff{Profile: "self-contained", PostPivotStartTime: testAnchor, ConvergenceTimeout: "10m", SeedVersion: "4.18.3"}, nil
	}
	// Past the deadline and NOT converged now -- a fresh decision would record failure and halt.
	s.now = mustParse(testAnchor).Add(2 * time.Hour)
	reconfigureConvergenceMet = func(context.Context, runtimeclient.Client) bool { return false }

	require.NoError(t, newTestMonitor().RunReconfigureResultWriter())
	require.NotNil(t, s.successResult)
	assert.Equal(t, ibistatus.ResultSuccess, s.successResult.Result)
	assert.Nil(t, s.failureResult, "a pending-success decision must never be downgraded to failure")
	assert.False(t, s.halted)
	assert.Equal(t, 1, s.disableCalls)
	assert.Zero(t, s.polls, "a pending-success republish must not poll or recheck convergence")
	assert.Equal(t, []string{eventWriteSuccess, eventDisable}, s.events)
}

// TestReconfigureResultWriter_PendingSuccess_WriteFailure_KeepsUnitEnabled proves that re-publishing a
// pending-success decision whose verdict write still fails leaves the unit enabled (for another
// retry) and never halts.
func TestReconfigureResultWriter_PendingSuccess_WriteFailure_KeepsUnitEnabled(t *testing.T) {
	s := stubReconfigureSeams(t)
	s.pendingSuccessPresent = true
	s.writeSuccErr = fmt.Errorf("p4 write failed")

	err := newTestMonitor().RunReconfigureResultWriter()
	require.Error(t, err)
	assert.ErrorIs(t, err, s.writeSuccErr)
	assert.Zero(t, s.disableCalls, "unit must stay enabled so the next boot can retry publishing the success")
	assert.False(t, s.halted, "a pending-success retry must never halt the node")
	assert.Equal(t, []string{eventWriteSuccess}, s.events)
}

// TestReconfigureResultWriter_TimeoutConfig proves the configured/default/invalid timeout
// resolution reaches the deadline the failure cause reports.
func TestReconfigureResultWriter_TimeoutConfig(t *testing.T) {
	testcases := []struct {
		name      string
		timeout   string
		wantInfix string
	}{
		{name: "configured", timeout: "15m", wantInfix: "15m"},
		{name: "absent uses default", timeout: "", wantInfix: "1h0m0s"},
		{name: "invalid falls back to default", timeout: "90", wantInfix: "1h0m0s"},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			stubReconfigureSeams(t)
			readReconfigureHandoff = func(string) (*reconfigure.Handoff, error) {
				return &reconfigure.Handoff{Profile: "self-contained", PostPivotStartTime: testAnchor, ConvergenceTimeout: tc.timeout}, nil
			}
			err := newTestMonitor().RunReconfigureResultWriter()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantInfix)
		})
	}
}

// TestReconfigureResultWriter_UnparseableAnchor_NoOp proves an unparseable handoff anchor is a
// fail-safe no-op (never halts).
func TestReconfigureResultWriter_UnparseableAnchor_NoOp(t *testing.T) {
	s := stubReconfigureSeams(t)
	readReconfigureHandoff = func(string) (*reconfigure.Handoff, error) {
		return &reconfigure.Handoff{Profile: "self-contained", PostPivotStartTime: "not-a-time"}, nil
	}
	require.NoError(t, newTestMonitor().RunReconfigureResultWriter())
	assert.False(t, s.halted)
	assert.Nil(t, s.failureResult)
}
