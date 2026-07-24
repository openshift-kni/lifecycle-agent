package utils

import (
	"context"
	"fmt"
	"testing"

	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	ipcv1 "github.com/openshift-kni/lifecycle-agent/api/ipconfig/v1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newIBU(stage ibuv1.ImageBasedUpgradeStage, conditions ...metav1.Condition) *ibuv1.ImageBasedUpgrade {
	return &ibuv1.ImageBasedUpgrade{
		ObjectMeta: metav1.ObjectMeta{Name: "upgrade", Generation: 1},
		Spec:       ibuv1.ImageBasedUpgradeSpec{Stage: stage},
		Status:     ibuv1.ImageBasedUpgradeStatus{Conditions: conditions},
	}
}

func newIPC(stage ipcv1.IPConfigStage, conditions ...metav1.Condition) *ipcv1.IPConfig {
	return &ipcv1.IPConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "ipconfig", Generation: 1},
		Spec:       ipcv1.IPConfigSpec{Stage: stage},
		Status:     ipcv1.IPConfigStatus{Conditions: conditions},
	}
}

func cond(typ ConditionType, status metav1.ConditionStatus, reason ConditionReason) metav1.Condition {
	return metav1.Condition{
		Type:   string(typ),
		Status: status,
		Reason: string(reason),
	}
}

func newFailStatusClient() client.Client {
	return fake.NewClientBuilder().WithScheme(testscheme).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
			return fmt.Errorf("injected error")
		},
	}).Build()
}

func TestGetInProgressConditionType(t *testing.T) {
	tests := []struct {
		name     string
		stage    ibuv1.ImageBasedUpgradeStage
		expected ConditionType
	}{
		{"Idle", ibuv1.Stages.Idle, ConditionTypes.Idle},
		{"Prep", ibuv1.Stages.Prep, ConditionTypes.PrepInProgress},
		{"Upgrade", ibuv1.Stages.Upgrade, ConditionTypes.UpgradeInProgress},
		{"Rollback", ibuv1.Stages.Rollback, ConditionTypes.RollbackInProgress},
		{"unknown stage", "Unknown", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, GetInProgressConditionType(tt.stage))
		})
	}
}

func TestGetCompletedConditionType(t *testing.T) {
	tests := []struct {
		name     string
		stage    ibuv1.ImageBasedUpgradeStage
		expected ConditionType
	}{
		{"Idle", ibuv1.Stages.Idle, ConditionTypes.Idle},
		{"Prep", ibuv1.Stages.Prep, ConditionTypes.PrepCompleted},
		{"Upgrade", ibuv1.Stages.Upgrade, ConditionTypes.UpgradeCompleted},
		{"Rollback", ibuv1.Stages.Rollback, ConditionTypes.RollbackCompleted},
		{"unknown stage", "Unknown", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, GetCompletedConditionType(tt.stage))
		})
	}
}

func TestGetIPInProgressConditionType(t *testing.T) {
	tests := []struct {
		name     string
		stage    ipcv1.IPConfigStage
		expected ConditionType
	}{
		{"Idle", ipcv1.IPStages.Idle, ConditionTypes.Idle},
		{"Config", ipcv1.IPStages.Config, ConditionTypes.ConfigInProgress},
		{"Rollback", ipcv1.IPStages.Rollback, ConditionTypes.RollbackInProgress},
		{"unknown stage", "Unknown", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, GetIPInProgressConditionType(tt.stage))
		})
	}
}

func TestGetIPCompletedConditionType(t *testing.T) {
	tests := []struct {
		name     string
		stage    ipcv1.IPConfigStage
		expected ConditionType
	}{
		{"Idle", ipcv1.IPStages.Idle, ConditionTypes.Idle},
		{"Config", ipcv1.IPStages.Config, ConditionTypes.ConfigCompleted},
		{"Rollback", ipcv1.IPStages.Rollback, ConditionTypes.RollbackCompleted},
		{"unknown stage", "Unknown", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, GetIPCompletedConditionType(tt.stage))
		})
	}
}

