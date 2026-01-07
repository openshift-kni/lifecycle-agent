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
	"reflect"
	"testing"

	"github.com/go-logr/logr"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	ipcv1 "github.com/openshift-kni/lifecycle-agent/api/ipconfig/v1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var (
	testscheme = scheme.Scheme
)

func init() {
	testscheme.AddKnownTypes(ibuv1.GroupVersion, &ibuv1.ImageBasedUpgrade{})
	testscheme.AddKnownTypes(ipcv1.GroupVersion, &ipcv1.IPConfig{})
}

func getFakeClientFromObjects(objs ...client.Object) (client.WithWatch, error) {
	c := fake.NewClientBuilder().WithScheme(testscheme).WithObjects(objs...).WithStatusSubresource(objs...).Build()
	return c, nil
}

type Condition struct {
	Type   utils.ConditionType
	Status v1.ConditionStatus
	Reason utils.ConditionReason
}

func TestIsTransitionRequested(t *testing.T) {
	testcases := []struct {
		name         string
		desiredStage ibuv1.ImageBasedUpgradeStage
		expected     bool
		conditions   []Condition
	}{
		{
			name:         "idle while idle is true",
			desiredStage: ibuv1.Stages.Idle,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.Idle, metav1.ConditionTrue, ""},
			},
		},
		{
			name:         "idle while aborting",
			desiredStage: ibuv1.Stages.Idle,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.Aborting},
			},
		},
		{
			name:         "idle while finalizing",
			desiredStage: ibuv1.Stages.Idle,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.Finalizing},
			},
		},
		{
			name:         "idle while abort failed",
			desiredStage: ibuv1.Stages.Idle,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.AbortFailed},
			},
		},
		{
			name:         "idle while finalize failed",
			desiredStage: ibuv1.Stages.Idle,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.FinalizeFailed},
			},
		},
		{
			name:         "idle when prep in progress",
			desiredStage: ibuv1.Stages.Idle,
			conditions: []Condition{
				{utils.ConditionTypes.PrepInProgress, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "idle when prep completed",
			desiredStage: ibuv1.Stages.Idle,
			conditions: []Condition{
				{utils.ConditionTypes.PrepCompleted, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "idle when prep failed",
			desiredStage: ibuv1.Stages.Idle,
			conditions: []Condition{
				{utils.ConditionTypes.PrepCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.PrepInProgress, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "idle when upgrade completed",
			desiredStage: ibuv1.Stages.Idle,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "idle when rollback completed",
			desiredStage: ibuv1.Stages.Idle,
			conditions: []Condition{
				{utils.ConditionTypes.RollbackCompleted, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "idle when upgrade faild",
			desiredStage: ibuv1.Stages.Idle,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "idle when rollback failed",
			desiredStage: ibuv1.Stages.Idle,
			conditions: []Condition{
				{utils.ConditionTypes.RollbackCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "prep when prep completed",
			desiredStage: ibuv1.Stages.Prep,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.PrepCompleted, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.PrepInProgress, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
		},
		{
			name:         "prep with idle true",
			desiredStage: ibuv1.Stages.Prep,
			conditions:   []Condition{{utils.ConditionTypes.Idle, metav1.ConditionTrue, ""}},
			expected:     true,
		},
		{
			name:         "prep when prep failed",
			desiredStage: ibuv1.Stages.Prep,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.PrepCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.PrepInProgress, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
		},
		{
			name:         "prep when prep in progress",
			desiredStage: ibuv1.Stages.Prep,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.PrepInProgress, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
		},
		{
			name:         "upgrade with prep completed",
			desiredStage: ibuv1.Stages.Upgrade,
			conditions: []Condition{
				{utils.ConditionTypes.PrepCompleted, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "upgrade when upgrade completed",
			desiredStage: ibuv1.Stages.Upgrade,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
		},
		{
			name:         "upgrade when upgrade failed",
			desiredStage: ibuv1.Stages.Upgrade,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
		},
		{
			name:         "upgrade when upgrade in progress",
			desiredStage: ibuv1.Stages.Upgrade,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
		},
		{
			name:         "rollback when rollback completed",
			desiredStage: ibuv1.Stages.Rollback,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.RollbackCompleted, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.RollbackInProgress, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
		},
		{
			name:         "rollback when rollback failed",
			desiredStage: ibuv1.Stages.Rollback,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.RollbackCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.RollbackInProgress, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
		},
		{
			name:         "rollback when rollback in progress",
			desiredStage: ibuv1.Stages.Rollback,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.RollbackInProgress, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
		},
		{
			name:         "rollback when upgrade failed",
			desiredStage: ibuv1.Stages.Rollback,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "rollback when upgrade completed",
			desiredStage: ibuv1.Stages.Rollback,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "rollback when upgrade in progress",
			desiredStage: ibuv1.Stages.Rollback,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			var ibu = &ibuv1.ImageBasedUpgrade{
				ObjectMeta: v1.ObjectMeta{
					Name: utils.IBUName,
				},
			}
			ibu.Spec.Stage = tc.desiredStage
			for _, c := range tc.conditions {
				utils.SetStatusCondition(&ibu.Status.Conditions,
					c.Type, c.Reason, c.Status, "message", ibu.Generation)
			}
			value := isTransitionRequested(ibu)
			assert.Equal(t, tc.expected, value)
		})
	}
}

func TestValidateStageTransisions(t *testing.T) {
	type ExpectedCondition struct {
		ConditionType   utils.ConditionType
		ConditionReason utils.ConditionReason
		ConditionStatus v1.ConditionStatus
		Message         string
	}
	testcases := []struct {
		name                     string
		stage                    ibuv1.ImageBasedUpgradeStage
		conditions               []Condition
		expectedConditions       []ExpectedCondition
		unexpectedConditionTypes []utils.ConditionType
		expected                 bool
		afterPivot               bool
	}{
		{
			name:       "idle when prep in progress",
			stage:      ibuv1.Stages.Idle,
			conditions: []Condition{{utils.ConditionTypes.PrepInProgress, metav1.ConditionTrue, ""}},
			expected:   true,
		},
		{
			name:       "idle when prep completed",
			stage:      ibuv1.Stages.Idle,
			conditions: []Condition{{utils.ConditionTypes.PrepCompleted, metav1.ConditionTrue, ""}},
			expected:   true,
		},
		{
			name:  "idle when prep failed",
			stage: ibuv1.Stages.Idle,
			conditions: []Condition{{utils.ConditionTypes.PrepCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.PrepInProgress, metav1.ConditionFalse, ""}},
			expected: true,
		},
		{
			name:  "idle when prep failed with invalid upgrade transition",
			stage: ibuv1.Stages.Idle,
			conditions: []Condition{{utils.ConditionTypes.PrepCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.PrepInProgress, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionFalse, utils.ConditionReasons.InvalidTransition}},
			expected:                 true,
			unexpectedConditionTypes: []utils.ConditionType{utils.ConditionTypes.UpgradeInProgress},
		},
		{
			name:  "idle when prep failed with invalid rollback transition",
			stage: ibuv1.Stages.Idle,
			conditions: []Condition{{utils.ConditionTypes.PrepCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.PrepInProgress, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.RollbackInProgress, metav1.ConditionFalse, utils.ConditionReasons.InvalidTransition}},
			expected:                 true,
			unexpectedConditionTypes: []utils.ConditionType{utils.ConditionTypes.RollbackInProgress},
		},
		{
			name:       "idle when upgrade completed",
			stage:      ibuv1.Stages.Idle,
			conditions: []Condition{{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionTrue, ""}},
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.Finalizing, metav1.ConditionFalse,
				utils.Finalizing,
			}},
			expected: true,
		},
		{
			name:       "idle when upgrade failed before pivot or after uncontrolled rollback",
			stage:      ibuv1.Stages.Idle,
			afterPivot: false,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, utils.ConditionReasons.Failed},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.Aborting,
				metav1.ConditionFalse,
				utils.Aborting,
			}},
			expected: true,
		},
		{
			name:       "idle when upgrade failed with invalid rollback transition",
			stage:      ibuv1.Stages.Idle,
			afterPivot: false,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, utils.ConditionReasons.Failed},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
				{utils.ConditionTypes.RollbackInProgress, metav1.ConditionFalse, utils.ConditionReasons.InvalidTransition},
			},
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.Aborting,
				metav1.ConditionFalse,
				utils.Aborting,
			}},
			expected: true,
		},
		{
			name:       "idle when upgrade failed after pivot",
			stage:      ibuv1.Stages.Idle,
			afterPivot: true,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.InvalidTransition,
				metav1.ConditionFalse,
				"Transition to Idle not allowed - Rollback first",
			}},
			expected: false,
		},
		{
			name:       "idle when upgrade in progress before pivot",
			stage:      ibuv1.Stages.Idle,
			afterPivot: false,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionTrue, utils.ConditionReasons.InProgress},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.Aborting, metav1.ConditionFalse,
				utils.Aborting,
			}},
			expected: true,
		},
		{
			name:       "idle when upgrade in progress after pivot",
			stage:      ibuv1.Stages.Idle,
			afterPivot: true,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionTrue, utils.ConditionReasons.InProgress},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.InvalidTransition,
				metav1.ConditionFalse,
				"Transition to Idle not allowed - Rollback first",
			}},
			expected: false,
		},
		{
			name:       "idle when rollback in progress",
			stage:      ibuv1.Stages.Idle,
			afterPivot: true,
			conditions: []Condition{
				{utils.ConditionTypes.RollbackInProgress, metav1.ConditionTrue, utils.ConditionReasons.InProgress},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, utils.ConditionReasons.Failed},
			},
			expected: false,
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.InvalidTransition,
				metav1.ConditionFalse,
				"Transition to Idle not allowed - Rollback is in progress",
			}},
		},
		{
			name:       "idle when rollback in progress after pivoting back",
			stage:      ibuv1.Stages.Idle,
			afterPivot: false,
			conditions: []Condition{
				{utils.ConditionTypes.RollbackInProgress, metav1.ConditionTrue, utils.ConditionReasons.InProgress},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, utils.ConditionReasons.Failed},
			},
			expected: false,
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.InvalidTransition,
				metav1.ConditionFalse,
				"Abort or finalize not allowed",
			}},
		},
		{
			name:  "idle when rollback failed",
			stage: ibuv1.Stages.Idle,
			conditions: []Condition{{utils.ConditionTypes.RollbackCompleted, metav1.ConditionFalse, utils.ConditionReasons.Failed},
				{utils.ConditionTypes.RollbackInProgress, metav1.ConditionFalse, utils.ConditionReasons.Failed},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress}},
			afterPivot: true,
			expected:   false,
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.InvalidTransition, metav1.ConditionFalse,
				"Transition to Idle not allowed - Rollback failed",
			}},
		},
		{
			name:  "idle when rollback failed after pivoting back",
			stage: ibuv1.Stages.Idle,
			conditions: []Condition{{utils.ConditionTypes.RollbackCompleted, metav1.ConditionFalse, utils.ConditionReasons.Failed},
				{utils.ConditionTypes.RollbackInProgress, metav1.ConditionFalse, utils.ConditionReasons.Failed},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress}},
			expected: false,
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.InvalidTransition, metav1.ConditionFalse,
				"Transition to Idle not allowed - Rollback failed",
			}},
		},
		{
			name:       "idle when rollback completed",
			stage:      ibuv1.Stages.Idle,
			conditions: []Condition{{utils.ConditionTypes.RollbackCompleted, metav1.ConditionTrue, ""}},
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.Finalizing, metav1.ConditionFalse,
				utils.Finalizing,
			}},
			expected: true,
		},
		{
			name:       "prep without idle completed",
			stage:      ibuv1.Stages.Prep,
			conditions: []Condition{},
			expected:   false,
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.PrepInProgress,
				utils.ConditionReasons.InvalidTransition,
				metav1.ConditionFalse,
				"Previous stage not succeeded",
			}},
		},
		{
			name:       "prep with idle completed",
			stage:      ibuv1.Stages.Prep,
			conditions: []Condition{{utils.ConditionTypes.Idle, metav1.ConditionTrue, ""}},
			expected:   true,
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.InProgress,
				metav1.ConditionFalse,
				utils.InProgress,
			}, {
				utils.ConditionTypes.PrepInProgress,
				utils.ConditionReasons.InProgress,
				metav1.ConditionTrue,
				utils.InProgress,
			}},
		},
		{
			name:  "prep when idle completed with invalid upgrade transition",
			stage: ibuv1.Stages.Prep,
			conditions: []Condition{{utils.ConditionTypes.Idle, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionFalse, utils.ConditionReasons.InvalidTransition}},
			expected: true,
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.InProgress,
				metav1.ConditionFalse,
				utils.InProgress,
			}, {
				utils.ConditionTypes.PrepInProgress,
				utils.ConditionReasons.InProgress,
				metav1.ConditionTrue,
				utils.InProgress,
			}},
			unexpectedConditionTypes: []utils.ConditionType{utils.ConditionTypes.UpgradeInProgress},
		},
		{
			name:  "prep when idle completed with invalid rollback transition",
			stage: ibuv1.Stages.Prep,
			conditions: []Condition{{utils.ConditionTypes.Idle, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.RollbackInProgress, metav1.ConditionFalse, utils.ConditionReasons.InvalidTransition}},
			expected: true,
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.InProgress,
				metav1.ConditionFalse,
				utils.InProgress,
			}, {
				utils.ConditionTypes.PrepInProgress,
				utils.ConditionReasons.InProgress,
				metav1.ConditionTrue,
				utils.InProgress,
			}},
			unexpectedConditionTypes: []utils.ConditionType{utils.ConditionTypes.RollbackInProgress},
		},
		{
			name:       "upgrade without prep completed",
			stage:      ibuv1.Stages.Upgrade,
			conditions: []Condition{},
			expected:   false,
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.UpgradeInProgress,
				utils.ConditionReasons.InvalidTransition,
				metav1.ConditionFalse,
				"Previous stage not succeeded",
			}},
		},
		{
			name:       "upgrade with prep completed",
			stage:      ibuv1.Stages.Upgrade,
			conditions: []Condition{{utils.ConditionTypes.PrepCompleted, metav1.ConditionTrue, ""}},
			expected:   true,
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.UpgradeInProgress,
				utils.ConditionReasons.InProgress,
				metav1.ConditionTrue,
				utils.InProgress,
			}},
		},
		{
			name:  "upgrade when prep completed with invalid rollback transition",
			stage: ibuv1.Stages.Upgrade,
			conditions: []Condition{{utils.ConditionTypes.Idle, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.PrepCompleted, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.RollbackInProgress, metav1.ConditionFalse, utils.ConditionReasons.InvalidTransition}},
			expected: true,
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.UpgradeInProgress,
				utils.ConditionReasons.InProgress,
				metav1.ConditionTrue,
				utils.InProgress,
			}},
			unexpectedConditionTypes: []utils.ConditionType{utils.ConditionTypes.RollbackInProgress},
		},
		{
			name:  "rollback when upgrade failed",
			stage: ibuv1.Stages.Rollback,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionFalse, ""},
			},
			expected: true,
			expectedConditions: []ExpectedCondition{
				{
					utils.ConditionTypes.UpgradeInProgress,
					utils.ConditionReasons.Failed,
					metav1.ConditionFalse,
					utils.RollbackRequested,
				},
				{
					utils.ConditionTypes.UpgradeCompleted,
					utils.ConditionReasons.Failed,
					metav1.ConditionFalse,
					utils.RollbackRequested,
				},
				{
					utils.ConditionTypes.RollbackInProgress,
					utils.ConditionReasons.InProgress,
					metav1.ConditionTrue,
					utils.InProgress,
				},
			},
			afterPivot: true,
		},
		{
			name:  "rollback when upgrade completed",
			stage: ibuv1.Stages.Rollback,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionFalse, ""},
			},
			expected: true,
			expectedConditions: []ExpectedCondition{
				{
					utils.ConditionTypes.UpgradeInProgress,
					utils.ConditionReasons.Failed,
					metav1.ConditionFalse,
					utils.RollbackRequested,
				},
				{
					utils.ConditionTypes.UpgradeCompleted,
					utils.ConditionReasons.Failed,
					metav1.ConditionFalse,
					utils.RollbackRequested,
				},
				{
					utils.ConditionTypes.RollbackInProgress,
					utils.ConditionReasons.InProgress,
					metav1.ConditionTrue,
					utils.InProgress,
				},
			},
			afterPivot: true,
		},
		{
			name:  "rollback when upgrade completed with invalid Idle transition",
			stage: ibuv1.Stages.Rollback,
			conditions: []Condition{
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InvalidTransition},
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionFalse, ""},
			},
			expected: true,
			expectedConditions: []ExpectedCondition{
				{
					utils.ConditionTypes.Idle,
					utils.ConditionReasons.InProgress,
					metav1.ConditionFalse,
					utils.InProgress,
				},
				{
					utils.ConditionTypes.UpgradeInProgress,
					utils.ConditionReasons.Failed,
					metav1.ConditionFalse,
					utils.RollbackRequested,
				},
				{
					utils.ConditionTypes.UpgradeCompleted,
					utils.ConditionReasons.Failed,
					metav1.ConditionFalse,
					utils.RollbackRequested,
				},
				{
					utils.ConditionTypes.RollbackInProgress,
					utils.ConditionReasons.InProgress,
					metav1.ConditionTrue,
					utils.InProgress,
				},
			},
			afterPivot: true,
		},
		{
			name:       "rollback when upgrade in progress",
			stage:      ibuv1.Stages.Rollback,
			conditions: []Condition{{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionTrue, ""}},
			expected:   true,
			expectedConditions: []ExpectedCondition{
				{
					utils.ConditionTypes.UpgradeInProgress,
					utils.ConditionReasons.Failed,
					metav1.ConditionFalse,
					utils.RollbackRequested,
				},
				{
					utils.ConditionTypes.UpgradeCompleted,
					utils.ConditionReasons.Failed,
					metav1.ConditionFalse,
					utils.RollbackRequested,
				},
				{
					utils.ConditionTypes.RollbackInProgress,
					utils.ConditionReasons.InProgress,
					metav1.ConditionTrue,
					utils.InProgress,
				},
			},
			afterPivot: true,
		},
		{
			name:       "rollback without upgrade in progress",
			stage:      ibuv1.Stages.Rollback,
			conditions: []Condition{},
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.RollbackInProgress,
				utils.ConditionReasons.InvalidTransition, metav1.ConditionFalse,
				"Upgrade not started or already finalized",
			}},
			expected:   false,
			afterPivot: true,
		},
	}
	for _, tc := range testcases {

		var ibu = &ibuv1.ImageBasedUpgrade{
			ObjectMeta: v1.ObjectMeta{
				Name: utils.IBUName,
			},
			Spec: ibuv1.ImageBasedUpgradeSpec{
				Stage: tc.stage,
			},
		}
		for _, c := range tc.conditions {
			utils.SetStatusCondition(&ibu.Status.Conditions,
				c.Type, c.Reason, c.Status, "message", ibu.Generation)
		}

		t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			result := validateStageTransition(ibu, tc.afterPivot)
			assert.Equal(t, tc.expected, result)
			for _, expectedCondition := range tc.expectedConditions {
				con := meta.FindStatusCondition(ibu.Status.Conditions, string(expectedCondition.ConditionType))
				assert.Equal(t, con == nil, false)
				if con != nil {
					assert.Equal(t, expectedCondition, ExpectedCondition{
						utils.ConditionType(con.Type),
						utils.ConditionReason(con.Reason),
						con.Status,
						con.Message})
				}
			}

			for _, unexpectedConditionT := range tc.unexpectedConditionTypes {
				con := meta.FindStatusCondition(ibu.Status.Conditions, string(unexpectedConditionT))
				assert.Equal(t, con == nil, true)
			}
		})
	}
}

