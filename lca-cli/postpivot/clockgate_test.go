package postpivot

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	clusterconfig_api "github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ibistatus"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedclusterinfo"
)

// capturedHalt records what the clock gate handed to the terminal write-back + halt, so tests
// can assert the failure path without performing a real p4 mount or emergency-target isolation.
type capturedHalt struct {
	called bool
	result *ibistatus.ReconfigureResult
	cause  error
}

// stubClockGateSeams redirects the clock-gate package-var seams to test doubles and returns a
// capturedHalt the failure path records into. The stubbed writeReconfigureFailureAndHalt
// returns the cause, mirroring the real helper after it has isolated the node.
func stubClockGateSeams(t *testing.T) *capturedHalt {
	t.Helper()
	origNow := nowUTC
	origChrony := chronyConfigFilePath
	origLower := seedCertLowerBound
	origWrite := writeReconfigureFailureAndHalt

	c := &capturedHalt{}
	chronyConfigFilePath = filepath.Join(t.TempDir(), "chrony.conf")
	writeReconfigureFailureAndHalt = func(_ ops.Ops, _ *logrus.Logger, r *ibistatus.ReconfigureResult, cause error) error {
		c.called = true
		c.result = r
		c.cause = cause
		return cause
	}
	t.Cleanup(func() {
		nowUTC = origNow
		chronyConfigFilePath = origChrony
		seedCertLowerBound = origLower
		writeReconfigureFailureAndHalt = origWrite
	})
	return c
}

func setNow(t time.Time) { nowUTC = func() time.Time { return t.UTC() } }

func rfc3339(s string) time.Time {
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return ts
}

// p4Present returns a block-device list containing the ibi-status (p4) partition so the gate
// treats the run as the self-contained USB IBI profile.
func p4Present() []ops.BlockDevice {
	return []ops.BlockDevice{{Name: "/dev/sdb4", Label: common.IBIStatusFSLabel, FSType: "ext4"}}
}

func newGatePostPivot(t *testing.T, mockOps ops.Ops) *PostPivot {
	t.Helper()
	return NewPostPivot(nil, logrus.New(), mockOps, "", t.TempDir(), "")
}

// runClockGate runs the clock gate for tests that do not need to inspect whether its temporary
// Chrony configuration remained active.
func runClockGate(t *testing.T, pp *PostPivot, seedReconfiguration *clusterconfig_api.SeedReconfiguration,
	seedClusterInfo *seedclusterinfo.SeedClusterInfo) error {
	t.Helper()
	_, err := pp.clockGateWithChronyConfig(seedReconfiguration, seedClusterInfo)
	return err
}

// TestClockGate_RecertAlreadyCompleted_NoOp proves that once recert has completed (recert.done
// present), a later boot with a wildly implausible clock is a strict no-op: no p4 access, no
// failure record, no halt.
func TestClockGate_RecertAlreadyCompleted_NoOp(t *testing.T) {
	c := stubClockGateSeams(t)
	setNow(rfc3339("1970-01-01T00:00:00Z")) // grossly implausible

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	// No ops calls expected: the marker short-circuits before anything else.
	pp := newGatePostPivot(t, mockOps)
	require.NoError(t, os.WriteFile(filepath.Join(pp.workingDir, "recert.done"), []byte(""), 0o600))

	seedReconfig := &clusterconfig_api.SeedReconfiguration{}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}

	assert.NoError(t, runClockGate(t, pp, seedReconfig, seedClusterInfo))
	assert.False(t, c.called, "the gate must not halt a converged node")
}