func TestSetStatusCondition(t *testing.T) {
	tests := []struct {
		name       string
		initial    []metav1.Condition
		typ        ConditionType
		reason     ConditionReason
		status     metav1.ConditionStatus
		message    string
		assertFunc func(t *testing.T, conditions []metav1.Condition)
	}{
		{
			name:    "adds new condition to empty slice",
			initial: []metav1.Condition{},
			typ:     ConditionTypes.PrepInProgress,
			reason:  ConditionReasons.InProgress,
			status:  metav1.ConditionTrue,
			message: "prep started",
			assertFunc: func(t *testing.T, conditions []metav1.Condition) {
				assert.Len(t, conditions, 1)
				assert.Equal(t, string(ConditionTypes.PrepInProgress), conditions[0].Type)
				assert.Equal(t, metav1.ConditionTrue, conditions[0].Status)
				assert.Equal(t, string(ConditionReasons.InProgress), conditions[0].Reason)
				assert.Equal(t, "prep started", conditions[0].Message)
			},
		},
		{
			name: "updates existing condition status",
			initial: []metav1.Condition{
				cond(ConditionTypes.PrepInProgress, metav1.ConditionTrue, ConditionReasons.InProgress),
			},
			typ:     ConditionTypes.PrepInProgress,
			reason:  ConditionReasons.Completed,
			status:  metav1.ConditionFalse,
			message: "prep done",
			assertFunc: func(t *testing.T, conditions []metav1.Condition) {
				assert.Len(t, conditions, 1)
				assert.Equal(t, metav1.ConditionFalse, conditions[0].Status)
				assert.Equal(t, string(ConditionReasons.Completed), conditions[0].Reason)
			},
		},
		{
			name: "Idle condition reorder when status changes and not last",
			initial: []metav1.Condition{
				cond(ConditionTypes.Idle, metav1.ConditionTrue, ConditionReasons.Idle),
				cond(ConditionTypes.PrepInProgress, metav1.ConditionTrue, ConditionReasons.InProgress),
			},
			typ:     ConditionTypes.Idle,
			reason:  ConditionReasons.InProgress,
			status:  metav1.ConditionFalse,
			message: "becoming not idle",
			assertFunc: func(t *testing.T, conditions []metav1.Condition) {
				assert.Equal(t, metav1.ConditionFalse, conditions[len(conditions)-1].Status)
				assert.Equal(t, string(ConditionTypes.Idle), conditions[len(conditions)-1].Type)
			},
		},
		{
			name: "Idle condition no reorder when already last",
			initial: []metav1.Condition{
				cond(ConditionTypes.PrepInProgress, metav1.ConditionTrue, ConditionReasons.InProgress),
				cond(ConditionTypes.Idle, metav1.ConditionTrue, ConditionReasons.Idle),
			},
			typ:     ConditionTypes.Idle,
			reason:  ConditionReasons.InProgress,
			status:  metav1.ConditionFalse,
			message: "still last",
			assertFunc: func(t *testing.T, conditions []metav1.Condition) {
				assert.Equal(t, string(ConditionTypes.Idle), conditions[len(conditions)-1].Type)
				assert.Equal(t, metav1.ConditionFalse, conditions[len(conditions)-1].Status)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conditions := make([]metav1.Condition, len(tt.initial))
			copy(conditions, tt.initial)
			SetStatusCondition(&conditions, tt.typ, tt.reason, tt.status, tt.message, 1)
			tt.assertFunc(t, conditions)
		})
	}
}

func TestResetStatusConditions(t *testing.T) {
	tests := []struct {
		name    string
		initial []metav1.Condition
	}{
		{
			name:    "empty conditions",
			initial: []metav1.Condition{},
		},
		{
			name: "removes non-Idle conditions and sets Idle true",
			initial: []metav1.Condition{
				cond(ConditionTypes.Idle, metav1.ConditionFalse, ConditionReasons.InProgress),
				cond(ConditionTypes.PrepInProgress, metav1.ConditionTrue, ConditionReasons.InProgress),
				cond(ConditionTypes.PrepCompleted, metav1.ConditionTrue, ConditionReasons.Completed),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conditions := make([]metav1.Condition, len(tt.initial))
			copy(conditions, tt.initial)
			ResetStatusConditions(&conditions, 1)
			assert.Len(t, conditions, 1)
			assert.Equal(t, string(ConditionTypes.Idle), conditions[0].Type)
			assert.Equal(t, metav1.ConditionTrue, conditions[0].Status)
			assert.Equal(t, string(ConditionReasons.Idle), conditions[0].Reason)
		})
	}
}

func TestIsIdleConditionTrue(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		expected   bool
	}{
		{"empty conditions", nil, false},
		{"no Idle condition", []metav1.Condition{cond(ConditionTypes.PrepInProgress, metav1.ConditionTrue, ConditionReasons.InProgress)}, false},
		{"Idle true", []metav1.Condition{cond(ConditionTypes.Idle, metav1.ConditionTrue, ConditionReasons.Idle)}, true},
		{"Idle false", []metav1.Condition{cond(ConditionTypes.Idle, metav1.ConditionFalse, ConditionReasons.InProgress)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, IsIdleConditionTrue(tt.conditions))
		})
	}
}

func TestIsStageCompleted(t *testing.T) {
	tests := []struct {
		name     string
		ibu      *ibuv1.ImageBasedUpgrade
		stage    ibuv1.ImageBasedUpgradeStage
		expected bool
	}{
		{
			"completed true",
			newIBU(ibuv1.Stages.Upgrade, cond(ConditionTypes.UpgradeCompleted, metav1.ConditionTrue, ConditionReasons.Completed)),
			ibuv1.Stages.Upgrade,
			true,
		},
		{
			"completed false (failed)",
			newIBU(ibuv1.Stages.Upgrade, cond(ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, ConditionReasons.Failed)),
			ibuv1.Stages.Upgrade,
			false,
		},
		{
			"no completed condition",
			newIBU(ibuv1.Stages.Upgrade),
			ibuv1.Stages.Upgrade,
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, IsStageCompleted(tt.ibu, tt.stage))
		})
	}
}

