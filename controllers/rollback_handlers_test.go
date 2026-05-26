/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	controllerruntime "sigs.k8s.io/controller-runtime"
)

var rollbackFailedConditions = []metav1.Condition{
	{Type: string(utils.ConditionTypes.RollbackCompleted), Reason: string(utils.ConditionReasons.Failed), Status: metav1.ConditionFalse},
	{Type: string(utils.ConditionTypes.RollbackInProgress), Reason: string(utils.ConditionReasons.Failed), Status: metav1.ConditionFalse},
}

func assertConditionsMatch(t *testing.T, want, got []metav1.Condition) {
	t.Helper()
	assert.Len(t, got, len(want))
	for i, wantCond := range want {
		assert.Equal(t, wantCond.Type, got[i].Type)
		assert.Equal(t, wantCond.Reason, got[i].Reason)
		assert.Equal(t, wantCond.Status, got[i].Status)
	}
}

func TestHandleRollback(t *testing.T) {
	tests := []struct {
		name                     string
		isOrigStaterootBooted    bool
		isOrigStaterootBootedErr error
		wantConditions           []metav1.Condition
	}{
		{
			name:                  "orig stateroot booted completes rollback",
			isOrigStaterootBooted: true,
			wantConditions: []metav1.Condition{
				{Type: string(utils.ConditionTypes.RollbackInProgress), Reason: string(utils.ConditionReasons.Completed), Status: metav1.ConditionFalse},
				{Type: string(utils.ConditionTypes.RollbackCompleted), Reason: string(utils.ConditionReasons.Completed), Status: metav1.ConditionTrue},
			},
		},
		{
			name:                     "IsOrigStaterootBooted error fails rollback",
			isOrigStaterootBootedErr: fmt.Errorf("failed to determine booted stateroot"),
			wantConditions:           rollbackFailedConditions,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockRebootClient := reboot.NewMockRebootIntf(ctrl)
			mockRebootClient.EXPECT().IsOrigStaterootBooted(gomock.Any()).
				Return(tt.isOrigStaterootBooted, tt.isOrigStaterootBootedErr)

			r := &ImageBasedUpgradeReconciler{
				Log:          logr.Discard(),
				RebootClient: mockRebootClient,
			}

			ibu := &ibuv1.ImageBasedUpgrade{
				Spec: ibuv1.ImageBasedUpgradeSpec{
					SeedImageRef: ibuv1.SeedImageRef{Version: "4.15.2"},
				},
			}

			result, err := r.handleRollback(context.Background(), ibu)
			assert.NoError(t, err)
			assert.Equal(t, doNotRequeue(), result)
			assertConditionsMatch(t, tt.wantConditions, ibu.Status.Conditions)
		})
	}
}

