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
	// The runtime-selected reference is a real external source (not local mode), and the
	// selected source matches the configured trusted server.
	mockOps.EXPECT().RunInHostNamespace("chronyc", "-n", "tracking").
		Return("Reference ID    : C000020A (192.0.2.10)\nStratum         : 2\nLeap status     : Normal", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "sourcename", "192.0.2.10").Return("192.0.2.10\n", nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{ChronyConfig: "server 192.0.2.10 iburst\n"}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	gateConfigActive, err := pp.clockGateWithChronyConfig(seedReconfig, seedClusterInfo)
	require.NoError(t, err)
	assert.True(t, gateConfigActive, "a synchronized trusted source must report the temporary config as active")

	// The config was written for the early bring-up, with the gate's transient makestep policy.
	data, err := os.ReadFile(chronyConfigFilePath) //nolint:gosec // test path
	require.NoError(t, err)
	assert.Contains(t, string(data), "server 192.0.2.10")
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

// TestClockGate_ChronySourceNameUnavailable_FallsBackToWindow proves that an older chronyc
// without sourcename support does not become an abort-to-trust path. The gate restores the
// original chrony configuration and safely falls back to the seed-age plausibility window.
func TestClockGate_ChronySourceNameUnavailable_FallsBackToWindow(t *testing.T) {
	c := stubClockGateSeams(t)
	setNow(rfc3339("1970-01-01T00:00:00Z"))

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	mockOps.EXPECT().SystemctlAction("restart", "chronyd").Return("", nil).Times(2)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "waitsync", chronySyncMaxTries, chronySyncMaxCorrection, "0", "1").Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "-n", "tracking").
		Return("Reference ID    : C000020A (192.0.2.10)\nStratum         : 2\nLeap status     : Normal", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "sourcename", "192.0.2.10").
		Return("", fmt.Errorf("unknown command")).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{ChronyConfig: "server ntp.example.com iburst\n"}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	err := runClockGate(t, pp, seedReconfig, seedClusterInfo)
	require.Error(t, err)
	require.True(t, c.called)
	require.NotNil(t, c.result.Clock)
	assert.Equal(t, ibistatus.DecisionRTCWindow, c.result.Clock.Decision)

	data, readErr := os.ReadFile(chronyConfigFilePath) //nolint:gosec // test path
	require.NoError(t, readErr)
	assert.Equal(t, seedReconfig.ChronyConfig, string(data))
}

// TestClockGate_SyncedRTCRefclock_FallsBackToWindow proves that RTC refclocks are not retained
// as trusted sources. Explicit RTC-refid collision rejection is covered separately in
// TestChronySelectedSourceIsTrusted.
func TestClockGate_SyncedRTCRefclock_FallsBackToWindow(t *testing.T) {
	c := stubClockGateSeams(t)
	setNow(rfc3339("1970-01-01T00:00:00Z"))

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	mockOps.EXPECT().SystemctlAction("restart", "chronyd").Return("", nil).Times(2)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "waitsync", chronySyncMaxTries, chronySyncMaxCorrection, "0", "1").Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "-n", "tracking").
		Return("Reference ID    : 52544330 (RTC0)\nStratum         : 1\nLeap status     : Normal", nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{
		ChronyConfig: "server 192.0.2.10 iburst\nrefclock RTC /dev/rtc0\n",
	}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	err := runClockGate(t, pp, seedReconfig, seedClusterInfo)
	require.Error(t, err)
	require.True(t, c.called)
	assert.Equal(t, ibistatus.DecisionRTCWindow, c.result.Clock.Decision)
}

// TestClockGate_UnlockedSOCKWithLocalFallback_FallsBackToWindow proves that an unlocked SOCK
// source cannot establish full-time trust merely from its configuration. NMEA-only sockets
// must fall back to the seed-age plausibility window.
func TestClockGate_UnlockedSOCKWithLocalFallback_FallsBackToWindow(t *testing.T) {
	c := stubClockGateSeams(t)
	setNow(rfc3339("1970-01-01T00:00:00Z"))

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{
		ChronyConfig: "refclock SOCK /var/run/chrony.ttyS0.sock\nlocal stratum 10\n",
	}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	err := runClockGate(t, pp, seedReconfig, seedClusterInfo)
	require.Error(t, err)
	require.True(t, c.called)
	assert.Equal(t, ibistatus.DecisionRTCWindow, c.result.Clock.Decision)
}

