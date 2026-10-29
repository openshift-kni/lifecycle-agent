package ibi_preparation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	preinstallUtils "github.com/rh-ecosystem-edge/preinstall-utils/pkg"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"github.com/openshift-kni/lifecycle-agent/api/ibiconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/localimages"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
)

func TestDiskPreparation(t *testing.T) {
	installationDisk := "/dev/sda"
	extraPartitionLabel := "label"
	extraPartitionStart := "-40"
	extraPartitionNumber := uint(5)

	testcases := []struct {
		name                string
		partitionError      bool
		setupFolderError    bool
		UseContainersFolder bool
		skipDiskCleanup     bool
		failCleanupDisk     bool
		CoreosInstallerArgs []string
	}{
		{
			name:                "PrepareDisk with external partition - happy flow",
			partitionError:      false,
			setupFolderError:    false,
			UseContainersFolder: false,
			skipDiskCleanup:     false,
			failCleanupDisk:     false,
		},
		{
			name:                "cleanup disk fails though installation continues",
			partitionError:      false,
			setupFolderError:    false,
			UseContainersFolder: false,
			skipDiskCleanup:     false,
			failCleanupDisk:     true,
		},
		{
			name:                "fail to create external partition",
			partitionError:      true,
			setupFolderError:    false,
			UseContainersFolder: false,
			skipDiskCleanup:     false,
			failCleanupDisk:     false,
		},
		{
			name:                "PrepareDisk without external partition - happy flow",
			partitionError:      false,
			setupFolderError:    false,
			UseContainersFolder: true,
			skipDiskCleanup:     false,
			failCleanupDisk:     false,
		},
		{
			name:                "PrepareDisk setup folder - fail",
			partitionError:      false,
			setupFolderError:    true,
			UseContainersFolder: true,
			skipDiskCleanup:     false,
			failCleanupDisk:     false,
		},
		{
			name:                "PrepareDisk with coreos installer params provided - happy flow",
			partitionError:      false,
			setupFolderError:    false,
			UseContainersFolder: false,
			skipDiskCleanup:     false,
			failCleanupDisk:     false,
			CoreosInstallerArgs: []string{"--save-partindex=5"},
		},
	}

	for _, tc := range testcases {
		ctrl := gomock.NewController(t)
		mockOps := ops.NewMockOps(ctrl)
		cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
		t.Run(tc.name, func(t *testing.T) {
			log := &logrus.Logger{}
			ibiCobfig := &ibiconfig.IBIPrepareConfig{
				InstallationDisk:     installationDisk,
				ExtraPartitionLabel:  extraPartitionLabel,
				ExtraPartitionStart:  extraPartitionStart,
				ExtraPartitionNumber: extraPartitionNumber,
				UseContainersFolder:  tc.UseContainersFolder,
				SkipDiskCleanup:      tc.skipDiskCleanup,
				CoreosInstallerArgs:  tc.CoreosInstallerArgs,
			}
			ibi := NewIBIPrepare(log, mockOps, nil, nil, cleanupMock,
				ibiCobfig)

			if !tc.skipDiskCleanup {
				if tc.failCleanupDisk {
					cleanupMock.EXPECT().CleanupInstallDevice(installationDisk).Return(fmt.Errorf("dummy"))
				} else {
					cleanupMock.EXPECT().CleanupInstallDevice(installationDisk).Return(nil)
				}
			}
			args := append([]string{"install", "/dev/sda"}, tc.CoreosInstallerArgs...)
			mockOps.EXPECT().RunInHostNamespace("coreos-installer", args).Return("", nil).Times(1)

			if !tc.UseContainersFolder {
				if !tc.partitionError {
					mockOps.EXPECT().CreateExtraPartition(installationDisk, extraPartitionLabel,
						extraPartitionStart, extraPartitionNumber).Return(nil).Times(1)
				} else {
					mockOps.EXPECT().CreateExtraPartition(installationDisk, extraPartitionLabel,
						extraPartitionStart, extraPartitionNumber).Return(fmt.Errorf("dummy")).Times(1)
				}

				mockOps.EXPECT().SetupContainersFolderCommands().Return(nil).Times(0)
			} else {
				mockOps.EXPECT().CreateExtraPartition(gomock.Any(), gomock.Any(),
					gomock.Any(), gomock.Any()).Return(nil).Times(0)
				if !tc.setupFolderError {
					mockOps.EXPECT().SetupContainersFolderCommands().Return(nil).Times(1)
				} else {
					mockOps.EXPECT().SetupContainersFolderCommands().Return(fmt.Errorf("dummy")).Times(1)
				}
			}

			err := ibi.diskPreparation()
			assert.Equal(t, err != nil, tc.partitionError || tc.setupFolderError)
		})
	}
}

func TestPostDeployment(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	log := &logrus.Logger{}
	ibiConfig := &ibiconfig.IBIPrepareConfig{}
	ibi := NewIBIPrepare(log, mockOps, nil, nil, nil, ibiConfig)

	// Test case when the post deployment script does not exist
	tmpDir := t.TempDir()
	postSH := path.Join(tmpDir, "post.sh")
	mockOps.EXPECT().RunBashInHostNamespace(gomock.Any()).Return("", nil).Times(0)
	err := ibi.postDeployment(postSH)
	assert.Nil(t, err)

	file, err := os.Create(postSH)
	assert.Nil(t, err)
	defer func() { _ = file.Close() }()
	// Test case when the post deployment script exists and executes without error
	mockOps.EXPECT().RunBashInHostNamespace(postSH).Return("", nil).Times(1)
	err = ibi.postDeployment(postSH)
	assert.Nil(t, err)

	// Test case when the post deployment script exists but fails to execute
	mockOps.EXPECT().RunBashInHostNamespace(postSH).Return("", fmt.Errorf("dummy")).Times(1)
	err = ibi.postDeployment(postSH)
	assert.NotNil(t, err)
}

func TestSetupPrepareAuthFileCreatesAndCleansUp(t *testing.T) {
	// Absent file: the helper creates it with the synthetic empty authfile content and
	// 0600 mode (parent dirs created as needed); cleanup removes it.
	p := filepath.Join(t.TempDir(), "sub", "pull-secret.json")

	cleanup, err := setupPrepareAuthFile(p)
	assert.NoError(t, err)

	content, err := os.ReadFile(p) //nolint:gosec // test path
	assert.NoError(t, err)
	assert.Equal(t, common.IBIEmptyPullSecret, string(content))

	info, err := os.Stat(p)
	assert.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	cleanup()
	_, err = os.Stat(p)
	assert.True(t, os.IsNotExist(err), "synthetic authfile should be removed by cleanup")
}

func TestSetupPrepareAuthFilePreservesExisting(t *testing.T) {
	// A pre-existing file (operator-provided or a leftover from an interrupted run) must be
	// left byte-for-byte unchanged and must NOT be removed by cleanup.
	p := filepath.Join(t.TempDir(), "pull-secret.json")
	sentinel := `{"auths":{"real.registry.example.com":{"auth":"c2VjcmV0"}}}`
	assert.NoError(t, os.WriteFile(p, []byte(sentinel), 0o600))

	cleanup, err := setupPrepareAuthFile(p)
	assert.NoError(t, err)

	content, err := os.ReadFile(p) //nolint:gosec // test path
	assert.NoError(t, err)
	assert.Equal(t, sentinel, string(content), "pre-existing authfile must be left untouched")

	cleanup()
	content, err = os.ReadFile(p) //nolint:gosec // test path
	assert.NoError(t, err)
	assert.Equal(t, sentinel, string(content), "cleanup must not remove a file this run did not create")
}

func TestSetupPrepareAuthFileCleanupOnFailure(t *testing.T) {
	dir := t.TempDir()

	// Synthetic file created by this run: a later-step failure triggers the deferred
	// cleanup, which must remove the leaked authfile.
	created := filepath.Join(dir, "created.json")
	func() {
		cleanup, err := setupPrepareAuthFile(created)
		assert.NoError(t, err)
		defer cleanup()
		// Simulate a later step in Run() failing; the deferred cleanup still runs.
	}()
	_, err := os.Stat(created)
	assert.True(t, os.IsNotExist(err), "synthetic authfile must be removed on the error path")

	// Pre-existing file must survive the same error path.
	existing := filepath.Join(dir, "existing.json")
	sentinel := `{"auths":{"real":{}}}`
	assert.NoError(t, os.WriteFile(existing, []byte(sentinel), 0o600))
	func() {
		cleanup, err := setupPrepareAuthFile(existing)
		assert.NoError(t, err)
		defer cleanup()
	}()
	content, err := os.ReadFile(existing) //nolint:gosec // test path
	assert.NoError(t, err)
	assert.Equal(t, sentinel, string(content), "pre-existing authfile must survive the error path")
}

func TestPrepareAuthFilePathCrossChroot(t *testing.T) {
	// Run() writes the synthetic authfile to common.PathOutsideChroot(common.IBIPullSecretFilePath),
	// while both consumers reference common.IBIPullSecretFilePath directly: the seed pull in the
	// host namespace (ibipreparation.go) and the precache after chrooting into common.Host. This
	// asserts the write target and the consumer reference resolve to the same file, so one
	// outside-chroot write covers both. common.Host is a const and absent in the test env, so
	// PathOutsideChroot is the identity; t.Chdir makes the relative var/tmp path land under temp.
	t.Chdir(t.TempDir())

	writeTarget := common.PathOutsideChroot(common.IBIPullSecretFilePath)
	cleanup, err := setupPrepareAuthFile(writeTarget)
	assert.NoError(t, err)
	defer cleanup()

	content, err := os.ReadFile(common.IBIPullSecretFilePath) //nolint:gosec // test path
	assert.NoError(t, err)
	assert.Equal(t, common.IBIEmptyPullSecret, string(content),
		"file written to the outside-chroot path must be visible at the consumer reference path")
}