func TestStartRollback(t *testing.T) {
	tests := []struct {
		name                             string
		stateroot                        string
		getUnbootedStaterootNameErr      error
		remountSysrootReturn             func() error
		getUnbootedDeploymentIndexReturn func() (int, error)
		isSetDefaultFeatureEnabled       *bool
		setDefaultDeploymentReturn       func() error
		rebootReturn                     func() error
		wantResult                       controllerruntime.Result
		wantConditions                   []metav1.Condition
	}{
		{
			name:                        "GetUnbootedStaterootName fails",
			getUnbootedStaterootNameErr: fmt.Errorf("no unbooted stateroot"),
			wantResult:                  doNotRequeue(),
			wantConditions:              rollbackFailedConditions,
		},
		{
			name:      "RemountSysroot fails",
			stateroot: "rhcos_4.15.2",
			remountSysrootReturn: func() error {
				return fmt.Errorf("remount failed")
			},
			wantResult:     doNotRequeue(),
			wantConditions: rollbackFailedConditions,
		},
		{
			name:      "GetUnbootedDeploymentIndex fails",
			stateroot: "rhcos_4.15.2",
			remountSysrootReturn: func() error {
				return nil
			},
			getUnbootedDeploymentIndexReturn: func() (int, error) {
				return 0, fmt.Errorf("deployment index error")
			},
			wantResult:     doNotRequeue(),
			wantConditions: rollbackFailedConditions,
		},
		{
			name:      "SetDefaultDeployment fails when set-default feature is enabled",
			stateroot: "rhcos_4.15.2",
			remountSysrootReturn: func() error {
				return nil
			},
			getUnbootedDeploymentIndexReturn: func() (int, error) {
				return 1, nil
			},
			isSetDefaultFeatureEnabled: BoolPointer(true),
			setDefaultDeploymentReturn: func() error {
				return fmt.Errorf("set default failed")
			},
			wantResult:     doNotRequeue(),
			wantConditions: rollbackFailedConditions,
		},
		{
			name:      "set-default feature disabled and deployment not at index 0 requeues",
			stateroot: "rhcos_4.15.2",
			remountSysrootReturn: func() error {
				return nil
			},
			getUnbootedDeploymentIndexReturn: func() (int, error) {
				return 1, nil
			},
			isSetDefaultFeatureEnabled: BoolPointer(false),
			wantResult:                 requeueWithShortInterval(),
			wantConditions: []metav1.Condition{
				{Type: string(utils.ConditionTypes.RollbackInProgress), Reason: string(utils.ConditionReasons.InProgress), Status: metav1.ConditionTrue},
			},
		},
		{
			name:      "reboot fails after successful setup",
			stateroot: "rhcos_4.15.2",
			remountSysrootReturn: func() error {
				return nil
			},
			getUnbootedDeploymentIndexReturn: func() (int, error) {
				return 1, nil
			},
			isSetDefaultFeatureEnabled: BoolPointer(true),
			setDefaultDeploymentReturn: func() error {
				return nil
			},
			rebootReturn: func() error {
				return fmt.Errorf("reboot failed")
			},
			wantResult:     doNotRequeue(),
			wantConditions: rollbackFailedConditions,
		},
		{
			name:      "successful rollback with set-default feature enabled",
			stateroot: "rhcos_4.15.2",
			remountSysrootReturn: func() error {
				return nil
			},
			getUnbootedDeploymentIndexReturn: func() (int, error) {
				return 1, nil
			},
			isSetDefaultFeatureEnabled: BoolPointer(true),
			setDefaultDeploymentReturn: func() error {
				return nil
			},
			rebootReturn: func() error {
				return nil
			},
			wantResult: doNotRequeue(),
			wantConditions: []metav1.Condition{
				{Type: string(utils.ConditionTypes.RollbackInProgress), Reason: string(utils.ConditionReasons.InProgress), Status: metav1.ConditionTrue},
			},
		},
		{
			name:      "successful rollback with set-default feature disabled and deployment at index 0",
			stateroot: "rhcos_4.15.2",
			remountSysrootReturn: func() error {
				return nil
			},
			getUnbootedDeploymentIndexReturn: func() (int, error) {
				return 0, nil
			},
			isSetDefaultFeatureEnabled: BoolPointer(false),
			rebootReturn: func() error {
				return nil
			},
			wantResult: doNotRequeue(),
			wantConditions: []metav1.Condition{
				{Type: string(utils.ConditionTypes.RollbackInProgress), Reason: string(utils.ConditionReasons.InProgress), Status: metav1.ConditionTrue},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockRpmostreeclient := rpmostreeclient.NewMockIClient(ctrl)
			mockOps := ops.NewMockOps(ctrl)
			mockOstreeClient := ostreeclient.NewMockIClient(ctrl)
			mockRebootClient := reboot.NewMockRebootIntf(ctrl)

			mockRpmostreeclient.EXPECT().GetUnbootedStaterootName().
				Return(tt.stateroot, tt.getUnbootedStaterootNameErr)

			if tt.remountSysrootReturn != nil {
				mockOps.EXPECT().RemountSysroot().Return(tt.remountSysrootReturn())
			}

			if tt.getUnbootedDeploymentIndexReturn != nil {
				mockRpmostreeclient.EXPECT().GetUnbootedDeploymentIndex().
					Return(tt.getUnbootedDeploymentIndexReturn())
			}

			if tt.isSetDefaultFeatureEnabled != nil {
				mockOstreeClient.EXPECT().IsOstreeAdminSetDefaultFeatureEnabled().
					Return(*tt.isSetDefaultFeatureEnabled)
			}

			if tt.setDefaultDeploymentReturn != nil {
				mockOstreeClient.EXPECT().SetDefaultDeployment(gomock.Any()).
					Return(tt.setDefaultDeploymentReturn())
			}

			if tt.rebootReturn != nil {
				tmpDir := t.TempDir()
				origPrefix := common.OstreeDeployPathPrefix
				common.OstreeDeployPathPrefix = tmpDir
				t.Cleanup(func() { common.OstreeDeployPathPrefix = origPrefix })

				staterootPath := common.GetStaterootPath(tt.stateroot)
				err := os.MkdirAll(filepath.Join(staterootPath, common.LCAConfigDir), 0777)
				assert.NoError(t, err)

				mockRebootClient.EXPECT().RebootToNewStateRoot("rollback").
					Return(tt.rebootReturn())
			}

			r := &ImageBasedUpgradeReconciler{
				Log:             logr.Discard(),
				RPMOstreeClient: mockRpmostreeclient,
				Ops:             mockOps,
				OstreeClient:    mockOstreeClient,
				RebootClient:    mockRebootClient,
				Recorder:        record.NewFakeRecorder(10),
			}

			ibu := &ibuv1.ImageBasedUpgrade{}

			result, err := r.startRollback(context.Background(), ibu)
			assert.NoError(t, err)
			assert.Equal(t, tt.wantResult, result)
			assertConditionsMatch(t, tt.wantConditions, ibu.Status.Conditions)
		})
	}
}

