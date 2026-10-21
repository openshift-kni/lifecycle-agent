package ibi_preparation

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	preinstallUtils "github.com/rh-ecosystem-edge/preinstall-utils/pkg"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"github.com/openshift-kni/lifecycle-agent/api/ibiconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

// TestWriteFileDurableAtomic proves the durable write publishes the file atomically via a
// temp file and rename, leaves no temp file behind, applies 0600, and atomically replaces
// an existing file on a subsequent write. This is the sequence the result write-back uses
// to make prepare.json durable before the terminal action.
func TestWriteFileDurableAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, common.PrepareResultFileName)

	assert.NoError(t, writeFileDurable(path, []byte("hello\n")))

	got, err := os.ReadFile(path) //nolint:gosec // test path
	assert.NoError(t, err)
	assert.Equal(t, "hello\n", string(got))

	info, err := os.Stat(path)
	assert.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// No temp file left behind.
	entries, err := os.ReadDir(dir)
	assert.NoError(t, err)
	assert.Len(t, entries, 1)
	assert.Equal(t, common.PrepareResultFileName, entries[0].Name())

	// A second write atomically replaces the content without leaking temp files.
	assert.NoError(t, writeFileDurable(path, []byte("world\n")))
	got, err = os.ReadFile(path) //nolint:gosec // test path
	assert.NoError(t, err)
	assert.Equal(t, "world\n", string(got))
	entries, err = os.ReadDir(dir)
	assert.NoError(t, err)
	assert.Len(t, entries, 1)
}

// TestBuildResultSuccessContent proves a successful prepare records the documented schema:
// phase, profile, success result, seed version, full image counts, a parseable timestamp,
// no error, and reentry: none.
func TestBuildResultSuccessContent(t *testing.T) {
	ibi := &IBIPrepare{log: logrus.New(), config: &ibiconfig.IBIPrepareConfig{
		Profile: ibiconfig.ProfileInstallOnly, SeedVersion: "4.18.3",
	}}

	r := ibi.buildResult(188, nil)

	assert.Equal(t, prepareResultPhase, r.Phase)
	assert.Equal(t, ibiconfig.ProfileInstallOnly, r.Profile)
	assert.Equal(t, resultSuccess, r.Result)
	assert.Equal(t, "4.18.3", r.SeedVersion)
	assert.Equal(t, reentryNone, r.Reentry)
	assert.Nil(t, r.Error)
	if assert.NotNil(t, r.Checks) {
		assert.Equal(t, 188, r.Checks.ImagesExpected)
		assert.Equal(t, 188, r.Checks.ImagesPresent)
	}
	_, err := time.Parse(time.RFC3339, r.Timestamp)
	assert.NoError(t, err, "timestamp must be RFC3339")
}

// TestBuildResultFailureContent proves a failed prepare records result: failure with the
// error message and omits the image counts (presence is unknown on failure).
func TestBuildResultFailureContent(t *testing.T) {
	ibi := &IBIPrepare{log: logrus.New(), config: &ibiconfig.IBIPrepareConfig{
		Profile: ibiconfig.ProfileSelfContained, SeedVersion: "4.18.3",
	}}

	r := ibi.buildResult(188, fmt.Errorf("boom"))

	assert.Equal(t, resultFailure, r.Result)
	assert.Equal(t, ibiconfig.ProfileSelfContained, r.Profile)
	assert.Nil(t, r.Checks, "image counts are omitted on failure")
	if assert.NotNil(t, r.Error) {
		assert.Equal(t, "boom", *r.Error)
	}
}

