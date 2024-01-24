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
	"testing"

	"github.com/go-logr/logr"
	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
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
	testscheme.AddKnownTypes(lcav1alpha1.GroupVersion, &lcav1alpha1.ImageBasedUpgrade{})
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
		desiredStage lcav1alpha1.ImageBasedUpgradeStage
		expected     bool
		conditions   []Condition
	}{
		{
			name:         "idle while idle is true",
			desiredStage: lcav1alpha1.Stages.Idle,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.Idle, metav1.ConditionTrue, ""},
			},
		},
		{
			name:         "idle while aborting",
			desiredStage: lcav1alpha1.Stages.Idle,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.Aborting},
			},
		},
		{
			name:         "idle while finalizing",
			desiredStage: lcav1alpha1.Stages.Idle,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.Finalizing},
			},
		},
		{
			name:         "idle while abort failed",
			desiredStage: lcav1alpha1.Stages.Idle,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.AbortFailed},
			},
		},
		{
			name:         "idle while finalize failed",
			desiredStage: lcav1alpha1.Stages.Idle,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.FinalizeFailed},
			},
		},
		{
			name:         "idle when prep in progress",
			desiredStage: lcav1alpha1.Stages.Idle,
			conditions: []Condition{
				{utils.ConditionTypes.PrepInProgress, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "idle when prep completed",
			desiredStage: lcav1alpha1.Stages.Idle,
			conditions: []Condition{
				{utils.ConditionTypes.PrepCompleted, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "idle when prep failed",
			desiredStage: lcav1alpha1.Stages.Idle,
			conditions: []Condition{
				{utils.ConditionTypes.PrepCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.PrepInProgress, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "idle when upgrade completed",
			desiredStage: lcav1alpha1.Stages.Idle,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "idle when rollback completed",
			desiredStage: lcav1alpha1.Stages.Idle,
			conditions: []Condition{
				{utils.ConditionTypes.RollbackCompleted, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "idle when upgrade faild",
			desiredStage: lcav1alpha1.Stages.Idle,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "idle when rollback failed",
			desiredStage: lcav1alpha1.Stages.Idle,
			conditions: []Condition{
				{utils.ConditionTypes.RollbackCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "prep when prep completed",
			desiredStage: lcav1alpha1.Stages.Prep,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.PrepCompleted, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.PrepInProgress, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
		},
		{
			name:         "prep with idle true",
			desiredStage: lcav1alpha1.Stages.Prep,
			conditions:   []Condition{{utils.ConditionTypes.Idle, metav1.ConditionTrue, ""}},
			expected:     true,
		},
		{
			name:         "prep when prep failed",
			desiredStage: lcav1alpha1.Stages.Prep,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.PrepCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.PrepInProgress, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
		},
		{
			name:         "prep when prep in progress",
			desiredStage: lcav1alpha1.Stages.Prep,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.PrepInProgress, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
		},
		{
			name:         "upgrade with prep completed",
			desiredStage: lcav1alpha1.Stages.Upgrade,
			conditions: []Condition{
				{utils.ConditionTypes.PrepCompleted, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "upgrade when upgrade completed",
			desiredStage: lcav1alpha1.Stages.Upgrade,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
		},
		{
			name:         "upgrade when upgrade failed",
			desiredStage: lcav1alpha1.Stages.Upgrade,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
		},
		{
			name:         "upgrade when upgrade in progress",
			desiredStage: lcav1alpha1.Stages.Upgrade,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
		},
		{
			name:         "rollback when rollback completed",
			desiredStage: lcav1alpha1.Stages.Rollback,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.RollbackCompleted, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.RollbackInProgress, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
		},
		{
			name:         "rollback when rollback failed",
			desiredStage: lcav1alpha1.Stages.Rollback,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.RollbackCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.RollbackInProgress, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
		},
		{
			name:         "rollback when rollback in progress",
			desiredStage: lcav1alpha1.Stages.Rollback,
			expected:     false,
			conditions: []Condition{
				{utils.ConditionTypes.RollbackInProgress, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
		},
		{
			name:         "rollback when upgrade failed",
			desiredStage: lcav1alpha1.Stages.Rollback,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "rollback when upgrade completed",
			desiredStage: lcav1alpha1.Stages.Rollback,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
		{
			name:         "rollback when upgrade in progress",
			desiredStage: lcav1alpha1.Stages.Rollback,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionTrue, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expected: true,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			var ibu = &lcav1alpha1.ImageBasedUpgrade{
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
		name               string
		stage              lcav1alpha1.ImageBasedUpgradeStage
		conditions         []Condition
		expectedConditions []ExpectedCondition
		expected           bool
		afterPivot         bool
	}{
		{
			name:       "idle when prep in progress",
			stage:      lcav1alpha1.Stages.Idle,
			conditions: []Condition{{utils.ConditionTypes.PrepInProgress, metav1.ConditionTrue, ""}},
			expected:   true,
		},
		{
			name:       "idle when prep completed",
			stage:      lcav1alpha1.Stages.Idle,
			conditions: []Condition{{utils.ConditionTypes.PrepCompleted, metav1.ConditionTrue, ""}},
			expected:   true,
		},
		{
			name:  "idle when prep failed",
			stage: lcav1alpha1.Stages.Idle,
			conditions: []Condition{{utils.ConditionTypes.PrepCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.PrepInProgress, metav1.ConditionFalse, ""}},
			expected: true,
		},
		{
			name:       "idle when upgrade completed",
			stage:      lcav1alpha1.Stages.Idle,
			conditions: []Condition{{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionTrue, ""}},
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.Finalizing, metav1.ConditionFalse,
				"Finalizing",
			}},
			expected: true,
		},
		{
			name:       "idle when upgrade failed before pivot",
			stage:      lcav1alpha1.Stages.Idle,
			afterPivot: false,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.Aborting,
				metav1.ConditionFalse,
				"Aborting",
			}},
			expected: true,
		},
		{
			name:       "idle when upgrade failed after pivot",
			stage:      lcav1alpha1.Stages.Idle,
			afterPivot: true,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, ""},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.InvalidTransition,
				metav1.ConditionFalse,
				"Abort or finalize not allowed",
			}},
			expected: false,
		},
		{
			name:       "idle when upgrade in progress before pivot",
			stage:      lcav1alpha1.Stages.Idle,
			afterPivot: false,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionTrue, utils.ConditionReasons.InProgress},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.Aborting, metav1.ConditionFalse,
				"Aborting",
			}},
			expected: true,
		},
		{
			name:       "idle when upgrade in progress after pivot",
			stage:      lcav1alpha1.Stages.Idle,
			afterPivot: true,
			conditions: []Condition{
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionTrue, utils.ConditionReasons.InProgress},
				{utils.ConditionTypes.Idle, metav1.ConditionFalse, utils.ConditionReasons.InProgress},
			},
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.InvalidTransition,
				metav1.ConditionFalse,
				"Abort or finalize not allowed",
			}},
			expected: false,
		},
		{
			name:       "idle when rollback in progress",
			stage:      lcav1alpha1.Stages.Idle,
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
				"Abort or finalize not allowed",
			}},
		},
		{
			name:       "idle when rollback in progress after pivoting back",
			stage:      lcav1alpha1.Stages.Idle,
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
			name:       "idle when rollback failed",
			stage:      lcav1alpha1.Stages.Idle,
			conditions: []Condition{{utils.ConditionTypes.RollbackCompleted, metav1.ConditionFalse, ""}},
			expected:   true,
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.Idle, metav1.ConditionTrue,
				"Idle",
			}},
		},
		{
			name:       "idle when rollback completed",
			stage:      lcav1alpha1.Stages.Idle,
			conditions: []Condition{{utils.ConditionTypes.RollbackCompleted, metav1.ConditionTrue, ""}},
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.Finalizing, metav1.ConditionFalse,
				"Finalizing",
			}},
			expected: true,
		},
		{
			name:       "prep without idle completed",
			stage:      lcav1alpha1.Stages.Prep,
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
			stage:      lcav1alpha1.Stages.Prep,
			conditions: []Condition{{utils.ConditionTypes.Idle, metav1.ConditionTrue, ""}},
			expected:   true,
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.InProgress,
				metav1.ConditionFalse,
				"In progress",
			}, {
				utils.ConditionTypes.PrepInProgress,
				utils.ConditionReasons.InProgress,
				metav1.ConditionTrue,
				"In progress",
			}},
		},
		{
			name:       "upgrade without prep completed",
			stage:      lcav1alpha1.Stages.Upgrade,
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
			stage:      lcav1alpha1.Stages.Upgrade,
			conditions: []Condition{{utils.ConditionTypes.PrepCompleted, metav1.ConditionTrue, ""}},
			expected:   true,
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.UpgradeInProgress,
				utils.ConditionReasons.InProgress,
				metav1.ConditionTrue,
				"In progress",
			}},
		},
		{
			name:  "rollback when upgrade failed",
			stage: lcav1alpha1.Stages.Rollback,
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
					"Rollback requested",
				},
				{
					utils.ConditionTypes.UpgradeCompleted,
					utils.ConditionReasons.Failed,
					metav1.ConditionFalse,
					"Rollback requested",
				},
				{
					utils.ConditionTypes.RollbackInProgress,
					utils.ConditionReasons.InProgress,
					metav1.ConditionTrue,
					"In progress",
				},
			},
			afterPivot: true,
		},
		{
			name:  "rollback when upgrade completed",
			stage: lcav1alpha1.Stages.Rollback,
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
					"Rollback requested",
				},
				{
					utils.ConditionTypes.UpgradeCompleted,
					utils.ConditionReasons.Failed,
					metav1.ConditionFalse,
					"Rollback requested",
				},
				{
					utils.ConditionTypes.RollbackInProgress,
					utils.ConditionReasons.InProgress,
					metav1.ConditionTrue,
					"In progress",
				},
			},
			afterPivot: true,
		},
		{
			name:       "rollback when upgrade in progress",
			stage:      lcav1alpha1.Stages.Rollback,
			conditions: []Condition{{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionTrue, ""}},
			expected:   true,
			expectedConditions: []ExpectedCondition{
				{
					utils.ConditionTypes.UpgradeInProgress,
					utils.ConditionReasons.Failed,
					metav1.ConditionFalse,
					"Rollback requested",
				},
				{
					utils.ConditionTypes.UpgradeCompleted,
					utils.ConditionReasons.Failed,
					metav1.ConditionFalse,
					"Rollback requested",
				},
				{
					utils.ConditionTypes.RollbackInProgress,
					utils.ConditionReasons.InProgress,
					metav1.ConditionTrue,
					"In progress",
				},
			},
			afterPivot: true,
		},
		{
			name:       "rollback without upgrade in progress",
			stage:      lcav1alpha1.Stages.Rollback,
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

		var ibu = &lcav1alpha1.ImageBasedUpgrade{
			ObjectMeta: v1.ObjectMeta{
				Name: utils.IBUName,
			},
			Spec: lcav1alpha1.ImageBasedUpgradeSpec{
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
		})
	}
}

