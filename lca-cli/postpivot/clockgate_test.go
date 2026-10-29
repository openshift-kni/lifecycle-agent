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

	assert.NoError(t, pp.clockGate(seedReconfig, seedClusterInfo))
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
	assert.NoError(t, pp.clockGate(&clusterconfig_api.SeedReconfiguration{}, seedClusterInfo))
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
	assert.NoError(t, pp.clockGate(&clusterconfig_api.SeedReconfiguration{}, seedClusterInfo))
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
	err := pp.clockGate(&clusterconfig_api.SeedReconfiguration{}, seedClusterInfo)
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
	err := pp.clockGate(&clusterconfig_api.SeedReconfiguration{}, seedClusterInfo)
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
	assert.NoError(t, pp.clockGate(seedReconfig, seedClusterInfo))
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
	err := pp.clockGate(seedReconfig, seedClusterInfo)
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
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{ChronyConfig: "server ntp.example.com iburst\n"}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	assert.NoError(t, pp.clockGate(seedReconfig, seedClusterInfo))

	// The config was written for the early bring-up.
	data, err := os.ReadFile(chronyConfigFilePath) //nolint:gosec // test path
	require.NoError(t, err)
	assert.Contains(t, string(data), "server ntp.example.com")
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
	mockOps.EXPECT().SystemctlAction("restart", "chronyd").Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chronyc", "waitsync", chronySyncMaxTries, chronySyncMaxCorrection, "0", "1").
		Return("", fmt.Errorf("timeout")).Times(1)
	pp := newGatePostPivot(t, mockOps)

	seedReconfig := &clusterconfig_api.SeedReconfiguration{ChronyConfig: "server ntp.example.com iburst\n"}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{SeedCreationTime: "2025-01-01T00:00:00Z"}
	err := pp.clockGate(seedReconfig, seedClusterInfo)
	require.Error(t, err)
	require.True(t, c.called)
	assert.Equal(t, ibistatus.DecisionRTCWindow, c.result.Clock.Decision)
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

		err := pp.clockGate(&clusterconfig_api.SeedReconfiguration{}, &seedclusterinfo.SeedClusterInfo{})
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

		assert.NoError(t, pp.clockGate(&clusterconfig_api.SeedReconfiguration{}, &seedclusterinfo.SeedClusterInfo{}))
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

		assert.NoError(t, pp.clockGate(&clusterconfig_api.SeedReconfiguration{}, &seedclusterinfo.SeedClusterInfo{}))
		assert.False(t, c.called)
	})
}

func TestChronyConfigHasTrustedSource(t *testing.T) {
	testcases := []struct {
		name   string
		config string
		want   bool
	}{
		{name: "empty", config: "", want: false},
		{name: "server external", config: "server ntp.example.com iburst", want: true},
		{name: "pool external", config: "pool pool.ntp.org iburst", want: true},
		{name: "peer external", config: "peer 10.0.0.5", want: true},
		{name: "hardware refclock", config: "refclock PHC /dev/ptp0 poll 0", want: true},
		{name: "local clock pseudo-ip", config: "server 127.127.1.0", want: false},
		{name: "loopback server", config: "server 127.0.0.1", want: false},
		{name: "local stratum fallback", config: "local stratum 10", want: false},
		{name: "rtcsync only", config: "rtcsync", want: false},
		{name: "comments ignored", config: "# server ntp.example.com\n! pool bad\nrtcsync", want: false},
		{name: "mixed local and real", config: "server 127.127.1.0\nserver ntp.example.com", want: true},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, chronyConfigHasTrustedSource(tc.config))
		})
	}
}

func TestResolveMaxSeedAge(t *testing.T) {
	testcases := []struct {
		name      string
		configure string
		want      time.Duration
		wantErr   bool
	}{
		{name: "empty uses default", configure: "", want: defaultMaxSeedAge},
		{name: "whitespace uses default", configure: "   ", want: defaultMaxSeedAge},
		{name: "valid duration", configure: "4320h", want: 4320 * time.Hour},
		{name: "not parseable", configure: "180d", wantErr: true},
		{name: "negative", configure: "-1h", wantErr: true},
		{name: "zero", configure: "0h", wantErr: true},
		{name: "overflow / absurd", configure: "200000h", wantErr: true},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveMaxSeedAge(tc.configure)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
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