func TestImageBasedUpgradeReconciler_Reconcile(t *testing.T) {
	testcases := []struct {
		name         string
		ibu          client.Object
		ipc          client.Object
		request      reconcile.Request
		validateFunc func(t *testing.T, result ctrl.Result, ibu *ibuv1.ImageBasedUpgrade)
	}{
		{
			name: "idle IBU",
			ibu: &ibuv1.ImageBasedUpgrade{
				ObjectMeta: v1.ObjectMeta{
					Name: utils.IBUName,
				},
				Spec: ibuv1.ImageBasedUpgradeSpec{
					Stage: ibuv1.Stages.Idle,
				},
			},
			// Ensure IBU gating does not requeue early due to a missing IPConfig CR.
			// (If IPConfig exists but is not initialized yet, reconciliation is allowed to proceed.)
			ipc: &ipcv1.IPConfig{
				ObjectMeta: v1.ObjectMeta{
					Name:       common.IPConfigName,
					Generation: 5,
				},
				Spec: ipcv1.IPConfigSpec{
					Stage: ipcv1.IPStages.Idle,
				},
			},
			request: reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: utils.IBUName,
				},
			},
			validateFunc: func(t *testing.T, result ctrl.Result, ibu *ibuv1.ImageBasedUpgrade) {
				idleCondition := meta.FindStatusCondition(ibu.Status.Conditions, string(utils.ConditionTypes.Idle))
				assert.Equal(t, idleCondition.Status, metav1.ConditionTrue)
				if result != doNotRequeue() {
					t.Errorf("expect no requeue")
				}
			},
		},
	}
	for _, tc := range testcases {
		t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{tc.ibu}
			if tc.ipc != nil {
				objs = append(objs, tc.ipc)
			}
			fakeClient, err := getFakeClientFromObjects(objs...)
			if err != nil {
				t.Errorf("error in creating fake client")
			}

			ctrl := gomock.NewController(t)
			mockClient := rpmostreeclient.NewMockIClient(ctrl)
			mockClient.EXPECT().IsStaterootBooted("rhcos_").Return(false, nil)

			r := &ImageBasedUpgradeReconciler{
				Client:          fakeClient,
				NoncachedClient: fakeClient,
				Log:             logr.Discard(),
				Scheme:          fakeClient.Scheme(),
				RPMOstreeClient: mockClient,
			}
			result, err := r.Reconcile(context.TODO(), tc.request)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			ibu := &ibuv1.ImageBasedUpgrade{}
			if err := fakeClient.Get(context.TODO(), types.NamespacedName{Name: utils.IBUName}, ibu); err != nil {
				t.Errorf("unexcepted error: %v", err.Error())
			}
			tc.validateFunc(t, result, ibu)
		})
	}
}