func TestChrootIfPathExists(t *testing.T) {
	log := &logrus.Logger{}

	// When the path is absent there is nothing to chroot into: no Chroot call, the working
	// directory is left untouched, and the returned unchroot is a no-op.
	t.Run("absent path skips chroot and leaves cwd untouched", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		mockOps := ops.NewMockOps(ctrl)
		ibi := NewIBIPrepare(log, mockOps, nil, nil, nil, &ibiconfig.IBIPrepareConfig{})

		wdBefore, err := os.Getwd()
		assert.NoError(t, err)

		unchroot, err := ibi.chrootIfPathExists(filepath.Join(t.TempDir(), "does-not-exist"))
		assert.NoError(t, err)
		assert.NoError(t, unchroot())

		wdAfter, err := os.Getwd()
		assert.NoError(t, err)
		assert.Equal(t, wdBefore, wdAfter, "cwd must be unchanged when chroot is skipped")
	})

	// When the path exists the process is chrooted and the working directory is moved into the
	// new root ("/"), so relative paths (e.g. the precache --authfile) resolve against the new
	// root instead of escaping to the pre-chroot directory. The unchroot func is propagated.
	t.Run("present path chroots and chdirs into the new root", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		mockOps := ops.NewMockOps(ctrl)
		ibi := NewIBIPrepare(log, mockOps, nil, nil, nil, &ibiconfig.IBIPrepareConfig{})

		wdBefore, err := os.Getwd()
		assert.NoError(t, err)
		// Chroot is mocked, so the os.Chdir("/") inside chrootIfPathExists really changes this
		// process's cwd; restore it so later tests are unaffected.
		defer func() { _ = os.Chdir(wdBefore) }()

		chrootPath := t.TempDir()
		unchrootCalled := false
		mockOps.EXPECT().Chroot(chrootPath).Return(func() error {
			unchrootCalled = true
			return nil
		}, nil).Times(1)

		unchroot, err := ibi.chrootIfPathExists(chrootPath)
		assert.NoError(t, err)

		wdAfter, err := os.Getwd()
		assert.NoError(t, err)
		assert.Equal(t, "/", wdAfter, "cwd must be the new root after a successful chroot")

		assert.NoError(t, unchroot())
		assert.True(t, unchrootCalled, "the ops.Chroot unchroot func must be propagated to the caller")
	})
}

func TestCleanupRhcosSysroot(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ostreeClientMock := ostreeclient.NewMockIClient(ctrl)
	log := &logrus.Logger{}
	ibiConfig := &ibiconfig.IBIPrepareConfig{}
	ibi := NewIBIPrepare(log, mockOps, nil, ostreeClientMock, nil, ibiConfig)
	tmpDir := t.TempDir()
	origPrefix := common.OstreeDeployPathPrefix
	t.Cleanup(func() { common.OstreeDeployPathPrefix = origPrefix })
	common.OstreeDeployPathPrefix = tmpDir

	// Test case when rhcosOstreePath doesn't exists, we still should succeed
	ostreeClientMock.EXPECT().Undeploy(rhcosOstreeIndex).Return(nil).Times(1)
	mockOps.EXPECT().RunBashInHostNamespace(gomock.Any(), gomock.Any(), gomock.Any()).Return("", nil).Times(0)
	err := ibi.cleanupRhcosSysroot()
	assert.Nil(t, err)

	// failed to undeploy
	ostreeClientMock.EXPECT().Undeploy(rhcosOstreeIndex).Return(fmt.Errorf("dummy")).Times(1)
	err = ibi.cleanupRhcosSysroot()
	assert.NotNil(t, err)

	// Happy flow
	if err := os.MkdirAll(filepath.Join(tmpDir, rhcosOstreePath), 0o700); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	ostreeClientMock.EXPECT().Undeploy(rhcosOstreeIndex).Return(nil).Times(1)
	mockOps.EXPECT().RunBashInHostNamespace("rm", "-rf", filepath.Join(tmpDir, rhcosOstreePath)).Return("", nil).Times(1)
	err = ibi.cleanupRhcosSysroot()
	assert.Nil(t, err)

	// Failed to remove folder
	ostreeClientMock.EXPECT().Undeploy(rhcosOstreeIndex).Return(nil).Times(1)
	mockOps.EXPECT().RunBashInHostNamespace("rm", "-rf", filepath.Join(tmpDir, rhcosOstreePath)).Return("", fmt.Errorf("dummy")).Times(1)
	err = ibi.cleanupRhcosSysroot()
	assert.NotNil(t, err)
}

// --- Disconnected offline image-sourcing tests (CNF-27161) ---

const (
	testSeedName   = "quay.io/openshift-release-dev/ocp-release"
	testSeedDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testSeedOCITag = "ocp-release"
)

func testSeedImageRef() string { return testSeedName + "@" + testSeedDigest }

// Host containers-storage configuration the fake host podman reports. The seed
// import must target this store (the one ops.MountImage reads) rather than the
// Go-resolved default store.
const (
	testHostGraphDriver = "overlay"
	testHostGraphRoot   = "/var/lib/containers/storage"
	testHostRunRoot     = "/run/containers/storage"
)

func testPodmanInfoJSON() string {
	return fmt.Sprintf(`{"store":{"graphDriverName":%q,"graphRoot":%q,"runRoot":%q}}`,
		testHostGraphDriver, testHostGraphRoot, testHostRunRoot)
}

// wantSeedStoreConfig is the StoreConfig the seed import must build from the host
// podman info: the reported driver, with graphroot/runroot translated into this
// process's mount namespace (PathOutsideChroot).
func wantSeedStoreConfig() localimages.StoreConfig {
	return localimages.StoreConfig{
		GraphDriverName: testHostGraphDriver,
		GraphRoot:       common.PathOutsideChroot(testHostGraphRoot),
		RunRoot:         common.PathOutsideChroot(testHostRunRoot),
	}
}

// expectPodmanInfo registers the host podman store-config query the seed import
// performs to resolve the store ops.MountImage reads.
func expectPodmanInfo(mockOps *ops.MockOps) {
	mockOps.EXPECT().RunInHostNamespace("podman", "info", "--format", "json").
		Return(testPodmanInfoJSON(), nil).Times(1)
}

func testManifest() *localimages.DigestManifest {
	return &localimages.DigestManifest{
		Version:       localimages.SourcesManifestVersion,
		OCILayoutPath: "oci",
		Images: []localimages.ImageEntry{
			{
				Name:    testSeedName,
				Digest:  testSeedDigest,
				OCITag:  testSeedOCITag,
				Sources: []string{testSeedImageRef(), testSeedName + ":4.15.0"},
			},
			{
				Name:    "quay.io/openshift-release-dev/ocp-v4.0-art-dev",
				Digest:  "sha256:2222222222222222222222222222222222222222222222222222222222222222",
				OCITag:  "workload",
				Sources: []string{"quay.io/openshift-release-dev/ocp-v4.0-art-dev:4.15.0"},
			},
		},
	}
}

// fakeImporter is a test double for imageImporter that records how it was
// constructed and used, without opening a real containers-storage store.
type fakeImporter struct {
	cfg         localimages.StoreConfig
	events      *[]string
	importedOne []localimages.ImageEntry
	importedAll int
	verifiedAll int
	mediaRoots  []string
	importErr   error
	verifyErr   error
	closed      bool
}

func (f *fakeImporter) ImportImage(_ context.Context, mediaRoot string, _ *localimages.DigestManifest, e localimages.ImageEntry) error {
	f.mediaRoots = append(f.mediaRoots, mediaRoot)
	f.importedOne = append(f.importedOne, e)
	if f.events != nil {
		*f.events = append(*f.events, "importSeed")
	}
	return f.importErr
}

func (f *fakeImporter) ImportAll(_ context.Context, mediaRoot string, _ *localimages.DigestManifest) error {
	f.mediaRoots = append(f.mediaRoots, mediaRoot)
	f.importedAll++
	if f.events != nil {
		*f.events = append(*f.events, "importClosure")
	}
	return f.importErr
}

func (f *fakeImporter) VerifyAll(_ context.Context, _ *localimages.DigestManifest) error {
	f.verifiedAll++
	if f.events != nil {
		*f.events = append(*f.events, "verifyClosure")
	}
	return f.verifyErr
}

func (f *fakeImporter) Close() error {
	f.closed = true
	return nil
}

// importerFactory routes newImageImporter calls to the seed importer (the host
// store resolved from podman info) or the installed-stateroot importers (graphroot
// under ostree/deploy). The stateroot store is opened twice -- once for the closure
// import and once for the post-import self-check -- with the same config, so they
// are distinguished by order: the first stateroot open is the closure importer, the
// second is the verify importer.
type importerFactory struct {
	seed      *fakeImporter
	closure   *fakeImporter
	verify    *fakeImporter
	events    *[]string
	importErr error
	verifyErr error
}

func (ff *importerFactory) new(_ *logrus.Logger, cfg localimages.StoreConfig) (imageImporter, error) {
	imp := &fakeImporter{cfg: cfg, events: ff.events, importErr: ff.importErr, verifyErr: ff.verifyErr}
	switch {
	case !strings.Contains(cfg.GraphRoot, "ostree/deploy"):
		ff.seed = imp
	case ff.closure == nil:
		ff.closure = imp
	default:
		ff.verify = imp
	}
	return imp, nil
}

// saveSeams snapshots the package seams and restores them when the test ends.
func saveSeams(t *testing.T) {
	t.Helper()
	origParse, origNew, origSetup, origPreflight := parseDigestManifest, newImageImporter, setupStateroot, preflightSeedOSTree
	preflightSeedOSTree = func(*IBIPrepare, context.Context, *localimages.DigestManifest, localimages.ImageEntry) error {
		return nil
	}
	origMount, origCollect := statusPartitionMountDir, collectStatusLogs
	origPersist, origCopy, origVerify := persistReadOnlyStoreFn, copyReadOnlyStore, verifyPersistedStore
	origExchange := exchangeStoreDirs
	origPivotTimeout := pivotRebootTimeout
	t.Cleanup(func() {
		parseDigestManifest = origParse
		newImageImporter = origNew
		setupStateroot = origSetup
		preflightSeedOSTree = origPreflight
		statusPartitionMountDir = origMount
		collectStatusLogs = origCollect
		persistReadOnlyStoreFn = origPersist
		copyReadOnlyStore = origCopy
		verifyPersistedStore = origVerify
		exchangeStoreDirs = origExchange
		pivotRebootTimeout = origPivotTimeout
	})
}

// useTempStatusPartition points the ibi-status (p4) result write-back and re-entry guard
// at a real temporary directory and stubs out best-effort log collection, so disconnected
// Run tests exercise the real atomic result write without a real mount or journalctl.
// saveSeams(t) must have been called first so the seams are restored. It returns the
// directory, into which the caller may pre-seed a prepare.json to exercise the guard.
func useTempStatusPartition(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	statusPartitionMountDir = dir
	collectStatusLogs = func(*IBIPrepare, *statusPartition) error { return nil }
	return dir
}