// TestFailureHaltsPoweredOn proves a core-prepare failure (here coreos-installer) writes a
// failure result to p4 and halts the node powered-on via the emergency target, never
// powering off.
func TestFailureHaltsPoweredOn(t *testing.T) {
	stubDisconnectedSeams(t)
	statusDir := useTempStatusPartition(t)

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
	cfg := disconnectedCfg(ibiconfig.ProfileInstallOnly)
	cfg.Shutdown = true // even with Shutdown set, a failure must halt powered-on
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, cleanupMock, cfg)

	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("sdb1", "sdb2"), nil).Times(2)
	mockOps.EXPECT().MountReadOnly("sdb1", cfg.LocalImagesPath).Return(nil).Times(1)
	mockOps.EXPECT().Mount("sdb2", statusDir).Return(nil).Times(1)
	cleanupMock.EXPECT().CleanupInstallDevice(cfg.InstallationDisk).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("coreos-installer", []string{"install", cfg.InstallationDisk}).
		Return("", fmt.Errorf("install failed")).Times(1)
	// Failure result write-back syncs, both partitions unmount, node halts powered-on.
	mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(1)
	mockOps.EXPECT().Umount("sdb1").Return(nil).Times(1)
	mockOps.EXPECT().Umount("sdb2").Return(nil).Times(1)
	mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

	err := ibi.Run()
	assert.Error(t, err)

	result := readPrepareResult(t, statusDir)
	assert.Equal(t, resultFailure, result.Result)
	assert.Equal(t, reentryNone, result.Reentry)
	assert.NotNil(t, result.Error)
}

// TestSuccessWriteFailureHaltsPoweredOn proves that when the core prepare succeeds but the
// required result record cannot be committed, the node halts powered-on instead of
// signalling success via power-off.
func TestSuccessWriteFailureHaltsPoweredOn(t *testing.T) {
	saveSeams(t)
	collectStatusLogs = func(*IBIPrepare, *statusPartition) error { return nil }

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	cfg := &ibiconfig.IBIPrepareConfig{Profile: ibiconfig.ProfileInstallOnly, SeedVersion: "4.15.0", Shutdown: true}
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, nil, cfg)

	// A mount dir that does not exist makes the result write fail (CreateTemp fails) even
	// though the core prepare (prepErr == nil) succeeded.
	unmounted := false
	sp := &statusPartition{
		deviceName: "sdb3",
		mountDir:   filepath.Join(t.TempDir(), "gone"),
		unmount:    func() error { unmounted = true; return nil },
	}

	mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(1)
	mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

	err := ibi.finalizeDisconnected(sp, 10, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "recording the result failed")
	assert.True(t, unmounted, "p4 must be unmounted before the terminal action")
}

// TestSuccessSyncFailureHaltsPoweredOn proves that when the core prepare and the result
// write both succeed but the final durability sync fails, the node halts powered-on instead
// of signalling success via power-off: the record may not survive a power loss.
func TestSuccessSyncFailureHaltsPoweredOn(t *testing.T) {
	saveSeams(t)
	collectStatusLogs = func(*IBIPrepare, *statusPartition) error { return nil }
	dir := t.TempDir()

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	cfg := &ibiconfig.IBIPrepareConfig{Profile: ibiconfig.ProfileInstallOnly, SeedVersion: "4.15.0", Shutdown: true}
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, nil, cfg)

	unmounted := false
	sp := &statusPartition{deviceName: "sdb3", mountDir: dir, unmount: func() error { unmounted = true; return nil }}

	mockOps.EXPECT().RunInHostNamespace("sync").Return("", fmt.Errorf("sync failed")).Times(1)
	mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

	err := ibi.finalizeDisconnected(sp, 5, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "durability sync failed")
	assert.True(t, unmounted, "p4 must still be unmounted before the terminal action")

	// The success result is still recorded even though the sync failed.
	result := readPrepareResult(t, dir)
	assert.Equal(t, resultSuccess, result.Result)
}

// TestSuccessUnmountFailureHaltsPoweredOn proves that when the core prepare, result write,
// and sync all succeed but p4 cannot be unmounted, the node halts powered-on rather than
// powering off with the removable status filesystem still mounted.
func TestSuccessUnmountFailureHaltsPoweredOn(t *testing.T) {
	saveSeams(t)
	collectStatusLogs = func(*IBIPrepare, *statusPartition) error { return nil }
	dir := t.TempDir()

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	cfg := &ibiconfig.IBIPrepareConfig{Profile: ibiconfig.ProfileInstallOnly, SeedVersion: "4.15.0", Shutdown: true}
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, nil, cfg)

	sp := &statusPartition{deviceName: "sdb3", mountDir: dir, unmount: func() error { return fmt.Errorf("umount failed") }}

	mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(1)
	mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

	err := ibi.finalizeDisconnected(sp, 5, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unmounting the ibi-status partition failed")
}