func Test_getValidNextStageList(t *testing.T) {
	tests := []struct {
		name          string
		isAfterPivot  bool
		conditions    []Condition
		wantStageList []ibuv1.ImageBasedUpgradeStage
	}{
		{
			name:          "prep in progress",
			conditions:    []Condition{{utils.ConditionTypes.PrepInProgress, metav1.ConditionTrue, ""}},
			wantStageList: []ibuv1.ImageBasedUpgradeStage{ibuv1.Stages.Idle},
		},
		{
			name:          "prep completed",
			conditions:    []Condition{{utils.ConditionTypes.PrepCompleted, metav1.ConditionTrue, ""}},
			wantStageList: []ibuv1.ImageBasedUpgradeStage{ibuv1.Stages.Idle, ibuv1.Stages.Upgrade},
		},
		{
			name: "prep failed",
			conditions: []Condition{{utils.ConditionTypes.PrepCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.PrepInProgress, metav1.ConditionFalse, ""}},
			wantStageList: []ibuv1.ImageBasedUpgradeStage{ibuv1.Stages.Idle},
		},
		{
			name:          "upgrade completed",
			conditions:    []Condition{{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionTrue, ""}},
			wantStageList: []ibuv1.ImageBasedUpgradeStage{ibuv1.Stages.Idle, ibuv1.Stages.Rollback},
		},
		{
			name:         "upgrade failed before pivot",
			isAfterPivot: false,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			wantStageList: []ibuv1.ImageBasedUpgradeStage{ibuv1.Stages.Idle},
		},
		{
			name:         "upgrade failed after pivot",
			isAfterPivot: true,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			wantStageList: []ibuv1.ImageBasedUpgradeStage{ibuv1.Stages.Rollback},
		},
		{
			name:         "upgrade in progress before pivot",
			isAfterPivot: false,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionTrue, utils.ConditionReasons.InProgress},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			wantStageList: []ibuv1.ImageBasedUpgradeStage{ibuv1.Stages.Idle},
		},
		{
			name:         "upgrade in progress after pivot",
			isAfterPivot: true,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionTrue, utils.ConditionReasons.InProgress},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			wantStageList: []ibuv1.ImageBasedUpgradeStage{ibuv1.Stages.Rollback},
		},
		{
			name:         "rollback in progress",
			isAfterPivot: true,
			conditions: []Condition{
				{utils.ConditionTypes.RollbackInProgress, metav1.ConditionTrue, utils.ConditionReasons.InProgress},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, utils.ConditionReasons.Failed},
			},
			wantStageList: []ibuv1.ImageBasedUpgradeStage{},
		},
		{
			name:         "rollback in progress after pivoting back",
			isAfterPivot: false,
			conditions: []Condition{
				{utils.ConditionTypes.RollbackInProgress, metav1.ConditionTrue, utils.ConditionReasons.InProgress},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, utils.ConditionReasons.Failed},
			},
			wantStageList: []ibuv1.ImageBasedUpgradeStage{},
		},
		{
			// TODO consider allowing finalize the successful upgrade after a rollback failure
			name: "rollback failed after successful upgrade",
			conditions: []Condition{{utils.ConditionTypes.RollbackCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionTrue, utils.ConditionReasons.Completed}},
			wantStageList: []ibuv1.ImageBasedUpgradeStage{},
		},
		{
			name: "rollback failed after upgrade failure",
			conditions: []Condition{{utils.ConditionTypes.RollbackCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, utils.ConditionReasons.Failed}},
			wantStageList: []ibuv1.ImageBasedUpgradeStage{},
		},
		{
			name:          "rollback completed",
			conditions:    []Condition{{utils.ConditionTypes.RollbackCompleted, metav1.ConditionTrue, ""}},
			wantStageList: []ibuv1.ImageBasedUpgradeStage{ibuv1.Stages.Idle},
		},
		{
			name:          "finalizing",
			conditions:    []Condition{{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.Finalizing}},
			wantStageList: []ibuv1.ImageBasedUpgradeStage{},
		},
		{
			name:          "finalize failed",
			conditions:    []Condition{{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.FinalizeFailed}},
			wantStageList: []ibuv1.ImageBasedUpgradeStage{},
		},
		{
			name:          "aborting",
			conditions:    []Condition{{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.Aborting}},
			wantStageList: []ibuv1.ImageBasedUpgradeStage{},
		},
		{
			name:          "abort failed",
			conditions:    []Condition{{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.AbortFailed}},
			wantStageList: []ibuv1.ImageBasedUpgradeStage{},
		},
		{
			name:          "idle",
			conditions:    []Condition{{utils.ConditionTypes.Idle, metav1.ConditionTrue, ""}},
			wantStageList: []ibuv1.ImageBasedUpgradeStage{ibuv1.Stages.Prep},
		},
		{
			name:          "initial IBU creation - no status conditions",
			conditions:    []Condition{},
			wantStageList: []ibuv1.ImageBasedUpgradeStage{ibuv1.Stages.Idle},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ibu := &ibuv1.ImageBasedUpgrade{}
			for _, condition := range tt.conditions {
				utils.SetStatusCondition(&ibu.Status.Conditions, condition.Type, condition.Reason, condition.Status, "", 1)
			}
			if gotStageList := getValidNextStageList(ibu, tt.isAfterPivot); !reflect.DeepEqual(gotStageList, tt.wantStageList) {
				t.Errorf("getValidNextStageList() = %v, want %v", gotStageList, tt.wantStageList)
			}
		})
	}
}

