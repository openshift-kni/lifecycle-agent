package utils

import (
	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
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
	SeedGenInProgress  ConditionType
	SeedGenCompleted   ConditionType
}{
	Idle:               "Idle",
	PrepInProgress:     "PrepInProgress",
	PrepCompleted:      "PrepCompleted",
	UpgradeInProgress:  "UpgradeInProgress",
	UpgradeCompleted:   "UpgradeCompleted",
	RollbackInProgress: "RollbackInProgress",
	RollbackCompleted:  "RollbackCompleted",
	SeedGenInProgress:  "SeedGenInProgress",
	SeedGenCompleted:   "SeedGenCompleted",
}

var SeedGenConditionTypes = struct {
	SeedGenInProgress ConditionType
	SeedGenCompleted  ConditionType
}{
	SeedGenInProgress: "SeedGenInProgress",
	SeedGenCompleted:  "SeedGenCompleted",
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

var SeedGenConditionReasons = struct {
	Completed  ConditionReason
	Failed     ConditionReason
	InProgress ConditionReason
}{
	Completed:  "Completed",
	Failed:     "Failed",
	InProgress: "InProgress",
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

// IsStageCompleted checks if the completed condition status for the stage is true
func IsStageCompleted(ibu *lcav1alpha1.ImageBasedUpgrade, stage lcav1alpha1.ImageBasedUpgradeStage) bool {
	condition := GetCompletedCondition(ibu, stage)
	if condition != nil && condition.Status == metav1.ConditionTrue {
		return true
	}
	return false
}

// IsStageCompletedOrFailed checks if the completed condition for the stage is present
func IsStageCompletedOrFailed(ibu *lcav1alpha1.ImageBasedUpgrade, stage lcav1alpha1.ImageBasedUpgradeStage) bool {
	condition := GetCompletedCondition(ibu, stage)
	if condition != nil {
		return true
	}
	return false
}

// IsStageInProgress checks if ibu is working on the stage
func IsStageInProgress(ibu *lcav1alpha1.ImageBasedUpgrade, stage lcav1alpha1.ImageBasedUpgradeStage) bool {
	if stage == lcav1alpha1.Stages.Idle {
		idleCondition := meta.FindStatusCondition(ibu.Status.Conditions, string(ConditionTypes.Idle))
		if idleCondition == nil || idleCondition.Status == metav1.ConditionTrue {
			return false
		}

		switch idleCondition.Reason {
		case string(ConditionReasons.Aborting), string(ConditionReasons.AbortFailed), string(ConditionReasons.Finalizing), string(ConditionReasons.FinalizeFailed):
			return true
		}
		return false
	}

	condition := GetInProgressCondition(ibu, stage)
	if condition != nil && condition.Status == metav1.ConditionTrue {
		return true
	}
	return false
}

// GetCurrentInProgressStage returns the stage that is currently in progress
func GetCurrentInProgressStage(ibu *lcav1alpha1.ImageBasedUpgrade) lcav1alpha1.ImageBasedUpgradeStage {
	stages := []lcav1alpha1.ImageBasedUpgradeStage{
		lcav1alpha1.Stages.Idle,
		lcav1alpha1.Stages.Prep,
		lcav1alpha1.Stages.Upgrade,
		lcav1alpha1.Stages.Rollback,
	}

	for _, stage := range stages {
		if IsStageInProgress(ibu, stage) {
			return stage
		}
	}
	return ""
}

// GetInProgressCondition returns the in progress condition based on the stage
func GetInProgressCondition(ibu *lcav1alpha1.ImageBasedUpgrade, stage lcav1alpha1.ImageBasedUpgradeStage) *metav1.Condition {
	conditionType := GetInProgressConditionType(stage)
	if conditionType != "" {
		return meta.FindStatusCondition(ibu.Status.Conditions, string(conditionType))
	}
	return nil
}

// GetInProgressConditionType returns the in progress condition type based on the stage
func GetInProgressConditionType(stage lcav1alpha1.ImageBasedUpgradeStage) (conditionType ConditionType) {
	switch stage {
	case lcav1alpha1.Stages.Prep:
		conditionType = ConditionTypes.PrepInProgress
	case lcav1alpha1.Stages.Upgrade:
		conditionType = ConditionTypes.UpgradeInProgress
	case lcav1alpha1.Stages.Rollback:
		conditionType = ConditionTypes.RollbackInProgress
	}
	return
}

// GetCompletedCondition returns the completed condition based on the stage
func GetCompletedCondition(ibu *lcav1alpha1.ImageBasedUpgrade, stage lcav1alpha1.ImageBasedUpgradeStage) *metav1.Condition {
	conditionType := GetCompletedConditionType(stage)
	if conditionType != "" {
		return meta.FindStatusCondition(ibu.Status.Conditions, string(conditionType))
	}
	return nil
}

// GetCompletedConditionType returns the completed condition type based on the stage
func GetCompletedConditionType(stage lcav1alpha1.ImageBasedUpgradeStage) (conditionType ConditionType) {
	switch stage {
	case lcav1alpha1.Stages.Idle:
		conditionType = ConditionTypes.Idle
	case lcav1alpha1.Stages.Prep:
		conditionType = ConditionTypes.PrepCompleted
	case lcav1alpha1.Stages.Upgrade:
		conditionType = ConditionTypes.UpgradeCompleted
	case lcav1alpha1.Stages.Rollback:
		conditionType = ConditionTypes.RollbackCompleted
	}
	return
}

// GetPreviousStage returns the previous stage for the one passed in
func GetPreviousStage(stage lcav1alpha1.ImageBasedUpgradeStage) lcav1alpha1.ImageBasedUpgradeStage {
	switch stage {
	case lcav1alpha1.Stages.Prep:
		return lcav1alpha1.Stages.Idle
	case lcav1alpha1.Stages.Upgrade:
		return lcav1alpha1.Stages.Prep
	case lcav1alpha1.Stages.Rollback:
		return lcav1alpha1.Stages.Upgrade
	}
	return ""
}

// SetUpgradeStatusFailed updates the upgrade status to failed with message
func SetUpgradeStatusFailed(ibu *lcav1alpha1.ImageBasedUpgrade, msg string) {
	SetStatusCondition(&ibu.Status.Conditions,
		GetInProgressConditionType(lcav1alpha1.Stages.Upgrade),
		ConditionReasons.Failed,
		metav1.ConditionFalse,
		msg,
		ibu.Generation)
	SetStatusCondition(&ibu.Status.Conditions,
		GetCompletedConditionType(lcav1alpha1.Stages.Upgrade),
		ConditionReasons.Failed,
		metav1.ConditionFalse,
		"Upgrade failed",
		ibu.Generation)
}

// SetUpgradeStatusInProgress updates the upgrade status to in progress with message
func SetUpgradeStatusInProgress(ibu *lcav1alpha1.ImageBasedUpgrade, msg string) {
	SetStatusCondition(&ibu.Status.Conditions,
		GetInProgressConditionType(lcav1alpha1.Stages.Upgrade),
		ConditionReasons.InProgress,
		metav1.ConditionTrue,
		msg,
		ibu.Generation)
}

// SetUpgradeStatusCompleted updates the upgrade status to completed
func SetUpgradeStatusCompleted(ibu *lcav1alpha1.ImageBasedUpgrade) {
	SetStatusCondition(&ibu.Status.Conditions,
		GetCompletedConditionType(lcav1alpha1.Stages.Upgrade),
		ConditionReasons.Completed,
		metav1.ConditionTrue,
		"Upgrade completed",
		ibu.Generation)
	SetStatusCondition(&ibu.Status.Conditions,
		GetInProgressConditionType(lcav1alpha1.Stages.Upgrade),
		ConditionReasons.Completed,
		metav1.ConditionFalse,
		"Upgrade completed",
		ibu.Generation)
}