// TestClockGate_LocalRefclockWithLocalFallback_FallsBackToWindow proves that a refclock
// marked as an unsynchronised local standard cannot satisfy the trusted-source predicate.
func TestClockGate_LocalRefclockWithLocalFallback_FallsBackToWindow(t *testing.T) {
	c := stubClockGateSeams(t)
	setNow(rfc3339("1970-01-01T00:00:00Z"))

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{
		ChronyConfig: "refclock PHC /dev/ptp0 local\nlocal stratum 10\n",
	}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	err := runClockGate(t, pp, seedReconfig, seedClusterInfo)
	require.Error(t, err)
	require.True(t, c.called)
	assert.Equal(t, ibistatus.DecisionRTCWindow, c.result.Clock.Decision)
}

// TestClockGate_LockedSOCKToUntrustedRefclock_FallsBackToWindow proves that a lock option does
// not make a SOCK source trustworthy when its target is an RTC-backed refclock.
func TestClockGate_LockedSOCKToUntrustedRefclock_FallsBackToWindow(t *testing.T) {
	c := stubClockGateSeams(t)
	setNow(rfc3339("1970-01-01T00:00:00Z"))

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{
		ChronyConfig: "refclock RTC /dev/rtc0 refid BAD\nrefclock SOCK /var/run/chrony.sock lock BAD\nlocal stratum 10\n",
	}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	err := runClockGate(t, pp, seedReconfig, seedClusterInfo)
	require.Error(t, err)
	require.True(t, c.called)
	assert.Equal(t, ibistatus.DecisionRTCWindow, c.result.Clock.Decision)
}

// TestClockGate_UntrustedSourceStepCannotPoisonFallbackWindow proves that the seed-age window
// uses the clock observed before the temporary makestep policy. An untrusted source may step
// the runtime clock into the plausible window, but it cannot make the original implausible
// observation pass.
func TestClockGate_UntrustedSourceStepCannotPoisonFallbackWindow(t *testing.T) {
	c := stubClockGateSeams(t)
	clockStepped := false
	nowUTC = func() time.Time {
		if clockStepped {
			return rfc3339("2025-06-01T00:00:00Z")
		}
		return rfc3339("1970-01-01T00:00:00Z")
	}

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	mockOps.EXPECT().SystemctlAction("restart", "chronyd").Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "waitsync", chronySyncMaxTries, chronySyncMaxCorrection, "0", "1").
		DoAndReturn(func(string, ...string) (string, error) {
			clockStepped = true
			return "", nil
		}).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "-n", "tracking").
		Return("Reference ID    : 52544330 (RTC0)\nStratum         : 1\nLeap status     : Normal", nil).Times(1)
	mockOps.EXPECT().SystemctlAction("restart", "chronyd").Return("", nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{
		ChronyConfig: "server ntp.example.com iburst\nrefclock RTC /dev/rtc0\n",
	}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	err := runClockGate(t, pp, seedReconfig, seedClusterInfo)
	require.Error(t, err)
	require.True(t, c.called)
	require.NotNil(t, c.result.Clock)
	assert.Equal(t, ibistatus.DecisionRTCWindow, c.result.Clock.Decision)
	assert.Equal(t, "1970-01-01T00:00:00Z", c.result.Clock.ObservedTime)
}

// TestClockGate_IncludedRefclockDefaultID_FallsBackToWindow proves that an inline default
// refclock ID is not trusted when included configuration files may shift Chrony's global
// refclock numbering. The gate must use the seed-age plausibility window.
func TestClockGate_IncludedRefclockDefaultID_FallsBackToWindow(t *testing.T) {
	c := stubClockGateSeams(t)
	setNow(rfc3339("1970-01-01T00:00:00Z"))

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{
		ChronyConfig: "include /etc/chrony.d/*.conf\nrefclock PHC /dev/ptp0\nlocal stratum 10\n",
	}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	err := runClockGate(t, pp, seedReconfig, seedClusterInfo)
	require.Error(t, err)
	require.True(t, c.called)
	assert.Equal(t, ibistatus.DecisionRTCWindow, c.result.Clock.Decision)
}