func TestStartRollback_MarshalToFileFails(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRpmostreeclient := rpmostreeclient.NewMockIClient(ctrl)
	mockOps := ops.NewMockOps(ctrl)
	mockOstreeClient := ostreeclient.NewMockIClient(ctrl)
	mockRebootClient := reboot.NewMockRebootIntf(ctrl)

	mockRpmostreeclient.EXPECT().GetUnbootedStaterootName().Return("rhcos_4.15.2", nil)
	mockOps.EXPECT().RemountSysroot().Return(nil)
	mockRpmostreeclient.EXPECT().GetUnbootedDeploymentIndex().Return(0, nil)
	mockOstreeClient.EXPECT().IsOstreeAdminSetDefaultFeatureEnabled().Return(false)

	origPrefix := common.OstreeDeployPathPrefix
	tmpFile, createErr := os.CreateTemp(t.TempDir(), "ostree-prefix-*")
	assert.NoError(t, createErr)
	assert.NoError(t, tmpFile.Close())
	common.OstreeDeployPathPrefix = tmpFile.Name()
	t.Cleanup(func() { common.OstreeDeployPathPrefix = origPrefix })

	r := &ImageBasedUpgradeReconciler{
		Log:             logr.Discard(),
		RPMOstreeClient: mockRpmostreeclient,
		Ops:             mockOps,
		OstreeClient:    mockOstreeClient,
		RebootClient:    mockRebootClient,
		Recorder:        record.NewFakeRecorder(10),
	}

	ibu := &ibuv1.ImageBasedUpgrade{}

	result, err := r.startRollback(context.Background(), ibu)
	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue(), result)
	assertConditionsMatch(t, rollbackFailedConditions, ibu.Status.Conditions)
	assert.Contains(t, ibu.Status.Conditions[1].Message, "failed to write file")
}

func TestFinishRollback(t *testing.T) {
	r := &ImageBasedUpgradeReconciler{
		Log: logr.Discard(),
	}

	ibu := &ibuv1.ImageBasedUpgrade{}

	result, err := r.finishRollback(ibu)
	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue(), result)
	assertConditionsMatch(t, []metav1.Condition{
		{Type: string(utils.ConditionTypes.RollbackInProgress), Reason: string(utils.ConditionReasons.Completed), Status: metav1.ConditionFalse},
		{Type: string(utils.ConditionTypes.RollbackCompleted), Reason: string(utils.ConditionReasons.Completed), Status: metav1.ConditionTrue},
	}, ibu.Status.Conditions)
}