// statusDevices returns the block-device list a disconnected Run discovers: the p2
// local-images device and the p4 ibi-status device (both label-matched by Run). The p4
// device is reported as ext4, the only filesystem mountStatusPartition accepts.
func statusDevices(p2, p4 string) []ops.BlockDevice {
	return []ops.BlockDevice{
		{Name: p2, Label: common.LocalImagesFSLabel},
		{Name: p4, Label: common.IBIStatusFSLabel, FSType: fsTypeExt4},
	}
}

// readPrepareResult reads and parses the prepare.json written at the root of the ibi-status
// partition directory.
func readPrepareResult(t *testing.T, dir string) PrepareResult {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, common.PrepareResultFileName)) //nolint:gosec // test path
	assert.NoError(t, err)
	var result PrepareResult
	assert.NoError(t, json.Unmarshal(data, &result))
	return result
}

// stubDisconnectedSeams snapshots the package seams and installs the stubs a disconnected
// Run needs to reach the p4 (ibi-status) precondition and guard: a valid parsed manifest, a
// no-op stateroot setup, a fake importer factory, and a temp cwd for the synthetic
// authfile. The caller still wires the ops mock and the ibi-status mount dir per scenario.
func stubDisconnectedSeams(t *testing.T) {
	t.Helper()
	saveSeams(t)
	t.Chdir(t.TempDir())
	parseDigestManifest = func(string) (*localimages.DigestManifest, error) { return testManifest(), nil }
	setupStateroot = func(logr.Logger, ops.Ops, ostreeclient.IClient, rpmostreeclient.IClient, string, string, bool) error {
		return nil
	}
	newImageImporter = (&importerFactory{}).new
}

// disconnectedCfg returns a minimal valid disconnected IBIPrepareConfig for the given
// media profile.
func disconnectedCfg(profile string) *ibiconfig.IBIPrepareConfig {
	return &ibiconfig.IBIPrepareConfig{
		Disconnected:        true,
		Profile:             profile,
		LocalImagesPath:     "/var/mnt/ibi",
		ExpectedDigestsPath: "/var/mnt/ibi/manifest.json",
		SeedImage:           testSeedImageRef(),
		SeedVersion:         "4.15.0",
		InstallationDisk:    "/dev/sda",
		UseContainersFolder: true,
		PrecacheDisabled:    true,
	}
}

// seedSuccessMarker writes a completed-install prepare.json (result: success) at the root
// of the ibi-status partition directory, simulating a prior successful prepare.
func seedSuccessMarker(t *testing.T, dir string) {
	t.Helper()
	r := &PrepareResult{
		Phase:       prepareResultPhase,
		Profile:     ibiconfig.ProfileInstallOnly,
		Result:      resultSuccess,
		Timestamp:   nowRFC3339(),
		SeedVersion: "4.15.0",
		Reentry:     reentryNone,
	}
	data, err := json.MarshalIndent(r, "", "  ")
	assert.NoError(t, err)
	assert.NoError(t, os.WriteFile(filepath.Join(dir, common.PrepareResultFileName), append(data, '\n'), 0o600))
}

func idxOf(events []string, name string) int {
	for i, e := range events {
		if e == name {
			return i
		}
	}
	return -1
}

// TestMountLocalImages exercises the p2 discovery precondition directly: zero or
// more than one labelled device fails (no MountReadOnly); exactly one proceeds to a
// read-only mount and returns an idempotent unmount closure. When the boot-time
// systemd .mount unit already owns the mount point, the existing read-only mount is
// reused (no MountReadOnly, no Umount); a conflicting existing mount fails.
func TestMountLocalImages(t *testing.T) {
	const localImagesPath = "/var/mnt/ibi-images"

	cases := []struct {
		name       string
		devices    []ops.BlockDevice
		wantDevice string // device mountLocalImages should select (empty => expect error)
		// hostMounts is the host /proc/self/mounts content inspectHostMount reads. It is
		// consulted only when device discovery succeeds (exactly one labelled device).
		hostMounts  string
		expectReuse bool // mount is reused: no MountReadOnly, cleanup is a no-op
		expectErr   bool
	}{
		{
			name:      "zero matches fails before mount",
			devices:   []ops.BlockDevice{{Name: "sda", Label: "root"}},
			expectErr: true,
		},
		{
			name: "ambiguous matches fail before mount",
			devices: []ops.BlockDevice{
				{Name: "sdb1", Label: common.LocalImagesFSLabel},
				{Name: "sdc1", Label: common.LocalImagesFSLabel},
			},
			expectErr: true,
		},
		{
			name: "label on a partition proceeds to MountReadOnly",
			devices: []ops.BlockDevice{
				// With lsblk --list, partitions surface as flat top-level entries.
				{Name: "sdb", Label: ""},
				{Name: "sdb1", Label: "boot"},
				{Name: "sdb2", Label: common.LocalImagesFSLabel},
			},
			hostMounts: "/dev/sda3 /sysroot ext4 ro,relatime 0 0\n",
			wantDevice: "sdb2",
		},
		{
			name: "label on a whole device proceeds to MountReadOnly",
			devices: []ops.BlockDevice{
				{Name: "sda", Label: "root"},
				{Name: "sdb", Label: common.LocalImagesFSLabel},
			},
			hostMounts: "",
			wantDevice: "sdb",
		},
		{
			name: "existing read-only mount from the discovered device is reused",
			devices: []ops.BlockDevice{
				{Name: "sdb2", Label: common.LocalImagesFSLabel},
			},
			hostMounts:  "/dev/sdb2 /var/mnt/ibi-images ext4 ro,relatime 0 0\n",
			expectReuse: true,
		},
		{
			name: "mount point occupied by another device fails before mount",
			devices: []ops.BlockDevice{
				{Name: "sdb2", Label: common.LocalImagesFSLabel},
			},
			hostMounts: "/dev/sdc9 /var/mnt/ibi-images ext4 ro,relatime 0 0\n",
			expectErr:  true,
		},
		{
			name: "expected device mounted read-write fails before mount",
			devices: []ops.BlockDevice{
				{Name: "sdb2", Label: common.LocalImagesFSLabel},
			},
			hostMounts: "/dev/sdb2 /var/mnt/ibi-images ext4 rw,relatime 0 0\n",
			expectErr:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			mockOps := ops.NewMockOps(ctrl)
			ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, nil,
				&ibiconfig.IBIPrepareConfig{LocalImagesPath: localImagesPath})

			mockOps.EXPECT().ListBlockDevices().Return(tc.devices, nil).Times(1)
			// inspectHostMount is reached only once device discovery has selected exactly one
			// device; the discovery-failure cases return before reading the host mount table.
			discoverySucceeds := tc.wantDevice != "" || tc.expectReuse ||
				(tc.expectErr && countLabelMatches(tc.devices) == 1)
			if discoverySucceeds {
				mockOps.EXPECT().RunInHostNamespace("cat", "/proc/self/mounts").
					Return(tc.hostMounts, nil).Times(1)
			}
			if tc.wantDevice != "" {
				mockOps.EXPECT().MountReadOnly(tc.wantDevice, localImagesPath).Return(nil).Times(1)
			} else {
				mockOps.EXPECT().MountReadOnly(gomock.Any(), gomock.Any()).Times(0)
			}

			unmount, err := ibi.mountLocalImages()
			if tc.expectErr {
				assert.Error(t, err)
				assert.Nil(t, unmount)
				return
			}
			assert.NoError(t, err)
			if tc.expectReuse {
				// A reused systemd-owned mount must never be torn down from here.
				mockOps.EXPECT().Umount(gomock.Any()).Times(0)
				if assert.NotNil(t, unmount) {
					assert.NoError(t, unmount())
					assert.NoError(t, unmount())
				}
				return
			}
			if assert.NotNil(t, unmount) {
				// Idempotent: Umount runs at most once even if the closure is called
				// twice (explicit happy-path call + deferred error-path call).
				mockOps.EXPECT().Umount(tc.wantDevice).Return(nil).Times(1)
				assert.NoError(t, unmount())
				assert.NoError(t, unmount())
			}
		})
	}
}

// countLabelMatches returns how many block devices carry the local-images filesystem label.
func countLabelMatches(devices []ops.BlockDevice) int {
	n := 0
	for _, bd := range devices {
		if bd.Label == common.LocalImagesFSLabel {
			n++
		}
	}
	return n
}

// TestLocalImagesDiscoveryPreconditionFailsBeforeDiskWrite proves that a failed p2
// discovery (0 or >1 devices) fails Run() before any disk write: no p2 mount, no disk
// cleanup, no image write. Because p4 (ibi-status) is mounted and the guard passes first,
// the failure is now finalized -- a failure prepare.json is recorded and the node is halted
// powered-on (never powered off) -- rather than aborting Run bare.
func TestLocalImagesDiscoveryPreconditionFailsBeforeDiskWrite(t *testing.T) {
	cases := []struct {
		name    string
		devices []ops.BlockDevice
	}{
		{"zero p2 devices", []ops.BlockDevice{
			{Name: "sda", Label: "root"},
			{Name: "sdp4", Label: common.IBIStatusFSLabel, FSType: fsTypeExt4},
		}},
		{"multiple p2 devices", []ops.BlockDevice{
			{Name: "sdb1", Label: common.LocalImagesFSLabel},
			{Name: "sdc1", Label: common.LocalImagesFSLabel},
			{Name: "sdp4", Label: common.IBIStatusFSLabel, FSType: fsTypeExt4},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			saveSeams(t)
			statusDir := useTempStatusPartition(t)

			ctrl := gomock.NewController(t)
			mockOps := ops.NewMockOps(ctrl)
			cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
			cfg := &ibiconfig.IBIPrepareConfig{
				Disconnected:     true,
				Profile:          ibiconfig.ProfileInstallOnly,
				LocalImagesPath:  "/var/mnt/ibi",
				SeedImage:        testSeedImageRef(),
				SeedVersion:      "4.15.0",
				InstallationDisk: "/dev/sda",
			}
			ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, cleanupMock, cfg)

			// The same block-device list serves p4 discovery (first) and p2 discovery (second).
			mockOps.EXPECT().ListBlockDevices().Return(tc.devices, nil).Times(2)
			mockOps.EXPECT().Mount("sdp4", statusDir).Return(nil).Times(1)
			// Fail-fast before any disk write: no p2 mount, no cleanup of the install disk.
			mockOps.EXPECT().MountReadOnly(gomock.Any(), gomock.Any()).Times(0)
			cleanupMock.EXPECT().CleanupInstallDevice(gomock.Any()).Times(0)
			// Failure is finalized: result write-back syncs, p4 is unmounted, node halts powered-on.
			mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(1)
			mockOps.EXPECT().Umount("sdp4").Return(nil).Times(1)
			mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)
			mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

			err := ibi.Run()
			assert.Error(t, err)

			// A failure result is recorded on p4 so a later boot can read how prepare ended.
			result := readPrepareResult(t, statusDir)
			assert.Equal(t, resultFailure, result.Result)
		})
	}
}

