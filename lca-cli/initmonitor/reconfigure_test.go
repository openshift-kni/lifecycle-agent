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
}

// stubReconfigureSeams installs recording/deterministic seams and restores the originals.
// By default: no handoff on disk unless set, no existing result, convergence never met,
// time starts at the anchor and advances one poll interval per sleep.
func stubReconfigureSeams(t *testing.T) *reconfigureStub {
	t.Helper()
	s := &reconfigureStub{now: mustParse(testAnchor)}

	orig := struct {
		now         func() time.Time
		sleep       func(time.Duration)
		interval    time.Duration
		readHandoff func(string) (*reconfigure.Handoff, error)
		readResult  func(ops.Ops, *logrus.Logger) (*ibistatus.ReconfigureResult, error)
		writeSucc   func(ops.Ops, *logrus.Logger, *ibistatus.ReconfigureResult) error
		writeFail   func(ops.Ops, *logrus.Logger, *ibistatus.ReconfigureResult) error
		halt        func(ops.Ops, *logrus.Logger, error) error
		build       func(*InitMonitor) (runtimeclient.Client, error)
		converged   func(context.Context, runtimeclient.Client) bool
		disable     func(ops.Ops, *logrus.Logger) error
		handoffPath string
	}{
		reconfigureNowUTC, reconfigureSleep, reconfigurePollInterval, readReconfigureHandoff,
		readReconfigureResult, writeReconfigureSuccess, writeReconfigureFailure,
		haltReconfigurePoweredOn, reconfigureBuildClient, reconfigureConvergenceMet,
		reconfigureDisableUnit, reconfigureHandoffFilePath,
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
		return s.writeSuccErr
	}
	writeReconfigureFailure = func(_ ops.Ops, _ *logrus.Logger, r *ibistatus.ReconfigureResult) error {
		s.failureResult = r
		return s.writeFailErr
	}
	haltReconfigurePoweredOn = func(_ ops.Ops, _ *logrus.Logger, cause error) error {
		s.halted = true
		s.haltCause = cause
		return cause
	}
	reconfigureBuildClient = func(*InitMonitor) (runtimeclient.Client, error) { return nil, nil }
	reconfigureConvergenceMet = func(context.Context, runtimeclient.Client) bool { s.polls++; return false }
	reconfigureDisableUnit = func(ops.Ops, *logrus.Logger) error { s.disableCalls++; return s.disableErr }
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
// goes straight to the failure path (req #5).
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
	assert.Zero(t, s.polls, "must not poll when started after the deadline")
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
