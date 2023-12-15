package controllers

import (
	"context"
	"errors"
	"fmt"
	"github.com/go-logr/logr"
	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/ibu-imager/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/backuprestore"
	mock_backuprestore "github.com/openshift-kni/lifecycle-agent/internal/backuprestore/mocks"
	mock_clusterconfig "github.com/openshift-kni/lifecycle-agent/internal/clusterconfig/mocks"
	"github.com/openshift-kni/lifecycle-agent/internal/extramanifest"
	mock_extramanifest "github.com/openshift-kni/lifecycle-agent/internal/extramanifest/mocks"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/stretchr/testify/assert"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"go.uber.org/mock/gomock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"os"
	"path/filepath"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
	"testing"
)

func BoolPointer(b bool) *bool {
	return &b
}
func TestImageBasedUpgradeReconciler_handleBackup(t *testing.T) {
	mockController := gomock.NewController(t)
	mockBackuprestore := mock_backuprestore.NewMockBackuperRestorer(mockController)
	defer func() {
		mockController.Finish()
	}()

	tests := []struct {
		name        string
		inputVelero [][]*velerov1.Backup
		trackers    []func() (*backuprestore.BackupTracker, error)
		wantCtlRes  controllerruntime.Result
		wantErr     assert.ErrorAssertionFunc
	}{
		{
			name:        "backup successful",
			inputVelero: [][]*velerov1.Backup{{&velerov1.Backup{}}},
			trackers: []func() (*backuprestore.BackupTracker, error){
				func() (*backuprestore.BackupTracker, error) {
					return &backuprestore.BackupTracker{
						SucceededBackups: []string{"name-successful"},
					}, nil
				},
			},
			wantCtlRes: doNotRequeue(),
			wantErr:    assert.NoError,
		},
		{
			name:        "backup failed",
			inputVelero: [][]*velerov1.Backup{{&velerov1.Backup{}}},
			trackers: []func() (*backuprestore.BackupTracker, error){
				func() (*backuprestore.BackupTracker, error) {
					return &backuprestore.BackupTracker{
						FailedBackups: []string{"name-successful"},
					}, nil
				},
			},
			wantCtlRes: doNotRequeue(),
			wantErr:    assert.Error,
		},
		{
			name:        "backup pending",
			inputVelero: [][]*velerov1.Backup{{&velerov1.Backup{}}},
			trackers: []func() (*backuprestore.BackupTracker, error){
				func() (*backuprestore.BackupTracker, error) {
					return &backuprestore.BackupTracker{
						PendingBackups: []string{"name-pending"},
					}, nil
				},
			},
			wantCtlRes: requeueWithMediumInterval(),
			wantErr:    assert.NoError,
		},
		{
			name:        "backup progressing",
			inputVelero: [][]*velerov1.Backup{{&velerov1.Backup{}}},
			trackers: []func() (*backuprestore.BackupTracker, error){
				func() (*backuprestore.BackupTracker, error) {
					return &backuprestore.BackupTracker{
						ProgressingBackups: []string{"name-progressing"},
					}, nil
				},
			},
			wantCtlRes: requeueWithShortInterval(),
			wantErr:    assert.NoError,
		},
		{
			name:        "backup failed validation by oadp",
			inputVelero: [][]*velerov1.Backup{{&velerov1.Backup{}}},
			trackers: []func() (*backuprestore.BackupTracker, error){
				func() (*backuprestore.BackupTracker, error) {
					return &backuprestore.BackupTracker{
						FailedValidationBackups: []string{"name-validation-failed"},
					}, nil
				},
			},
			wantCtlRes: requeueWithMediumInterval(),
			wantErr:    assert.NoError,
		},
		{
			name:        "two backup groups, both successful",
			inputVelero: [][]*velerov1.Backup{{&velerov1.Backup{}, &velerov1.Backup{}}, {&velerov1.Backup{}}},
			trackers: []func() (*backuprestore.BackupTracker, error){
				func() (*backuprestore.BackupTracker, error) {
					return &backuprestore.BackupTracker{
						SucceededBackups: []string{"name-success-1", "name-success-2"},
					}, nil
				},
				func() (*backuprestore.BackupTracker, error) {
					return &backuprestore.BackupTracker{
						SucceededBackups: []string{"name-success-3"},
					}, nil
				},
			},
			wantCtlRes: doNotRequeue(),
			wantErr:    assert.NoError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			//setup

			mockBackuprestore.EXPECT().GetSortedBackupsFromConfigmap(gomock.Any(), gomock.Any()).Return(tt.inputVelero, nil)

			for _, track := range tt.trackers {
				mockBackuprestore.EXPECT().StartOrTrackBackup(gomock.Any(), gomock.Any()).Return(track())
			}

			// assert
			assert.Equalf(t, len(tt.trackers), len(tt.inputVelero), "make sure the numnber of groups and tracker match as pre cond for the test")
			uph := &UpgHandler{
				Client:        nil,
				Log:           logr.Logger{},
				BackupRestore: mockBackuprestore,
			}
			got, err := uph.HandleBackup(context.Background(), &lcav1alpha1.ImageBasedUpgrade{})
			if !tt.wantErr(t, err, fmt.Sprintf("handleBackup")) {
				return
			}
			assert.Equalf(t, tt.wantCtlRes.RequeueAfter, got.RequeueAfter, "ctl interval: handleBackup")

		})
	}
}