// TestClockGate_PPSModePHCWithLocalFallback_FallsBackToWindow proves a PHC configured for
// external PPS cannot establish full-time trust, even with local fallback available. The
// gate must use the seed-age plausibility window instead of treating the clock as
// authoritative.
func TestClockGate_PPSModePHCWithLocalFallback_FallsBackToWindow(t *testing.T) {
	c := stubClockGateSeams(t)
	setNow(rfc3339("1970-01-01T00:00:00Z"))

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{
		ChronyConfig: "refclock PHC /dev/ptp2:extpps:pin=1 width 0.2 poll 2\nlocal stratum 10\n",
	}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	err := runClockGate(t, pp, seedReconfig, seedClusterInfo)
	require.Error(t, err)
	require.True(t, c.called)
	assert.Equal(t, ibistatus.DecisionRTCWindow, c.result.Clock.Decision)
}

// TestClockGate_HardwareRefclockSelectedSource_Proceeds proves a trusted hardware refclock is
// accepted directly from the tracking snapshot without a source-name lookup.
func TestClockGate_HardwareRefclockSelectedSource_Proceeds(t *testing.T) {
	stubClockGateSeams(t)
	setNow(rfc3339("1970-01-01T00:00:00Z"))

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	mockOps.EXPECT().SystemctlAction("restart", "chronyd").Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "waitsync", chronySyncMaxTries, chronySyncMaxCorrection, "0", "1").Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "-n", "tracking").
		Return("Reference ID    : 50484330 (PHC0)\nStratum         : 0\nLeap status     : Normal", nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{ChronyConfig: "refclock PHC /dev/ptp0\n"}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	assert.NoError(t, runClockGate(t, pp, seedReconfig, seedClusterInfo))
}

// TestClockGate_HostnameResolvedToLoopback_FallsBackToWindow proves that a configured hostname
// which chronyd actually resolved to loopback is not treated as a trusted external source,
// even though the original configured name is preserved in `chronyc -N` output.
func TestClockGate_HostnameResolvedToLoopback_FallsBackToWindow(t *testing.T) {
	c := stubClockGateSeams(t)
	setNow(rfc3339("1970-01-01T00:00:00Z"))

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	mockOps.EXPECT().SystemctlAction("restart", "chronyd").Return("", nil).Times(2)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "waitsync", chronySyncMaxTries, chronySyncMaxCorrection, "0", "1").Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "-n", "tracking").
		Return("Reference ID    : 7F000001 (127.0.0.1)\nStratum         : 2\nLeap status     : Normal", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "sourcename", "127.0.0.1").Return("ntp.example.com\n", nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{ChronyConfig: "server ntp.example.com iburst\n"}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	err := runClockGate(t, pp, seedReconfig, seedClusterInfo)
	require.Error(t, err)
	require.True(t, c.called)
	assert.Equal(t, ibistatus.DecisionRTCWindow, c.result.Clock.Decision)
}

// TestClockGate_HostnameSelectedSource_Proceeds proves a configured hostname is accepted using
// chronyd's original source name and numeric selection, without a fresh DNS lookup whose answer
// may have rotated since chronyd resolved the source.
func TestClockGate_HostnameSelectedSource_Proceeds(t *testing.T) {
	stubClockGateSeams(t)
	setNow(rfc3339("1970-01-01T00:00:00Z"))

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	mockOps.EXPECT().SystemctlAction("restart", "chronyd").Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "waitsync", chronySyncMaxTries, chronySyncMaxCorrection, "0", "1").Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "-n", "tracking").
		Return("Reference ID    : C000020A (192.0.2.10)\nStratum         : 2\nLeap status     : Normal", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "sourcename", "192.0.2.10").Return("ntp.example.com\n", nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{ChronyConfig: "server ntp.example.com iburst\n"}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	assert.NoError(t, runClockGate(t, pp, seedReconfig, seedClusterInfo))
}