func TestIsStageFailed(t *testing.T) {
	tests := []struct {
		name     string
		ibu      *ibuv1.ImageBasedUpgrade
		stage    ibuv1.ImageBasedUpgradeStage
		expected bool
	}{
		{
			"completed false means failed",
			newIBU(ibuv1.Stages.Prep, cond(ConditionTypes.PrepCompleted, metav1.ConditionFalse, ConditionReasons.Failed)),
			ibuv1.Stages.Prep,
			true,
		},
		{
			"completed true means not failed",
			newIBU(ibuv1.Stages.Prep, cond(ConditionTypes.PrepCompleted, metav1.ConditionTrue, ConditionReasons.Completed)),
			ibuv1.Stages.Prep,
			false,
		},
		{
			"no condition",
			newIBU(ibuv1.Stages.Prep),
			ibuv1.Stages.Prep,
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, IsStageFailed(tt.ibu, tt.stage))
		})
	}
}

func TestIsStageCompletedOrFailed(t *testing.T) {
	tests := []struct {
		name     string
		ibu      *ibuv1.ImageBasedUpgrade
		stage    ibuv1.ImageBasedUpgradeStage
		expected bool
	}{
		{"completed", newIBU(ibuv1.Stages.Upgrade, cond(ConditionTypes.UpgradeCompleted, metav1.ConditionTrue, ConditionReasons.Completed)), ibuv1.Stages.Upgrade, true},
		{"failed", newIBU(ibuv1.Stages.Upgrade, cond(ConditionTypes.UpgradeCompleted, metav1.ConditionFalse, ConditionReasons.Failed)), ibuv1.Stages.Upgrade, true},
		{"no condition", newIBU(ibuv1.Stages.Upgrade), ibuv1.Stages.Upgrade, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, IsStageCompletedOrFailed(tt.ibu, tt.stage))
		})
	}
}

func TestIsStageInProgress(t *testing.T) {
	tests := []struct {
		name     string
		ibu      *ibuv1.ImageBasedUpgrade
		stage    ibuv1.ImageBasedUpgradeStage
		expected bool
	}{
		{
			"non-Idle stage in progress",
			newIBU(ibuv1.Stages.Prep, cond(ConditionTypes.PrepInProgress, metav1.ConditionTrue, ConditionReasons.InProgress)),
			ibuv1.Stages.Prep,
			true,
		},
		{
			"non-Idle stage not in progress",
			newIBU(ibuv1.Stages.Prep, cond(ConditionTypes.PrepInProgress, metav1.ConditionFalse, ConditionReasons.Completed)),
			ibuv1.Stages.Prep,
			false,
		},
		{
			"non-Idle stage no condition",
			newIBU(ibuv1.Stages.Prep),
			ibuv1.Stages.Prep,
			false,
		},
		{
			"Idle stage with True status is not in progress",
			newIBU(ibuv1.Stages.Idle, cond(ConditionTypes.Idle, metav1.ConditionTrue, ConditionReasons.Idle)),
			ibuv1.Stages.Idle,
			false,
		},
		{
			"Idle stage nil condition is not in progress",
			newIBU(ibuv1.Stages.Idle),
			ibuv1.Stages.Idle,
			false,
		},
		{
			"Idle stage Aborting is in progress",
			newIBU(ibuv1.Stages.Idle, cond(ConditionTypes.Idle, metav1.ConditionFalse, ConditionReasons.Aborting)),
			ibuv1.Stages.Idle,
			true,
		},
		{
			"Idle stage Finalizing is in progress",
			newIBU(ibuv1.Stages.Idle, cond(ConditionTypes.Idle, metav1.ConditionFalse, ConditionReasons.Finalizing)),
			ibuv1.Stages.Idle,
			true,
		},
		{
			"Idle stage AbortFailed is in progress",
			newIBU(ibuv1.Stages.Idle, cond(ConditionTypes.Idle, metav1.ConditionFalse, ConditionReasons.AbortFailed)),
			ibuv1.Stages.Idle,
			true,
		},
		{
			"Idle stage FinalizeFailed is in progress",
			newIBU(ibuv1.Stages.Idle, cond(ConditionTypes.Idle, metav1.ConditionFalse, ConditionReasons.FinalizeFailed)),
			ibuv1.Stages.Idle,
			true,
		},
		{
			"Idle stage InProgress reason is not in progress",
			newIBU(ibuv1.Stages.Idle, cond(ConditionTypes.Idle, metav1.ConditionFalse, ConditionReasons.InProgress)),
			ibuv1.Stages.Idle,
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, IsStageInProgress(tt.ibu, tt.stage))
		})
	}
}