func TestImageBasedUpgradeReconciler_handleRestore(t *testing.T) {
	mockController := gomock.NewController(t)
	mockBackuprestore := mock_backuprestore.NewMockBackuperRestorer(mockController)
	defer func() {
		mockController.Finish()
	}()

	tests := []struct {
		name        string
		inputVelero [][]*velerov1.Restore
		trackers    []func() (*backuprestore.RestoreTracker, error)
		wantCtlRes  controllerruntime.Result
		wantErr     assert.ErrorAssertionFunc
	}{
		{
			name:        "restore successful",
			inputVelero: [][]*velerov1.Restore{{&velerov1.Restore{}}},
			trackers: []func() (*backuprestore.RestoreTracker, error){
				func() (*backuprestore.RestoreTracker, error) {
					return &backuprestore.RestoreTracker{
						SucceededRestores: []string{"name-success"},
					}, nil
				},
			},
			wantCtlRes: doNotRequeue(),
			wantErr:    assert.NoError,
		},
		{
			name:        "restore failed",
			inputVelero: [][]*velerov1.Restore{{&velerov1.Restore{}}},
			trackers: []func() (*backuprestore.RestoreTracker, error){
				func() (*backuprestore.RestoreTracker, error) {
					return &backuprestore.RestoreTracker{
						FailedRestores: []string{"name-failed"},
					}, nil
				},
			},
			wantCtlRes: doNotRequeue(),
			wantErr:    assert.Error,
		},
		{
			name:        "restore pending",
			inputVelero: [][]*velerov1.Restore{{&velerov1.Restore{}}},
			trackers: []func() (*backuprestore.RestoreTracker, error){
				func() (*backuprestore.RestoreTracker, error) {
					return &backuprestore.RestoreTracker{
						PendingRestores: []string{"name-pending"},
					}, nil
				},
			},
			wantCtlRes: requeueWithMediumInterval(),
			wantErr:    assert.NoError,
		},
		{
			name:        "restore progressing",
			inputVelero: [][]*velerov1.Restore{{&velerov1.Restore{}}},
			trackers: []func() (*backuprestore.RestoreTracker, error){
				func() (*backuprestore.RestoreTracker, error) {
					return &backuprestore.RestoreTracker{
						ProgressingRestores: []string{"name-progressing"},
					}, nil
				},
			},
			wantCtlRes: requeueWithShortInterval(),
			wantErr:    assert.NoError,
		},
		{
			name:        "restore missing",
			inputVelero: [][]*velerov1.Restore{{&velerov1.Restore{}}},
			trackers: []func() (*backuprestore.RestoreTracker, error){
				func() (*backuprestore.RestoreTracker, error) {
					return &backuprestore.RestoreTracker{
						MissingBackups: []string{"name-missing"},
					}, nil
				},
			},
			wantCtlRes: requeueWithMediumInterval(),
			wantErr:    assert.NoError,
		},
		{
			name:        "two restore groups, both successful",
			inputVelero: [][]*velerov1.Restore{{&velerov1.Restore{}}, {&velerov1.Restore{}}},
			trackers: []func() (*backuprestore.RestoreTracker, error){
				func() (*backuprestore.RestoreTracker, error) {
					return &backuprestore.RestoreTracker{
						SucceededRestores: []string{"name-successful-1"},
					}, nil
				},
				func() (*backuprestore.RestoreTracker, error) {
					return &backuprestore.RestoreTracker{
						SucceededRestores: []string{"name-successful-2"},
					}, nil
				},
			},
			wantCtlRes: doNotRequeue(),
			wantErr:    assert.NoError,
		},
		{
			name:        "two restore groups, one successful one failed",
			inputVelero: [][]*velerov1.Restore{{&velerov1.Restore{}, &velerov1.Restore{}}, {&velerov1.Restore{}}},
			trackers: []func() (*backuprestore.RestoreTracker, error){
				func() (*backuprestore.RestoreTracker, error) {
					return &backuprestore.RestoreTracker{
						SucceededRestores: []string{"name-successful-1", "name-successful-2"},
					}, nil
				},
				func() (*backuprestore.RestoreTracker, error) {
					return &backuprestore.RestoreTracker{
						FailedRestores: []string{"name-failed-1"},
					}, nil
				},
			},
			wantCtlRes: doNotRequeue(),
			wantErr:    assert.Error,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			//setup
			mockBackuprestore.EXPECT().LoadRestoresFromOadpRestorePath().Return(tt.inputVelero, nil).Times(1)

			for _, track := range tt.trackers {
				mockBackuprestore.EXPECT().StartOrTrackRestore(gomock.Any(), gomock.Any()).Return(track()).Times(1)
			}

			// assert
			assert.Equalf(t, len(tt.trackers), len(tt.inputVelero), "make sure the numnber of groups and tracker match as pre cond for the test")
			uph := &UpgHandler{
				Client:        nil,
				Log:           logr.Logger{},
				BackupRestore: mockBackuprestore,
			}
			got, err := uph.HandleRestore(context.Background())
			if !tt.wantErr(t, err, fmt.Sprintf("handleRestore(%v, %v)", context.Background(), &lcav1alpha1.ImageBasedUpgrade{})) {
				return
			}
			assert.Equalf(t, tt.wantCtlRes.RequeueAfter, got.RequeueAfter, "ctl interval: handleRestore")

		})
	}
}