func TestImageBasedUpgradeReconciler_gateIBUByIPConfig(t *testing.T) {
	t.Run("ipconfig not found => requeues soon (no status update)", func(t *testing.T) {
		ibuObj := &ibuv1.ImageBasedUpgrade{
			ObjectMeta: v1.ObjectMeta{
				Name:       utils.IBUName,
				Generation: 7,
			},
			Spec: ibuv1.ImageBasedUpgradeSpec{
				Stage: ibuv1.Stages.Prep,
			},
		}

		c, err := getFakeClientFromObjects(ibuObj)
		if err != nil {
			t.Fatalf("failed to create fake client: %v", err)
		}

		ibu := &ibuv1.ImageBasedUpgrade{}
		assert.NoError(t, c.Get(context.TODO(), types.NamespacedName{Name: utils.IBUName}, ibu))

		r := &ImageBasedUpgradeReconciler{Client: c, NoncachedClient: c}
		res, err := r.gateIBUByIPConfig(context.TODO(), ibu)
		assert.NoError(t, err)
		assert.Equal(t, requeueImmediately(), res)

		updated := &ibuv1.ImageBasedUpgrade{}
		assert.NoError(t, c.Get(context.TODO(), types.NamespacedName{Name: utils.IBUName}, updated))
		cond := meta.FindStatusCondition(updated.Status.Conditions, string(utils.ConditionTypes.PrepInProgress))
		assert.Nil(t, cond)
	})

	t.Run("ipconfig exists but has no conditions => allowed (ipconfig not initialized)", func(t *testing.T) {
		ibuObj := &ibuv1.ImageBasedUpgrade{
			ObjectMeta: v1.ObjectMeta{
				Name:       utils.IBUName,
				Generation: 7,
			},
			Spec: ibuv1.ImageBasedUpgradeSpec{
				Stage: ibuv1.Stages.Prep,
			},
		}

		ipcObj := &ipcv1.IPConfig{
			ObjectMeta: v1.ObjectMeta{
				Name:       common.IPConfigName,
				Generation: 5,
			},
			Spec: ipcv1.IPConfigSpec{
				Stage: ipcv1.IPStages.Idle,
			},
		}

		c, err := getFakeClientFromObjects(ibuObj, ipcObj)
		if err != nil {
			t.Fatalf("failed to create fake client: %v", err)
		}

		ibu := &ibuv1.ImageBasedUpgrade{}
		assert.NoError(t, c.Get(context.TODO(), types.NamespacedName{Name: utils.IBUName}, ibu))

		r := &ImageBasedUpgradeReconciler{Client: c, NoncachedClient: c}
		res, err := r.gateIBUByIPConfig(context.TODO(), ibu)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := &ibuv1.ImageBasedUpgrade{}
		assert.NoError(t, c.Get(context.TODO(), types.NamespacedName{Name: utils.IBUName}, updated))
		cond := meta.FindStatusCondition(updated.Status.Conditions, string(utils.ConditionTypes.PrepInProgress))
		assert.Nil(t, cond)
	})

	t.Run("ipconfig exists and is not idle => blocked and requeues short interval", func(t *testing.T) {
		ibuObj := &ibuv1.ImageBasedUpgrade{
			ObjectMeta: v1.ObjectMeta{
				Name:       utils.IBUName,
				Generation: 7,
			},
			Spec: ibuv1.ImageBasedUpgradeSpec{
				Stage: ibuv1.Stages.Prep,
			},
		}

		ipcObj := &ipcv1.IPConfig{
			ObjectMeta: v1.ObjectMeta{
				Name:       common.IPConfigName,
				Generation: 5,
			},
			Spec: ipcv1.IPConfigSpec{
				Stage: ipcv1.IPStages.Config,
			},
		}
		utils.SetIPConfigStatusInProgress(ipcObj, "config in progress")

		c, err := getFakeClientFromObjects(ibuObj, ipcObj)
		if err != nil {
			t.Fatalf("failed to create fake client: %v", err)
		}

		ibu := &ibuv1.ImageBasedUpgrade{}
		assert.NoError(t, c.Get(context.TODO(), types.NamespacedName{Name: utils.IBUName}, ibu))

		r := &ImageBasedUpgradeReconciler{Client: c, NoncachedClient: c}
		res, err := r.gateIBUByIPConfig(context.TODO(), ibu)
		assert.NoError(t, err)
		assert.Equal(t, requeueWithShortInterval(), res)

		updated := &ibuv1.ImageBasedUpgrade{}
		assert.NoError(t, c.Get(context.TODO(), types.NamespacedName{Name: utils.IBUName}, updated))
		cond := meta.FindStatusCondition(updated.Status.Conditions, string(utils.ConditionTypes.PrepInProgress))
		if assert.NotNil(t, cond) {
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, string(utils.ConditionReasons.Blocked), cond.Reason)
			assert.Equal(t, utils.IPCNotIdle, cond.Message)
		}
	})

	t.Run("ipconfig not idle but blocked => still blocked (no deadlock avoidance)", func(t *testing.T) {
		ibuObj := &ibuv1.ImageBasedUpgrade{
			ObjectMeta: v1.ObjectMeta{
				Name:       utils.IBUName,
				Generation: 7,
			},
			Spec: ibuv1.ImageBasedUpgradeSpec{
				Stage: ibuv1.Stages.Prep,
			},
		}
		utils.SetIBUStatusBlocked(ibuObj, "Blocked by gating: previous")

		ipcObj := &ipcv1.IPConfig{
			ObjectMeta: v1.ObjectMeta{
				Name:       common.IPConfigName,
				Generation: 5,
			},
			Spec: ipcv1.IPConfigSpec{
				Stage: ipcv1.IPStages.Config,
			},
		}
		utils.SetIPStatusBlocked(ipcObj, utils.IBUNotIdle)

		c, err := getFakeClientFromObjects(ibuObj, ipcObj)
		if err != nil {
			t.Fatalf("failed to create fake client: %v", err)
		}

		ibu := &ibuv1.ImageBasedUpgrade{}
		assert.NoError(t, c.Get(context.TODO(), types.NamespacedName{Name: utils.IBUName}, ibu))

		r := &ImageBasedUpgradeReconciler{Client: c, NoncachedClient: c}
		res, err := r.gateIBUByIPConfig(context.TODO(), ibu)
		assert.NoError(t, err)
		// IPC being Blocked is treated as "allowed to proceed" (deadlock avoidance).
		// If IBU was previously blocked, gating should unblock it and not requeue.
		assert.Equal(t, doNotRequeue(), res)

		updated := &ibuv1.ImageBasedUpgrade{}
		assert.NoError(t, c.Get(context.TODO(), types.NamespacedName{Name: utils.IBUName}, updated))
		cond := meta.FindStatusCondition(updated.Status.Conditions, string(utils.ConditionTypes.PrepInProgress))
		assert.Nil(t, cond)
	})

	t.Run("ipconfig idle => allowed and unblocks self", func(t *testing.T) {
		ibuObj := &ibuv1.ImageBasedUpgrade{
			ObjectMeta: v1.ObjectMeta{
				Name:       utils.IBUName,
				Generation: 7,
			},
			Spec: ibuv1.ImageBasedUpgradeSpec{
				Stage: ibuv1.Stages.Prep,
			},
		}
		utils.SetIBUStatusBlocked(ibuObj, "Blocked by gating: previous")

		ipcObj := &ipcv1.IPConfig{
			ObjectMeta: v1.ObjectMeta{
				Name:       common.IPConfigName,
				Generation: 5,
			},
			Spec: ipcv1.IPConfigSpec{
				Stage: ipcv1.IPStages.Idle,
			},
		}
		utils.ResetStatusConditions(&ipcObj.Status.Conditions, ipcObj.Generation)

		c, err := getFakeClientFromObjects(ibuObj, ipcObj)
		if err != nil {
			t.Fatalf("failed to create fake client: %v", err)
		}

		ibu := &ibuv1.ImageBasedUpgrade{}
		assert.NoError(t, c.Get(context.TODO(), types.NamespacedName{Name: utils.IBUName}, ibu))

		r := &ImageBasedUpgradeReconciler{Client: c, NoncachedClient: c}
		res, err := r.gateIBUByIPConfig(context.TODO(), ibu)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := &ibuv1.ImageBasedUpgrade{}
		assert.NoError(t, c.Get(context.TODO(), types.NamespacedName{Name: utils.IBUName}, updated))
		cond := meta.FindStatusCondition(updated.Status.Conditions, string(utils.ConditionTypes.PrepInProgress))
		assert.Nil(t, cond)
	})
}