func TestGetInProgressStage(t *testing.T) {
	tests := []struct {
		name     string
		ibu      *ibuv1.ImageBasedUpgrade
		expected ibuv1.ImageBasedUpgradeStage
	}{
		{"Prep in progress", newIBU(ibuv1.Stages.Prep, cond(ConditionTypes.PrepInProgress, metav1.ConditionTrue, ConditionReasons.InProgress)), ibuv1.Stages.Prep},
		{"Idle aborting", newIBU(ibuv1.Stages.Idle, cond(ConditionTypes.Idle, metav1.ConditionFalse, ConditionReasons.Aborting)), ibuv1.Stages.Idle},
		{"none in progress", newIBU(ibuv1.Stages.Idle, cond(ConditionTypes.Idle, metav1.ConditionTrue, ConditionReasons.Idle)), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, GetInProgressStage(tt.ibu))
		})
	}
}

func TestGetInProgressCondition(t *testing.T) {
	ibu := newIBU(ibuv1.Stages.Upgrade, cond(ConditionTypes.UpgradeInProgress, metav1.ConditionTrue, ConditionReasons.InProgress))
	c := GetInProgressCondition(ibu, ibuv1.Stages.Upgrade)
	assert.NotNil(t, c)
	assert.Equal(t, string(ConditionTypes.UpgradeInProgress), c.Type)

	assert.Nil(t, GetInProgressCondition(ibu, ibuv1.Stages.Prep))
	assert.Nil(t, GetInProgressCondition(ibu, "Unknown"))
}

func TestGetCompletedCondition(t *testing.T) {
	ibu := newIBU(ibuv1.Stages.Upgrade, cond(ConditionTypes.UpgradeCompleted, metav1.ConditionTrue, ConditionReasons.Completed))
	c := GetCompletedCondition(ibu, ibuv1.Stages.Upgrade)
	assert.NotNil(t, c)
	assert.Equal(t, string(ConditionTypes.UpgradeCompleted), c.Type)

	assert.Nil(t, GetCompletedCondition(ibu, ibuv1.Stages.Prep))
	assert.Nil(t, GetCompletedCondition(ibu, "Unknown"))
}

func TestIBUSetStatusInProgress(t *testing.T) {
	tests := []struct {
		name   string
		stage  ibuv1.ImageBasedUpgradeStage
		setter func(*ibuv1.ImageBasedUpgrade, string)
	}{
		{"Prep", ibuv1.Stages.Prep, SetPrepStatusInProgress},
		{"Upgrade", ibuv1.Stages.Upgrade, SetUpgradeStatusInProgress},
		{"Rollback", ibuv1.Stages.Rollback, SetRollbackStatusInProgress},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ibu := newIBU(tt.stage)
			tt.setter(ibu, "in progress")
			c := GetInProgressCondition(ibu, tt.stage)
			assert.NotNil(t, c)
			assert.Equal(t, metav1.ConditionTrue, c.Status)
			assert.Equal(t, string(ConditionReasons.InProgress), c.Reason)
			assert.Equal(t, "in progress", c.Message)
		})
	}
}

func TestIBUSetStatusFailed(t *testing.T) {
	tests := []struct {
		name   string
		stage  ibuv1.ImageBasedUpgradeStage
		setter func(*ibuv1.ImageBasedUpgrade, string)
	}{
		{"Prep", ibuv1.Stages.Prep, SetPrepStatusFailed},
		{"Upgrade", ibuv1.Stages.Upgrade, SetUpgradeStatusFailed},
		{"Rollback", ibuv1.Stages.Rollback, SetRollbackStatusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ibu := newIBU(tt.stage)
			tt.setter(ibu, "something broke")

			completed := GetCompletedCondition(ibu, tt.stage)
			assert.NotNil(t, completed)
			assert.Equal(t, metav1.ConditionFalse, completed.Status)
			assert.Equal(t, string(ConditionReasons.Failed), completed.Reason)

			inProgress := GetInProgressCondition(ibu, tt.stage)
			assert.NotNil(t, inProgress)
			assert.Equal(t, metav1.ConditionFalse, inProgress.Status)
			assert.Equal(t, string(ConditionReasons.Failed), inProgress.Reason)
			assert.Equal(t, "something broke", inProgress.Message)
		})
	}
}

func TestIBUSetStatusCompleted(t *testing.T) {
	tests := []struct {
		name   string
		stage  ibuv1.ImageBasedUpgradeStage
		setter func(*ibuv1.ImageBasedUpgrade)
	}{
		{"Upgrade", ibuv1.Stages.Upgrade, SetUpgradeStatusCompleted},
		{"Rollback", ibuv1.Stages.Rollback, SetRollbackStatusCompleted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ibu := newIBU(tt.stage)
			tt.setter(ibu)

			inProgress := GetInProgressCondition(ibu, tt.stage)
			assert.NotNil(t, inProgress)
			assert.Equal(t, metav1.ConditionFalse, inProgress.Status)
			assert.Equal(t, string(ConditionReasons.Completed), inProgress.Reason)

			completed := GetCompletedCondition(ibu, tt.stage)
			assert.NotNil(t, completed)
			assert.Equal(t, metav1.ConditionTrue, completed.Status)
			assert.Equal(t, string(ConditionReasons.Completed), completed.Reason)
		})
	}
}