func TestImageBasedUpgradeReconciler_prePivot(t *testing.T) {

	var (
		mockController      = gomock.NewController(t)
		mockClusterconfig   = mock_clusterconfig.NewMockUpgradeClusterConfigGatherer(mockController)
		mockExtramanifest   = mock_extramanifest.NewMockEManifestHandler(mockController)
		mockBackuprestore   = mock_backuprestore.NewMockBackuperRestorer(mockController)
		mockOps             = ops.NewMockOps(mockController)
		mockRpmostreeclient = rpmostreeclient.NewMockIClient(mockController)
		ostreeclientMock    = ostreeclient.NewMockIClient(mockController)
		mockExec            = ops.NewMockExecute(mockController)
	)

	defer func() {
		mockController.Finish()
	}()

	type args struct {
		ctx context.Context
		ibu lcav1alpha1.ImageBasedUpgrade
	}
	tests := []struct {
		name                                        string
		args                                        args
		getSortedBackupsFromConfigmapReturn         func() ([][]*velerov1.Backup, error)
		getStartOrTrackBackupReturn                 func() (*backuprestore.BackupTracker, error)
		remountSysrootReturn                        func() error
		exportOadpConfigurationToDirReturn          func() error
		exportRestoresToDirReturn                   func() error
		exportExtraManifestToDirReturn              func() error
		fetchClusterConfigReturn                    func() error
		fetchLvmConfigReturn                        func() error
		exportIBUCRNew                              bool
		exportIBUCROrig                             bool
		rebootToNewStateRootReturn                  func() (string, error)
		isOstreeAdminSetDefaultFeatureEnabledReturn *bool
		want                                        controllerruntime.Result
		wantErr                                     assert.ErrorAssertionFunc
		wantConditions                              []metav1.Condition
	}{
		{
			name: "backup failed request no requeue",
			args: args{
				ibu: lcav1alpha1.ImageBasedUpgrade{},
			},
			getSortedBackupsFromConfigmapReturn: func() ([][]*velerov1.Backup, error) {
				return nil, backuprestore.NewBRFailedError("Backup", "this is a test - error")
			},
			want:    doNotRequeue(),
			wantErr: assert.NoError,
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: "this is a test - error",
				},
				{
					Type:    string(utils.ConditionTypes.UpgradeCompleted),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: "Upgrade failed",
				},
			},
		},
		{
			name: "backup failed validation request medium requeue",
			args: args{
				ibu: lcav1alpha1.ImageBasedUpgrade{},
			},
			getSortedBackupsFromConfigmapReturn: func() ([][]*velerov1.Backup, error) {
				return [][]*velerov1.Backup{},
					backuprestore.NewBRFailedValidationError("Backup", "this is a test - FailedValidation")
			},
			want:    requeueWithMediumInterval(),
			wantErr: assert.NoError,
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.InProgress),
					Status:  metav1.ConditionTrue,
					Message: "this is a test - FailedValidation",
				},
			},
		},
		{
			name: "backup not found in configmap request medium requeue",
			args: args{
				ibu: lcav1alpha1.ImageBasedUpgrade{},
			},
			getSortedBackupsFromConfigmapReturn: func() ([][]*velerov1.Backup, error) {
				return nil,
					backuprestore.NewBRNotFoundError("this is a test - NotFound")
			},
			want:    requeueWithMediumInterval(),
			wantErr: assert.NoError,
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.InProgress),
					Status:  metav1.ConditionTrue,
					Message: "this is a test - NotFound",
				},
			},
		},
		{
			name: "backup requested a medium interval requeue",
			args: args{
				ibu: lcav1alpha1.ImageBasedUpgrade{},
			},
			getSortedBackupsFromConfigmapReturn: func() ([][]*velerov1.Backup, error) {
				return [][]*velerov1.Backup{{&velerov1.Backup{}}}, nil
			},
			getStartOrTrackBackupReturn: func() (*backuprestore.BackupTracker, error) {
				return &backuprestore.BackupTracker{
					FailedValidationBackups: []string{"name-validation-failed"},
				}, nil
			},
			want:    requeueWithMediumInterval(),
			wantErr: assert.NoError,
		},
		{
			name: "ExportOadpConfigurationToDir with failed validation error",
			args: args{
				ibu: lcav1alpha1.ImageBasedUpgrade{},
			},
			getSortedBackupsFromConfigmapReturn: func() ([][]*velerov1.Backup, error) {
				return nil, nil
			},
			remountSysrootReturn: func() error {
				return nil
			},
			exportOadpConfigurationToDirReturn: func() error {
				return backuprestore.NewBRFailedError("OADP", "this is a test - NotFound stop reconcile")
			},
			want:    doNotRequeue(),
			wantErr: assert.NoError,
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: "this is a test - NotFound stop reconcile",
				},
				{
					Type:    string(utils.ConditionTypes.UpgradeCompleted),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: "Upgrade failed",
				},
			},
		},
		{
			name: "ExportOadpConfigurationToDir with unknown error",
			args: args{
				ibu: lcav1alpha1.ImageBasedUpgrade{},
			},
			getSortedBackupsFromConfigmapReturn: func() ([][]*velerov1.Backup, error) {
				return nil, nil
			},
			remountSysrootReturn: func() error {
				return nil
			},
			exportOadpConfigurationToDirReturn: func() error {
				return fmt.Errorf("unknown error")
			},
			want:           doNotRequeue(),
			wantErr:        assert.Error,
			wantConditions: []metav1.Condition{},
		},
		{
			name: "ExportRestoresToDir with failed validation error",
			args: args{
				ibu: lcav1alpha1.ImageBasedUpgrade{},
			},
			getSortedBackupsFromConfigmapReturn: func() ([][]*velerov1.Backup, error) {
				return nil, nil
			},
			remountSysrootReturn: func() error {
				return nil
			},
			exportOadpConfigurationToDirReturn: func() error {
				return nil
			},
			exportRestoresToDirReturn: func() error {
				return backuprestore.NewBRFailedValidationError("Restore", "ExportRestoresToDir validation failed")
			},

			want:    requeueWithMediumInterval(),
			wantErr: assert.NoError,
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.InProgress),
					Status:  metav1.ConditionTrue,
					Message: "ExportRestoresToDir validation failed",
				},
			},
		},
		{
			name: "ExportRestoresToDir with unknown error",
			args: args{
				ibu: lcav1alpha1.ImageBasedUpgrade{},
			},
			getSortedBackupsFromConfigmapReturn: func() ([][]*velerov1.Backup, error) {
				return nil, nil
			},
			remountSysrootReturn: func() error {
				return nil
			},
			exportOadpConfigurationToDirReturn: func() error {
				return nil
			},
			exportRestoresToDirReturn: func() error {
				return fmt.Errorf("unknown error")
			},

			want:           doNotRequeue(),
			wantErr:        assert.Error,
			wantConditions: []metav1.Condition{},
		},
		{
			name: "ExportExtraManifestToDir with any error",
			args: args{
				ibu: lcav1alpha1.ImageBasedUpgrade{},
			},
			getSortedBackupsFromConfigmapReturn: func() ([][]*velerov1.Backup, error) {
				return nil, nil
			},
			remountSysrootReturn: func() error {
				return nil
			},
			exportOadpConfigurationToDirReturn: func() error {
				return nil
			},
			exportRestoresToDirReturn: func() error {
				return nil
			},
			exportExtraManifestToDirReturn: func() error {
				return fmt.Errorf("any error")
			},
			want:           doNotRequeue(),
			wantErr:        assert.Error,
			wantConditions: []metav1.Condition{},
		},
		{
			name: "FetchClusterConfig with any error",
			args: args{
				ibu: lcav1alpha1.ImageBasedUpgrade{},
			},
			getSortedBackupsFromConfigmapReturn: func() ([][]*velerov1.Backup, error) {
				return nil, nil
			},
			remountSysrootReturn: func() error {
				return nil
			},
			exportOadpConfigurationToDirReturn: func() error {
				return nil
			},
			exportRestoresToDirReturn: func() error {
				return nil
			},
			exportExtraManifestToDirReturn: func() error {
				return nil
			},
			fetchClusterConfigReturn: func() error {
				return fmt.Errorf("any error")
			},
			want:           doNotRequeue(),
			wantErr:        assert.Error,
			wantConditions: []metav1.Condition{},
		},
		{
			name: "FetchClusterConfig with any error",
			args: args{
				ibu: lcav1alpha1.ImageBasedUpgrade{},
			},
			getSortedBackupsFromConfigmapReturn: func() ([][]*velerov1.Backup, error) {
				return nil, nil
			},
			remountSysrootReturn: func() error {
				return nil
			},
			exportOadpConfigurationToDirReturn: func() error {
				return nil
			},
			exportRestoresToDirReturn: func() error {
				return nil
			},
			exportExtraManifestToDirReturn: func() error {
				return nil
			},
			fetchClusterConfigReturn: func() error {
				return nil
			},
			fetchLvmConfigReturn: func() error {
				return fmt.Errorf("any error")
			},
			want:           doNotRequeue(),
			wantErr:        assert.Error,
			wantConditions: []metav1.Condition{},
		},
		{
			name: "Export IBU Crs successfully and reboot fail",
			args: args{
				ibu: lcav1alpha1.ImageBasedUpgrade{},
			},
			getSortedBackupsFromConfigmapReturn: func() ([][]*velerov1.Backup, error) {
				return nil, nil
			},
			remountSysrootReturn: func() error {
				return nil
			},
			exportOadpConfigurationToDirReturn: func() error {
				return nil
			},
			exportRestoresToDirReturn: func() error {
				return nil
			},
			exportExtraManifestToDirReturn: func() error {
				return nil
			},
			fetchClusterConfigReturn: func() error {
				return nil
			},
			fetchLvmConfigReturn: func() error {
				return nil
			},
			exportIBUCRNew:  true,
			exportIBUCROrig: true,
			isOstreeAdminSetDefaultFeatureEnabledReturn: BoolPointer(false),
			rebootToNewStateRootReturn: func() (string, error) {
				return "", fmt.Errorf("reboot failed")
			},
			want:    doNotRequeue(),
			wantErr: assert.NoError,
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: "reboot failed",
				},
				{
					Type:    string(utils.ConditionTypes.UpgradeCompleted),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: "Upgrade failed",
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.getSortedBackupsFromConfigmapReturn != nil {
				mockBackuprestore.EXPECT().GetSortedBackupsFromConfigmap(gomock.Any(), gomock.Any()).Return(tt.getSortedBackupsFromConfigmapReturn())
			}
			if tt.getStartOrTrackBackupReturn != nil {
				mockBackuprestore.EXPECT().StartOrTrackBackup(gomock.Any(), gomock.Any()).Return(tt.getStartOrTrackBackupReturn())
			}
			if tt.exportOadpConfigurationToDirReturn != nil {
				mockBackuprestore.EXPECT().ExportOadpConfigurationToDir(gomock.Any(), gomock.Any(), gomock.Any()).Return(tt.exportOadpConfigurationToDirReturn()).Times(1)
			}
			if tt.remountSysrootReturn != nil {
				mockOps.EXPECT().RemountSysroot().Return(tt.remountSysrootReturn())
			}
			if tt.exportRestoresToDirReturn != nil {
				mockBackuprestore.EXPECT().ExportRestoresToDir(gomock.Any(), gomock.Any(), gomock.Any()).Return(tt.exportRestoresToDirReturn()).Times(1)
			}
			if tt.exportExtraManifestToDirReturn != nil {
				mockExtramanifest.EXPECT().ExportExtraManifestToDir(gomock.Any(), gomock.Any(), gomock.Any()).Return(tt.exportExtraManifestToDirReturn()).Times(1)
			}
			if tt.fetchClusterConfigReturn != nil {
				mockClusterconfig.EXPECT().FetchClusterConfig(gomock.Any(), gomock.Any()).Return(tt.fetchClusterConfigReturn()).Times(1)
			}
			if tt.fetchLvmConfigReturn != nil {
				mockClusterconfig.EXPECT().FetchLvmConfig(gomock.Any(), gomock.Any()).Return(tt.fetchLvmConfigReturn()).Times(1)
			}
			ibuTempDirNew := t.TempDir()
			if tt.exportIBUCRNew {
				origGetStaterootVarPath := getStaterootVarPath
				defer func() {
					getStaterootVarPath = origGetStaterootVarPath
				}()
				getStaterootVarPath = func(stateroot string) string {
					_ = os.MkdirAll(filepath.Join(ibuTempDirNew, "/opt"), 0777)
					file, _ := os.OpenFile(filepath.Join(ibuTempDirNew, utils.IBUFilePath), os.O_CREATE, 0777)
					file.Close()
					return ibuTempDirNew
				}
			}
			ibuTempDirOrig := t.TempDir()
			if tt.exportIBUCRNew {
				origIbuPreStaterootPath := ibuPreStaterootPath
				defer func() {
					ibuPreStaterootPath = origIbuPreStaterootPath
				}()
				_ = os.MkdirAll(filepath.Join(ibuTempDirOrig, "/opt"), 0777)
				file, _ := os.OpenFile(filepath.Join(ibuTempDirOrig, utils.IBUFilePath), os.O_CREATE, 0777)
				file.Close()
				ibuPreStaterootPath = filepath.Join(ibuTempDirOrig, utils.IBUFilePath)
			}
			if tt.isOstreeAdminSetDefaultFeatureEnabledReturn != nil {
				ostreeclientMock.EXPECT().IsOstreeAdminSetDefaultFeatureEnabled().Return(*tt.isOstreeAdminSetDefaultFeatureEnabledReturn).Times(1)
			}
			if tt.rebootToNewStateRootReturn != nil {
				mockExec.EXPECT().Execute(gomock.Any(), gomock.Any()).Return(tt.rebootToNewStateRootReturn()).Times(1)
			}
			uh := &UpgHandler{
				Client:          nil,
				Log:             logr.Logger{},
				BackupRestore:   mockBackuprestore,
				ExtraManifest:   mockExtramanifest,
				ClusterConfig:   mockClusterconfig,
				Executor:        mockExec,
				Ops:             mockOps,
				Recorder:        record.NewFakeRecorder(1),
				RPMOstreeClient: mockRpmostreeclient,
				OstreeClient:    ostreeclientMock,
			}

			got, err := uh.PrePivot(context.Background(), &tt.args.ibu)

			// assert
			if !tt.wantErr(t, err, fmt.Sprintf("prePivot(%v, %v)", tt.args.ctx, tt.args.ibu)) {
				return
			}
			assert.Equalf(t, tt.want.RequeueAfter, got.RequeueAfter, "prePivot(%v, %v)", tt.args.ctx, tt.args.ibu)
			assert.Equalf(t, len(tt.wantConditions), len(tt.args.ibu.Status.Conditions), "prePivot(%v, %v)", tt.args.ctx, tt.args.ibu)
			for i, curCond := range tt.wantConditions {
				assert.Equalf(t, curCond.Type, tt.args.ibu.Status.Conditions[i].Type, "prePivot(%v, %v)", tt.args.ctx, tt.args.ibu)
				assert.Equalf(t, curCond.Reason, tt.args.ibu.Status.Conditions[i].Reason, "prePivot(%v, %v)", tt.args.ctx, tt.args.ibu)
				assert.Equalf(t, curCond.Status, tt.args.ibu.Status.Conditions[i].Status, "prePivot(%v, %v)", tt.args.ctx, tt.args.ibu)
				assert.Equalf(t, curCond.Message, tt.args.ibu.Status.Conditions[i].Message, "prePivot(%v, %v)", tt.args.ctx, tt.args.ibu)
			}

			// assert if IBU was correctly stored in new stateroot
			if tt.exportIBUCRNew {
				if _, err := os.Stat(filepath.Join(ibuTempDirNew, utils.IBUFilePath)); !errors.Is(err, os.ErrNotExist) {
					dat, _ := os.ReadFile(filepath.Join(ibuTempDirNew, utils.IBUFilePath))
					savedIbu := lcav1alpha1.ImageBasedUpgrade{}
					err = yaml.Unmarshal(dat, &savedIbu)
					assert.Equalf(t, err, nil, "")
				}
			}
			// assert if IBU was correctly stored in orignal stateroot
			if tt.exportIBUCROrig {
				if _, err := os.Stat(filepath.Join(ibuTempDirOrig, utils.IBUFilePath)); !errors.Is(err, os.ErrNotExist) {
					dat, _ := os.ReadFile(filepath.Join(ibuTempDirOrig, utils.IBUFilePath))
					savedIbu := lcav1alpha1.ImageBasedUpgrade{}
					err = yaml.Unmarshal(dat, &savedIbu)
					assert.Equalf(t, len(savedIbu.Status.Conditions), 2, "")
					assert.Equalf(t, savedIbu.Status.Conditions[0].Message, "Uncontrolled rollback", "")
					assert.Equalf(t, savedIbu.Status.Conditions[1].Message, "Upgrade failed", "")
				}
			}
		})
	}
}

