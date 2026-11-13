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

// readReconfigureResult loads and unmarshals the reconfigure result from dir.
func readReconfigureResult(t *testing.T, dir string) *ReconfigureResult {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, common.ReconfigureResultFileName)) //nolint:gosec // test path
	require.NoError(t, err)
	var r ReconfigureResult
	require.NoError(t, json.Unmarshal(data, &r))
	return &r
}

// seedReconfigureResult writes a pre-existing reconfigure result into dir so the idempotence
// guard has something to find.
func seedReconfigureResult(t *testing.T, dir string, r *ReconfigureResult) {
	t.Helper()
	data, err := json.MarshalIndent(r, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, common.ReconfigureResultFileName), data, 0o600))
}

// TestStatusPartitionPresent proves that presence detection cleanly distinguishes absence
// from an ambiguous or invalid ibi-status partition.
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
		{
			name: "multiple labelled devices are invalid",
			devices: []ops.BlockDevice{
				{Name: "/dev/sdb4", Label: common.IBIStatusFSLabel, FSType: "ext4"},
				{Name: "/dev/sdc4", Label: common.IBIStatusFSLabel, FSType: "ext4"},
			},
			wantErr: true,
		},
		{
			name: "non-ext4 labelled device is invalid",
			devices: []ops.BlockDevice{
				{Name: "/dev/sdb4", Label: common.IBIStatusFSLabel, FSType: "vfat"},
			},
			wantErr: true,
		},
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

// TestMountStatusPartitionAbsent proves the mount path reports a clean absence as an explicit
// missing-label error rather than treating it as an ambiguous zero-match count.
func TestMountStatusPartitionAbsent(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return([]ops.BlockDevice{{Name: "/dev/sda", Label: "root", FSType: "xfs"}}, nil).Times(1)

	_, err := mountStatusPartition(mockOps, false)
	require.Error(t, err)
	assert.Equal(t, fmt.Sprintf("no block device labelled %q was found", common.IBIStatusFSLabel), err.Error())
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
	// The durable write fails before the sync step, so no sync is attempted; the mount is
	// still unmounted and the node still halts.
	mockOps.EXPECT().Umount("/dev/sdb4").Return(nil).Times(1)
	mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)

	cause := fmt.Errorf("implausible clock")
	err := WriteFailureAndHalt(mockOps, logrus.New(), &ReconfigureResult{Result: ResultFailure}, cause)
	assert.ErrorIs(t, err, cause)
}

// TestWriteFailureAndHalt_GuardNoOp_StillHalts proves that when a terminal verdict already
// exists on p4 the idempotence guard suppresses the p4 write, yet the node is still halted
// powered-on -- the guard never suppresses the terminal action.
func TestWriteFailureAndHalt_GuardNoOp_StillHalts(t *testing.T) {
	dir := useTempStatusPartition(t)
	// Seed an existing terminal success record so the guard no-ops the write.
	seedReconfigureResult(t, dir, &ReconfigureResult{Phase: PhaseReconfigure, Result: ResultSuccess})

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("/dev/sdb4"), nil).Times(1)
	mockOps.EXPECT().Mount("/dev/sdb4", dir).Return(nil).Times(1)
	// No writeResult/sync: the guard skipped the write. The mount is still unmounted and the
	// node still halts.
	mockOps.EXPECT().Umount("/dev/sdb4").Return(nil).Times(1)
	mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)

	cause := fmt.Errorf("implausible clock")
	err := WriteFailureAndHalt(mockOps, logrus.New(), &ReconfigureResult{Result: ResultFailure}, cause)
	assert.ErrorIs(t, err, cause)

	// The pre-existing success verdict is preserved untouched.
	got := readReconfigureResult(t, dir)
	assert.Equal(t, ResultSuccess, got.Result)
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

// TestWriteSuccess proves the success path mounts p4 read-write, writes a well-formed durable
// reconfigure.json success record, syncs, and unmounts -- with no halt.
func TestWriteSuccess(t *testing.T) {
	dir := useTempStatusPartition(t)

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("/dev/sdb4"), nil).Times(1)
	mockOps.EXPECT().Mount("/dev/sdb4", dir).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(1)
	mockOps.EXPECT().Umount("/dev/sdb4").Return(nil).Times(1)
	// No isolate: WriteSuccess never halts.

	result := &ReconfigureResult{
		Phase:       PhaseReconfigure,
		Profile:     "self-contained",
		Result:      ResultSuccess,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		SeedVersion: "4.18.3",
	}
	require.NoError(t, WriteSuccess(mockOps, logrus.New(), result))

	got := readReconfigureResult(t, dir)
	assert.Equal(t, ResultSuccess, got.Result)
	assert.Equal(t, PhaseReconfigure, got.Phase)
}