func TestSetPrepStatusCompleted(t *testing.T) {
	ibu := newIBU(ibuv1.Stages.Prep)
	SetPrepStatusCompleted(ibu, "done")

	inProgress := GetInProgressCondition(ibu, ibuv1.Stages.Prep)
	assert.NotNil(t, inProgress)
	assert.Equal(t, metav1.ConditionFalse, inProgress.Status)

	completed := GetCompletedCondition(ibu, ibuv1.Stages.Prep)
	assert.NotNil(t, completed)
	assert.Equal(t, metav1.ConditionTrue, completed.Status)
}

func TestSetUpgradeStatusRollbackRequested(t *testing.T) {
	ibu := newIBU(ibuv1.Stages.Upgrade)
	SetUpgradeStatusRollbackRequested(ibu)

	completed := GetCompletedCondition(ibu, ibuv1.Stages.Upgrade)
	assert.NotNil(t, completed)
	assert.Equal(t, metav1.ConditionFalse, completed.Status)
	assert.Equal(t, RollbackRequested, completed.Message)

	inProgress := GetInProgressCondition(ibu, ibuv1.Stages.Upgrade)
	assert.NotNil(t, inProgress)
	assert.Equal(t, metav1.ConditionFalse, inProgress.Status)
	assert.Equal(t, RollbackRequested, inProgress.Message)
}

func TestSetIdleStatusInProgress(t *testing.T) {
	ibu := newIBU(ibuv1.Stages.Idle, cond(ConditionTypes.Idle, metav1.ConditionTrue, ConditionReasons.Idle))
	SetIdleStatusInProgress(ibu, ConditionReasons.Aborting, Aborting)
	idle := GetInProgressCondition(ibu, ibuv1.Stages.Idle)
	assert.NotNil(t, idle)
	assert.Equal(t, metav1.ConditionFalse, idle.Status)
	assert.Equal(t, string(ConditionReasons.Aborting), idle.Reason)
	assert.Equal(t, Aborting, idle.Message)
}

func TestSetStatusInvalidTransition(t *testing.T) {
	ibu := newIBU(ibuv1.Stages.Prep)
	SetStatusInvalidTransition(ibu, "cannot go there")
	c := GetInProgressCondition(ibu, ibuv1.Stages.Prep)
	assert.NotNil(t, c)
	assert.Equal(t, string(ConditionReasons.InvalidTransition), c.Reason)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, "cannot go there", c.Message)
}

func TestClearInvalidTransitionStatusConditions(t *testing.T) {
	tests := []struct {
		name       string
		ibu        *ibuv1.ImageBasedUpgrade
		assertFunc func(t *testing.T, ibu *ibuv1.ImageBasedUpgrade)
	}{
		{
			name: "Idle with InvalidTransition reverts to InProgress",
			ibu:  newIBU(ibuv1.Stages.Idle, cond(ConditionTypes.Idle, metav1.ConditionFalse, ConditionReasons.InvalidTransition)),
			assertFunc: func(t *testing.T, ibu *ibuv1.ImageBasedUpgrade) {
				c := GetInProgressCondition(ibu, ibuv1.Stages.Idle)
				assert.NotNil(t, c)
				assert.Equal(t, string(ConditionReasons.InProgress), c.Reason)
			},
		},
		{
			name: "PrepInProgress with InvalidTransition is removed",
			ibu:  newIBU(ibuv1.Stages.Prep, cond(ConditionTypes.PrepInProgress, metav1.ConditionFalse, ConditionReasons.InvalidTransition)),
			assertFunc: func(t *testing.T, ibu *ibuv1.ImageBasedUpgrade) {
				assert.Nil(t, GetInProgressCondition(ibu, ibuv1.Stages.Prep))
			},
		},
		{
			name: "no invalid transitions is no-op",
			ibu:  newIBU(ibuv1.Stages.Prep, cond(ConditionTypes.PrepInProgress, metav1.ConditionTrue, ConditionReasons.InProgress)),
			assertFunc: func(t *testing.T, ibu *ibuv1.ImageBasedUpgrade) {
				c := GetInProgressCondition(ibu, ibuv1.Stages.Prep)
				assert.NotNil(t, c)
				assert.Equal(t, string(ConditionReasons.InProgress), c.Reason)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ClearInvalidTransitionStatusConditions(tt.ibu)
			tt.assertFunc(t, tt.ibu)
		})
	}
}