// TestManifestValidatedBeforeDiskWrite proves that a missing/malformed manifest or a
// manifest lacking the seed entry fails Run() in preflight -- after the p2 mount but
// before any disk write -- that p2 is unmounted on the way out, and that (because p4 is
// mounted and the guard passes first) the failure is finalized: a failure prepare.json is
// recorded and the node is halted powered-on rather than aborting Run bare.
func TestManifestValidatedBeforeDiskWrite(t *testing.T) {
	cases := []struct {
		name  string
		parse func(string) (*localimages.DigestManifest, error)
	}{
		{
			name:  "missing or malformed manifest",
			parse: func(string) (*localimages.DigestManifest, error) { return nil, fmt.Errorf("boom") },
		},
		{
			name: "seed not present in manifest",
			parse: func(string) (*localimages.DigestManifest, error) {
				return &localimages.DigestManifest{
					Version:       localimages.SourcesManifestVersion,
					OCILayoutPath: "oci",
					Images:        []localimages.ImageEntry{{Name: "other", Digest: "sha256:dead", OCITag: "t"}},
				}, nil
			},
		},
		{
			// A v1 manifest cannot carry the source references the disconnected
			// contract requires, so preflight must reject it before any disk write.
			name: "v1 manifest rejected (sources contract requires v2)",
			parse: func(string) (*localimages.DigestManifest, error) {
				return &localimages.DigestManifest{
					Version:       1,
					OCILayoutPath: "oci",
					Images:        []localimages.ImageEntry{{Name: testSeedName, Digest: testSeedDigest, OCITag: testSeedOCITag}},
				}, nil
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			saveSeams(t)
			statusDir := useTempStatusPartition(t)
			parseDigestManifest = tc.parse
			newImageImporter = func(*logrus.Logger, localimages.StoreConfig) (imageImporter, error) {
				t.Fatal("importer must not be created before manifest validation")
				return nil, nil
			}

			ctrl := gomock.NewController(t)
			mockOps := ops.NewMockOps(ctrl)
			cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
			cfg := &ibiconfig.IBIPrepareConfig{
				Disconnected:        true,
				Profile:             ibiconfig.ProfileInstallOnly,
				LocalImagesPath:     "/var/mnt/ibi",
				ExpectedDigestsPath: "/var/mnt/ibi/manifest.json",
				SeedImage:           testSeedImageRef(),
				SeedVersion:         "4.15.0",
				InstallationDisk:    "/dev/sda",
			}
			ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, cleanupMock, cfg)

			// The same block-device list serves p4 discovery (first) and p2 discovery (second).
			mockOps.EXPECT().ListBlockDevices().
				Return(statusDevices("sdb1", "sdp4"), nil).Times(2)
			mockOps.EXPECT().Mount("sdp4", statusDir).Return(nil).Times(1)
			mockOps.EXPECT().RunInHostNamespace("cat", "/proc/self/mounts").Return("", nil).Times(1)
			mockOps.EXPECT().MountReadOnly("sdb1", cfg.LocalImagesPath).Return(nil).Times(1)
			// No disk write.
			cleanupMock.EXPECT().CleanupInstallDevice(gomock.Any()).Times(0)
			// p2 is unmounted on the error path (deferred unmount); the failure is finalized:
			// sync, p4 unmount, and halt powered-on (never power off).
			mockOps.EXPECT().Umount("sdb1").Return(nil).Times(1)
			mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(1)
			mockOps.EXPECT().Umount("sdp4").Return(nil).Times(1)
			mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)
			mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

			err := ibi.Run()
			assert.Error(t, err)

			// A failure result is recorded on p4 so a later boot can read how prepare ended.
			result := readPrepareResult(t, statusDir)
			assert.Equal(t, resultFailure, result.Result)
		})
	}
}

// TestSeedReferenceMatchesSetupStaterootLookup proves the imported seed is
// addressable as i.config.SeedImage: the reference the importer writes
// (entry.Name@entry.Digest) equals the reference SetupStateroot later resolves
// (i.config.SeedImage, passed to ops.MountImage). A non-digest-pinned SeedImage is
// rejected in preflight.
func TestSeedReferenceMatchesSetupStaterootLookup(t *testing.T) {
	t.Run("resolved and imported seed ref equals the SetupStateroot lookup ref", func(t *testing.T) {
		saveSeams(t)
		parseDigestManifest = func(string) (*localimages.DigestManifest, error) { return testManifest(), nil }

		cfg := &ibiconfig.IBIPrepareConfig{
			Disconnected:        true,
			LocalImagesPath:     "/var/mnt/ibi",
			ExpectedDigestsPath: "/m.json",
			SeedImage:           testSeedImageRef(),
			SeedVersion:         "4.15.0",
		}
		ctrl := gomock.NewController(t)
		mockOps := ops.NewMockOps(ctrl)
		expectPodmanInfo(mockOps)
		ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, nil, cfg)

		manifest, seedEntry, err := ibi.preflightValidateManifest()
		assert.NoError(t, err)
		assert.Equal(t, cfg.SeedImage, seedEntry.Name+"@"+seedEntry.Digest,
			"preflight must resolve the entry whose canonical ref equals SeedImage")

		ff := &importerFactory{}
		newImageImporter = ff.new
		assert.NoError(t, ibi.importSeedToLiveStore(context.Background(), manifest, seedEntry))
		if assert.Len(t, ff.seed.importedOne, 1) {
			imported := ff.seed.importedOne[0]
			assert.Equal(t, cfg.SeedImage, imported.Name+"@"+imported.Digest,
				"the imported seed must be addressable as config.SeedImage (the MountImage arg)")
		}
	})

	t.Run("non-digest-pinned SeedImage is rejected in preflight", func(t *testing.T) {
		saveSeams(t)
		parseDigestManifest = func(string) (*localimages.DigestManifest, error) { return testManifest(), nil }

		cfg := &ibiconfig.IBIPrepareConfig{
			Disconnected:        true,
			LocalImagesPath:     "/var/mnt/ibi",
			ExpectedDigestsPath: "/m.json",
			SeedImage:           testSeedName + ":latest", // a tag, not a digest
			SeedVersion:         "4.15.0",
		}
		ibi := NewIBIPrepare(logrus.New(), nil, nil, nil, nil, cfg)

		_, _, err := ibi.preflightValidateManifest()
		assert.Error(t, err)
	})
}

// TestSeedImportedIntoHostStore asserts the seed is imported into the host
// containers-storage store that ops.MountImage reads -- resolved from the host
// podman's own configuration (driver + graphroot/runroot translated into this
// process's mount namespace), not the Go-resolved default store -- and that the
// importer is closed.
func TestSeedImportedIntoHostStore(t *testing.T) {
	saveSeams(t)
	ff := &importerFactory{}
	newImageImporter = ff.new

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	expectPodmanInfo(mockOps)

	cfg := &ibiconfig.IBIPrepareConfig{Disconnected: true, LocalImagesPath: "/var/mnt/ibi", SeedImage: testSeedImageRef()}
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, nil, cfg)

	manifest := testManifest()
	seedEntry := manifest.Images[0]
	assert.NoError(t, ibi.importSeedToLiveStore(context.Background(), manifest, seedEntry))

	if assert.NotNil(t, ff.seed) {
		assert.Equal(t, wantSeedStoreConfig(), ff.seed.cfg,
			"seed must target the host store resolved from podman info, not the default store")
		assert.Equal(t, []string{cfg.LocalImagesPath}, ff.seed.mediaRoots)
		if assert.Len(t, ff.seed.importedOne, 1) {
			assert.Equal(t, seedEntry, ff.seed.importedOne[0])
		}
		assert.True(t, ff.seed.closed, "seed importer must be closed")
	}
	assert.Nil(t, ff.closure, "the closure store must not be opened for the seed import")
}

// TestSeedImportFailsOnIncompleteHostStoreConfig proves the seed import fails fast
// (before opening a store) when the host podman reports an incomplete store
// configuration, rather than silently falling back to the Go-resolved default store.
func TestSeedImportFailsOnIncompleteHostStoreConfig(t *testing.T) {
	saveSeams(t)
	newImageImporter = func(*logrus.Logger, localimages.StoreConfig) (imageImporter, error) {
		t.Fatal("importer must not be opened when the host store config is incomplete")
		return nil, nil
	}

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	// Missing graphRoot/runRoot in the reported store config.
	mockOps.EXPECT().RunInHostNamespace("podman", "info", "--format", "json").
		Return(`{"store":{"graphDriverName":"overlay"}}`, nil).Times(1)

	cfg := &ibiconfig.IBIPrepareConfig{Disconnected: true, LocalImagesPath: "/var/mnt/ibi", SeedImage: testSeedImageRef()}
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, nil, cfg)

	manifest := testManifest()
	assert.Error(t, ibi.importSeedToLiveStore(context.Background(), manifest, manifest.Images[0]))
}

// TestClosureImportedIntoInstalledGraphroot asserts the closure is imported into the
// installed stateroot's graphroot via an explicit overlay store with a temporary
// runroot that is cleaned up afterwards.
func TestClosureImportedIntoInstalledGraphroot(t *testing.T) {
	saveSeams(t)
	ff := &importerFactory{}
	newImageImporter = ff.new
	origPrefix := common.OstreeDeployPathPrefix
	t.Cleanup(func() { common.OstreeDeployPathPrefix = origPrefix })
	common.OstreeDeployPathPrefix = "/mnt/"

	cfg := &ibiconfig.IBIPrepareConfig{Disconnected: true, LocalImagesPath: "/var/mnt/ibi", SeedVersion: "4.15.0"}
	ibi := NewIBIPrepare(logrus.New(), nil, nil, nil, nil, cfg)

	manifest := testManifest()
	assert.NoError(t, ibi.importClosureToStateroot(context.Background(), manifest))

	osname := common.GetStaterootName(cfg.SeedVersion)
	wantGraphRoot := filepath.Join(common.GetStaterootPath(osname), common.ContainerStoragePath, "storage")

	if assert.NotNil(t, ff.closure) {
		assert.Equal(t, "overlay", ff.closure.cfg.GraphDriverName)
		assert.Equal(t, wantGraphRoot, ff.closure.cfg.GraphRoot,
			"closure must target the installed stateroot graphroot")
		assert.NotEmpty(t, ff.closure.cfg.RunRoot, "a writable temp runroot is required for the second store open")
		assert.Equal(t, 1, ff.closure.importedAll, "the full closure must be imported")
		assert.Equal(t, []string{cfg.LocalImagesPath}, ff.closure.mediaRoots)
		assert.True(t, ff.closure.closed, "closure importer must be closed to release store locks")

		_, statErr := os.Stat(ff.closure.cfg.RunRoot)
		assert.True(t, os.IsNotExist(statErr), "the temporary runroot must be removed after the import")
	}
}