// TestInstallOnlySuccessPowersOff proves a successful prepare writes the success result and
// takes the existing power-off terminal action (install-only), and that a best-effort log
// bundle failure does not block the result write or the terminal action.
func TestInstallOnlySuccessPowersOff(t *testing.T) {
	saveSeams(t)
	collectStatusLogs = func(*IBIPrepare, *statusPartition) error { return fmt.Errorf("log collection failed") }
	dir := t.TempDir()

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	cfg := &ibiconfig.IBIPrepareConfig{Profile: ibiconfig.ProfileInstallOnly, SeedVersion: "4.15.0", Shutdown: true}
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, nil, cfg)
	sp := &statusPartition{deviceName: "sdb3", mountDir: dir, unmount: func() error { return nil }}

	mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("shutdown", "now").Return("", nil).Times(1)
	mockOps.EXPECT().SystemctlAction(gomock.Any(), gomock.Any()).Times(0)

	err := ibi.finalizeDisconnected(sp, 5, nil)
	assert.NoError(t, err)

	result := readPrepareResult(t, dir)
	assert.Equal(t, resultSuccess, result.Result, "result must be recorded despite best-effort log failure")
}

// TestFailureWriterPreservesExistingSuccessMarker proves the result writer's
// defense-in-depth: a failure result never overwrites an existing completed-install marker
// (result: success), so a success can never be downgraded to failure (which would reopen the
// re-wipe hole) even if the guard ordering were bypassed.
func TestFailureWriterPreservesExistingSuccessMarker(t *testing.T) {
	saveSeams(t)
	collectStatusLogs = func(*IBIPrepare, *statusPartition) error { return nil }
	dir := t.TempDir()

	r := &PrepareResult{Phase: prepareResultPhase, Profile: ibiconfig.ProfileInstallOnly,
		Result: resultSuccess, Timestamp: nowRFC3339(), SeedVersion: "4.15.0", Reentry: reentryNone}
	data, err := json.MarshalIndent(r, "", "  ")
	assert.NoError(t, err)
	assert.NoError(t, os.WriteFile(filepath.Join(dir, common.PrepareResultFileName), append(data, '\n'), 0o600))

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	cfg := &ibiconfig.IBIPrepareConfig{Profile: ibiconfig.ProfileInstallOnly, SeedVersion: "4.15.0"}
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, nil, cfg)
	sp := &statusPartition{deviceName: "sdb3", mountDir: dir, unmount: func() error { return nil }}

	mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(1)
	mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

	err = ibi.finalizeDisconnected(sp, 5, fmt.Errorf("prepare blew up"))
	assert.Error(t, err)

	result := readPrepareResult(t, dir)
	assert.Equal(t, resultSuccess, result.Result, "a completed-install marker must never be downgraded to failure")
}

// TestReentryWithSuccessMarkerBlocksWipe proves that a boot re-entering a completed install
// (an existing result: success marker) refuses to wipe: no disk cleanup or install runs,
// the success marker is preserved, reentry: blocked is recorded, and the node halts
// powered-on.
func TestReentryWithSuccessMarkerBlocksWipe(t *testing.T) {
	stubDisconnectedSeams(t)
	statusDir := useTempStatusPartition(t)
	seedSuccessMarker(t, statusDir)

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
	cfg := disconnectedCfg(ibiconfig.ProfileInstallOnly)
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, cleanupMock, cfg)

	// p4 mount + guard are the first preconditions, so a blocked re-entry halts before p2.
	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("sdb1", "sdb2"), nil).Times(1)
	mockOps.EXPECT().MountReadOnly(gomock.Any(), gomock.Any()).Times(0)
	mockOps.EXPECT().Mount("sdb2", statusDir).Return(nil).Times(1)
	// No wipe: the installation disk is never cleaned up or written.
	cleanupMock.EXPECT().CleanupInstallDevice(gomock.Any()).Times(0)
	// p4 unmounts BEFORE the halt (not merely on the way out): isolating emergency.target
	// may stop this prepare service before a deferred-only unmount could run, so the explicit
	// unmount must precede the terminal action. The node halts powered-on, never powers off;
	// p2 is never mounted.
	gomock.InOrder(
		mockOps.EXPECT().Umount("sdb2").Return(nil).Times(1),
		mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1),
	)
	mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

	err := ibi.Run()
	assert.ErrorIs(t, err, errReentryBlocked)

	result := readPrepareResult(t, statusDir)
	assert.Equal(t, resultSuccess, result.Result, "the completed-install marker must be preserved")
	assert.Equal(t, reentryBlocked, result.Reentry)
}

