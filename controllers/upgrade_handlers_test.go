package controllers

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/internal/backuprestore"
	mock_backuprestore "github.com/openshift-kni/lifecycle-agent/internal/backuprestore/mocks"
	"github.com/stretchr/testify/assert"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"go.uber.org/mock/gomock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"testing"
)

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
			r := &ImageBasedUpgradeReconciler{
				Log:           logr.Logger{},
				BackupRestore: mockBackuprestore,
			}

			for _, track := range tt.trackers {
				mockBackuprestore.EXPECT().StartOrTrackBackup(gomock.Any(), gomock.Any()).Return(track())
			}

			// assert
			assert.Equalf(t, len(tt.trackers), len(tt.inputVelero), "make sure the numnber of groups and tracker match as pre cond for the test")
			got, err := r.handleBackup(context.Background(), &lcav1alpha1.ImageBasedUpgrade{})
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

			r := &ImageBasedUpgradeReconciler{
				Log:           logr.Logger{},
				BackupRestore: mockBackuprestore,
			}

			for _, track := range tt.trackers {
				mockBackuprestore.EXPECT().StartOrTrackRestore(gomock.Any(), gomock.Any()).Return(track()).Times(1)
			}

			// assert
			assert.Equalf(t, len(tt.trackers), len(tt.inputVelero), "make sure the numnber of groups and tracker match as pre cond for the test")
			got, err := r.handleRestore(context.Background())
			if !tt.wantErr(t, err, fmt.Sprintf("handleRestore(%v, %v)", context.Background(), &lcav1alpha1.ImageBasedUpgrade{})) {
				return
			}
			assert.Equalf(t, tt.wantCtlRes.RequeueAfter, got.RequeueAfter, "ctl interval: handleRestore")

		})
	}
}