// TestWriteFailure proves the write-only failure path writes the record and does not halt.
func TestWriteFailure(t *testing.T) {
	dir := useTempStatusPartition(t)

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("/dev/sdb4"), nil).Times(1)
	mockOps.EXPECT().Mount("/dev/sdb4", dir).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(1)
	mockOps.EXPECT().Umount("/dev/sdb4").Return(nil).Times(1)
	// No isolate: WriteFailure never halts.

	msg := "convergence not reached"
	require.NoError(t, WriteFailure(mockOps, logrus.New(), &ReconfigureResult{
		Phase: PhaseReconfigure, Result: ResultFailure, Error: &msg,
	}))

	got := readReconfigureResult(t, dir)
	assert.Equal(t, ResultFailure, got.Result)
}

// TestReadReconfigureResult proves the reader returns the parsed record when present, a clean
// nil for an absent file, and a nil (treated as no verdict) for a malformed file.
func TestReadReconfigureResult(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		dir := useTempStatusPartition(t)
		seedReconfigureResult(t, dir, &ReconfigureResult{Phase: PhaseReconfigure, Result: ResultSuccess})

		ctrl := gomock.NewController(t)
		mockOps := ops.NewMockOps(ctrl)
		mockOps.EXPECT().ListBlockDevices().Return(statusDevices("/dev/sdb4"), nil).Times(1)
		mockOps.EXPECT().MountReadOnly("/dev/sdb4", dir).Return(nil).Times(1)
		mockOps.EXPECT().Umount("/dev/sdb4").Return(nil).Times(1)

		got, err := ReadReconfigureResult(mockOps, logrus.New())
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, ResultSuccess, got.Result)
		assert.True(t, got.IsTerminal())
	})

	t.Run("absent", func(t *testing.T) {
		dir := useTempStatusPartition(t)

		ctrl := gomock.NewController(t)
		mockOps := ops.NewMockOps(ctrl)
		mockOps.EXPECT().ListBlockDevices().Return(statusDevices("/dev/sdb4"), nil).Times(1)
		mockOps.EXPECT().MountReadOnly("/dev/sdb4", dir).Return(nil).Times(1)
		mockOps.EXPECT().Umount("/dev/sdb4").Return(nil).Times(1)

		got, err := ReadReconfigureResult(mockOps, logrus.New())
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("malformed is treated as no verdict", func(t *testing.T) {
		dir := useTempStatusPartition(t)
		require.NoError(t, os.WriteFile(filepath.Join(dir, common.ReconfigureResultFileName), []byte("{not json"), 0o600))

		ctrl := gomock.NewController(t)
		mockOps := ops.NewMockOps(ctrl)
		mockOps.EXPECT().ListBlockDevices().Return(statusDevices("/dev/sdb4"), nil).Times(1)
		mockOps.EXPECT().MountReadOnly("/dev/sdb4", dir).Return(nil).Times(1)
		mockOps.EXPECT().Umount("/dev/sdb4").Return(nil).Times(1)

		got, err := ReadReconfigureResult(mockOps, logrus.New())
		require.NoError(t, err)
		assert.Nil(t, got)
	})
}

// TestWriters_Idempotent proves the write-only guard never overwrites an existing terminal
// verdict (neither success nor failure), for both writers.
func TestWriters_Idempotent(t *testing.T) {
	testcases := []struct {
		name     string
		existing string
		write    func(ops.Ops, *logrus.Logger, *ReconfigureResult) error
		attempt  string
	}{
		{name: "success does not overwrite failure", existing: ResultFailure, write: WriteSuccess, attempt: ResultSuccess},
		{name: "failure does not overwrite success", existing: ResultSuccess, write: WriteFailure, attempt: ResultFailure},
		{name: "success does not overwrite success", existing: ResultSuccess, write: WriteSuccess, attempt: ResultSuccess},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			dir := useTempStatusPartition(t)
			seedReconfigureResult(t, dir, &ReconfigureResult{Phase: PhaseReconfigure, Result: tc.existing, SeedVersion: "original"})

			ctrl := gomock.NewController(t)
			mockOps := ops.NewMockOps(ctrl)
			mockOps.EXPECT().ListBlockDevices().Return(statusDevices("/dev/sdb4"), nil).Times(1)
			mockOps.EXPECT().Mount("/dev/sdb4", dir).Return(nil).Times(1)
			mockOps.EXPECT().Umount("/dev/sdb4").Return(nil).Times(1)
			// No write/sync: the guard skipped the p4 write.

			require.NoError(t, tc.write(mockOps, logrus.New(), &ReconfigureResult{
				Phase: PhaseReconfigure, Result: tc.attempt, SeedVersion: "overwrite-attempt",
			}))

			// The original record is preserved untouched.
			got := readReconfigureResult(t, dir)
			assert.Equal(t, tc.existing, got.Result)
			assert.Equal(t, "original", got.SeedVersion)
		})
	}
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