// TestClockGate_NotSelfContained_Skipped proves the gate is a no-op when no ibi-status (p4)
// partition is present (install-only / IBU).
func TestClockGate_NotSelfContained_Skipped(t *testing.T) {
	c := stubClockGateSeams(t)
	setNow(rfc3339("1970-01-01T00:00:00Z"))

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return([]ops.BlockDevice{{Name: "/dev/sda", Label: "root", FSType: "xfs"}}, nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	assert.NoError(t, runClockGate(t, pp, &clusterconfig_api.SeedReconfiguration{}, seedClusterInfo))
	assert.False(t, c.called)
}

// TestClockGate_PlausibleClock_Proceeds proves a clock inside the plausible window passes
// without halting.
func TestClockGate_PlausibleClock_Proceeds(t *testing.T) {
	stubClockGateSeams(t)
	setNow(rfc3339("2025-06-01T00:00:00Z")) // within [2025-01-01, +180d]

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z", SeedClusterOCPVersion: "4.18.3"}
	assert.NoError(t, runClockGate(t, pp, &clusterconfig_api.SeedReconfiguration{}, seedClusterInfo))
}

// TestClockGate_ClockBeforeSeed_Fails proves a clock earlier than the seed creation time halts
// the node before recert with an rtc-window failure record.
func TestClockGate_ClockBeforeSeed_Fails(t *testing.T) {
	c := stubClockGateSeams(t)
	setNow(rfc3339("2024-01-01T00:00:00Z")) // before seed creation

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z", SeedClusterOCPVersion: "4.18.3"}
	err := runClockGate(t, pp, &clusterconfig_api.SeedReconfiguration{}, seedClusterInfo)
	require.Error(t, err)
	require.True(t, c.called)
	assert.Equal(t, ibistatus.ResultFailure, c.result.Result)
	assert.Equal(t, ibistatus.DecisionRTCWindow, c.result.Clock.Decision)
	assert.Equal(t, "4.18.3", c.result.SeedVersion)
}

// TestClockGate_ClockAfterCeiling_Fails proves a clock beyond seedCreation + maxSeedAge halts
// the node before recert.
func TestClockGate_ClockAfterCeiling_Fails(t *testing.T) {
	c := stubClockGateSeams(t)
	setNow(rfc3339("2026-01-01T00:00:00Z")) // well beyond +180d

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	err := runClockGate(t, pp, &clusterconfig_api.SeedReconfiguration{}, seedClusterInfo)
	require.Error(t, err)
	require.True(t, c.called)
	assert.Equal(t, ibistatus.DecisionRTCWindow, c.result.Clock.Decision)
}

// TestClockGate_ConfigurableMaxSeedAge proves a widened maxSeedAge admits a clock the default
// would reject.
func TestClockGate_ConfigurableMaxSeedAge(t *testing.T) {
	stubClockGateSeams(t)
	setNow(rfc3339("2026-01-01T00:00:00Z")) // beyond default 180d, within 2y

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{MaxSeedAge: "17520h"} // 2 years
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	assert.NoError(t, runClockGate(t, pp, seedReconfig, seedClusterInfo))
}

// TestClockGate_InvalidMaxSeedAge_Fails proves a malformed maxSeedAge aborts the reconfigure
// with a plain config error (so recert never runs) rather than halting the node powered-on: a
// tuning-knob typo must not brick the node.
func TestClockGate_InvalidMaxSeedAge_Fails(t *testing.T) {
	c := stubClockGateSeams(t)
	setNow(rfc3339("2025-06-01T00:00:00Z"))

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{MaxSeedAge: "not-a-duration"}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	err := runClockGate(t, pp, seedReconfig, seedClusterInfo)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maxSeedAge")
	assert.False(t, c.called, "a config-error must not halt the node powered-on")
}

// TestClockGate_SyncedLocalSource_Proceeds proves that a trusted chrony source that confirms
// synchronization makes the clock authoritative: the gate passes even when the raw clock would
// be rejected by the window.
func TestClockGate_SyncedLocalSource_Proceeds(t *testing.T) {
	stubClockGateSeams(t)
	setNow(rfc3339("1970-01-01T00:00:00Z")) // window would reject, but sync is authoritative

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	mockOps.EXPECT().SystemctlAction("restart", "chronyd").Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "waitsync", chronySyncMaxTries, chronySyncMaxCorrection, "0", "1").Return("", nil).Times(1)
	// The runtime-selected reference is a real external source (not local mode).
	mockOps.EXPECT().RunInHostNamespace("chronyc", "-n", "tracking").
		Return("Reference ID    : 0A000005 (10.0.0.5)\nStratum         : 2\nLeap status     : Normal", nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{ChronyConfig: "server ntp.example.com iburst\n"}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	gateConfigActive, err := pp.clockGateWithChronyConfig(seedReconfig, seedClusterInfo)
	require.NoError(t, err)
	assert.True(t, gateConfigActive, "a synchronized trusted source must report the temporary config as active")

	// The config was written for the early bring-up, with the gate's transient makestep policy.
	data, err := os.ReadFile(chronyConfigFilePath) //nolint:gosec // test path
	require.NoError(t, err)
	assert.Contains(t, string(data), "server ntp.example.com")
	assert.Contains(t, string(data), "makestep 1 -1")
}

// TestRestoreChronyConfigAfterGate proves the original configuration is restored and chronyd
// is restarted after the gate's temporary makestep policy was installed.
func TestRestoreChronyConfigAfterGate(t *testing.T) {
	stubClockGateSeams(t)
	originalConfig := "server 192.0.2.10 iburst\nmakestep 1 3\n"
	require.NoError(t, os.WriteFile(chronyConfigFilePath, []byte("server 192.0.2.10 iburst\nmakestep 1 -1\n"), 0o644)) //nolint:gosec // test path

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().SystemctlAction("restart", "chronyd").Return("", nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	require.NoError(t, pp.restoreChronyConfigAfterGate(originalConfig))
	data, err := os.ReadFile(chronyConfigFilePath) //nolint:gosec // test path
	require.NoError(t, err)
	assert.Equal(t, originalConfig, string(data))
}

// TestRestoreChronyConfigAfterGate_EmptyConfig is a defensive test for a path that is currently
// unreachable through the gate: only a configuration containing a trusted source can activate
// the temporary policy. It proves that future callers cannot leave makestep-only content on
// disk by restoring an empty operator configuration.
func TestRestoreChronyConfigAfterGate_EmptyConfig(t *testing.T) {
	stubClockGateSeams(t)
	require.NoError(t, os.WriteFile(chronyConfigFilePath, []byte("makestep 1 -1\n"), 0o644)) //nolint:gosec // test path

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().SystemctlAction("restart", "chronyd").Return("", nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	require.NoError(t, pp.restoreChronyConfigAfterGate(""))
	data, err := os.ReadFile(chronyConfigFilePath) //nolint:gosec // test path
	require.NoError(t, err)
	assert.Empty(t, data)
}

// TestBringChronyUpAndConfirmSync_RestartFails_RestoresConfig proves a chronyd restart failure
// after the temporary write also restores the original configuration.
func TestBringChronyUpAndConfirmSync_RestartFails_RestoresConfig(t *testing.T) {
	stubClockGateSeams(t)
	originalConfig := "server ntp.example.com iburst\nmakestep 1 3\n"

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().SystemctlAction("restart", "chronyd").Return("", fmt.Errorf("unit failed")).Times(1)
	mockOps.EXPECT().SystemctlAction("restart", "chronyd").Return("", nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	err := pp.bringChronyUpAndConfirmSync(originalConfig)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unit failed")

	data, err := os.ReadFile(chronyConfigFilePath) //nolint:gosec // test path
	require.NoError(t, err)
	assert.Equal(t, originalConfig, string(data))
}

// TestClockGate_ConfiguredSourceUnavailable_FallsBackToWindow proves that a configured trusted
// source whose sync is not confirmed (offline) does not bypass the gate: the RTC window still
// decides. Here the clock is implausible, so the gate fails.
func TestClockGate_ConfiguredSourceUnavailable_FallsBackToWindow(t *testing.T) {
	c := stubClockGateSeams(t)
	setNow(rfc3339("1970-01-01T00:00:00Z"))

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	mockOps.EXPECT().SystemctlAction("restart", "chronyd").Return("", nil).Times(2)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "waitsync", chronySyncMaxTries, chronySyncMaxCorrection, "0", "1").
		Return("", fmt.Errorf("timeout")).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{ChronyConfig: "server ntp.example.com iburst\n"}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	gateConfigActive, err := pp.clockGateWithChronyConfig(seedReconfig, seedClusterInfo)
	require.Error(t, err)
	assert.False(t, gateConfigActive, "a failed synchronization must not leave the gate config active")
	require.True(t, c.called)
	assert.Equal(t, ibistatus.DecisionRTCWindow, c.result.Clock.Decision)

	data, err := os.ReadFile(chronyConfigFilePath) //nolint:gosec // test path
	require.NoError(t, err)
	assert.Equal(t, seedReconfig.ChronyConfig, string(data))
}

// TestClockGate_ChronyRestorationFailure_Aborts proves a failure to restore the temporary
// Chrony policy records a terminal config error and halts instead of falling back to the RTC
// plausibility window.
func TestClockGate_ChronyRestorationFailure_Aborts(t *testing.T) {
	c := stubClockGateSeams(t)
	setNow(rfc3339("2025-06-01T00:00:00Z"))

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	mockOps.EXPECT().SystemctlAction("restart", "chronyd").Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "waitsync", chronySyncMaxTries, chronySyncMaxCorrection, "0", "1").
		Return("", fmt.Errorf("timeout")).Times(1)
	mockOps.EXPECT().SystemctlAction("restart", "chronyd").Return("", fmt.Errorf("restore restart failed")).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{ChronyConfig: "server ntp.example.com iburst\n"}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	gateConfigActive, err := pp.clockGateWithChronyConfig(seedReconfig, seedClusterInfo)
	require.Error(t, err)
	assert.False(t, gateConfigActive)
	assert.ErrorContains(t, err, "timeout")
	assert.ErrorContains(t, err, "restore restart failed")
	require.True(t, c.called, "a restoration failure must leave a technician-readable terminal record")
	assert.Equal(t, ibistatus.ResultFailure, c.result.Result)
	require.NotNil(t, c.result.Clock)
	assert.Equal(t, ibistatus.DecisionConfigError, c.result.Clock.Decision)

	data, readErr := os.ReadFile(chronyConfigFilePath) //nolint:gosec // test path
	require.NoError(t, readErr)
	assert.Equal(t, seedReconfig.ChronyConfig, string(data))
}

// TestClockGate_SyncedLocalMode_FallsBackToWindow proves that a config which satisfies waitsync
// only through chrony's local mode (its own clock, no real external source selected) does not
// bypass the gate: the runtime tracking check rejects the local-mode reference, so the RTC
// window still decides. Here the clock is implausible, so the gate fails.
func TestClockGate_SyncedLocalMode_FallsBackToWindow(t *testing.T) {
	c := stubClockGateSeams(t)
	setNow(rfc3339("1970-01-01T00:00:00Z"))

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	mockOps.EXPECT().SystemctlAction("restart", "chronyd").Return("", nil).Times(2)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "waitsync", chronySyncMaxTries, chronySyncMaxCorrection, "0", "1").Return("", nil).Times(1)
	// chronyd reports synchronized, but to its own local clock (7F7F01xx) -- not a real source.
	mockOps.EXPECT().RunInHostNamespace("chronyc", "-n", "tracking").
		Return("Reference ID    : 7F7F0101 ()\nStratum         : 10\nLeap status     : Normal", nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	// A mixed config: a reachable-looking server plus a local-stratum fallback.
	seedReconfig := &clusterconfig_api.SeedReconfiguration{ChronyConfig: "server ntp.example.com iburst\nlocal stratum 10\n"}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	err := runClockGate(t, pp, seedReconfig, seedClusterInfo)
	require.Error(t, err)
	require.True(t, c.called)
	assert.Equal(t, ibistatus.DecisionRTCWindow, c.result.Clock.Decision)

	data, err := os.ReadFile(chronyConfigFilePath) //nolint:gosec // test path
	require.NoError(t, err)
	assert.Equal(t, seedReconfig.ChronyConfig, string(data))
}

// TestClockGate_OldSeedNoTime_CertLowerBound proves the degraded old-seed path: with no
// SeedCreationTime, a clock before the cert lower bound fails, a clock after it proceeds, and an
// underivable bound proceeds with a warning.
func TestClockGate_OldSeedNoTime_CertLowerBound(t *testing.T) {
	lower := rfc3339("2024-01-01T00:00:00Z")

	t.Run("before lower bound fails", func(t *testing.T) {
		c := stubClockGateSeams(t)
		setNow(rfc3339("2000-01-01T00:00:00Z"))
		seedCertLowerBound = func(*logrus.Logger) (time.Time, bool) { return lower, true }

		ctrl := gomock.NewController(t)
		mockOps := ops.NewMockOps(ctrl)
		mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
		pp := newGatePostPivot(t, mockOps)

		err := runClockGate(t, pp, &clusterconfig_api.SeedReconfiguration{}, &seedclusterinfo.SeedClusterInfo{})
		require.Error(t, err)
		require.True(t, c.called)
		assert.Equal(t, ibistatus.DecisionCertLowerBound, c.result.Clock.Decision)
	})

	t.Run("after lower bound proceeds", func(t *testing.T) {
		c := stubClockGateSeams(t)
		setNow(rfc3339("2025-06-01T00:00:00Z"))
		seedCertLowerBound = func(*logrus.Logger) (time.Time, bool) { return lower, true }

		ctrl := gomock.NewController(t)
		mockOps := ops.NewMockOps(ctrl)
		mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
		pp := newGatePostPivot(t, mockOps)

		assert.NoError(t, runClockGate(t, pp, &clusterconfig_api.SeedReconfiguration{}, &seedclusterinfo.SeedClusterInfo{}))
		assert.False(t, c.called)
	})

	t.Run("no bound derivable proceeds with warning", func(t *testing.T) {
		c := stubClockGateSeams(t)
		setNow(rfc3339("2000-01-01T00:00:00Z"))
		seedCertLowerBound = func(*logrus.Logger) (time.Time, bool) { return time.Time{}, false }

		ctrl := gomock.NewController(t)
		mockOps := ops.NewMockOps(ctrl)
		mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
		pp := newGatePostPivot(t, mockOps)

		assert.NoError(t, runClockGate(t, pp, &clusterconfig_api.SeedReconfiguration{}, &seedclusterinfo.SeedClusterInfo{}))
		assert.False(t, c.called)
	})
}

func TestChronyConfigWithMakestep(t *testing.T) {
	t.Run("appends makestep to a config without one", func(t *testing.T) {
		got := chronyConfigWithMakestep("server ntp.example.com iburst\nrtcsync\n")
		assert.Equal(t, "server ntp.example.com iburst\nrtcsync\nmakestep 1 -1\n", got)
	})
	t.Run("replaces an existing makestep directive", func(t *testing.T) {
		got := chronyConfigWithMakestep("server ntp.example.com iburst\nmakestep 1 3\n")
		assert.Equal(t, "server ntp.example.com iburst\nmakestep 1 -1\n", got)
	})
	t.Run("preserves comments and blank lines", func(t *testing.T) {
		got := chronyConfigWithMakestep("# keep this comment\n\nserver ntp.example.com iburst\n")
		assert.Equal(t, "# keep this comment\n\nserver ntp.example.com iburst\nmakestep 1 -1\n", got)
	})
	t.Run("empty config gets only makestep", func(t *testing.T) {
		assert.Equal(t, "makestep 1 -1\n", chronyConfigWithMakestep(""))
	})
}

func TestParseChronyReferenceID(t *testing.T) {
	testcases := []struct {
		name   string
		output string
		want   string
		wantOk bool
	}{
		{
			name:   "external source",
			output: "Reference ID    : 0A000005 (10.0.0.5)\nStratum         : 2",
			want:   "0A000005",
			wantOk: true,
		},
		{
			name:   "local mode no name",
			output: "Reference ID    : 7F7F0101 ()\nStratum         : 10",
			want:   "7F7F0101",
			wantOk: true,
		},
		{name: "no reference id line", output: "Stratum : 2\nLeap status : Normal", want: "", wantOk: false},
		{name: "empty", output: "", want: "", wantOk: false},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseChronyReferenceID(tc.output)
			assert.Equal(t, tc.wantOk, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestDefaultSeedCertLowerBound proves the lower-bound helper reads the earliest NotBefore from
// control-plane certs and reports ok=false when none can be read.
func TestDefaultSeedCertLowerBound(t *testing.T) {
	t.Run("no certs returns false", func(t *testing.T) {
		orig := controlPlaneCertFiles
		controlPlaneCertFiles = []string{filepath.Join(t.TempDir(), "missing.pem")}
		t.Cleanup(func() { controlPlaneCertFiles = orig })

		_, ok := defaultSeedCertLowerBound(logrus.New())
		assert.False(t, ok)
	})
}