func TestImageBasedUpgradeReconciler_postPivot(t *testing.T) {

	var (
		mockController    = gomock.NewController(t)
		mockExtramanifest = mock_extramanifest.NewMockEManifestHandler(mockController)
		mockBackuprestore = mock_backuprestore.NewMockBackuperRestorer(mockController)
	)

	defer func() {
		mockController.Finish()
	}()

	type fields struct {
		BackupRestore  backuprestore.BackuperRestorer
		ExtraManifest  extramanifest.EManifestHandler
		UpgradeHandler UpgradeHandler
	}
	type args struct {
		ctx context.Context
		ibu *lcav1alpha1.ImageBasedUpgrade
	}
	tests := []struct {
		name                              string
		fields                            fields
		args                              args
		want                              controllerruntime.Result
		wantErr                           assert.ErrorAssertionFunc
		checkHealthReturn                 func(c client.Reader, l logr.Logger) error
		applyExtraManifestsReturn         func() error
		restoreOadpConfigurationsReturn   func() error
		loadRestoresFromOadpRestoreReturn func() ([][]*velerov1.Restore, error)
		startOrTrackRestoreReturn         func() (*backuprestore.RestoreTracker, error)
		wantConditions                    []metav1.Condition
	}{
		{
			name: "healthchecks return error",
			args: args{ibu: &lcav1alpha1.ImageBasedUpgrade{}},
			checkHealthReturn: func(c client.Reader, l logr.Logger) error {
				return fmt.Errorf("any error from hc")
			},
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: "any error from hc",
				},
				{
					Type:    string(utils.ConditionTypes.UpgradeCompleted),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: "Upgrade failed",
				},
			},
			wantErr: assert.NoError,
		},
		{
			name: "healthchecks return error",
			args: args{ibu: &lcav1alpha1.ImageBasedUpgrade{}},
			checkHealthReturn: func(c client.Reader, l logr.Logger) error {
				return nil
			},
			applyExtraManifestsReturn: func() error {
				return extramanifest.NewEMFailedError("Test error EM")
			},
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: "Test error EM",
				},
				{
					Type:    string(utils.ConditionTypes.UpgradeCompleted),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: "Upgrade failed",
				},
			},
			wantErr: assert.NoError,
		},
		{
			name: "RestoreOadpConfigurations return error",
			args: args{ibu: &lcav1alpha1.ImageBasedUpgrade{}},
			checkHealthReturn: func(c client.Reader, l logr.Logger) error {
				return nil
			},
			applyExtraManifestsReturn: func() error {
				return nil
			},
			restoreOadpConfigurationsReturn: func() error {
				return backuprestore.NewBRStorageBackendUnavailableError("error RestoreOadpConfigurations")
			},
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: "error RestoreOadpConfigurations",
				},
				{
					Type:    string(utils.ConditionTypes.UpgradeCompleted),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: "Upgrade failed",
				},
			},
			wantErr: assert.NoError,
		},
		{
			name: "handleRestore with restore error",
			args: args{ibu: &lcav1alpha1.ImageBasedUpgrade{}},
			checkHealthReturn: func(c client.Reader, l logr.Logger) error {
				return nil
			},
			applyExtraManifestsReturn: func() error {
				return nil
			},
			restoreOadpConfigurationsReturn: func() error {
				return nil
			},
			loadRestoresFromOadpRestoreReturn: func() ([][]*velerov1.Restore, error) {
				return [][]*velerov1.Restore{{&velerov1.Restore{}}}, nil
			},
			startOrTrackRestoreReturn: func() (*backuprestore.RestoreTracker, error) {
				return &backuprestore.RestoreTracker{FailedRestores: []string{"name-failed"}}, nil
			},
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: "Failed restore CRs: name-failed",
				},
				{
					Type:    string(utils.ConditionTypes.UpgradeCompleted),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: "Upgrade failed",
				},
			},
			wantErr: assert.NoError,
		},
		{
			name: "upgrade completed",
			args: args{ibu: &lcav1alpha1.ImageBasedUpgrade{}},
			checkHealthReturn: func(c client.Reader, l logr.Logger) error {
				return nil
			},
			applyExtraManifestsReturn: func() error {
				return nil
			},
			restoreOadpConfigurationsReturn: func() error {
				return nil
			},
			loadRestoresFromOadpRestoreReturn: func() ([][]*velerov1.Restore, error) {
				return nil, nil
			},
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.Completed),
					Status:  metav1.ConditionFalse,
					Message: "Upgrade completed",
				},
				{
					Type:    string(utils.ConditionTypes.UpgradeCompleted),
					Reason:  string(utils.ConditionReasons.Completed),
					Status:  metav1.ConditionTrue,
					Message: "Upgrade completed",
				},
			},
			wantErr: assert.NoError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uh := &UpgHandler{
				Client:        nil,
				Log:           logr.Logger{},
				BackupRestore: mockBackuprestore,
				ExtraManifest: mockExtramanifest,
			}

			oldHC := CheckHealth
			defer func() {
				CheckHealth = oldHC
			}()

			CheckHealth = tt.checkHealthReturn

			if tt.applyExtraManifestsReturn != nil {
				mockExtramanifest.EXPECT().ApplyExtraManifests(gomock.Any(), gomock.Any()).Return(tt.applyExtraManifestsReturn()).Times(1)
			}
			if tt.restoreOadpConfigurationsReturn != nil {
				mockBackuprestore.EXPECT().RestoreOadpConfigurations(gomock.Any()).Return(tt.restoreOadpConfigurationsReturn()).Times(1)
			}
			if tt.loadRestoresFromOadpRestoreReturn != nil {
				mockBackuprestore.EXPECT().LoadRestoresFromOadpRestorePath().Return(tt.loadRestoresFromOadpRestoreReturn()).Times(1)
			}
			if tt.startOrTrackRestoreReturn != nil {
				mockBackuprestore.EXPECT().StartOrTrackRestore(gomock.Any(), gomock.Any()).Return(tt.startOrTrackRestoreReturn()).Times(1)
			}

			got, err := uh.PostPivot(tt.args.ctx, tt.args.ibu)
			// assert
			if !tt.wantErr(t, err, fmt.Sprintf("postPivot(%v, %v)", tt.args.ctx, tt.args.ibu)) {
				return
			}
			assert.Equalf(t, tt.want.RequeueAfter, got.RequeueAfter, "postPivot(%v, %v)", tt.args.ctx, tt.args.ibu)
			assert.Equalf(t, len(tt.wantConditions), len(tt.args.ibu.Status.Conditions), "postPivot(%v, %v)", tt.args.ctx, tt.args.ibu)
			for i, curCond := range tt.wantConditions {
				assert.Equalf(t, curCond.Type, tt.args.ibu.Status.Conditions[i].Type, "postPivot(%v, %v)", tt.args.ctx, tt.args.ibu)
				assert.Equalf(t, curCond.Reason, tt.args.ibu.Status.Conditions[i].Reason, "postPivot(%v, %v)", tt.args.ctx, tt.args.ibu)
				assert.Equalf(t, curCond.Status, tt.args.ibu.Status.Conditions[i].Status, "postPivot(%v, %v)", tt.args.ctx, tt.args.ibu)
				assert.Equalf(t, curCond.Message, tt.args.ibu.Status.Conditions[i].Message, "postPivot(%v, %v)", tt.args.ctx, tt.args.ibu)
			}
		})
	}
}