// TestClosureImportRunsOutsideChroot proves the closure import performs no chroot --
// it runs in the live-ISO namespace where the p2 layout is visible, never under the
// /host chroot that precache enters.
func TestClosureImportRunsOutsideChroot(t *testing.T) {
	saveSeams(t)
	ff := &importerFactory{}
	newImageImporter = ff.new
	origPrefix := common.OstreeDeployPathPrefix
	t.Cleanup(func() { common.OstreeDeployPathPrefix = origPrefix })
	common.OstreeDeployPathPrefix = "/mnt/"

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	// No chroot during the closure import.
	mockOps.EXPECT().Chroot(gomock.Any()).Times(0)

	cfg := &ibiconfig.IBIPrepareConfig{Disconnected: true, LocalImagesPath: "/var/mnt/ibi", SeedVersion: "4.15.0"}
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, nil, cfg)

	assert.NoError(t, ibi.importClosureToStateroot(context.Background(), testManifest()))
}

// TestClosureImportRunsBeforePrecache drives Run() with precache enabled but no seed
// info file, so precacheFlow fails. It proves the closure import has already run
// (importedAll == 1) before precache does any work, and that precache never reached a
// chroot before failing (closure import precedes the /host chroot region).
func TestClosureImportRunsBeforePrecache(t *testing.T) {
	saveSeams(t)
	t.Chdir(t.TempDir())
	useTempStatusPartition(t)
	parseDigestManifest = func(string) (*localimages.DigestManifest, error) { return testManifest(), nil }
	ff := &importerFactory{}
	newImageImporter = ff.new
	setupStateroot = func(logr.Logger, ops.Ops, ostreeclient.IClient, rpmostreeclient.IClient, string, string, bool) error {
		return nil
	}

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
	cfg := &ibiconfig.IBIPrepareConfig{
		Disconnected:        true,
		Profile:             ibiconfig.ProfileInstallOnly,
		LocalImagesPath:     "/var/mnt/ibi",
		ExpectedDigestsPath: "/m.json",
		SeedImage:           testSeedImageRef(),
		SeedVersion:         "4.15.0",
		InstallationDisk:    "/dev/sda",
		UseContainersFolder: true,
		PrecacheDisabled:    false,
	}
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, cleanupMock, cfg)

	// p2 and p4 are discovered from the same block-device list (two label matches).
	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("sdb1", "sdb2"), nil).Times(2)
	mockOps.EXPECT().RunInHostNamespace("cat", "/proc/self/mounts").Return("", nil).Times(1)
	mockOps.EXPECT().MountReadOnly("sdb1", cfg.LocalImagesPath).Return(nil).Times(1)
	mockOps.EXPECT().Mount("sdb2", statusPartitionMountDir).Return(nil).Times(1)
	cleanupMock.EXPECT().CleanupInstallDevice(cfg.InstallationDisk).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("coreos-installer", []string{"install", cfg.InstallationDisk}).Return("", nil).Times(1)
	mockOps.EXPECT().SetupContainersFolderCommands().Return(nil).Times(1)
	// seed import resolves the host store from podman info.
	expectPodmanInfo(mockOps)
	// precache fails reading the seed info file before chrooting or pulling.
	mockOps.EXPECT().Chroot(gomock.Any()).Times(0)
	// Failure path: result write-back syncs, both partitions are unmounted, and the node is
	// halted powered-on (never powered off).
	mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(1)
	mockOps.EXPECT().Umount("sdb1").Return(nil).Times(1)
	mockOps.EXPECT().Umount("sdb2").Return(nil).Times(1)
	mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

	err := ibi.Run()
	assert.Error(t, err)
	if assert.NotNil(t, ff.closure) {
		assert.Equal(t, 1, ff.closure.importedAll, "closure must be imported before precache runs")
	}
}

// TestRunDisconnectedOrderingAndUnmountLast drives the full disconnected happy path
// (precache disabled) and asserts the end-to-end ordering: p2 discovery, manifest
// validation, and the mandatory p4 (ibi-status) mount all run before the disk write; the
// seed imports before SetupStateroot; the closure imports after SetupStateroot and before
// the core sync; no podman pull happens; p2 is unmounted after the core sync; and the p4
// result write-back + p4 unmount happen last, after p2 is released. The written
// prepare.json records success.
func TestRunDisconnectedOrderingAndUnmountLast(t *testing.T) {
	saveSeams(t)
	t.Chdir(t.TempDir()) // the synthetic authfile writes to a relative var/tmp path
	statusDir := useTempStatusPartition(t)

	var events []string
	parseDigestManifest = func(string) (*localimages.DigestManifest, error) {
		events = append(events, "parseManifest")
		return testManifest(), nil
	}
	ff := &importerFactory{events: &events}
	newImageImporter = ff.new
	preflightSeedOSTree = func(*IBIPrepare, context.Context, *localimages.DigestManifest, localimages.ImageEntry) error {
		events = append(events, "preflightSeedOSTree")
		return nil
	}
	setupStateroot = func(logr.Logger, ops.Ops, ostreeclient.IClient, rpmostreeclient.IClient, string, string, bool) error {
		events = append(events, "setupStateroot")
		return nil
	}

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)
	cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
	cfg := &ibiconfig.IBIPrepareConfig{
		Disconnected:        true,
		Profile:             ibiconfig.ProfileInstallOnly,
		LocalImagesPath:     "/var/mnt/ibi",
		ExpectedDigestsPath: "/var/mnt/ibi/manifest.json",
		SeedImage:           testSeedImageRef(),
		SeedVersion:         "4.15.0",
		InstallationDisk:    "/dev/sda",
		UseContainersFolder: true,
		PrecacheDisabled:    true,
	}
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, ostreeMock, cleanupMock, cfg)

	// p2 and p4 are discovered from the same block-device list (two label matches).
	mockOps.EXPECT().ListBlockDevices().DoAndReturn(func() ([]ops.BlockDevice, error) {
		events = append(events, "listBlockDevices")
		return statusDevices("sdb1", "sdb2"), nil
	}).Times(2)
	mockOps.EXPECT().RunInHostNamespace("cat", "/proc/self/mounts").Return("", nil).Times(1)
	mockOps.EXPECT().MountReadOnly("sdb1", cfg.LocalImagesPath).DoAndReturn(func(_, _ string) error {
		events = append(events, "mountReadOnlyP2")
		return nil
	}).Times(1)
	mockOps.EXPECT().Mount("sdb2", statusDir).DoAndReturn(func(_, _ string) error {
		events = append(events, "mountP4")
		return nil
	}).Times(1)
	cleanupMock.EXPECT().CleanupInstallDevice(cfg.InstallationDisk).DoAndReturn(func(string) error {
		events = append(events, "cleanupInstallDevice")
		return nil
	}).Times(1)
	mockOps.EXPECT().RunInHostNamespace("coreos-installer", []string{"install", cfg.InstallationDisk}).
		DoAndReturn(func(string, ...string) (string, error) {
			events = append(events, "coreosInstaller")
			return "", nil
		}).Times(1)
	mockOps.EXPECT().SetupContainersFolderCommands().DoAndReturn(func() error {
		events = append(events, "setupContainersFolder")
		return nil
	}).Times(1)
	// seed import resolves the host store from podman info before SetupStateroot.
	expectPodmanInfo(mockOps)
	ostreeMock.EXPECT().Undeploy(rhcosOstreeIndex).DoAndReturn(func(int) error {
		events = append(events, "undeploy")
		return nil
	}).Times(1)
	// The core flow syncs once; the result write-back syncs again before the terminal action.
	mockOps.EXPECT().RunInHostNamespace("sync").DoAndReturn(func(string, ...string) (string, error) {
		events = append(events, "sync")
		return "", nil
	}).Times(2)
	mockOps.EXPECT().Umount("sdb1").DoAndReturn(func(string) error {
		events = append(events, "umountP2")
		return nil
	}).Times(1)
	mockOps.EXPECT().Umount("sdb2").DoAndReturn(func(string) error {
		events = append(events, "umountP4")
		return nil
	}).Times(1)

	err := ibi.Run()
	assert.NoError(t, err)

	// No podman pull on the disconnected happy path: any such call would be an
	// unexpected call on the strict mock controller and fail the test. Assert the
	// relative ordering of the recorded events.
	for _, name := range []string{
		"listBlockDevices", "mountReadOnlyP2", "parseManifest", "mountP4", "preflightSeedOSTree", "cleanupInstallDevice",
		"importSeed", "setupStateroot", "importClosure", "verifyClosure", "sync", "umountP2", "umountP4",
	} {
		assert.GreaterOrEqual(t, idxOf(events, name), 0, "missing event %q", name)
	}
	// Preconditions (p2 discovery + manifest + mandatory p4 mount) run before the disk write.
	assert.Less(t, idxOf(events, "mountReadOnlyP2"), idxOf(events, "cleanupInstallDevice"))
	assert.Less(t, idxOf(events, "parseManifest"), idxOf(events, "cleanupInstallDevice"))
	assert.Less(t, idxOf(events, "mountP4"), idxOf(events, "cleanupInstallDevice"))
	assert.Less(t, idxOf(events, "preflightSeedOSTree"), idxOf(events, "cleanupInstallDevice"))
	// Seed imports before stateroot; closure imports after stateroot.
	assert.Less(t, idxOf(events, "importSeed"), idxOf(events, "setupStateroot"))
	assert.Less(t, idxOf(events, "setupStateroot"), idxOf(events, "importClosure"))
	// Self-check runs after the closure import (precache precedes it, disabled here)
	// and before the core sync; p2 is unmounted after the sync; the p4 result
	// write-back and p4 unmount are the last actions.
	assert.Less(t, idxOf(events, "importClosure"), idxOf(events, "verifyClosure"))
	assert.Less(t, idxOf(events, "verifyClosure"), idxOf(events, "sync"))
	assert.Less(t, idxOf(events, "sync"), idxOf(events, "umountP2"))
	assert.Less(t, idxOf(events, "umountP2"), idxOf(events, "umountP4"))
	assert.Equal(t, "umountP4", events[len(events)-1], "p4 unmount must be the last action")

	// The result write-back recorded a successful prepare at the p4 root.
	result := readPrepareResult(t, statusDir)
	assert.Equal(t, prepareResultPhase, result.Phase)
	assert.Equal(t, ibiconfig.ProfileInstallOnly, result.Profile)
	assert.Equal(t, resultSuccess, result.Result)
	assert.Equal(t, reentryNone, result.Reentry)
	assert.Nil(t, result.Error)
	if assert.NotNil(t, result.Checks) {
		assert.Equal(t, len(testManifest().Images), result.Checks.ImagesExpected)
		assert.Equal(t, result.Checks.ImagesExpected, result.Checks.ImagesPresent)
	}

	// Closure import and self-check both targeted the installed stateroot graphroot
	// under /mnt (the same store, opened once each).
	osname := common.GetStaterootName(cfg.SeedVersion)
	wantGraphRoot := filepath.Join(common.GetStaterootPath(osname), common.ContainerStoragePath, "storage")
	if assert.NotNil(t, ff.closure) {
		assert.Equal(t, wantGraphRoot, ff.closure.cfg.GraphRoot)
	}
	if assert.NotNil(t, ff.verify, "the self-check must open the installed stateroot store") {
		assert.Equal(t, wantGraphRoot, ff.verify.cfg.GraphRoot)
		assert.Equal(t, 1, ff.verify.verifiedAll, "the self-check must verify the manifest exactly once")
	}
}