// TestRepeatedReentryPreservesSuccessMarker proves repeated re-entries each refuse to wipe
// and leave result: success intact, so the guard never degrades into a wipe on a later boot.
func TestRepeatedReentryPreservesSuccessMarker(t *testing.T) {
	stubDisconnectedSeams(t)
	statusDir := useTempStatusPartition(t)
	seedSuccessMarker(t, statusDir)

	for boot := 1; boot <= 2; boot++ {
		ctrl := gomock.NewController(t)
		mockOps := ops.NewMockOps(ctrl)
		cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
		cfg := disconnectedCfg(ibiconfig.ProfileInstallOnly)
		ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, cleanupMock, cfg)

		// p4 mount + guard are the first preconditions, so a blocked re-entry halts before p2.
		mockOps.EXPECT().ListBlockDevices().Return(statusDevices("sdb1", "sdb2"), nil).Times(1)
		mockOps.EXPECT().MountReadOnly(gomock.Any(), gomock.Any()).Times(0)
		mockOps.EXPECT().Mount("sdb2", statusDir).Return(nil).Times(1)
		cleanupMock.EXPECT().CleanupInstallDevice(gomock.Any()).Times(0)
		mockOps.EXPECT().Umount("sdb2").Return(nil).Times(1)
		mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)
		mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

		err := ibi.Run()
		assert.ErrorIsf(t, err, errReentryBlocked, "boot %d must block the wipe", boot)

		result := readPrepareResult(t, statusDir)
		assert.Equalf(t, resultSuccess, result.Result, "boot %d must preserve the success marker", boot)
		assert.Equalf(t, reentryBlocked, result.Reentry, "boot %d must record a blocked re-entry", boot)
	}
}

// TestReentryOverFailureMarkerProceeds proves a prior failed prepare (result: failure) does
// not block a retry: the guard allows the wipe to proceed.
func TestReentryOverFailureMarkerProceeds(t *testing.T) {
	stubDisconnectedSeams(t)
	statusDir := useTempStatusPartition(t)
	// Seed a prior *failure* marker.
	failMsg := "earlier failure"
	r := &PrepareResult{Phase: prepareResultPhase, Profile: ibiconfig.ProfileInstallOnly,
		Result: resultFailure, Timestamp: nowRFC3339(), SeedVersion: "4.15.0", Error: &failMsg, Reentry: reentryNone}
	data, err := json.MarshalIndent(r, "", "  ")
	assert.NoError(t, err)
	assert.NoError(t, os.WriteFile(filepath.Join(statusDir, common.PrepareResultFileName), append(data, '\n'), 0o600))

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)
	cfg := disconnectedCfg(ibiconfig.ProfileInstallOnly)
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, ostreeMock, cleanupMock, cfg)

	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("sdb1", "sdb2"), nil).Times(2)
	mockOps.EXPECT().MountReadOnly("sdb1", cfg.LocalImagesPath).Return(nil).Times(1)
	mockOps.EXPECT().Mount("sdb2", statusDir).Return(nil).Times(1)
	// The retry proceeds: the disk is cleaned up and written.
	cleanupMock.EXPECT().CleanupInstallDevice(cfg.InstallationDisk).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("coreos-installer", []string{"install", cfg.InstallationDisk}).Return("", nil).Times(1)
	mockOps.EXPECT().SetupContainersFolderCommands().Return(nil).Times(1)
	expectPodmanInfo(mockOps)
	ostreeMock.EXPECT().Undeploy(rhcosOstreeIndex).Return(nil).Times(1)
	// Success terminal path: core sync + result sync, p2 and p4 unmount, no halt.
	mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(2)
	mockOps.EXPECT().Umount("sdb1").Return(nil).Times(1)
	mockOps.EXPECT().Umount("sdb2").Return(nil).Times(1)
	mockOps.EXPECT().SystemctlAction(gomock.Any(), gomock.Any()).Times(0)

	err = ibi.Run()
	assert.NoError(t, err)

	result := readPrepareResult(t, statusDir)
	assert.Equal(t, resultSuccess, result.Result, "a fresh successful prepare overwrites the prior failure marker")
	assert.Equal(t, reentryNone, result.Reentry)
}

