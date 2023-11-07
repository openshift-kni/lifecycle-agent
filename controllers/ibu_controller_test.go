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
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
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

const lcaNs = "openshift-lifecycle-agent"

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

type ConditionTypeAndStatus struct {
	ConditionType   utils.ConditionType
	ConditionStatus v1.ConditionStatus
}

func TestShouldTransition(t *testing.T) {
	testcases := []struct {
		name                   string
		desiredStage           lcav1alpha1.ImageBasedUpgradeStage
		currentInProgressStage lcav1alpha1.ImageBasedUpgradeStage
		expected               bool
		conditions             []ConditionTypeAndStatus
	}{
		{
			name:                   "prep when PrepCompleted is present",
			desiredStage:           lcav1alpha1.Stages.Prep,
			currentInProgressStage: "",
			expected:               false,
			conditions: []ConditionTypeAndStatus{
				{utils.ConditionTypes.PrepCompleted, metav1.ConditionFalse},
				{utils.ConditionTypes.PrepInProgress, metav1.ConditionFalse},
			},
		},
		{
			name:                   "upgrade when UpgradeCompleted is present ",
			desiredStage:           lcav1alpha1.Stages.Upgrade,
			currentInProgressStage: "",
			expected:               false,
			conditions: []ConditionTypeAndStatus{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse},
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionFalse},
			},
		},
		{
			name:                   "rollback when RollbackCompleted is present",
			desiredStage:           lcav1alpha1.Stages.Rollback,
			currentInProgressStage: "",
			expected:               false,
			conditions: []ConditionTypeAndStatus{
				{utils.ConditionTypes.RollbackCompleted, metav1.ConditionFalse},
				{utils.ConditionTypes.RollbackInProgress, metav1.ConditionFalse},
			},
		},
		{
			name:                   "different stage",
			desiredStage:           lcav1alpha1.Stages.Idle,
			currentInProgressStage: lcav1alpha1.Stages.Prep,
			expected:               true,
			conditions:             []ConditionTypeAndStatus{},
		},
	}
	for _, tc := range testcases {
		var ibu = &lcav1alpha1.ImageBasedUpgrade{
			ObjectMeta: v1.ObjectMeta{
				Name:      utils.IBUName,
				Namespace: lcaNs,
			},
		}
		ibu.Spec.Stage = tc.desiredStage
		for _, c := range tc.conditions {
			utils.SetStatusCondition(&ibu.Status.Conditions,
				c.ConditionType, "reason", c.ConditionStatus, "message", ibu.Generation)
		}
		value := shouldTransition(tc.currentInProgressStage, ibu)
		assert.Equal(t, tc.expected, value)
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
		conditions         []ConditionTypeAndStatus
		expectedConditions []ExpectedCondition
		expected           bool
	}{
		{
			name:       "idle when prep in progress",
			stage:      lcav1alpha1.Stages.Idle,
			conditions: []ConditionTypeAndStatus{{utils.ConditionTypes.PrepInProgress, metav1.ConditionTrue}},
			expected:   true,
		},
		{
			name:       "idle when prep completed",
			stage:      lcav1alpha1.Stages.Idle,
			conditions: []ConditionTypeAndStatus{{utils.ConditionTypes.PrepCompleted, metav1.ConditionTrue}},
			expected:   true,
		},
		{
			name:  "idle when prep failed",
			stage: lcav1alpha1.Stages.Idle,
			conditions: []ConditionTypeAndStatus{{utils.ConditionTypes.PrepCompleted, metav1.ConditionFalse},
				{utils.ConditionTypes.PrepInProgress, metav1.ConditionFalse}},
			expected: true,
		},
		{
			name:  "rollback when upgrade failed",
			stage: lcav1alpha1.Stages.Rollback,
			conditions: []ConditionTypeAndStatus{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionFalse},
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionFalse},
			},
			expected: true,
		},
		{
			name:  "rollback when upgrade completed",
			stage: lcav1alpha1.Stages.Rollback,
			conditions: []ConditionTypeAndStatus{
				{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionTrue},
				{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionTrue},
			},
			expected: true,
		},
		{
			name:       "rollback when upgrade in progress",
			stage:      lcav1alpha1.Stages.Rollback,
			conditions: []ConditionTypeAndStatus{{utils.ConditionTypes.UpgradeInProgress, metav1.ConditionTrue}},
			expected:   true,
		},
		{
			name:       "rollback without upgrade in progress",
			stage:      lcav1alpha1.Stages.Rollback,
			conditions: []ConditionTypeAndStatus{},
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.RollbackInProgress,
				utils.ConditionReasons.InvalidTransition, metav1.ConditionFalse,
				"Upgrade not started or already finalized",
			}},
			expected: false,
		},
		{
			name:       "idle when upgrade completed is true",
			stage:      lcav1alpha1.Stages.Idle,
			conditions: []ConditionTypeAndStatus{{utils.ConditionTypes.UpgradeCompleted, metav1.ConditionTrue}},
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.Finalizing, metav1.ConditionFalse,
				"Finalizing",
			}},
			expected: true,
		},
		{
			name:       "idle when rollback completed is true",
			stage:      lcav1alpha1.Stages.Idle,
			conditions: []ConditionTypeAndStatus{{utils.ConditionTypes.RollbackCompleted, metav1.ConditionTrue}},
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.Finalizing, metav1.ConditionFalse,
				"Finalizing",
			}},
			expected: true,
		},
		{
			name:  "idle when upgrade not completed",
			stage: lcav1alpha1.Stages.Idle,
			conditions: []ConditionTypeAndStatus{
				{utils.ConditionTypes.Idle, metav1.ConditionFalse},
			},
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.Aborting, metav1.ConditionFalse,
				"Aborting",
			}},
			expected: true,
		},
		{
			name:       "idle when rollback completed is false and no idle condition",
			stage:      lcav1alpha1.Stages.Idle,
			conditions: []ConditionTypeAndStatus{{utils.ConditionTypes.RollbackCompleted, metav1.ConditionFalse}},
			expected:   true,
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.Idle, metav1.ConditionTrue,
				"Idle",
			}},
		},
		{
			name:       "idle without rollback or upgrade completed",
			stage:      lcav1alpha1.Stages.Idle,
			conditions: []ConditionTypeAndStatus{},
			expected:   true,
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.Idle, metav1.ConditionTrue,
				"Idle",
			}},
		},
		{
			name:       "upgrade without prep completed",
			stage:      lcav1alpha1.Stages.Upgrade,
			conditions: []ConditionTypeAndStatus{},
			expected:   false,
		},
		{
			name:       "upgrade with prep completed",
			stage:      lcav1alpha1.Stages.Upgrade,
			conditions: []ConditionTypeAndStatus{{utils.ConditionTypes.PrepCompleted, metav1.ConditionTrue}},
			expected:   true,
			expectedConditions: []ExpectedCondition{{
				utils.ConditionTypes.UpgradeInProgress,
				utils.ConditionReasons.InProgress,
				metav1.ConditionTrue,
				"In progress",
			}},
		},
		{
			name:       "prep without idle completed",
			stage:      lcav1alpha1.Stages.Prep,
			conditions: []ConditionTypeAndStatus{},
			expected:   false,
		},
		{
			name:       "prep with idle completed",
			stage:      lcav1alpha1.Stages.Prep,
			conditions: []ConditionTypeAndStatus{{utils.ConditionTypes.Idle, metav1.ConditionTrue}},
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
	}
	for _, tc := range testcases {

		var ibu = &lcav1alpha1.ImageBasedUpgrade{
			ObjectMeta: v1.ObjectMeta{
				Name:      utils.IBUName,
				Namespace: lcaNs,
			},
			Spec: lcav1alpha1.ImageBasedUpgradeSpec{
				Stage: tc.stage,
			},
		}
		for _, c := range tc.conditions {
			utils.SetStatusCondition(&ibu.Status.Conditions,
				c.ConditionType, "reason", c.ConditionStatus, "message", ibu.Generation)
		}

		t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			result := validateStageTransition(ibu)
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
					Name:      utils.IBUName,
					Namespace: lcaNs,
				},
				Spec: lcav1alpha1.ImageBasedUpgradeSpec{
					Stage: lcav1alpha1.Stages.Idle,
				},
			},
			request: reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      utils.IBUName,
					Namespace: lcaNs,
				},
			},
			validateFunc: func(t *testing.T, result ctrl.Result, ibu *lcav1alpha1.ImageBasedUpgrade) {
				idleCondition := meta.FindStatusCondition(ibu.Status.Conditions, string(utils.ConditionTypes.Idle))
				assert.Equal(t, idleCondition.Status, metav1.ConditionTrue)
				if result != doNotRequeue() {
					t.Errorf("expect no requeue")
				}
			},
		},
	}
	ns := &corev1.Namespace{
		ObjectMeta: v1.ObjectMeta{
			Name: lcaNs,
		},
	}
	for _, tc := range testcases {
		t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{ns, tc.ibu}
			fakeClient, err := getFakeClientFromObjects(objs...)
			if err != nil {
				t.Errorf("error in creating fake client")
			}

			r := &ImageBasedUpgradeReconciler{
				Client: fakeClient,
				Log:    logr.Discard(),
				Scheme: fakeClient.Scheme(),
			}
			result, err := r.Reconcile(context.TODO(), tc.request)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			ibu := &lcav1alpha1.ImageBasedUpgrade{}
			if err := fakeClient.Get(context.TODO(), types.NamespacedName{Name: utils.IBUName, Namespace: lcaNs}, ibu); err != nil {
				t.Errorf("unexcepted error: %v", err.Error())
			}
			tc.validateFunc(t, result, ibu)
		})
	}
}