func TestSeedPreflightFailurePreservesInstallationDisk(t *testing.T) {
	stubDisconnectedSeams(t)
	statusDir := useTempStatusPartition(t)
	preflightSeedOSTree = func(*IBIPrepare, context.Context, *localimages.DigestManifest, localimages.ImageEntry) error {
		return errors.New("object checksum mismatch")
	}

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
	cfg := disconnectedCfg(ibiconfig.ProfileInstallOnly)
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, cleanupMock, cfg)

	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("sdb1", "sdb2"), nil).Times(2)
	mockOps.EXPECT().RunInHostNamespace("cat", "/proc/self/mounts").Return("", nil)
	mockOps.EXPECT().MountReadOnly("sdb1", cfg.LocalImagesPath).Return(nil)
	mockOps.EXPECT().Mount("sdb2", statusDir).Return(nil)
	mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil)
	mockOps.EXPECT().Umount("sdb1").Return(nil)
	mockOps.EXPECT().Umount("sdb2").Return(nil)
	mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil)
	cleanupMock.EXPECT().CleanupInstallDevice(gomock.Any()).Times(0)
	mockOps.EXPECT().RunInHostNamespace("coreos-installer", gomock.Any()).Times(0)

	err := ibi.Run()
	assert.ErrorContains(t, err, "seed preflight failed: object checksum mismatch")
	result := readPrepareResult(t, statusDir)
	assert.Equal(t, resultFailure, result.Result)
}

// TestRunReturnsErrorOnFailedFinalUnmount proves that when the final p2 unmount
// keeps failing, Run surfaces the failure as its result (regardless of Shutdown)
// rather than reporting success with p2 still mounted, the deferred cleanup retries
// the unmount on the way out, and the node is halted powered-on (never powered off)
// with a failure result recorded on p4.
func TestRunReturnsErrorOnFailedFinalUnmount(t *testing.T) {
	saveSeams(t)
	t.Chdir(t.TempDir())
	statusDir := useTempStatusPartition(t)

	parseDigestManifest = func(string) (*localimages.DigestManifest, error) { return testManifest(), nil }
	newImageImporter = (&importerFactory{}).new
	setupStateroot = func(logr.Logger, ops.Ops, ostreeclient.IClient, rpmostreeclient.IClient, string, string, bool) error {
		return nil
	}
	// This test targets the final p2 unmount failure; stub the self-contained read-only store
	// handoff to a no-op so the run reaches that step (the handoff itself is covered separately).
	persistReadOnlyStoreFn = func(*IBIPrepare, context.Context, *localimages.DigestManifest) error { return nil }

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)
	cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
	cfg := &ibiconfig.IBIPrepareConfig{
		Disconnected:        true,
		Profile:             ibiconfig.ProfileSelfContained,
		LocalImagesPath:     "/var/mnt/ibi",
		ExpectedDigestsPath: "/var/mnt/ibi/manifest.json",
		SeedImage:           testSeedImageRef(),
		SeedVersion:         "4.15.0",
		InstallationDisk:    "/dev/sda",
		UseContainersFolder: true,
		PrecacheDisabled:    true,
		Shutdown:            true, // even with Shutdown set, a failure must halt powered-on, not power off
	}
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, ostreeMock, cleanupMock, cfg)

	// p2 ("sdb2") and p4 ("sdb3") are discovered from the same block-device list.
	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("sdb2", "sdb3"), nil).Times(2)
	mockOps.EXPECT().RunInHostNamespace("cat", "/proc/self/mounts").Return("", nil).Times(1)
	mockOps.EXPECT().MountReadOnly("sdb2", cfg.LocalImagesPath).Return(nil).Times(1)
	mockOps.EXPECT().Mount("sdb3", statusDir).Return(nil).Times(1)
	cleanupMock.EXPECT().CleanupInstallDevice(cfg.InstallationDisk).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("coreos-installer", []string{"install", cfg.InstallationDisk}).Return("", nil).Times(1)
	mockOps.EXPECT().SetupContainersFolderCommands().Return(nil).Times(1)
	// seed import resolves the host store from podman info.
	expectPodmanInfo(mockOps)
	ostreeMock.EXPECT().Undeploy(rhcosOstreeIndex).Return(nil).Times(1)
	// The core flow syncs once; the failure result write-back syncs again.
	mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(2)
	// Explicit final p2 unmount fails; the deferred cleanup then retries it (also fails),
	// so p2 Umount is attempted twice. p4 is unmounted once in the result write-back.
	mockOps.EXPECT().Umount("sdb2").Return(fmt.Errorf("device busy")).Times(2)
	mockOps.EXPECT().Umount("sdb3").Return(nil).Times(1)
	// Failure halts powered-on; shutdown/power-off must never be invoked.
	mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

	err := ibi.Run()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unmount local images")

	// A failure result is recorded on p4 (so a later boot can read how prepare ended).
	result := readPrepareResult(t, statusDir)
	assert.Equal(t, resultFailure, result.Result)
	assert.Equal(t, reentryNone, result.Reentry)
	if assert.NotNil(t, result.Error) {
		assert.Contains(t, *result.Error, "unmount local images")
	}
}

// TestConnectedFlowUnchanged proves the connected flow is untouched: no p2
// discovery/mount/unmount, no manifest parse, no local import, and the seed is still
// pulled from the network with podman.
func TestConnectedFlowUnchanged(t *testing.T) {
	saveSeams(t)
	t.Chdir(t.TempDir())
	parseDigestManifest = func(string) (*localimages.DigestManifest, error) {
		t.Fatal("parseDigestManifest must not be called in the connected flow")
		return nil, nil
	}
	newImageImporter = func(*logrus.Logger, localimages.StoreConfig) (imageImporter, error) {
		t.Fatal("importer must not be created in the connected flow")
		return nil, nil
	}
	setupStateroot = func(logr.Logger, ops.Ops, ostreeclient.IClient, rpmostreeclient.IClient, string, string, bool) error {
		return nil
	}

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)
	cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
	cfg := &ibiconfig.IBIPrepareConfig{
		Disconnected:        false,
		SeedImage:           "quay.io/seed@sha256:abc",
		SeedVersion:         "4.15.0",
		InstallationDisk:    "/dev/sda",
		UseContainersFolder: true,
		PrecacheDisabled:    true,
	}
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, ostreeMock, cleanupMock, cfg)

	// No disconnected-only operations.
	mockOps.EXPECT().ListBlockDevices().Times(0)
	mockOps.EXPECT().MountReadOnly(gomock.Any(), gomock.Any()).Times(0)
	mockOps.EXPECT().Umount(gomock.Any()).Times(0)
	// Disk preparation + network seed pull, as before.
	cleanupMock.EXPECT().CleanupInstallDevice(cfg.InstallationDisk).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("coreos-installer", []string{"install", cfg.InstallationDisk}).Return("", nil).Times(1)
	mockOps.EXPECT().SetupContainersFolderCommands().Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("podman", "pull", "--authfile", common.IBIPullSecretFilePath, cfg.SeedImage).
		Return("", nil).Times(1)
	ostreeMock.EXPECT().Undeploy(rhcosOstreeIndex).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(1)

	err := ibi.Run()
	assert.NoError(t, err)
}

// --- Disconnected precache verification set (CNF-27161) ---

// writeContainersList writes a seed containers.list with the given references to a
// temp file and returns its path. PathOutsideChroot is a no-op in unit tests (no
// /host), so disconnectedPrecacheList reads exactly this file.
func writeContainersList(t *testing.T, refs ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "containers.list")
	assert.NoError(t, os.WriteFile(p, []byte(strings.Join(refs, "\n")+"\n"), 0o600))
	return p
}

// TestDisconnectedPrecacheSetEqualsManifestScope proves the disconnected precache
// presence set is one canonical name@digest per manifest entry -- the full
// profile-scoped closure -- not just the seed's containers.list. The seed list here
// references only the seed image, yet the returned set still includes the workload
// image (an entry absent from containers.list), proving manifest extras (e.g.
// ExtraPrecacheImages, recert, or the self-contained p3 site closure) are covered.
func TestDisconnectedPrecacheSetEqualsManifestScope(t *testing.T) {
	manifest := testManifest()
	// containers.list references only the seed (an exact source on its manifest
	// entry); the workload entry is deliberately NOT listed by the seed.
	listFile := writeContainersList(t, testSeedImageRef())
	ibi := &IBIPrepare{log: logrus.New(), config: &ibiconfig.IBIPrepareConfig{Disconnected: true}}

	got, err := ibi.disconnectedPrecacheList(listFile, manifest)
	assert.NoError(t, err)

	// One name@digest per manifest entry, in manifest order.
	want := make([]string, 0, len(manifest.Images))
	for _, e := range manifest.Images {
		want = append(want, e.Name+"@"+e.Digest)
	}
	assert.Equal(t, want, got)
	assert.Len(t, got, 2)
	// The workload image is not in the seed's containers.list but is still precached.
	assert.Contains(t, got, manifest.Images[1].Name+"@"+manifest.Images[1].Digest,
		"a manifest image absent from the seed containers.list must still be in the precache set")
}