// TestClockGate_PoolMemberSelectedSource_Proceeds proves a pool member is accepted by matching
// the pool's original configured name; the numeric member address need not be re-resolved.
func TestClockGate_PoolMemberSelectedSource_Proceeds(t *testing.T) {
	stubClockGateSeams(t)
	setNow(rfc3339("1970-01-01T00:00:00Z"))

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(p4Present(), nil).Times(1)
	mockOps.EXPECT().SystemctlAction("restart", "chronyd").Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "waitsync", chronySyncMaxTries, chronySyncMaxCorrection, "0", "1").Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "-n", "tracking").
		Return("Reference ID    : C6336414 (198.51.100.20)\nStratum         : 2\nLeap status     : Normal", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "sourcename", "198.51.100.20").Return("pool.example.com\n", nil).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{ChronyConfig: "pool pool.example.com iburst\n"}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	assert.NoError(t, runClockGate(t, pp, seedReconfig, seedClusterInfo))
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

// TestParseChronyTrackingSource proves that the selected numeric address or refclock
// identifier is extracted from the same tracking snapshot used for the local-mode check.
func TestParseChronyTrackingSource(t *testing.T) {
	testcases := []struct {
		name   string
		output string
		want   string
		wantOk bool
	}{
		{
			name:   "numeric IPv4 source",
			output: "Reference ID    : C000020A (192.0.2.10)\nStratum         : 2",
			want:   "192.0.2.10",
			wantOk: true,
		},
		{
			name:   "numeric IPv6 source",
			output: "Reference ID    : 5B1C9A93 (2001:db8::1)\nStratum         : 2",
			want:   "2001:db8::1",
			wantOk: true,
		},
		{
			name:   "refclock identifier",
			output: "Reference ID    : 50484330 (PHC0)\nStratum         : 0",
			want:   "PHC0",
			wantOk: true,
		},
		{name: "empty local-mode source", output: "Reference ID    : 7F7F0101 ()\nStratum         : 10", wantOk: false},
		{name: "no reference id line", output: "Stratum         : 2\nLeap status     : Normal", wantOk: false},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseChronyTrackingSource(tc.output)
			assert.Equal(t, tc.wantOk, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestParseChronySourceName proves that only a single non-empty source-name token is accepted.
func TestParseChronySourceName(t *testing.T) {
	got, ok := parseChronySourceName("ntp.example.com\n")
	assert.True(t, ok)
	assert.Equal(t, "ntp.example.com", got)

	_, ok = parseChronySourceName("")
	assert.False(t, ok)
	_, ok = parseChronySourceName("unexpected extra output\n")
	assert.False(t, ok)
}

// TestConfirmChronyReferenceTrusted_ErrorPaths proves that every selected-source verification
// failure fails closed with a descriptive error rather than treating the source as trusted.
func TestConfirmChronyReferenceTrusted_ErrorPaths(t *testing.T) {
	testcases := []struct {
		name              string
		trackingOutput    string
		trackingErr       error
		expectSourcename  bool
		sourcenameOutput  string
		sourcenameErr     error
		wantErrorContains string
	}{
		{
			name:              "tracking command fails",
			trackingErr:       fmt.Errorf("connection refused"),
			wantErrorContains: "failed to read chrony tracking state",
		},
		{
			name:              "reference id is missing",
			trackingOutput:    "Stratum         : 2\nLeap status     : Normal",
			wantErrorContains: "could not determine the synchronized chrony reference",
		},
		{
			name:              "selected source is missing",
			trackingOutput:    "Reference ID    : C000020A ()\nStratum         : 2\nLeap status     : Normal",
			wantErrorContains: "could not determine the selected chrony source",
		},
		{
			name:              "source name command fails",
			trackingOutput:    "Reference ID    : C000020A (192.0.2.10)\nStratum         : 2\nLeap status     : Normal",
			expectSourcename:  true,
			sourcenameErr:     fmt.Errorf("unknown command"),
			wantErrorContains: "failed to read the configured name",
		},
		{
			name:              "source name output is malformed",
			trackingOutput:    "Reference ID    : C000020A (192.0.2.10)\nStratum         : 2\nLeap status     : Normal",
			expectSourcename:  true,
			sourcenameOutput:  "unexpected extra output\n",
			wantErrorContains: "could not determine the configured name",
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			mockOps := ops.NewMockOps(ctrl)
			mockOps.EXPECT().RunInHostNamespace("chronyc", "-n", "tracking").
				Return(tc.trackingOutput, tc.trackingErr).Times(1)
			if tc.expectSourcename {
				mockOps.EXPECT().RunInHostNamespace("chronyc", "sourcename", "192.0.2.10").
					Return(tc.sourcenameOutput, tc.sourcenameErr).Times(1)
			}
			pp := newGatePostPivot(t, mockOps)

			err := pp.confirmChronyReferenceTrusted("server 192.0.2.10 iburst\n")
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.wantErrorContains)
		})
	}
}

// chronySelectionIsTrusted is a test helper that invokes chronySelectedSourceIsTrusted with
// no configured RTC refclock identifiers.
func chronySelectionIsTrusted(selectedName, selectedAddress string, trusted []clusterconfig_api.ChronyTrustedSource) bool {
	return chronySelectedSourceIsTrusted(selectedName, selectedAddress, trusted, nil)
}

// TestChronySelectedSourceIsTrusted proves that configured names, literal IPs, pools, and
// refclock identifiers are matched without trusting loopback or unrelated selections.
func TestChronySelectedSourceIsTrusted(t *testing.T) {
	t.Run("literal IP matches", func(t *testing.T) {
		assert.True(t, chronySelectionIsTrusted("192.0.2.10", "192.0.2.10", []clusterconfig_api.ChronyTrustedSource{{Kind: "server", Value: "192.0.2.10"}}))
	})
	t.Run("configured hostname matches original source name", func(t *testing.T) {
		assert.True(t, chronySelectionIsTrusted("ntp.example.com", "192.0.2.10", []clusterconfig_api.ChronyTrustedSource{{Kind: "server", Value: "ntp.example.com"}}))
	})
	t.Run("configured hostname match is case-insensitive", func(t *testing.T) {
		assert.True(t, chronySelectionIsTrusted("NTP.Example.COM", "192.0.2.10", []clusterconfig_api.ChronyTrustedSource{{Kind: "server", Value: "ntp.example.com"}}))
	})
	t.Run("dynamic pool member matches original pool name", func(t *testing.T) {
		assert.True(t, chronySelectionIsTrusted("pool.example.com", "198.51.100.20", []clusterconfig_api.ChronyTrustedSource{{Kind: "pool", Value: "pool.example.com"}}))
	})
	t.Run("refclock driver and unit match", func(t *testing.T) {
		assert.True(t, chronySelectionIsTrusted("PHC0", "PHC0", []clusterconfig_api.ChronyTrustedSource{{Kind: "refclock", Value: "PHC0"}}))
	})
	t.Run("custom refclock refid matches", func(t *testing.T) {
		assert.True(t, chronySelectionIsTrusted("PTP", "PTP", []clusterconfig_api.ChronyTrustedSource{{Kind: "refclock", Value: "PTP", ExplicitRefID: true}}))
	})
	t.Run("explicit refclock refid matches exactly", func(t *testing.T) {
		assert.True(t, chronySelectionIsTrusted("RTC", "RTC", []clusterconfig_api.ChronyTrustedSource{{Kind: "refclock", Value: "RTC", ExplicitRefID: true}}))
	})
	t.Run("explicit refclock refid does not accept a suffix", func(t *testing.T) {
		assert.False(t, chronySelectionIsTrusted("RTC0", "RTC0", []clusterconfig_api.ChronyTrustedSource{{Kind: "refclock", Value: "RTC", ExplicitRefID: true}}))
	})
	t.Run("RTC is not trusted", func(t *testing.T) {
		assert.False(t, chronySelectionIsTrusted("RTC0", "RTC0", []clusterconfig_api.ChronyTrustedSource{{Kind: "server", Value: "192.0.2.10"}}))
	})
	t.Run("refclock identifier does not match a network hostname", func(t *testing.T) {
		assert.False(t, chronySelectionIsTrusted("RTC0", "RTC0", []clusterconfig_api.ChronyTrustedSource{{Kind: "server", Value: "rtc0"}}))
	})
	t.Run("loopback address is not trusted", func(t *testing.T) {
		assert.False(t, chronySelectionIsTrusted("ntp.example.com", "127.0.0.1", []clusterconfig_api.ChronyTrustedSource{{Kind: "server", Value: "ntp.example.com"}}))
	})
	t.Run("unrelated source is not trusted", func(t *testing.T) {
		assert.False(t, chronySelectionIsTrusted("peer.example.com", "198.51.100.20", []clusterconfig_api.ChronyTrustedSource{{Kind: "server", Value: "192.0.2.10"}}))
	})
	t.Run("pool does not accept an unconfigured peer", func(t *testing.T) {
		assert.False(t, chronySelectionIsTrusted("peer.example.com", "198.51.100.20", []clusterconfig_api.ChronyTrustedSource{{Kind: "pool", Value: "pool.example.com"}}))
	})
	t.Run("network source is not rejected by a colliding RTC refid", func(t *testing.T) {
		assert.True(t, chronySelectedSourceIsTrusted("PHC1", "192.0.2.10",
			[]clusterconfig_api.ChronyTrustedSource{{Kind: "server", Value: "PHC1"}},
			[]clusterconfig_api.ChronyRefclockRefID{{Value: "PHC1", ExplicitRefID: true}}))
	})
	t.Run("network source does not match a trusted refclock identifier", func(t *testing.T) {
		assert.False(t, chronySelectedSourceIsTrusted("PHC1", "192.0.2.10",
			[]clusterconfig_api.ChronyTrustedSource{{Kind: "refclock", Value: "PHC1"}}, nil))
	})
	t.Run("default refclock identifier comparison is case-sensitive", func(t *testing.T) {
		assert.False(t, chronySelectionIsTrusted("phc0", "phc0",
			[]clusterconfig_api.ChronyTrustedSource{{Kind: "refclock", Value: "PHC0"}}))
	})
	t.Run("explicit refclock refid comparison is case-sensitive", func(t *testing.T) {
		assert.False(t, chronySelectionIsTrusted("ptp", "ptp",
			[]clusterconfig_api.ChronyTrustedSource{{Kind: "refclock", Value: "PTP", ExplicitRefID: true}}))
	})
	t.Run("explicit RTC refid rejection is case-sensitive", func(t *testing.T) {
		assert.True(t, chronySelectedSourceIsTrusted("PHC1", "PHC1",
			[]clusterconfig_api.ChronyTrustedSource{{Kind: "refclock", Value: "PHC1", ExplicitRefID: true}},
			[]clusterconfig_api.ChronyRefclockRefID{{Value: "phc1", ExplicitRefID: true}}))
	})
	t.Run("excluded refclock does not match a retained default identifier", func(t *testing.T) {
		trusted := clusterconfig_api.ChronyTrustedSources(
			"refclock PHC /dev/ptp0 pps\nrefclock PHC /dev/ptp1\n")
		require.Equal(t, []clusterconfig_api.ChronyTrustedSource{
			{Kind: "refclock", Value: "PHC1"},
		}, trusted)
		assert.False(t, chronySelectedSourceIsTrusted("PHC0", "PHC0", trusted, nil))
	})
	t.Run("explicit RTC refid collision is rejected", func(t *testing.T) {
		assert.False(t, chronySelectedSourceIsTrusted("PHC1", "PHC1",
			[]clusterconfig_api.ChronyTrustedSource{{Kind: "refclock", Value: "PHC1"}},
			[]clusterconfig_api.ChronyRefclockRefID{{Value: "PHC1", ExplicitRefID: true}}))
	})
	t.Run("default RTC identifier is rejected", func(t *testing.T) {
		assert.False(t, chronySelectedSourceIsTrusted("RTC0", "RTC0",
			[]clusterconfig_api.ChronyTrustedSource{{Kind: "refclock", Value: "PHC1"}},
			[]clusterconfig_api.ChronyRefclockRefID{{Value: "RTC0"}}))
	})

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
