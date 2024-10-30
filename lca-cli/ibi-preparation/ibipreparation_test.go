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
	defer file.Close()
	// Test case when the post deployment script exists and executes without error
	mockOps.EXPECT().RunBashInHostNamespace(postSH).Return("", nil).Times(1)
	err = ibi.postDeployment(postSH)
	assert.Nil(t, err)

	// Test case when the post deployment script exists but fails to execute
	mockOps.EXPECT().RunBashInHostNamespace(postSH).Return("", fmt.Errorf("dummy")).Times(1)
	err = ibi.postDeployment(postSH)
	assert.NotNil(t, err)
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