func TestImageBasedUpgradeReconciler_Reconcile(t *testing.T) {
	testcases := []struct {
		name         string
		ibu          client.Object
		request      reconcile.Request
		validateFunc func(t *testing.T, result ctrl.Result, ibu *lcav1alpha1.ImageBasedUpgrade)
	}{
		{
			name: "idle IBU",
			ibu: &lcav1alpha1.ImageBasedUpgrade{
				ObjectMeta: v1.ObjectMeta{
					Name: utils.IBUName,
				},
				Spec: lcav1alpha1.ImageBasedUpgradeSpec{
					Stage: lcav1alpha1.Stages.Idle,
				},
			},
			request: reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: utils.IBUName,
				},
			},
			validateFunc: func(t *testing.T, result ctrl.Result, ibu *lcav1alpha1.ImageBasedUpgrade) {
				idleCondition := meta.FindStatusCondition(ibu.Status.Conditions, string(utils.ConditionTypes.Idle))
				assert.Equal(t, idleCondition.Status, metav1.ConditionTrue)
				if result != requeueImmediately() {
					t.Errorf("expect requeue immediately")
				}
			},
		},
	}
	for _, tc := range testcases {
		t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{tc.ibu}
			fakeClient, err := getFakeClientFromObjects(objs...)
			if err != nil {
				t.Errorf("error in creating fake client")
			}

			ctrl := gomock.NewController(t)
			mockClient := rpmostreeclient.NewMockIClient(ctrl)
			mockClient.EXPECT().IsStaterootBooted("rhcos_").Return(false, nil)

			r := &ImageBasedUpgradeReconciler{
				Client:          fakeClient,
				Log:             logr.Discard(),
				Scheme:          fakeClient.Scheme(),
				RPMOstreeClient: mockClient,
			}
			result, err := r.Reconcile(context.TODO(), tc.request)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			ibu := &lcav1alpha1.ImageBasedUpgrade{}
			if err := fakeClient.Get(context.TODO(), types.NamespacedName{Name: utils.IBUName}, ibu); err != nil {
				t.Errorf("unexcepted error: %v", err.Error())
			}
			tc.validateFunc(t, result, ibu)
		})
	}
}