// TestDisconnectedPrecacheListNilManifest proves a disconnected precache without a
// manifest fails fast rather than silently verifying nothing.
func TestDisconnectedPrecacheListNilManifest(t *testing.T) {
	ibi := &IBIPrepare{log: logrus.New(), config: &ibiconfig.IBIPrepareConfig{Disconnected: true}}
	listFile := writeContainersList(t, testSeedImageRef())

	_, err := ibi.disconnectedPrecacheList(listFile, nil)
	assert.ErrorContains(t, err, "requires the on-media digest manifest")
}

// TestDisconnectedPrecacheListRejectsUnresolvedSeedImage proves a seed requirement
// the imported closure does not cover fails prepare instead of precaching only the
// manifest and leaving the node short an image.
func TestDisconnectedPrecacheListRejectsUnresolvedSeedImage(t *testing.T) {
	manifest := testManifest()
	missingDigest := "sha256:9999999999999999999999999999999999999999999999999999999999999999"
	listFile := writeContainersList(t,
		testSeedImageRef(),
		"quay.io/openshift-release-dev/extra@"+missingDigest, // not in the manifest
	)
	ibi := &IBIPrepare{log: logrus.New(), config: &ibiconfig.IBIPrepareConfig{Disconnected: true}}

	_, err := ibi.disconnectedPrecacheList(listFile, manifest)
	assert.ErrorContains(t, err, "does not cover")
	assert.ErrorContains(t, err, "quay.io/openshift-release-dev/extra@"+missingDigest)
}

// TestDisconnectedPrecacheSkipsHostPodmanCheck proves a disconnected prepare's precache step
// keeps the seed-to-manifest coverage check but does NOT run the live-ISO podman presence
// check or chroot into /host. Image residency is verified directly in the installed stateroot
// store by the mandatory self-check (verifyImportedClosure) instead, so a disconnected prepare
// does not depend on /host existing at all (a live ISO without /host). The strict mock
// controller fails the test on any unexpected host-namespace call, so the absence of a podman
// presence check is asserted by there being no such expectation.
func TestDisconnectedPrecacheSkipsHostPodmanCheck(t *testing.T) {
	manifest := testManifest()

	t.Run("covered seed list: coverage check passes, no chroot, no podman check", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		mockOps := ops.NewMockOps(ctrl)
		// A disconnected precache must not chroot into /host; the live-ISO podman presence
		// check (and the host-namespace commands it needs) are gone.
		mockOps.EXPECT().Chroot(gomock.Any()).Times(0)

		ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, nil,
			&ibiconfig.IBIPrepareConfig{Disconnected: true, PrecacheDisabled: false})
		// containers.list references the seed, which is an exact source on a manifest entry.
		listFile := writeContainersList(t, testSeedImageRef())

		assert.NoError(t, ibi.precacheFlow(listFile, "", defaultRegistriesConfFile, manifest))
	})

	t.Run("uncovered seed list: the coverage check still fails the prepare", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		mockOps := ops.NewMockOps(ctrl)
		mockOps.EXPECT().Chroot(gomock.Any()).Times(0)

		ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, nil,
			&ibiconfig.IBIPrepareConfig{Disconnected: true, PrecacheDisabled: false})
		// An image the manifest does not cover must still fail prepare at the retained
		// seed-to-manifest coverage check, before any residency verification.
		missing := "quay.io/openshift-release-dev/extra@sha256:9999999999999999999999999999999999999999999999999999999999999999"
		listFile := writeContainersList(t, testSeedImageRef(), missing)

		err := ibi.precacheFlow(listFile, "", defaultRegistriesConfFile, manifest)
		assert.ErrorContains(t, err, "does not cover")
	})
}

// TestVerifyManifestCoversSeed exercises exact source-reference matching: a seed
// reference is covered only when it equals a source listed on exactly one manifest
// entry (whose digest the fail-fast closure import necessarily imported). A
// name-only or wrong-tag match does not cover, and a source shared by two entries
// is ambiguous. The wrong-tag/covered-tag pair (same repository, two tags, one
// whose digest is absent) is called out explicitly below.
func TestVerifyManifestCoversSeed(t *testing.T) {
	const workloadTag = "quay.io/openshift-release-dev/ocp-v4.0-art-dev:4.15.0"
	workload := testManifest().Images[1]

	// ambiguousManifest lists the same source on two entries, so the reference
	// cannot be bound to a single imported digest.
	ambiguousManifest := &localimages.DigestManifest{
		Version:       localimages.SourcesManifestVersion,
		OCILayoutPath: "oci",
		Images: []localimages.ImageEntry{
			{Name: testSeedName, Digest: testSeedDigest, OCITag: "a", Sources: []string{testSeedName + ":4.15.0"}},
			{Name: testSeedName, Digest: "sha256:3333333333333333333333333333333333333333333333333333333333333333", OCITag: "b", Sources: []string{testSeedName + ":4.15.0"}},
		},
	}

	// twoTagManifest covers exactly repo:4.15.0 (imported as testSeedDigest). A
	// request for repo:4.16.0 -- a different tag of the same repository whose digest
	// is not in the manifest -- must fail rather than bind to the covered digest.
	twoTagManifest := &localimages.DigestManifest{
		Version:       localimages.SourcesManifestVersion,
		OCILayoutPath: "oci",
		Images: []localimages.ImageEntry{
			{Name: testSeedName, Digest: testSeedDigest, OCITag: "ocp-release", Sources: []string{testSeedName + ":4.15.0"}},
		},
	}

	for _, tc := range []struct {
		name     string
		refs     []string
		manifest *localimages.DigestManifest
		wantErr  string
	}{
		{
			name: "digest-pinned and tag sources covered by exact match",
			refs: []string{testSeedImageRef(), workloadTag},
		},
		{
			name: "covered tag (requested tag is an exact source)",
			refs: []string{testSeedName + ":4.15.0"},
			// Only the seed is referenced here; the workload is not required.
			manifest: twoTagManifest,
		},
		{
			name:     "wrong tag of the same repository is uncovered (digest absent)",
			refs:     []string{testSeedName + ":4.16.0"},
			manifest: twoTagManifest,
			wantErr:  "does not cover",
		},
		{
			name:    "bare name is not covered by a tagged source",
			refs:    []string{testSeedName},
			wantErr: "does not cover",
		},
		{
			name:    "tag ref with unknown name is uncovered",
			refs:    []string{"quay.io/openshift-release-dev/unknown:4.15.0"},
			wantErr: "does not cover",
		},
		{
			name:    "registry-rewritten source is uncovered (not an exact source)",
			refs:    []string{"mirror.local:5000/openshift-release-dev/ocp-v4.0-art-dev@" + workload.Digest},
			wantErr: "does not cover",
		},
		{
			name:     "reference listed on multiple entries is ambiguous",
			refs:     []string{testSeedName + ":4.15.0"},
			manifest: ambiguousManifest,
			wantErr:  "ambiguously",
		},
		{
			name:    "digest-pinned ref with unknown digest is uncovered",
			refs:    []string{testSeedName + "@sha256:0000000000000000000000000000000000000000000000000000000000000000"},
			wantErr: "does not cover",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := tc.manifest
			if manifest == nil {
				manifest = testManifest()
			}
			err := verifyManifestCoversSeed(tc.refs, manifest)
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// --- Mandatory image self-check gate (CNF-27162) ---

// TestSelfCheckPassesWhenAllPresent drives the disconnected happy path (precache
// disabled) and proves that when the self-check reports every manifest image
// resident, Run() proceeds through the terminal action: the manifest is verified
// exactly once in the installed stateroot store, the final sync runs, p2 is
// unmounted and the node is shut down.
func TestSelfCheckPassesWhenAllPresent(t *testing.T) {
	saveSeams(t)
	t.Chdir(t.TempDir())
	parseDigestManifest = func(string) (*localimages.DigestManifest, error) { return testManifest(), nil }
	ff := &importerFactory{} // verifyErr nil => self-check passes
	newImageImporter = ff.new
	setupStateroot = func(logr.Logger, ops.Ops, ostreeclient.IClient, rpmostreeclient.IClient, string, string, bool) error {
		return nil
	}

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)
	cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
	cfg := &ibiconfig.IBIPrepareConfig{
		Disconnected:        true,
		LocalImagesPath:     "/var/mnt/ibi",
		ExpectedDigestsPath: "/var/mnt/ibi/manifest.json",
		SeedImage:           testSeedImageRef(),
		SeedVersion:         "4.15.0",
		InstallationDisk:    "/dev/sda",
		UseContainersFolder: true,
		PrecacheDisabled:    true,
		Shutdown:            true, // prove the terminal action is reached on a passing self-check
	}
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, ostreeMock, cleanupMock, cfg)

	statusDir := useTempStatusPartition(t)
	// p2 (local images) and p4 (ibi-status) are discovered from the same block-device list.
	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("sdb1", "sdb2"), nil).Times(2)
	mockOps.EXPECT().RunInHostNamespace("cat", "/proc/self/mounts").Return("", nil).Times(1)
	mockOps.EXPECT().MountReadOnly("sdb1", cfg.LocalImagesPath).Return(nil).Times(1)
	mockOps.EXPECT().Mount("sdb2", statusDir).Return(nil).Times(1)
	cleanupMock.EXPECT().CleanupInstallDevice(cfg.InstallationDisk).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("coreos-installer", []string{"install", cfg.InstallationDisk}).Return("", nil).Times(1)
	mockOps.EXPECT().SetupContainersFolderCommands().Return(nil).Times(1)
	expectPodmanInfo(mockOps)
	ostreeMock.EXPECT().Undeploy(rhcosOstreeIndex).Return(nil).Times(1)
	// The core flow syncs once; the result write-back syncs again before the terminal action.
	mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(2)
	mockOps.EXPECT().Umount("sdb1").Return(nil).Times(1)
	mockOps.EXPECT().Umount("sdb2").Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("shutdown", "now").Return("", nil).Times(1)

	err := ibi.Run()
	assert.NoError(t, err)
	if assert.NotNil(t, ff.verify, "the self-check must open the installed stateroot store") {
		assert.Equal(t, 1, ff.verify.verifiedAll, "the self-check must verify the manifest exactly once")
	}
	// The disconnected flow records a successful prepare on the p4 partition.
	result := readPrepareResult(t, statusDir)
	assert.Equal(t, resultSuccess, result.Result)
}

