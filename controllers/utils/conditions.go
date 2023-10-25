package utils

import (
	ranv1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionType is a string representing the condition's type
type ConditionType string

// ConditionTypes define the different types of conditions that will be set
var ConditionTypes = struct {
	Idle               ConditionType
	PrepInProgress     ConditionType
	PrepCompleted      ConditionType
	UpgradeInProgress  ConditionType
	UpgradeCompleted   ConditionType
	RollbackInProgress ConditionType
	RollbackCompleted  ConditionType
}{
	Idle:               "Idle",
	PrepInProgress:     "PrepInProgress",
	PrepCompleted:      "PrepCompleted",
	UpgradeInProgress:  "UpgradeInProgress",
	UpgradeCompleted:   "UpgradeCompleted",
	RollbackInProgress: "RollbackInProgress",
	RollbackCompleted:  "RollbackCompleted",
}

// FinalConditionTypes defines the valid conditions for transitioning back to idle
var FinalConditionTypes = []ConditionType{ConditionTypes.UpgradeCompleted, ConditionTypes.RollbackCompleted}

// ConditionReason is a string representing the condition's reason
type ConditionReason string

// ConditionReasons define the different reasons that conditions will be set for
var ConditionReasons = struct {
	Idle              ConditionReason
	Completed         ConditionReason
	Failed            ConditionReason
	TimedOut          ConditionReason
	InProgress        ConditionReason
	Aborting          ConditionReason
	AbortCompleted    ConditionReason
	AbortFailed       ConditionReason
	Finalizing        ConditionReason
	FinalizeCompleted ConditionReason
	FinalizeFailed    ConditionReason
	InvalidTransition ConditionReason
}{
	Idle:              "Idle",
	Completed:         "Completed",
	Failed:            "Failed",
	TimedOut:          "TimedOut",
	InProgress:        "InProgress",
	Aborting:          "Aborting",
	AbortCompleted:    "AbortCompleted",
	AbortFailed:       "AbortFailed",
	Finalizing:        "Finalizing",
	FinalizeCompleted: "FinalizeCompleted",
	FinalizeFailed:    "FinalizeFailed",
	InvalidTransition: "InvalidTransition",
}

// SetStatusCondition is a convenience wrapper for meta.SetStatusCondition that takes in the types defined here and converts them to strings
func SetStatusCondition(existingConditions *[]metav1.Condition, conditionType ConditionType, conditionReason ConditionReason, conditionStatus metav1.ConditionStatus, message string, generation int64) {
	conditions := *existingConditions
	condition := meta.FindStatusCondition(*existingConditions, string(conditionType))
	if condition != nil &&
		condition.Status != conditionStatus &&
		conditions[len(conditions)-1].Type != string(conditionType) {
		meta.RemoveStatusCondition(existingConditions, string(conditionType))
	}
	meta.SetStatusCondition(
		existingConditions,
		metav1.Condition{
			Type:               string(conditionType),
			Status:             conditionStatus,
			Reason:             string(conditionReason),
			Message:            message,
			ObservedGeneration: generation,
		},
	)
}

// ResetStatusConditions remove all other conditions and sets idle to true
func ResetStatusConditions(existingConditions *[]metav1.Condition, generation int64) {
	for _, condition := range *existingConditions {
		if condition.Type != string(ConditionTypes.Idle) {
			meta.RemoveStatusCondition(existingConditions, condition.Type)
		}
	}
	meta.SetStatusCondition(
		existingConditions,
		metav1.Condition{
			Type:               string(ConditionTypes.Idle),
			Status:             metav1.ConditionTrue,
			Reason:             string(ConditionReasons.Idle),
			Message:            "Idle",
			ObservedGeneration: generation,
		},
	)
}

// GetCurrentInProgressStage returns the stage that is currently in progress
func GetCurrentInProgressStage(ibu *ranv1alpha1.ImageBasedUpgrade) ranv1alpha1.ImageBasedUpgradeStage {
	idleCondition := meta.FindStatusCondition(ibu.Status.Conditions, string(ConditionTypes.Idle))
	if idleCondition == nil || idleCondition.Status == metav1.ConditionTrue {
		return ""
	}

	switch idleCondition.Reason {
	case string(ConditionReasons.Aborting):
	case string(ConditionReasons.AbortFailed):
	case string(ConditionReasons.Finalizing):
	case string(ConditionReasons.FinalizeFailed):
		return ranv1alpha1.Stages.Idle
	}

	conditionToStageMap := map[ConditionType]ranv1alpha1.ImageBasedUpgradeStage{
		ConditionTypes.PrepInProgress:     ranv1alpha1.Stages.Prep,
		ConditionTypes.UpgradeInProgress:  ranv1alpha1.Stages.Upgrade,
		ConditionTypes.RollbackInProgress: ranv1alpha1.Stages.Rollback,
	}

	for conditionType, stage := range conditionToStageMap {
		condition := meta.FindStatusCondition(ibu.Status.Conditions, string(conditionType))
		if condition != nil && condition.Status == metav1.ConditionTrue {
			return stage
		}
	}
	return ""
}

// GetInProgressConditionType returns the pending condition type based on the current stage
func GetInProgressConditionType(stage ranv1alpha1.ImageBasedUpgradeStage) (conditionType ConditionType) {
	switch stage {
	case ranv1alpha1.Stages.Prep:
		conditionType = ConditionTypes.PrepInProgress
	case ranv1alpha1.Stages.Upgrade:
		conditionType = ConditionTypes.UpgradeInProgress
	case ranv1alpha1.Stages.Rollback:
		conditionType = ConditionTypes.RollbackInProgress
	}
	return
}

// GetCompletedConditionType returns the succeeded condition type based on the current stage
func GetCompletedConditionType(stage ranv1alpha1.ImageBasedUpgradeStage) (conditionType ConditionType) {
	switch stage {
	case ranv1alpha1.Stages.Idle:
		conditionType = ConditionTypes.Idle
	case ranv1alpha1.Stages.Prep:
		conditionType = ConditionTypes.PrepCompleted
	case ranv1alpha1.Stages.Upgrade:
		conditionType = ConditionTypes.UpgradeCompleted
	case ranv1alpha1.Stages.Rollback:
		conditionType = ConditionTypes.RollbackCompleted
	}
	return
}

// GetPreviousCompletedCondition returns the succeeded condition for the previous stage
func GetPreviousCompletedCondition(ibu *ranv1alpha1.ImageBasedUpgrade) *metav1.Condition {
	var conditionType ConditionType
	switch ibu.Spec.Stage {
	case ranv1alpha1.Stages.Prep:
		conditionType = ConditionTypes.Idle
	case ranv1alpha1.Stages.Upgrade:
		conditionType = ConditionTypes.PrepCompleted
	case ranv1alpha1.Stages.Rollback:
		conditionType = ConditionTypes.UpgradeCompleted
	}
	if conditionType != "" {
		return meta.FindStatusCondition(ibu.Status.Conditions, string(conditionType))
	}
	return nil
}