// TestZeroOrMultipleP4HaltsWithoutWipe proves both profiles require exactly one writable
// ibi-status device before any wipe: 0 or >1 devices halts powered-on without wiping.
func TestZeroOrMultipleP4HaltsWithoutWipe(t *testing.T) {
	for _, profile := range []string{ibiconfig.ProfileInstallOnly, ibiconfig.ProfileSelfContained} {
		for _, tc := range []struct {
			name    string
			devices []ops.BlockDevice
		}{
			{
				name:    "zero p4",
				devices: []ops.BlockDevice{{Name: "sdb1", Label: common.LocalImagesFSLabel}},
			},
			{
				name: "multiple p4",
				devices: []ops.BlockDevice{
					{Name: "sdb1", Label: common.LocalImagesFSLabel},
					{Name: "sdb2", Label: common.IBIStatusFSLabel},
					{Name: "sdb3", Label: common.IBIStatusFSLabel},
				},
			},
		} {
			t.Run(profile+"/"+tc.name, func(t *testing.T) {
				stubDisconnectedSeams(t)
				useTempStatusPartition(t)

				ctrl := gomock.NewController(t)
				mockOps := ops.NewMockOps(ctrl)
				cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
				cfg := disconnectedCfg(profile)
				ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, cleanupMock, cfg)

				// p4 discovery is the first precondition, so it fails before p2 is ever touched.
				mockOps.EXPECT().ListBlockDevices().Return(tc.devices, nil).Times(1)
				mockOps.EXPECT().MountReadOnly(gomock.Any(), gomock.Any()).Times(0)
				// p4 is never mounted and nothing is wiped.
				mockOps.EXPECT().Mount(gomock.Any(), gomock.Any()).Times(0)
				cleanupMock.EXPECT().CleanupInstallDevice(gomock.Any()).Times(0)
				// Node halts powered-on; p2 is never mounted so nothing to unmount.
				mockOps.EXPECT().Umount(gomock.Any()).Times(0)
				mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)
				mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

				err := ibi.Run()
				assert.Error(t, err)
			})
		}
	}
}

// TestP4MountFailureFailsClosed proves a p4 mount failure halts powered-on without wiping.
func TestP4MountFailureFailsClosed(t *testing.T) {
	stubDisconnectedSeams(t)
	statusDir := useTempStatusPartition(t)

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
	cfg := disconnectedCfg(ibiconfig.ProfileSelfContained)
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, cleanupMock, cfg)

	// p4 is the first precondition, so a p4 mount failure halts before p2 is ever touched.
	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("sdb1", "sdb2"), nil).Times(1)
	mockOps.EXPECT().MountReadOnly(gomock.Any(), gomock.Any()).Times(0)
	mockOps.EXPECT().Mount("sdb2", statusDir).Return(fmt.Errorf("mount failed")).Times(1)
	// No wipe; p2 never mounted; node halts powered-on.
	cleanupMock.EXPECT().CleanupInstallDevice(gomock.Any()).Times(0)
	mockOps.EXPECT().Umount(gomock.Any()).Times(0)
	mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

	err := ibi.Run()
	assert.Error(t, err)
}

