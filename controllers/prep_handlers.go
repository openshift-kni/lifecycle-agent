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
	"strings"
	"syscall"

	ranv1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/generated"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

func (r *ImageBasedUpgradeReconciler) handlePrep(ctx context.Context, ibu *ranv1alpha1.ImageBasedUpgrade) (result ctrl.Result, err error) {
	result = doNotRequeue()

	_, err = os.Stat(utils.Host)
	if err == nil {
		// change root directory to /host
		if err = syscall.Chroot(utils.Host); err != nil {
			return
		}
	} else {
		// continue if /host does not exist
		if !os.IsNotExist(err) {
			// fail if any other error
			return
		}
	}

	if _, err = os.Stat(utils.Path); os.IsNotExist(err) {
		err = os.Mkdir(utils.Path, 0700)
	}

	if err != nil {
		return
	}

	progressfile := filepath.Join(utils.Path, "prep-progress")

	_, err = os.Stat(progressfile)

	if err == nil {
		// in progress
		var content []byte
		content, err = os.ReadFile(progressfile)
		if err != nil {
			return
		}

		progress := strings.TrimSpace(string(content))
		r.Log.Info("Prep progress: " + progress)

		if progress == "Completed" {
			// If completed, update conditions and return doNotRequeue
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.GetCompletedConditionType(ranv1alpha1.Stages.Prep),
				utils.ConditionReasons.Completed,
				metav1.ConditionTrue,
				"Prep completed",
				ibu.Generation)
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.GetInProgressConditionType(ranv1alpha1.Stages.Prep),
				utils.ConditionReasons.Completed,
				metav1.ConditionFalse,
				"Prep completed",
				ibu.Generation)
		} else if progress == "Failed" {
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.GetCompletedConditionType(ranv1alpha1.Stages.Prep),
				utils.ConditionReasons.Completed,
				metav1.ConditionFalse,
				"Prep failed",
				ibu.Generation)
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.GetInProgressConditionType(ranv1alpha1.Stages.Prep),
				utils.ConditionReasons.Completed,
				metav1.ConditionFalse,
				"Prep failed",
				ibu.Generation)
		} else {
			result = requeueWithShortInterval()
		}
	} else if os.IsNotExist(err) {
		// launch the script
		if err = os.Chdir("/"); err != nil {
			return
		}

		scriptname := filepath.Join(utils.Path, utils.PrepHandlerScript)
		scriptcontent, _ := generated.Asset(utils.PrepHandlerScript)
		err = os.WriteFile(scriptname, scriptcontent, 0700)

		if err != nil {
			r.Log.Error(err, "Failed to write handler script")
			return
		}
		r.Log.Info("Handler script written")

		cmd := fmt.Sprintf("%s --seed-image %s --progress-file %s", scriptname, ibu.Spec.SeedImageRef.Image, progressfile)
		go utils.ExecuteCmd(cmd)
		result = requeueWithShortInterval()
	}

	return
}
