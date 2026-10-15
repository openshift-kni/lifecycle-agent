package ibi_preparation

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
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

func testManifest() *localimages.DigestManifest {
	return &localimages.DigestManifest{
		Version:       1,
		OCILayoutPath: "oci",
		Images: []localimages.ImageEntry{
			{Name: testSeedName, Digest: testSeedDigest, OCITag: testSeedOCITag},
			{
				Name:   "quay.io/openshift-release-dev/ocp-v4.0-art-dev",
				Digest: "sha256:2222222222222222222222222222222222222222222222222222222222222222",
				OCITag: "workload",
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
	mediaRoots  []string
	importErr   error
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

func (f *fakeImporter) Close() error {
	f.closed = true
	return nil
}

// importerFactory routes newImageImporter calls to the seed importer (default
// store, empty graphroot) or the closure importer (explicit graphroot).
type importerFactory struct {
	seed      *fakeImporter
	closure   *fakeImporter
	events    *[]string
	importErr error
}

func (ff *importerFactory) new(_ *logrus.Logger, cfg localimages.StoreConfig) (imageImporter, error) {
	imp := &fakeImporter{cfg: cfg, events: ff.events, importErr: ff.importErr}
	if cfg.GraphRoot == "" && cfg.GraphDriverName == "" {
		ff.seed = imp
	} else {
		ff.closure = imp
	}
	return imp, nil
}

// saveSeams snapshots the package seams and restores them when the test ends.
func saveSeams(t *testing.T) {
	t.Helper()
	origParse, origNew, origSetup := parseDigestManifest, newImageImporter, setupStateroot
	t.Cleanup(func() {
		parseDigestManifest = origParse
		newImageImporter = origNew
		setupStateroot = origSetup
	})
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
// read-only mount and returns an idempotent unmount closure.
func TestMountLocalImages(t *testing.T) {
	const localImagesPath = "/var/mnt/ibi-images"

	cases := []struct {
		name       string
		devices    []ops.BlockDevice
		wantDevice string // device mountLocalImages should select (empty => expect error)
		expectErr  bool
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
			wantDevice: "sdb2",
		},
		{
			name: "label on a whole device proceeds to MountReadOnly",
			devices: []ops.BlockDevice{
				{Name: "sda", Label: "root"},
				{Name: "sdb", Label: common.LocalImagesFSLabel},
			},
			wantDevice: "sdb",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			mockOps := ops.NewMockOps(ctrl)
			ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, nil,
				&ibiconfig.IBIPrepareConfig{LocalImagesPath: localImagesPath})

			mockOps.EXPECT().ListBlockDevices().Return(tc.devices, nil).Times(1)
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

// TestLocalImagesDiscoveryPreconditionFailsBeforeDiskWrite proves that a failed p2
// discovery (0 or >1 devices) fails Run() before any disk write: no mount, no disk
// cleanup, no image write.
func TestLocalImagesDiscoveryPreconditionFailsBeforeDiskWrite(t *testing.T) {
	cases := []struct {
		name    string
		devices []ops.BlockDevice
	}{
		{"zero devices", []ops.BlockDevice{{Name: "sda", Label: "root"}}},
		{"multiple devices", []ops.BlockDevice{
			{Name: "sdb1", Label: common.LocalImagesFSLabel},
			{Name: "sdc1", Label: common.LocalImagesFSLabel},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			mockOps := ops.NewMockOps(ctrl)
			cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
			cfg := &ibiconfig.IBIPrepareConfig{
				Disconnected:     true,
				LocalImagesPath:  "/var/mnt/ibi",
				SeedImage:        testSeedImageRef(),
				SeedVersion:      "4.15.0",
				InstallationDisk: "/dev/sda",
			}
			ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, cleanupMock, cfg)

			mockOps.EXPECT().ListBlockDevices().Return(tc.devices, nil).Times(1)
			// Fail-fast before any disk write: no mount, no cleanup of the install disk.
			mockOps.EXPECT().MountReadOnly(gomock.Any(), gomock.Any()).Times(0)
			cleanupMock.EXPECT().CleanupInstallDevice(gomock.Any()).Times(0)

			err := ibi.Run()
			assert.Error(t, err)
		})
	}
}

// TestManifestValidatedBeforeDiskWrite proves that a missing/malformed manifest or a
// manifest lacking the seed entry fails Run() in preflight -- after the p2 mount but
// before any disk write -- and that p2 is unmounted on the way out.
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
					Version:       1,
					OCILayoutPath: "oci",
					Images:        []localimages.ImageEntry{{Name: "other", Digest: "sha256:dead", OCITag: "t"}},
				}, nil
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			saveSeams(t)
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
				LocalImagesPath:     "/var/mnt/ibi",
				ExpectedDigestsPath: "/var/mnt/ibi/manifest.json",
				SeedImage:           testSeedImageRef(),
				SeedVersion:         "4.15.0",
				InstallationDisk:    "/dev/sda",
			}
			ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, cleanupMock, cfg)

			mockOps.EXPECT().ListBlockDevices().
				Return([]ops.BlockDevice{{Name: "sdb1", Label: common.LocalImagesFSLabel}}, nil).Times(1)
			mockOps.EXPECT().MountReadOnly("sdb1", cfg.LocalImagesPath).Return(nil).Times(1)
			// No disk write.
			cleanupMock.EXPECT().CleanupInstallDevice(gomock.Any()).Times(0)
			// p2 is unmounted on the error path (deferred unmount).
			mockOps.EXPECT().Umount("sdb1").Return(nil).Times(1)

			err := ibi.Run()
			assert.Error(t, err)
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
		ibi := NewIBIPrepare(logrus.New(), nil, nil, nil, nil, cfg)

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

// TestSeedImportedIntoLiveStore asserts the seed is imported into the live/default
// store (zero-value StoreConfig) and the importer is closed.
func TestSeedImportedIntoLiveStore(t *testing.T) {
	saveSeams(t)
	ff := &importerFactory{}
	newImageImporter = ff.new

	cfg := &ibiconfig.IBIPrepareConfig{Disconnected: true, LocalImagesPath: "/var/mnt/ibi", SeedImage: testSeedImageRef()}
	ibi := NewIBIPrepare(logrus.New(), nil, nil, nil, nil, cfg)

	manifest := testManifest()
	seedEntry := manifest.Images[0]
	assert.NoError(t, ibi.importSeedToLiveStore(context.Background(), manifest, seedEntry))

	if assert.NotNil(t, ff.seed) {
		assert.Equal(t, localimages.StoreConfig{}, ff.seed.cfg, "seed must target the live/default store")
		assert.Equal(t, []string{cfg.LocalImagesPath}, ff.seed.mediaRoots)
		if assert.Len(t, ff.seed.importedOne, 1) {
			assert.Equal(t, seedEntry, ff.seed.importedOne[0])
		}
		assert.True(t, ff.seed.closed, "seed importer must be closed")
	}
	assert.Nil(t, ff.closure, "the closure store must not be opened for the seed import")
}

// TestClosureImportedIntoInstalledGraphroot asserts the closure is imported into the
// installed stateroot's graphroot via an explicit overlay store with a temporary
// runroot that is cleaned up afterwards.
func TestClosureImportedIntoInstalledGraphroot(t *testing.T) {
	saveSeams(t)
	ff := &importerFactory{}
	newImageImporter = ff.new
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
		LocalImagesPath:     "/var/mnt/ibi",
		ExpectedDigestsPath: "/m.json",
		SeedImage:           testSeedImageRef(),
		SeedVersion:         "4.15.0",
		InstallationDisk:    "/dev/sda",
		UseContainersFolder: true,
		PrecacheDisabled:    false,
	}
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, nil, cleanupMock, cfg)

	mockOps.EXPECT().ListBlockDevices().
		Return([]ops.BlockDevice{{Name: "sdb1", Label: common.LocalImagesFSLabel}}, nil).Times(1)
	mockOps.EXPECT().MountReadOnly("sdb1", cfg.LocalImagesPath).Return(nil).Times(1)
	cleanupMock.EXPECT().CleanupInstallDevice(cfg.InstallationDisk).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("coreos-installer", []string{"install", cfg.InstallationDisk}).Return("", nil).Times(1)
	mockOps.EXPECT().SetupContainersFolderCommands().Return(nil).Times(1)
	// precache fails reading the seed info file before chrooting or pulling.
	mockOps.EXPECT().Chroot(gomock.Any()).Times(0)
	// p2 is unmounted on the error path.
	mockOps.EXPECT().Umount("sdb1").Return(nil).Times(1)

	err := ibi.Run()
	assert.Error(t, err)
	if assert.NotNil(t, ff.closure) {
		assert.Equal(t, 1, ff.closure.importedAll, "closure must be imported before precache runs")
	}
}