// TestP4NonExt4FailsClosed proves a device that carries the ibi-status label but uses a
// non-ext4 filesystem (e.g. a hand-altered or mislabeled writable vfat stick) is rejected
// before it is mounted or adopted as the status partition: the node halts powered-on
// without wiping and p2 is never touched.
func TestP4NonExt4FailsClosed(t *testing.T) {
	stubDisconnectedSeams(t)
	useTempStatusPartition(t)

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
	cfg := disconnectedCfg(ibiconfig.ProfileInstallOnly)
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, cleanupMock, cfg)

	// Exactly one device carries the ibi-status label, but it is vfat rather than ext4.
	devices := []ops.BlockDevice{
		{Name: "sdb1", Label: common.LocalImagesFSLabel},
		{Name: "sdb2", Label: common.IBIStatusFSLabel, FSType: "vfat"},
	}
	// p4 discovery is the first precondition, so the fstype rejection halts before p2.
	mockOps.EXPECT().ListBlockDevices().Return(devices, nil).Times(1)
	mockOps.EXPECT().MountReadOnly(gomock.Any(), gomock.Any()).Times(0)
	// The wrong-filesystem device is never mounted and nothing is wiped.
	mockOps.EXPECT().Mount(gomock.Any(), gomock.Any()).Times(0)
	cleanupMock.EXPECT().CleanupInstallDevice(gomock.Any()).Times(0)
	mockOps.EXPECT().Umount(gomock.Any()).Times(0)
	mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

	err := ibi.Run()
	assert.Error(t, err)
}

// TestP4ReadOnlyFailsClosed proves a non-writable p4 (the writability probe fails) halts
// powered-on without wiping, and the probed partition is unmounted.
func TestP4ReadOnlyFailsClosed(t *testing.T) {
	stubDisconnectedSeams(t)
	// Point the mount dir at a path that does not exist so the writability probe fails even
	// though ops.Mount is mocked to succeed (simulating a read-only/unusable p4).
	statusPartitionMountDir = filepath.Join(t.TempDir(), "nonexistent")

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
	cfg := disconnectedCfg(ibiconfig.ProfileInstallOnly)
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, cleanupMock, cfg)

	// p4 is the first precondition, so the probe failure halts before p2 is ever touched.
	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("sdb1", "sdb2"), nil).Times(1)
	mockOps.EXPECT().MountReadOnly(gomock.Any(), gomock.Any()).Times(0)
	mockOps.EXPECT().Mount("sdb2", statusPartitionMountDir).Return(nil).Times(1)
	// The probe failure unmounts the p4 partition and fails closed; no wipe, p2 never mounted.
	cleanupMock.EXPECT().CleanupInstallDevice(gomock.Any()).Times(0)
	mockOps.EXPECT().Umount("sdb2").Return(nil).Times(1)
	mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

	err := ibi.Run()
	assert.Error(t, err)
}

// TestP4MalformedMarkerFailsClosed proves an existing but unreadable/malformed prepare.json
// is never treated as safe to wipe: the guard fails closed and halts powered-on, and the
// marker is left untouched.
func TestP4MalformedMarkerFailsClosed(t *testing.T) {
	stubDisconnectedSeams(t)
	statusDir := useTempStatusPartition(t)
	markerPath := filepath.Join(statusDir, common.PrepareResultFileName)
	assert.NoError(t, os.WriteFile(markerPath, []byte("{ this is not valid json"), 0o600))

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
	cfg := disconnectedCfg(ibiconfig.ProfileInstallOnly)
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, cleanupMock, cfg)

	// p4 mount + guard are the first preconditions, so a malformed marker halts before p2.
	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("sdb1", "sdb2"), nil).Times(1)
	mockOps.EXPECT().MountReadOnly(gomock.Any(), gomock.Any()).Times(0)
	mockOps.EXPECT().Mount("sdb2", statusDir).Return(nil).Times(1)
	// Fail closed before any wipe; p4 unmounts BEFORE the halt (an emergency isolate may
	// stop this service before a deferred-only unmount runs); node halts powered-on; p2
	// never mounted.
	cleanupMock.EXPECT().CleanupInstallDevice(gomock.Any()).Times(0)
	gomock.InOrder(
		mockOps.EXPECT().Umount("sdb2").Return(nil).Times(1),
		mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1),
	)
	mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

	err := ibi.Run()
	assert.Error(t, err)

	// The malformed marker is left untouched (never overwritten by the guard).
	got, err := os.ReadFile(markerPath) //nolint:gosec // test path
	assert.NoError(t, err)
	assert.Equal(t, "{ this is not valid json", string(got))
}