// TestSelfContainedPivotIssuedAfterSuccessFlush drives the full disconnected happy path for
// the self-contained profile (precache disabled) and proves the end-to-end terminal action:
// prepare.json: success is written and flushed (the finalize sync) to p4 and p4 is unmounted
// BEFORE the pivot reboot is issued, so the pivot is necessarily issued after the success
// record is durable. The node is never powered off (no shutdown now). Because a reboot never
// returns control in a unit test, the grace period (set to zero) elapses and the pivot is
// detected as not-taken: p4 is re-mounted, the result is amended with pivot: failed
// (preserving result: success), and the node halts powered-on via the emergency target.
func TestSelfContainedPivotIssuedAfterSuccessFlush(t *testing.T) {
	stubDisconnectedSeams(t)
	statusDir := useTempStatusPartition(t)
	pivotRebootTimeout = 0 // do not block on the real reboot grace period
	// Self-contained disconnected media also persists the read-only store before the final sync
	// (see runPreparation); stub it to a no-op so this test isolates the pivot terminal action.
	persistReadOnlyStoreFn = func(*IBIPrepare, context.Context, *localimages.DigestManifest) error { return nil }

	var events []string
	record := func(name string) func(string, ...string) (string, error) {
		return func(string, ...string) (string, error) { events = append(events, name); return "", nil }
	}
	// lastIdxOf returns the index of the last occurrence of name, so the ordering assertion
	// below keys off the finalize success-flush sync (the second sync) rather than the earlier
	// core-flow sync.
	lastIdxOf := func(name string) int {
		idx := -1
		for i, e := range events {
			if e == name {
				idx = i
			}
		}
		return idx
	}

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)
	cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
	cfg := disconnectedCfg(ibiconfig.ProfileSelfContained)
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, ostreeMock, cleanupMock, cfg)

	// p2 (local images) and p4 (ibi-status) are discovered from the same block-device list;
	// p4 is mounted for the write-back and re-mounted once more by the pivot-failure amend.
	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("sdb1", "sdb2"), nil).Times(2)
	mockOps.EXPECT().RunInHostNamespace("cat", "/proc/self/mounts").Return("", nil).Times(1)
	mockOps.EXPECT().MountReadOnly("sdb1", cfg.LocalImagesPath).Return(nil).Times(1)
	mockOps.EXPECT().Mount("sdb2", statusDir).Return(nil).Times(2)
	cleanupMock.EXPECT().CleanupInstallDevice(cfg.InstallationDisk).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("coreos-installer", []string{"install", cfg.InstallationDisk}).Return("", nil).Times(1)
	mockOps.EXPECT().SetupContainersFolderCommands().Return(nil).Times(1)
	expectPodmanInfo(mockOps)
	ostreeMock.EXPECT().Undeploy(rhcosOstreeIndex).Return(nil).Times(1)
	// Record the two commands whose ordering proves "flush before pivot": the finalize sync
	// (the success flush) and the pivot reboot. The core flow also syncs once.
	mockOps.EXPECT().RunInHostNamespace("sync").DoAndReturn(record("sync")).Times(2)
	mockOps.EXPECT().RunInHostNamespace("systemd-run", "--unit", "lca-ibi-pivot",
		"--description", gomock.Any(), "systemctl", "reboot").DoAndReturn(record("pivotReboot")).Times(1)
	mockOps.EXPECT().Umount("sdb1").Return(nil).Times(1)
	mockOps.EXPECT().Umount("sdb2").Return(nil).Times(2)
	mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

	err := ibi.Run()
	assert.Error(t, err)
	assert.ErrorContains(t, err, "did not reboot into the installed disk")

	// The pivot was issued only after the success record was flushed to p4: the finalize
	// success-flush sync (the last sync) must precede the pivot reboot.
	assert.GreaterOrEqual(t, idxOf(events, "pivotReboot"), 0, "the pivot reboot must be issued")
	assert.Less(t, lastIdxOf("sync"), idxOf(events, "pivotReboot"),
		"prepare.json: success must be flushed (finalize sync) before the pivot is issued")

	// The record is a successful prepare amended with pivot: failed (result preserved).
	result := readPrepareResult(t, statusDir)
	assert.Equal(t, resultSuccess, result.Result)
	assert.Equal(t, pivotFailed, result.Pivot)
}

// TestSelfCheckFailsOnMissingImage proves the self-check is a hard gate before the
// success terminal action: when VerifyAll reports a missing image, Run() returns that
// error and never reaches postDeployment, cleanupRhcosSysroot (ostree undeploy), the
// core sync, or a power-off shutdown. The failure is still finalized on the ibi-status
// (p4) partition -- a failure result is written, the finalize sync runs, p2 and p4 are
// unmounted -- and the node is halted powered-on (never powered off) even though
// Shutdown is set.
func TestSelfCheckFailsOnMissingImage(t *testing.T) {
	saveSeams(t)
	t.Chdir(t.TempDir())
	parseDigestManifest = func(string) (*localimages.DigestManifest, error) { return testManifest(), nil }
	ff := &importerFactory{verifyErr: fmt.Errorf("image quay.io/x@sha256:dead missing from the store")}
	newImageImporter = ff.new
	setupStateroot = func(logr.Logger, ops.Ops, ostreeclient.IClient, rpmostreeclient.IClient, string, string, bool) error {
		return nil
	}

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)
	cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
	cfg := &ibiconfig.IBIPrepareConfig{
		Disconnected:        true,
		LocalImagesPath:     "/var/mnt/ibi",
		ExpectedDigestsPath: "/var/mnt/ibi/manifest.json",
		SeedImage:           testSeedImageRef(),
		SeedVersion:         "4.15.0",
		InstallationDisk:    "/dev/sda",
		UseContainersFolder: true,
		PrecacheDisabled:    true,
		Shutdown:            true, // must NOT be honored: the gate fails before any terminal action
	}
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, ostreeMock, cleanupMock, cfg)

	statusDir := useTempStatusPartition(t)
	// p2 (local images) and p4 (ibi-status) are discovered from the same block-device list.
	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("sdb1", "sdb2"), nil).Times(2)
	mockOps.EXPECT().RunInHostNamespace("cat", "/proc/self/mounts").Return("", nil).Times(1)
	mockOps.EXPECT().MountReadOnly("sdb1", cfg.LocalImagesPath).Return(nil).Times(1)
	mockOps.EXPECT().Mount("sdb2", statusDir).Return(nil).Times(1)
	cleanupMock.EXPECT().CleanupInstallDevice(cfg.InstallationDisk).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("coreos-installer", []string{"install", cfg.InstallationDisk}).Return("", nil).Times(1)
	mockOps.EXPECT().SetupContainersFolderCommands().Return(nil).Times(1)
	expectPodmanInfo(mockOps)
	// The gate fails before postDeployment / cleanupRhcosSysroot, so there is no ostree
	// undeploy and no core sync. The failure is still finalized on p4: the failure result is
	// written, the finalize sync runs, p2 and p4 are unmounted, and the node is halted
	// powered-on (never powered off) despite Shutdown being set.
	ostreeMock.EXPECT().Undeploy(gomock.Any()).Times(0)
	mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(1)
	mockOps.EXPECT().Umount("sdb1").Return(nil).Times(1)
	mockOps.EXPECT().Umount("sdb2").Return(nil).Times(1)
	mockOps.EXPECT().SystemctlAction("isolate", emergencyTarget).Return("", nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

	err := ibi.Run()
	assert.Error(t, err)
	assert.ErrorContains(t, err, "image self-check failed")
	if assert.NotNil(t, ff.verify, "the self-check must open the installed stateroot store") {
		assert.Equal(t, 1, ff.verify.verifiedAll)
	}
	// A failure result is recorded on p4 so a later boot can read how prepare ended.
	result := readPrepareResult(t, statusDir)
	assert.Equal(t, resultFailure, result.Result)
}

// TestVerifyImportedClosureInstalledStore proves the mandatory image self-check verifies
// residency directly in the installed stateroot store (not the live ISO's /host store): it
// opens the installed graphroot (under the ostree deploy prefix) with an overlay driver and a
// temporary runroot that is cleaned up afterwards, calls VerifyAll on the manifest exactly
// once, and closes the importer. A VerifyAll error (a missing/unresolvable image) is surfaced
// to the caller.
func TestVerifyImportedClosureInstalledStore(t *testing.T) {
	origPrefix := common.OstreeDeployPathPrefix
	t.Cleanup(func() { common.OstreeDeployPathPrefix = origPrefix })
	common.OstreeDeployPathPrefix = "/mnt/"
	osname := common.GetStaterootName("4.15.0")
	wantGraphRoot := filepath.Join(common.GetStaterootPath(osname), common.ContainerStoragePath, "storage")

	t.Run("all images resident: verifies once in the installed store", func(t *testing.T) {
		saveSeams(t)
		ff := &importerFactory{}
		newImageImporter = ff.new
		ibi := NewIBIPrepare(logrus.New(), nil, nil, nil, nil,
			&ibiconfig.IBIPrepareConfig{Disconnected: true, SeedVersion: "4.15.0"})

		assert.NoError(t, ibi.verifyImportedClosure(context.Background(), testManifest()))

		// Called standalone, the single installed-store open lands in the factory's first
		// ostree-deploy slot (closure); it is the importer the self-check verified against.
		if assert.NotNil(t, ff.closure, "the self-check must open the installed stateroot store") {
			assert.Equal(t, "overlay", ff.closure.cfg.GraphDriverName)
			assert.Equal(t, wantGraphRoot, ff.closure.cfg.GraphRoot,
				"the self-check must read the installed stateroot graphroot, not the host store")
			assert.NotEmpty(t, ff.closure.cfg.RunRoot, "a writable temp runroot is required for the store open")
			assert.Equal(t, 1, ff.closure.verifiedAll, "the manifest must be verified exactly once")
			assert.True(t, ff.closure.closed, "the self-check importer must be closed")
			_, statErr := os.Stat(ff.closure.cfg.RunRoot)
			assert.True(t, os.IsNotExist(statErr), "the temporary runroot must be removed after the self-check")
		}
	})

	t.Run("missing image: VerifyAll error fails the self-check", func(t *testing.T) {
		saveSeams(t)
		ff := &importerFactory{verifyErr: fmt.Errorf("image quay.io/x@sha256:dead missing from the store")}
		newImageImporter = ff.new
		ibi := NewIBIPrepare(logrus.New(), nil, nil, nil, nil,
			&ibiconfig.IBIPrepareConfig{Disconnected: true, SeedVersion: "4.15.0"})

		err := ibi.verifyImportedClosure(context.Background(), testManifest())
		assert.ErrorContains(t, err, "verifying imported closure")
	})
}