func TestUpdateIBUStatus(t *testing.T) {
	t.Run("nil client returns nil", func(t *testing.T) {
		ibu := newIBU(ibuv1.Stages.Upgrade)
		assert.NoError(t, UpdateIBUStatus(context.Background(), nil, ibu))
	})

	t.Run("sets ObservedGeneration on matching conditions", func(t *testing.T) {
		ibu := newIBU(ibuv1.Stages.Upgrade,
			cond(ConditionTypes.UpgradeInProgress, metav1.ConditionTrue, ConditionReasons.InProgress),
			cond(ConditionTypes.Idle, metav1.ConditionFalse, ConditionReasons.InProgress),
		)
		ibu.Generation = 5
		testscheme.AddKnownTypes(ibuv1.GroupVersion, &ibuv1.ImageBasedUpgrade{})
		c := fake.NewClientBuilder().WithScheme(testscheme).WithObjects(ibu).WithStatusSubresource(ibu).Build()
		err := UpdateIBUStatus(context.Background(), c, ibu)
		assert.NoError(t, err)
		assert.Equal(t, int64(5), ibu.Status.ObservedGeneration)
		for _, cond := range ibu.Status.Conditions {
			if cond.Type == string(ConditionTypes.UpgradeInProgress) {
				assert.Equal(t, int64(5), cond.ObservedGeneration)
			}
		}
	})

	t.Run("returns error when status update fails", func(t *testing.T) {
		ibu := newIBU(ibuv1.Stages.Upgrade)
		err := UpdateIBUStatus(context.Background(), newFailStatusClient(), ibu)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to update IBU status")
	})
}

func TestUpdateIPCStatus(t *testing.T) {
	t.Run("nil client returns error", func(t *testing.T) {
		ipc := newIPC(ipcv1.IPStages.Config)
		err := UpdateIPCStatus(context.Background(), nil, ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "client is nil")
	})

	t.Run("sets ObservedGeneration on matching conditions", func(t *testing.T) {
		ipc := newIPC(ipcv1.IPStages.Config,
			cond(ConditionTypes.ConfigInProgress, metav1.ConditionTrue, ConditionReasons.InProgress),
			cond(ConditionTypes.Idle, metav1.ConditionFalse, ConditionReasons.InProgress),
		)
		ipc.Generation = 3
		testscheme.AddKnownTypes(ipcv1.GroupVersion, &ipcv1.IPConfig{})
		c := fake.NewClientBuilder().WithScheme(testscheme).WithObjects(ipc).WithStatusSubresource(ipc).Build()
		err := UpdateIPCStatus(context.Background(), c, ipc)
		assert.NoError(t, err)
		assert.Equal(t, int64(3), ipc.Status.ObservedGeneration)
	})

	t.Run("returns error when status update fails", func(t *testing.T) {
		ipc := newIPC(ipcv1.IPStages.Config)
		err := UpdateIPCStatus(context.Background(), newFailStatusClient(), ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to update IPConfig status")
	})
}

func TestIsIPStageCompleted(t *testing.T) {
	tests := []struct {
		name     string
		ipc      *ipcv1.IPConfig
		stage    ipcv1.IPConfigStage
		expected bool
	}{
		{"completed true", newIPC(ipcv1.IPStages.Config, cond(ConditionTypes.ConfigCompleted, metav1.ConditionTrue, ConditionReasons.Completed)), ipcv1.IPStages.Config, true},
		{"completed false", newIPC(ipcv1.IPStages.Config, cond(ConditionTypes.ConfigCompleted, metav1.ConditionFalse, ConditionReasons.Failed)), ipcv1.IPStages.Config, false},
		{"no condition", newIPC(ipcv1.IPStages.Config), ipcv1.IPStages.Config, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, IsIPStageCompleted(tt.ipc, tt.stage))
		})
	}
}

func TestIsIPStageFailed(t *testing.T) {
	tests := []struct {
		name     string
		ipc      *ipcv1.IPConfig
		stage    ipcv1.IPConfigStage
		expected bool
	}{
		{"completed false is failed", newIPC(ipcv1.IPStages.Config, cond(ConditionTypes.ConfigCompleted, metav1.ConditionFalse, ConditionReasons.Failed)), ipcv1.IPStages.Config, true},
		{"completed true is not failed", newIPC(ipcv1.IPStages.Config, cond(ConditionTypes.ConfigCompleted, metav1.ConditionTrue, ConditionReasons.Completed)), ipcv1.IPStages.Config, false},
		{"no condition", newIPC(ipcv1.IPStages.Config), ipcv1.IPStages.Config, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, IsIPStageFailed(tt.ipc, tt.stage))
		})
	}
}

