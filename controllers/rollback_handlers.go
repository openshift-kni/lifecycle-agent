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

	ranv1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

func (r *ImageBasedUpgradeReconciler) handleRollback(ctx context.Context, ibu *ranv1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {

	// TODO actual steps
	// If completed, update conditions and return doNotRequeue
	utils.SetStatusCondition(&ibu.Status.Conditions,
		utils.GetCompletedConditionType(ranv1alpha1.Stages.Rollback),
		utils.ConditionReasons.Completed,
		metav1.ConditionTrue,
		"Rollback completed",
		ibu.Generation)
	utils.SetStatusCondition(&ibu.Status.Conditions,
		utils.GetInProgressConditionType(ranv1alpha1.Stages.Rollback),
		utils.ConditionReasons.Completed,
		metav1.ConditionFalse,
		"Rollback completed",
		ibu.Generation)
	return doNotRequeue(), nil
}