// TestRunDisconnectedOrderingAndUnmountLast drives the full disconnected happy path
// (precache disabled) and asserts the end-to-end ordering: discovery and manifest
// validation run before the disk write; the seed imports before SetupStateroot; the
// closure imports after SetupStateroot and before the final sync; no podman pull
// happens; and p2 is unmounted last, after sync.
func TestRunDisconnectedOrderingAndUnmountLast(t *testing.T) {
	saveSeams(t)
	t.Chdir(t.TempDir()) // the synthetic authfile writes to a relative var/tmp path

	var events []string
	parseDigestManifest = func(string) (*localimages.DigestManifest, error) {
		events = append(events, "parseManifest")
		return testManifest(), nil
	}
	ff := &importerFactory{events: &events}
	newImageImporter = ff.new
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
		LocalImagesPath:     "/var/mnt/ibi",
		ExpectedDigestsPath: "/var/mnt/ibi/manifest.json",
		SeedImage:           testSeedImageRef(),
		SeedVersion:         "4.15.0",
		InstallationDisk:    "/dev/sda",
		UseContainersFolder: true,
		PrecacheDisabled:    true,
	}
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, ostreeMock, cleanupMock, cfg)

	mockOps.EXPECT().ListBlockDevices().DoAndReturn(func() ([]ops.BlockDevice, error) {
		events = append(events, "listBlockDevices")
		return []ops.BlockDevice{{Name: "sdb1", Label: common.LocalImagesFSLabel}}, nil
	}).Times(1)
	mockOps.EXPECT().MountReadOnly("sdb1", cfg.LocalImagesPath).DoAndReturn(func(_, _ string) error {
		events = append(events, "mountReadOnly")
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
	ostreeMock.EXPECT().Undeploy(rhcosOstreeIndex).DoAndReturn(func(int) error {
		events = append(events, "undeploy")
		return nil
	}).Times(1)
	mockOps.EXPECT().RunInHostNamespace("sync").DoAndReturn(func(string, ...string) (string, error) {
		events = append(events, "sync")
		return "", nil
	}).Times(1)
	mockOps.EXPECT().Umount("sdb1").DoAndReturn(func(string) error {
		events = append(events, "umount")
		return nil
	}).Times(1)

	err := ibi.Run()
	assert.NoError(t, err)

	// No podman pull on the disconnected happy path: any such call would be an
	// unexpected call on the strict mock controller and fail the test. Assert the
	// relative ordering of the recorded events.
	for _, name := range []string{
		"listBlockDevices", "mountReadOnly", "parseManifest", "cleanupInstallDevice",
		"importSeed", "setupStateroot", "importClosure", "sync", "umount",
	} {
		assert.GreaterOrEqual(t, idxOf(events, name), 0, "missing event %q", name)
	}
	// Preconditions (discovery + manifest) run before the disk write.
	assert.Less(t, idxOf(events, "mountReadOnly"), idxOf(events, "cleanupInstallDevice"))
	assert.Less(t, idxOf(events, "parseManifest"), idxOf(events, "cleanupInstallDevice"))
	// Seed imports before stateroot; closure imports after stateroot.
	assert.Less(t, idxOf(events, "importSeed"), idxOf(events, "setupStateroot"))
	assert.Less(t, idxOf(events, "setupStateroot"), idxOf(events, "importClosure"))
	// Closure import precedes the final sync; p2 is unmounted last, after sync.
	assert.Less(t, idxOf(events, "importClosure"), idxOf(events, "sync"))
	assert.Less(t, idxOf(events, "sync"), idxOf(events, "umount"))
	assert.Equal(t, "umount", events[len(events)-1], "p2 unmount must be the last action")

	// Closure targeted the installed stateroot graphroot under /mnt.
	if assert.NotNil(t, ff.closure) {
		osname := common.GetStaterootName(cfg.SeedVersion)
		assert.Equal(t, filepath.Join(common.GetStaterootPath(osname), common.ContainerStoragePath, "storage"),
			ff.closure.cfg.GraphRoot)
	}
}

// TestRunReturnsErrorOnFailedFinalUnmount proves that when the final p2 unmount
// keeps failing, Run surfaces the failure as its result (regardless of Shutdown)
// rather than reporting success with p2 still mounted, and that the deferred cleanup
// retries the unmount on the way out.
func TestRunReturnsErrorOnFailedFinalUnmount(t *testing.T) {
	saveSeams(t)
	t.Chdir(t.TempDir())

	parseDigestManifest = func(string) (*localimages.DigestManifest, error) { return testManifest(), nil }
	newImageImporter = (&importerFactory{}).new
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
		Shutdown:            false, // the unmount error must surface regardless of Shutdown
	}
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, ostreeMock, cleanupMock, cfg)

	mockOps.EXPECT().ListBlockDevices().
		Return([]ops.BlockDevice{{Name: "sdb2", Label: common.LocalImagesFSLabel}}, nil).Times(1)
	mockOps.EXPECT().MountReadOnly("sdb2", cfg.LocalImagesPath).Return(nil).Times(1)
	cleanupMock.EXPECT().CleanupInstallDevice(cfg.InstallationDisk).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("coreos-installer", []string{"install", cfg.InstallationDisk}).Return("", nil).Times(1)
	mockOps.EXPECT().SetupContainersFolderCommands().Return(nil).Times(1)
	ostreeMock.EXPECT().Undeploy(rhcosOstreeIndex).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(1)
	// Explicit final unmount fails; the deferred cleanup then retries it (also fails),
	// so Umount is attempted twice. shutdown must never be invoked.
	mockOps.EXPECT().Umount("sdb2").Return(fmt.Errorf("device busy")).Times(2)
	mockOps.EXPECT().RunInHostNamespace("shutdown", gomock.Any()).Times(0)

	err := ibi.Run()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unmount local images")
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
