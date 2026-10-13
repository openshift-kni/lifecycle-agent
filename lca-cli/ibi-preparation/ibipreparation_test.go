package ibi_preparation

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"testing"

	preinstallUtils "github.com/rh-ecosystem-edge/preinstall-utils/pkg"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"github.com/openshift-kni/lifecycle-agent/api/ibiconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
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