// TestP4UnknownResultMarkerFailsClosed proves a parseable prepare.json carrying an empty or
// unrecognized result value is never treated as safe to wipe: the guard fails closed and
// halts powered-on, leaving the marker untouched.
func TestP4UnknownResultMarkerFailsClosed(t *testing.T) {
	for _, marker := range []string{
		`{"phase":"prepare","result":"","reentry":"none"}`,
		`{"phase":"prepare","result":"partial","reentry":"none"}`,
	} {
		t.Run(marker, func(t *testing.T) {
			stubDisconnectedSeams(t)
			statusDir := useTempStatusPartition(t)
			markerPath := filepath.Join(statusDir, common.PrepareResultFileName)
			assert.NoError(t, os.WriteFile(markerPath, []byte(marker), 0o600))

			ctrl := gomock.NewController(t)
			mockOps := ops.NewMockOps(ctrl)
			cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
			cfg := disconnectedCfg(ibiconfig.ProfileInstallOnly)
			ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, cleanupMock, cfg)

			// p4 mount + guard are the first preconditions, so an unknown marker halts before p2.
			mockOps.EXPECT().ListBlockDevices().Return(statusDevices("sdb1", "sdb2"), nil).Times(1)
			mockOps.EXPECT().MountReadOnly(gomock.Any(), gomock.Any()).Times(0)
			mockOps.EXPECT().Mount("sdb2", statusDir).Return(nil).Times(1)
			// Fail closed before any wipe; p4 unmounts; node halts powered-on; p2 never mounted.
			cleanupMock.EXPECT().CleanupInstallDevice(gomock.Any()).Times(0)
			mockOps.EXPECT().Umount("sdb2").Return(nil).Times(1)
			mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)
			mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

			err := ibi.Run()
			assert.Error(t, err)

			// The marker is left untouched (never overwritten by the guard).
			got, err := os.ReadFile(markerPath) //nolint:gosec // test path
			assert.NoError(t, err)
			assert.Equal(t, marker, string(got))
		})
	}
}

// TestWriteLogBundle proves the default log collection captures the journal under the logs
// directory as a gzip bundle that decompresses to the captured output.
func TestWriteLogBundle(t *testing.T) {
	dir := t.TempDir()
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, nil, &ibiconfig.IBIPrepareConfig{})
	sp := &statusPartition{deviceName: "sdb2", mountDir: dir, unmount: func() error { return nil }}

	cmd := fmt.Sprintf("set -o pipefail; journalctl --no-pager -b | tail -c %d", prepareLogBundleCap)
	mockOps.EXPECT().RunBashInHostNamespace(cmd).Return("boot journal contents", nil).Times(1)

	assert.NoError(t, ibi.writeLogBundle(sp))

	bundle := filepath.Join(dir, common.IBIStatusLogsDir, prepareLogBundleName)
	data, err := os.ReadFile(bundle) //nolint:gosec // test path
	assert.NoError(t, err)
	if assert.GreaterOrEqual(t, len(data), 2) {
		assert.Equal(t, []byte{0x1f, 0x8b}, data[:2], "bundle must be gzip-compressed")
	}

	gz, err := gzip.NewReader(bytes.NewReader(data))
	assert.NoError(t, err)
	got, err := io.ReadAll(gz)
	assert.NoError(t, err)
	assert.Equal(t, "boot journal contents", string(got))
}

// TestWriteLogBundleJournalFailure proves a journal collection failure is surfaced to the
// caller (which treats it as best-effort) and no bundle is written.
func TestWriteLogBundleJournalFailure(t *testing.T) {
	dir := t.TempDir()
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, nil, &ibiconfig.IBIPrepareConfig{})
	sp := &statusPartition{deviceName: "sdb2", mountDir: dir, unmount: func() error { return nil }}

	cmd := fmt.Sprintf("set -o pipefail; journalctl --no-pager -b | tail -c %d", prepareLogBundleCap)
	mockOps.EXPECT().RunBashInHostNamespace(cmd).Return("", fmt.Errorf("no journal")).Times(1)

	assert.Error(t, ibi.writeLogBundle(sp))
	_, err := os.Stat(filepath.Join(dir, common.IBIStatusLogsDir, prepareLogBundleName))
	assert.True(t, os.IsNotExist(err), "no bundle must be written when journal collection fails")
}