func TestIsIPStageCompletedOrFailed(t *testing.T) {
	tests := []struct {
		name     string
		ipc      *ipcv1.IPConfig
		stage    ipcv1.IPConfigStage
		expected bool
	}{
		{"completed", newIPC(ipcv1.IPStages.Config, cond(ConditionTypes.ConfigCompleted, metav1.ConditionTrue, ConditionReasons.Completed)), ipcv1.IPStages.Config, true},
		{"failed", newIPC(ipcv1.IPStages.Config, cond(ConditionTypes.ConfigCompleted, metav1.ConditionFalse, ConditionReasons.Failed)), ipcv1.IPStages.Config, true},
		{"no condition", newIPC(ipcv1.IPStages.Config), ipcv1.IPStages.Config, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, IsIPStageCompletedOrFailed(tt.ipc, tt.stage))
		})
	}
}

func TestIsIPStageInProgress(t *testing.T) {
	tests := []struct {
		name     string
		ipc      *ipcv1.IPConfig
		stage    ipcv1.IPConfigStage
		expected bool
	}{
		{"Config in progress", newIPC(ipcv1.IPStages.Config, cond(ConditionTypes.ConfigInProgress, metav1.ConditionTrue, ConditionReasons.InProgress)), ipcv1.IPStages.Config, true},
		{"Config not in progress", newIPC(ipcv1.IPStages.Config, cond(ConditionTypes.ConfigInProgress, metav1.ConditionFalse, ConditionReasons.Completed)), ipcv1.IPStages.Config, false},
		{"Idle with ConditionFalse and InProgress reason is in progress", newIPC(ipcv1.IPStages.Idle, cond(ConditionTypes.Idle, metav1.ConditionFalse, ConditionReasons.InProgress)), ipcv1.IPStages.Idle, true},
		{"Idle with ConditionTrue is not in progress", newIPC(ipcv1.IPStages.Idle, cond(ConditionTypes.Idle, metav1.ConditionTrue, ConditionReasons.Idle)), ipcv1.IPStages.Idle, false},
		{"no condition", newIPC(ipcv1.IPStages.Config), ipcv1.IPStages.Config, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, IsIPStageInProgress(tt.ipc, tt.stage))
		})
	}
}

func TestGetIPInProgressStage(t *testing.T) {
	tests := []struct {
		name     string
		ipc      *ipcv1.IPConfig
		expected ipcv1.IPConfigStage
	}{
		{"Config in progress", newIPC(ipcv1.IPStages.Config, cond(ConditionTypes.ConfigInProgress, metav1.ConditionTrue, ConditionReasons.InProgress)), ipcv1.IPStages.Config},
		{"Idle in progress", newIPC(ipcv1.IPStages.Idle, cond(ConditionTypes.Idle, metav1.ConditionFalse, ConditionReasons.InProgress)), ipcv1.IPStages.Idle},
		{"none in progress", newIPC(ipcv1.IPStages.Idle, cond(ConditionTypes.Idle, metav1.ConditionFalse, ConditionReasons.Idle)), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, GetIPInProgressStage(tt.ipc))
		})
	}
}

func TestGetIPInProgressCondition(t *testing.T) {
	ipc := newIPC(ipcv1.IPStages.Config, cond(ConditionTypes.ConfigInProgress, metav1.ConditionTrue, ConditionReasons.InProgress))
	c := GetIPInProgressCondition(ipc, ipcv1.IPStages.Config)
	assert.NotNil(t, c)
	assert.Equal(t, string(ConditionTypes.ConfigInProgress), c.Type)

	assert.Nil(t, GetIPInProgressCondition(ipc, ipcv1.IPStages.Rollback))
	assert.Nil(t, GetIPInProgressCondition(ipc, "Unknown"))
}

func TestGetIPCompletedCondition(t *testing.T) {
	ipc := newIPC(ipcv1.IPStages.Config, cond(ConditionTypes.ConfigCompleted, metav1.ConditionTrue, ConditionReasons.Completed))
	c := GetIPCompletedCondition(ipc, ipcv1.IPStages.Config)
	assert.NotNil(t, c)
	assert.Equal(t, string(ConditionTypes.ConfigCompleted), c.Type)

	assert.Nil(t, GetIPCompletedCondition(ipc, ipcv1.IPStages.Rollback))
	assert.Nil(t, GetIPCompletedCondition(ipc, "Unknown"))
}

func TestIPSetStatusInProgress(t *testing.T) {
	tests := []struct {
		name   string
		stage  ipcv1.IPConfigStage
		setter func(*ipcv1.IPConfig, string)
	}{
		{"Config", ipcv1.IPStages.Config, SetIPConfigStatusInProgress},
		{"Rollback", ipcv1.IPStages.Rollback, SetIPRollbackStatusInProgress},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ipc := newIPC(tt.stage)
			tt.setter(ipc, "in progress")
			c := GetIPInProgressCondition(ipc, tt.stage)
			assert.NotNil(t, c)
			assert.Equal(t, metav1.ConditionTrue, c.Status)
			assert.Equal(t, string(ConditionReasons.InProgress), c.Reason)
		})
	}
}

