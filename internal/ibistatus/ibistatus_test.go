package ibistatus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

// statusDevice returns a block device list containing a single ibi-status ext4 partition.
func statusDevices(name string) []ops.BlockDevice {
	return []ops.BlockDevice{
		{Name: "/dev/other", Label: "root", FSType: "xfs"},
		{Name: name, Label: common.IBIStatusFSLabel, FSType: "ext4"},
	}
}

// useTempStatusPartition redirects the p4 mount point to a temp dir so the result write-back
// lands there instead of a real mount, and returns the directory.
func useTempStatusPartition(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig := statusMountDir
	statusMountDir = dir
	t.Cleanup(func() { statusMountDir = orig })
	return dir
}

func readReconfigureResult(t *testing.T, dir string) *ReconfigureResult {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, common.ReconfigureResultFileName)) //nolint:gosec // test path
	require.NoError(t, err)
	var r ReconfigureResult
	require.NoError(t, json.Unmarshal(data, &r))
	return &r
}

func TestStatusPartitionPresent(t *testing.T) {
	testcases := []struct {
		name    string
		devices []ops.BlockDevice
		listErr error
		want    bool
		wantErr bool
	}{
		{name: "present", devices: statusDevices("/dev/sdb4"), want: true},
		{name: "absent", devices: []ops.BlockDevice{{Name: "/dev/sda", Label: "root", FSType: "xfs"}}, want: false},
		{name: "list error", listErr: fmt.Errorf("boom"), wantErr: true},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			mockOps := ops.NewMockOps(ctrl)
			mockOps.EXPECT().ListBlockDevices().Return(tc.devices, tc.listErr).Times(1)

			got, err := StatusPartitionPresent(mockOps)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestWriteFailureAndHalt proves the failure path mounts p4, writes a well-formed durable
// reconfigure.json failure record, syncs, unmounts, and halts the node powered-on via the
// emergency target, returning the original cause.
func TestWriteFailureAndHalt(t *testing.T) {
	dir := useTempStatusPartition(t)

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("/dev/sdb4"), nil).Times(1)
	mockOps.EXPECT().Mount("/dev/sdb4", dir).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(1)
	mockOps.EXPECT().Umount("/dev/sdb4").Return(nil).Times(1)
	mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)

	cause := fmt.Errorf("implausible clock")
	result := &ReconfigureResult{
		Phase:       PhaseReconfigure,
		Profile:     "self-contained",
		Result:      ResultFailure,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		SeedVersion: "4.18.3",
		Clock:       &ClockGateResult{ObservedTime: "1970-01-01T00:00:00Z", Decision: DecisionRTCWindow},
	}
	msg := cause.Error()
	result.Error = &msg

	err := WriteFailureAndHalt(mockOps, logrus.New(), result, cause)
	assert.ErrorIs(t, err, cause)

	got := readReconfigureResult(t, dir)
	assert.Equal(t, PhaseReconfigure, got.Phase)
	assert.Equal(t, ResultFailure, got.Result)
	assert.Equal(t, "self-contained", got.Profile)
	assert.Equal(t, "4.18.3", got.SeedVersion)
	require.NotNil(t, got.Error)
	assert.Equal(t, "implausible clock", *got.Error)
	require.NotNil(t, got.Clock)
	assert.Equal(t, DecisionRTCWindow, got.Clock.Decision)
	_, perr := time.Parse(time.RFC3339, got.Timestamp)
	assert.NoError(t, perr)

	// 0600 and no temp file left behind.
	info, err := os.Stat(filepath.Join(dir, common.ReconfigureResultFileName))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

// TestWriteFailureAndHalt_MountFails_StillHalts proves that when p4 cannot be mounted (no
// labelled device) the failure cannot be recorded, but the node is still halted powered-on so
// recert never runs.
func TestWriteFailureAndHalt_MountFails_StillHalts(t *testing.T) {
	useTempStatusPartition(t)

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return([]ops.BlockDevice{{Name: "/dev/sda", Label: "root", FSType: "xfs"}}, nil).Times(1)
	// No Mount/write/sync/unmount; the node must still halt.
	mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)

	cause := fmt.Errorf("implausible clock")
	err := WriteFailureAndHalt(mockOps, logrus.New(), &ReconfigureResult{Result: ResultFailure}, cause)
	assert.ErrorIs(t, err, cause)
}

// TestWriteFailureAndHalt_WriteFails_StillHalts proves that when the durable write fails (the
// mount point does not exist) the node is still halted powered-on.
func TestWriteFailureAndHalt_WriteFails_StillHalts(t *testing.T) {
	// Point the mount dir at a path that does not exist so the durable write fails.
	orig := statusMountDir
	statusMountDir = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { statusMountDir = orig })

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("/dev/sdb4"), nil).Times(1)
	mockOps.EXPECT().Mount("/dev/sdb4", statusMountDir).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(1)
	mockOps.EXPECT().Umount("/dev/sdb4").Return(nil).Times(1)
	mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)

	cause := fmt.Errorf("implausible clock")
	err := WriteFailureAndHalt(mockOps, logrus.New(), &ReconfigureResult{Result: ResultFailure}, cause)
	assert.ErrorIs(t, err, cause)
}

// TestWriteFailureAndHalt_IsolateFails proves that when the emergency-target isolation itself
// fails, the returned error wraps the original cause so the post-pivot flow still aborts before
// recert.
func TestWriteFailureAndHalt_IsolateFails(t *testing.T) {
	dir := useTempStatusPartition(t)

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("/dev/sdb4"), nil).Times(1)
	mockOps.EXPECT().Mount("/dev/sdb4", dir).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(1)
	mockOps.EXPECT().Umount("/dev/sdb4").Return(nil).Times(1)
	mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", fmt.Errorf("isolate failed")).Times(1)

	cause := fmt.Errorf("implausible clock")
	err := WriteFailureAndHalt(mockOps, logrus.New(), &ReconfigureResult{Result: ResultFailure}, cause)
	assert.ErrorIs(t, err, cause)
	assert.Contains(t, err.Error(), "isolate failed")
}

// TestWriteFileDurableAtomic proves the durable write publishes the file atomically via a temp
// file and rename, leaves no temp file behind, and applies 0600.
func TestWriteFileDurableAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, common.ReconfigureResultFileName)

	require.NoError(t, writeFileDurable(path, []byte("hello\n")))
	got, err := os.ReadFile(path) //nolint:gosec // test path
	require.NoError(t, err)
	assert.Equal(t, "hello\n", string(got))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}
