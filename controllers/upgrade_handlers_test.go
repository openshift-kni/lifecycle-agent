package controllers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/backuprestore"
	mock_backuprestore "github.com/openshift-kni/lifecycle-agent/internal/backuprestore/mocks"
	mock_clusterconfig "github.com/openshift-kni/lifecycle-agent/internal/clusterconfig/mocks"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/extramanifest"
	mock_extramanifest "github.com/openshift-kni/lifecycle-agent/internal/extramanifest/mocks"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
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
		name       string
		hasContent bool
		tracker    func() (*backuprestore.BackupTracker, error)
		wantCtlRes controllerruntime.Result
		wantErr    assert.ErrorAssertionFunc
	}{
		{
			name:       "backup successful",
			hasContent: true,
			tracker: func() (*backuprestore.BackupTracker, error) {
				return &backuprestore.BackupTracker{
					SucceededBackups: []string{"name-successful"},
				}, nil
			},
			wantCtlRes: doNotRequeue(),
			wantErr:    assert.NoError,
		},
		{
			name:       "backup failed",
			hasContent: true,
			tracker: func() (*backuprestore.BackupTracker, error) {
				return &backuprestore.BackupTracker{
					FailedBackups: []string{"name-failed"},
				}, nil
			},
			wantCtlRes: doNotRequeue(),
			wantErr:    assert.Error,
		},
		{
			name:       "no OADP content - skip",
			hasContent: false,
			wantCtlRes: doNotRequeue(),
			wantErr:    assert.NoError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ibu := &ibuv1.ImageBasedUpgrade{}
			if tt.hasContent {
				ibu.Spec.OADPContent = []ibuv1.ConfigMapRef{{Name: "test-cm"}}
				mockBackuprestore.EXPECT().PatchPVsReclaimPolicy(gomock.Any()).Return(nil)
				mockBackuprestore.EXPECT().StartBackup(gomock.Any(), gomock.Any(), gomock.Any()).Return(tt.tracker())
			}

			uph := &UpgHandler{
				Client:          nil,
				NoncachedClient: nil,
				Log:             logr.Logger{},
				BackupRestore:   mockBackuprestore,
			}
			got, err := uph.HandleBackup(context.Background(), ibu, "/tmp/test")
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
		name                          string
		tracker                       func() (*backuprestore.RestoreTracker, error)
		restorePVsReclaimPolicyReturn func() error
		wantCtlRes                    controllerruntime.Result
		wantErr                       assert.ErrorAssertionFunc
	}{
		{
			name: "restore successful",
			tracker: func() (*backuprestore.RestoreTracker, error) {
				return &backuprestore.RestoreTracker{
					SucceededRestores: []string{"name-success"},
				}, nil
			},
			restorePVsReclaimPolicyReturn: func() error {
				return nil
			},
			wantCtlRes: doNotRequeue(),
			wantErr:    assert.NoError,
		},
		{
			name: "restore failed",
			tracker: func() (*backuprestore.RestoreTracker, error) {
				return &backuprestore.RestoreTracker{
					FailedRestores: []string{"name-failed"},
				}, nil
			},
			wantCtlRes: doNotRequeue(),
			wantErr:    assert.Error,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockBackuprestore.EXPECT().StartRestore(gomock.Any()).Return(tt.tracker()).Times(1)

			if tt.restorePVsReclaimPolicyReturn != nil {
				mockBackuprestore.EXPECT().RestorePVsReclaimPolicy(gomock.Any()).Return(nil).Times(1)
			}

			uph := &UpgHandler{
				Client:          nil,
				NoncachedClient: nil,
				Log:             logr.Logger{},
				BackupRestore:   mockBackuprestore,
			}
			got, err := uph.HandleRestore(context.Background())
			if !tt.wantErr(t, err, fmt.Sprintf("handleRestore")) {
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
		mockRebootClient    = reboot.NewMockRebootIntf(mockController)
	)

	defer func() {
		mockController.Finish()
	}()

	type args struct {
		ctx context.Context
		ibu ibuv1.ImageBasedUpgrade
	}
	tests := []struct {
		name                                            string
		args                                            args
		healthCheckError                                error
		getStartBackupReturn                            func() (*backuprestore.BackupTracker, error)
		getPatchPVsReclaimPolicyReturn                  func() error
		getValidateBackupConfigmapsReturn               func() error
		remountSysrootReturn                            func() error
		exportExtraManifestToDirReturn                  func() error
		extractAndExportManifestFromPoliciesToDirReturn func() error
		fetchClusterConfigReturn                        func() error
		fetchLvmConfigReturn                            func() error
		fetchCertManagerConfigReturn                    func() error
		exportIBUCRNew                                  bool
		exportIBUCROrig                                 bool
		rebootToNewStateRootReturn                      func() error
		isOstreeAdminSetDefaultFeatureEnabledReturn     *bool
		want                                            controllerruntime.Result
		wantErr                                         assert.ErrorAssertionFunc
		wantConditions                                  []metav1.Condition
	}{
		{
			name: "Pre-pivot upgrade requests requeue because of healthcheck fail",
			args: args{
				ibu: ibuv1.ImageBasedUpgrade{},
			},
			healthCheckError: fmt.Errorf("test error from HC package"),
			want:             requeueWithHealthCheckInterval(),
			wantErr:          assert.NoError,
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.InProgress),
					Status:  metav1.ConditionTrue,
					Message: "Waiting for system to stabilize before Upgrade (pre-pivot) stage can continue: test error from HC package",
				},
			},
		},
		{
			name: "backup failed request no requeue",
			args: args{
				ibu: ibuv1.ImageBasedUpgrade{
					Spec: ibuv1.ImageBasedUpgradeSpec{
						OADPContent: []ibuv1.ConfigMapRef{{Name: "test-cm"}},
					},
				},
			},
			getPatchPVsReclaimPolicyReturn: func() error {
				return nil
			},
			getStartBackupReturn: func() (*backuprestore.BackupTracker, error) {
				return nil, backuprestore.NewBRFailedError("Backup", "this is a test - error")
			},
			remountSysrootReturn: func() error {
				return nil
			},
			want:    doNotRequeue(),
			wantErr: assert.NoError,
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeCompleted),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: utils.UpgradeFailed,
				},
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: "error while running backup: this is a test - error",
				},
			},
		},
		{
			name: "backup failed validation request no requeue",
			args: args{
				ibu: ibuv1.ImageBasedUpgrade{
					Spec: ibuv1.ImageBasedUpgradeSpec{
						OADPContent: []ibuv1.ConfigMapRef{{Name: "test-cm"}},
					},
				},
			},
			getPatchPVsReclaimPolicyReturn: func() error {
				return nil
			},
			getStartBackupReturn: func() (*backuprestore.BackupTracker, error) {
				return nil,
					backuprestore.NewBRFailedValidationError("Backup", "this is a test - FailedValidation")
			},
			remountSysrootReturn: func() error {
				return nil
			},
			want:    doNotRequeue(),
			wantErr: assert.NoError,
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeCompleted),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: utils.UpgradeFailed,
				},
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: "error while running backup: this is a test - FailedValidation",
				},
			},
		},
		{
			name: "ExportExtraManifestToDir with any error",
			args: args{
				ibu: ibuv1.ImageBasedUpgrade{
					Spec: ibuv1.ImageBasedUpgradeSpec{
						SeedImageRef: ibuv1.SeedImageRef{Version: "4.15.2"},
					},
				},
			},
			remountSysrootReturn: func() error {
				return nil
			},
			extractAndExportManifestFromPoliciesToDirReturn: func() error {
				return nil
			},
			exportExtraManifestToDirReturn: func() error {
				return fmt.Errorf("any error")
			},
			want:    doNotRequeue(),
			wantErr: assert.Error,
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.InProgress),
					Status:  metav1.ConditionTrue,
					Message: "Exporting Policy and Config Manifests",
				},
			},
		},
		{
			name: "FetchClusterConfig with any error",
			args: args{
				ibu: ibuv1.ImageBasedUpgrade{
					Spec: ibuv1.ImageBasedUpgradeSpec{
						SeedImageRef: ibuv1.SeedImageRef{Version: "4.15.2"},
					},
				},
			},
			remountSysrootReturn: func() error {
				return nil
			},
			extractAndExportManifestFromPoliciesToDirReturn: func() error {
				return nil
			},
			exportExtraManifestToDirReturn: func() error {
				return nil
			},
			fetchClusterConfigReturn: func() error {
				return fmt.Errorf("any error")
			},
			want:    doNotRequeue(),
			wantErr: assert.Error,
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.InProgress),
					Status:  metav1.ConditionTrue,
					Message: "Exporting Cluster, LVM, and cert-manager configuration",
				},
			},
		},
		{
			name: "Export IBU Crs successfully and reboot fail",
			args: args{
				ibu: ibuv1.ImageBasedUpgrade{
					Spec: ibuv1.ImageBasedUpgradeSpec{
						SeedImageRef: ibuv1.SeedImageRef{Version: "4.15.2"},
					},
				},
			},
			remountSysrootReturn: func() error {
				return nil
			},
			extractAndExportManifestFromPoliciesToDirReturn: func() error {
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
			fetchCertManagerConfigReturn: func() error {
				return nil
			},
			exportIBUCRNew:  true,
			exportIBUCROrig: true,
			isOstreeAdminSetDefaultFeatureEnabledReturn: BoolPointer(false),
			rebootToNewStateRootReturn: func() error {
				return fmt.Errorf("reboot failed")
			},
			want:    doNotRequeue(),
			wantErr: assert.NoError,
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeCompleted),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: utils.UpgradeFailed,
				},
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: "reboot failed",
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.getPatchPVsReclaimPolicyReturn != nil {
				mockBackuprestore.EXPECT().PatchPVsReclaimPolicy(gomock.Any()).Return(tt.getPatchPVsReclaimPolicyReturn())
			}
			if tt.getStartBackupReturn != nil {
				mockBackuprestore.EXPECT().StartBackup(gomock.Any(), gomock.Any(), gomock.Any()).Return(tt.getStartBackupReturn())
			}
			if tt.remountSysrootReturn != nil {
				mockOps.EXPECT().RemountSysroot().Return(tt.remountSysrootReturn())
			}
			if tt.extractAndExportManifestFromPoliciesToDirReturn != nil {
				mockExtramanifest.EXPECT().ExtractAndExportManifestFromPoliciesToDir(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(tt.extractAndExportManifestFromPoliciesToDirReturn()).Times(1)
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
			if tt.fetchCertManagerConfigReturn != nil {
				mockClusterconfig.EXPECT().PreserveCertManagerConfig(gomock.Any(), gomock.Any()).Return(tt.fetchCertManagerConfigReturn()).Times(1)
			}

			oldHC := CheckHealth
			defer func() {
				CheckHealth = oldHC
			}()
			CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error {
				return tt.healthCheckError
			}

			ibuTempDirNew := t.TempDir()
			if tt.exportIBUCRNew {
				origGetStaterootVarPath := getStaterootVarPath
				defer func() {
					getStaterootVarPath = origGetStaterootVarPath
				}()
				getStaterootVarPath = func(stateroot string) string {
					_ = os.MkdirAll(filepath.Join(ibuTempDirNew, "/opt"), 0777)
					return ibuTempDirNew
				}

				origGetStaterootPath := getStaterootPath
				defer func() {
					getStaterootPath = origGetStaterootPath
				}()
				getStaterootPath = func(stateroot string) string {
					_ = os.MkdirAll(filepath.Join(ibuTempDirNew, common.LCAConfigDir), 0777)
					file, _ := os.OpenFile(filepath.Join(ibuTempDirNew, utils.IBUFilePath), os.O_CREATE, 0777)
					_ = file.Close()
					return ibuTempDirNew
				}

			}
			ibuTempDirOrig := t.TempDir()
			if tt.exportIBUCRNew {
				origIbuPreStaterootPath := ibuPreStaterootPath
				defer func() {
					ibuPreStaterootPath = origIbuPreStaterootPath
				}()
				_ = os.MkdirAll(filepath.Join(ibuTempDirOrig, common.LCAConfigDir), 0777)
				file, _ := os.OpenFile(filepath.Join(ibuTempDirOrig, utils.IBUFilePath), os.O_CREATE, 0777)
				_ = file.Close()
				ibuPreStaterootPath = filepath.Join(ibuTempDirOrig, utils.IBUFilePath)
			}
			if tt.isOstreeAdminSetDefaultFeatureEnabledReturn != nil {
				ostreeclientMock.EXPECT().IsOstreeAdminSetDefaultFeatureEnabled().Return(*tt.isOstreeAdminSetDefaultFeatureEnabledReturn).Times(1)
			}
			if tt.rebootToNewStateRootReturn != nil {
				mockRebootClient.EXPECT().RebootToNewStateRoot(gomock.Any()).Return(tt.rebootToNewStateRootReturn()).Times(1)
			}

			uh := &UpgHandler{
				Client:          nil,
				NoncachedClient: nil,
				Log:             logr.Logger{},
				BackupRestore:   mockBackuprestore,
				ExtraManifest:   mockExtramanifest,
				ClusterConfig:   mockClusterconfig,
				Executor:        mockExec,
				Ops:             mockOps,
				Recorder:        events.NewFakeRecorder(1),
				RPMOstreeClient: mockRpmostreeclient,
				OstreeClient:    ostreeclientMock,
				RebootClient:    mockRebootClient,
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
					savedIbu := ibuv1.ImageBasedUpgrade{}
					err = yaml.Unmarshal(dat, &savedIbu)
					assert.Equalf(t, err, nil, "")
				}
			}
			// assert if IBU was correctly stored in orignal stateroot
			if tt.exportIBUCROrig {
				if _, err := os.Stat(filepath.Join(ibuTempDirOrig, utils.IBUFilePath)); !errors.Is(err, os.ErrNotExist) {
					dat, _ := os.ReadFile(filepath.Join(ibuTempDirOrig, utils.IBUFilePath))
					savedIbu := ibuv1.ImageBasedUpgrade{}
					_ = yaml.Unmarshal(dat, &savedIbu)
					assert.Equalf(t, len(savedIbu.Status.Conditions), 2, "")
					assert.Equalf(t, savedIbu.Status.Conditions[0].Message, utils.UpgradeFailed, "")
					assert.Equalf(t, savedIbu.Status.Conditions[1].Message, "Uncontrolled rollback", "")
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
		mockRebootClient  = reboot.NewMockRebootIntf(mockController)
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
		ibu *ibuv1.ImageBasedUpgrade
	}
	tests := []struct {
		name                          string
		fields                        fields
		args                          args
		want                          controllerruntime.Result
		wantErr                       assert.ErrorAssertionFunc
		checkHealthReturn             func(ctx context.Context, c client.Reader, l logr.Logger) error
		checkDeferredHealthReturn     func(ctx context.Context, c client.Reader, l logr.Logger) error
		applyExtraManifestsReturn     func() error
		applyPolicyManifestsReturn    func() error
		startRestoreReturn            func() (*backuprestore.RestoreTracker, error)
		restorePVsReclaimPolicyReturn func() error
		cleanupBackupsReturn          func() error
		initiateRollbackReturn        func() error
		disableInitMonitorReturn      func() error
		wantConditions                []metav1.Condition
	}{
		{
			name: "healthchecks return error",
			args: args{ibu: &ibuv1.ImageBasedUpgrade{}},
			checkHealthReturn: func(ctx context.Context, c client.Reader, l logr.Logger) error {
				return fmt.Errorf("any error from hc")
			},
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.InProgress),
					Status:  metav1.ConditionTrue,
					Message: "Waiting for system to stabilize: any error from hc",
				},
			},
			want:    requeueWithHealthCheckInterval(),
			wantErr: assert.NoError,
		},
		{
			name: "extraManifests return error",
			args: args{ibu: &ibuv1.ImageBasedUpgrade{}},
			checkHealthReturn: func(ctx context.Context, c client.Reader, l logr.Logger) error {
				return nil
			},
			applyPolicyManifestsReturn: func() error {
				return nil
			},
			applyExtraManifestsReturn: func() error {
				return extramanifest.NewEMFailedError("Test error EM")
			},
			initiateRollbackReturn: func() error {
				return nil
			},
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeCompleted),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: utils.UpgradeFailed,
				},
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: "Test error EM",
				},
			},
			wantErr: assert.NoError,
		},
		{
			name: "handleRestore with restore error",
			args: args{ibu: &ibuv1.ImageBasedUpgrade{}},
			checkHealthReturn: func(ctx context.Context, c client.Reader, l logr.Logger) error {
				return nil
			},
			applyPolicyManifestsReturn: func() error {
				return nil
			},
			applyExtraManifestsReturn: func() error {
				return nil
			},
			startRestoreReturn: func() (*backuprestore.RestoreTracker, error) {
				return &backuprestore.RestoreTracker{FailedRestores: []string{"name-failed"}}, nil
			},
			initiateRollbackReturn: func() error {
				return nil
			},
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeCompleted),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: utils.UpgradeFailed,
				},
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.Failed),
					Status:  metav1.ConditionFalse,
					Message: "Failed restores: name-failed",
				},
			},
			wantErr: assert.NoError,
		},
		{
			name: "deferred healthchecks return error after restore",
			args: args{ibu: &ibuv1.ImageBasedUpgrade{}},
			checkHealthReturn: func(ctx context.Context, c client.Reader, l logr.Logger) error {
				return nil
			},
			checkDeferredHealthReturn: func(ctx context.Context, c client.Reader, l logr.Logger) error {
				return fmt.Errorf("node is not yet ready: cnfdf40")
			},
			applyPolicyManifestsReturn: func() error {
				return nil
			},
			applyExtraManifestsReturn: func() error {
				return nil
			},
			startRestoreReturn: func() (*backuprestore.RestoreTracker, error) {
				return &backuprestore.RestoreTracker{
					SucceededRestores: []string{"name-success"},
				}, nil
			},
			restorePVsReclaimPolicyReturn: func() error {
				return nil
			},
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.InProgress),
					Status:  metav1.ConditionTrue,
					Message: "Waiting for system to fully stabilize: node is not yet ready: cnfdf40",
				},
			},
			want:    requeueWithHealthCheckInterval(),
			wantErr: assert.NoError,
		},
		{
			name: "upgrade completed",
			args: args{ibu: &ibuv1.ImageBasedUpgrade{}},
			checkHealthReturn: func(ctx context.Context, c client.Reader, l logr.Logger) error {
				return nil
			},
			applyPolicyManifestsReturn: func() error {
				return nil
			},
			applyExtraManifestsReturn: func() error {
				return nil
			},
			startRestoreReturn: func() (*backuprestore.RestoreTracker, error) {
				return &backuprestore.RestoreTracker{
					SucceededRestores: []string{"name-success"},
				}, nil
			},
			restorePVsReclaimPolicyReturn: func() error {
				return nil
			},
			disableInitMonitorReturn: func() error {
				return nil
			},
			wantConditions: []metav1.Condition{
				{
					Type:    string(utils.ConditionTypes.UpgradeInProgress),
					Reason:  string(utils.ConditionReasons.Completed),
					Status:  metav1.ConditionFalse,
					Message: utils.UpgradeCompleted,
				},
				{
					Type:    string(utils.ConditionTypes.UpgradeCompleted),
					Reason:  string(utils.ConditionReasons.Completed),
					Status:  metav1.ConditionTrue,
					Message: utils.UpgradeCompleted,
				},
			},
			wantErr: assert.NoError,
		},
	}
	for _, tt := range tests {

		t.Run(tt.name, func(t *testing.T) {
			uh := &UpgHandler{
				Client:          nil,
				NoncachedClient: nil,
				Log:             logr.Logger{},
				BackupRestore:   mockBackuprestore,
				ExtraManifest:   mockExtramanifest,
				RebootClient:    mockRebootClient,
			}

			oldMandatory := CheckMandatoryHealth
			oldDeferred := CheckDeferredHealth
			defer func() {
				CheckMandatoryHealth = oldMandatory
				CheckDeferredHealth = oldDeferred
			}()

			CheckMandatoryHealth = tt.checkHealthReturn
			if tt.checkDeferredHealthReturn != nil {
				CheckDeferredHealth = tt.checkDeferredHealthReturn
			} else {
				CheckDeferredHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error {
					return nil
				}
			}

			if tt.applyPolicyManifestsReturn != nil {
				mockExtramanifest.EXPECT().ApplyExtraManifests(gomock.Any(), common.PathOutsideChroot(extramanifest.PolicyManifestPath)).Return(tt.applyPolicyManifestsReturn()).Times(1)
			}
			if tt.applyExtraManifestsReturn != nil {
				mockExtramanifest.EXPECT().ApplyExtraManifests(gomock.Any(), common.PathOutsideChroot(extramanifest.CmManifestPath)).Return(tt.applyExtraManifestsReturn()).Times(1)
			}
			if tt.startRestoreReturn != nil {
				mockBackuprestore.EXPECT().StartRestore(gomock.Any()).Return(tt.startRestoreReturn()).Times(1)
			}
			if tt.restorePVsReclaimPolicyReturn != nil {
				mockBackuprestore.EXPECT().RestorePVsReclaimPolicy(gomock.Any()).Return(tt.restorePVsReclaimPolicyReturn()).Times(1)
			}
			if tt.initiateRollbackReturn != nil {
				mockRebootClient.EXPECT().InitiateRollback(gomock.Any()).Return(tt.initiateRollbackReturn()).Times(1)
			}
			if tt.disableInitMonitorReturn != nil {
				mockRebootClient.EXPECT().DisableInitMonitor().Return(tt.disableInitMonitorReturn()).Times(1)
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