func TestIPSetStatusFailed(t *testing.T) {
	tests := []struct {
		name   string
		stage  ipcv1.IPConfigStage
		setter func(*ipcv1.IPConfig, string)
	}{
		{"Config", ipcv1.IPStages.Config, SetIPConfigStatusFailed},
		{"Rollback", ipcv1.IPStages.Rollback, SetIPRollbackStatusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ipc := newIPC(tt.stage)
			tt.setter(ipc, "something broke")

			completed := GetIPCompletedCondition(ipc, tt.stage)
			assert.NotNil(t, completed)
			assert.Equal(t, metav1.ConditionFalse, completed.Status)
			assert.Equal(t, string(ConditionReasons.Failed), completed.Reason)

			inProgress := GetIPInProgressCondition(ipc, tt.stage)
			assert.NotNil(t, inProgress)
			assert.Equal(t, metav1.ConditionFalse, inProgress.Status)
			assert.Equal(t, string(ConditionReasons.Failed), inProgress.Reason)
		})
	}
}

func TestIPSetStatusCompleted(t *testing.T) {
	tests := []struct {
		name   string
		stage  ipcv1.IPConfigStage
		setter func(*ipcv1.IPConfig, string)
	}{
		{"Config", ipcv1.IPStages.Config, SetIPConfigStatusCompleted},
		{"Rollback", ipcv1.IPStages.Rollback, SetIPRollbackStatusCompleted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ipc := newIPC(tt.stage)
			tt.setter(ipc, "done")

			inProgress := GetIPInProgressCondition(ipc, tt.stage)
			assert.NotNil(t, inProgress)
			assert.Equal(t, metav1.ConditionFalse, inProgress.Status)

			completed := GetIPCompletedCondition(ipc, tt.stage)
			assert.NotNil(t, completed)
			assert.Equal(t, metav1.ConditionTrue, completed.Status)
		})
	}
}

func TestSetIPIdleStatusFalse(t *testing.T) {
	ipc := newIPC(ipcv1.IPStages.Idle, cond(ConditionTypes.Idle, metav1.ConditionTrue, ConditionReasons.Idle))
	SetIPIdleStatusFalse(ipc, ConditionReasons.InProgress, InProgress)
	c := GetIPInProgressCondition(ipc, ipcv1.IPStages.Idle)
	assert.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, string(ConditionReasons.InProgress), c.Reason)
}

func TestSetIPStatusInvalidTransition(t *testing.T) {
	t.Run("sets InvalidTransition on known stage", func(t *testing.T) {
		ipc := newIPC(ipcv1.IPStages.Config)
		SetIPStatusInvalidTransition(ipc, "not allowed")
		c := GetIPInProgressCondition(ipc, ipcv1.IPStages.Config)
		assert.NotNil(t, c)
		assert.Equal(t, string(ConditionReasons.InvalidTransition), c.Reason)
		assert.Equal(t, metav1.ConditionFalse, c.Status)
	})

	t.Run("no-op on unknown stage", func(t *testing.T) {
		ipc := newIPC("Unknown")
		SetIPStatusInvalidTransition(ipc, "msg")
		assert.Empty(t, ipc.Status.Conditions)
	})
}

func TestClearIPInvalidTransitionStatusConditions(t *testing.T) {
	tests := []struct {
		name       string
		ipc        *ipcv1.IPConfig
		assertFunc func(t *testing.T, ipc *ipcv1.IPConfig)
	}{
		{
			name: "Idle with InvalidTransition is replaced with ConfigurationInProgress",
			ipc:  newIPC(ipcv1.IPStages.Idle, cond(ConditionTypes.Idle, metav1.ConditionFalse, ConditionReasons.InvalidTransition)),
			assertFunc: func(t *testing.T, ipc *ipcv1.IPConfig) {
				c := GetIPInProgressCondition(ipc, ipcv1.IPStages.Idle)
				assert.NotNil(t, c)
				assert.Equal(t, string(ConditionReasons.ConfigurationInProgress), c.Reason)
			},
		},
		{
			name: "ConfigInProgress with InvalidTransition is removed",
			ipc:  newIPC(ipcv1.IPStages.Config, cond(ConditionTypes.ConfigInProgress, metav1.ConditionFalse, ConditionReasons.InvalidTransition)),
			assertFunc: func(t *testing.T, ipc *ipcv1.IPConfig) {
				assert.Nil(t, GetIPInProgressCondition(ipc, ipcv1.IPStages.Config))
			},
		},
		{
			name: "no invalid transitions is no-op",
			ipc:  newIPC(ipcv1.IPStages.Config, cond(ConditionTypes.ConfigInProgress, metav1.ConditionTrue, ConditionReasons.InProgress)),
			assertFunc: func(t *testing.T, ipc *ipcv1.IPConfig) {
				c := GetIPInProgressCondition(ipc, ipcv1.IPStages.Config)
				assert.NotNil(t, c)
				assert.Equal(t, string(ConditionReasons.InProgress), c.Reason)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ClearIPInvalidTransitionStatusConditions(tt.ipc)
			tt.assertFunc(t, tt.ipc)
		})
	}
}
