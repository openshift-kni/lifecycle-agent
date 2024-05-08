package ibi_preparation

import (
	"fmt"
	"testing"

	preinstallUtils "github.com/rh-ecosystem-edge/preinstall-utils/pkg"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

func TestDiskPreparation(t *testing.T) {
	installationDisk := "/dev/sda"
	extraPartitionLabel := "label"
	extraPartitionStart := "-40"
	extraPartitionNumber := 5

	testcases := []struct {
		name                          string
		partitionError                bool
		setupFolderError              bool
		shouldCreateExternalPartition bool
		skipDiskCleanup               bool
		failCleanupDisk               bool
	}{
		{
			name:                          "PrepareDisk with external partition - happy flow",
			partitionError:                false,
			setupFolderError:              false,
			shouldCreateExternalPartition: true,
			skipDiskCleanup:               false,
			failCleanupDisk:               false,
		},
		{
			name:                          "cleanup disk fails though installation continues",
			partitionError:                false,
			setupFolderError:              false,
			shouldCreateExternalPartition: true,
			skipDiskCleanup:               false,
			failCleanupDisk:               true,
		},
		{
			name:                          "fail to create external partition",
			partitionError:                true,
			setupFolderError:              false,
			shouldCreateExternalPartition: true,
			skipDiskCleanup:               false,
			failCleanupDisk:               false,
		},
		{
			name:                          "PrepareDisk without external partition - happy flow",
			partitionError:                false,
			setupFolderError:              false,
			shouldCreateExternalPartition: false,
			skipDiskCleanup:               false,
			failCleanupDisk:               false,
		},
		{
			name:                          "PrepareDisk setup folder - fail",
			partitionError:                false,
			setupFolderError:              true,
			shouldCreateExternalPartition: false,
			skipDiskCleanup:               false,
			failCleanupDisk:               false,
		},
	}

	for _, tc := range testcases {
		ctrl := gomock.NewController(t)
		mockOps := ops.NewMockOps(ctrl)
		cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
		t.Run(tc.name, func(t *testing.T) {
			log := &logrus.Logger{}
			ibi := NewIBIPrepare(log, mockOps, nil, nil, cleanupMock,
				"seedImage", "authFile", "pullSecretFile",
				"seedExpectedVersion", installationDisk, extraPartitionLabel,
				extraPartitionStart, false, false, false,
				tc.shouldCreateExternalPartition, tc.skipDiskCleanup, 5)

			if !tc.skipDiskCleanup {
				if tc.failCleanupDisk {
					cleanupMock.EXPECT().CleanupInstallDevice(installationDisk).Return(fmt.Errorf("dummy"))
				} else {
					cleanupMock.EXPECT().CleanupInstallDevice(installationDisk).Return(nil)
				}
			}

			mockOps.EXPECT().RunInHostNamespace("coreos-installer", "install", "/dev/sda").Return("", nil).Times(1)

			if tc.shouldCreateExternalPartition {
				if !tc.partitionError {
					mockOps.EXPECT().CreateExtraPartition(installationDisk, extraPartitionLabel,
						extraPartitionStart, extraPartitionNumber).Return(nil).Times(1)
				} else {
					mockOps.EXPECT().CreateExtraPartition(installationDisk, extraPartitionLabel,
						extraPartitionStart, extraPartitionNumber).Return(fmt.Errorf("dummy")).Times(1)
				}

				mockOps.EXPECT().SetupContainersFolderCommands().Return(nil).Times(0)
			}
			if !tc.shouldCreateExternalPartition {
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
