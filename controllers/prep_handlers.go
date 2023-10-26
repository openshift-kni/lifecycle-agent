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

	ranv1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/generated"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

func pathOutsideChroot(filename string) string {
	return filepath.Join(utils.Host, filename)
}

func (r *ImageBasedUpgradeReconciler) launchGetSeedImage(
	ibu *ranv1alpha1.ImageBasedUpgrade, progressfile string) (result ctrl.Result, err error) {
	if err = os.Chdir("/"); err != nil {
		return
	}

	// Write the script
	scriptname := filepath.Join(utils.Path, utils.PrepGetSeedImage)
	scriptcontent, _ := generated.Asset(utils.PrepGetSeedImage)
	err = os.WriteFile(pathOutsideChroot(scriptname), scriptcontent, 0o700)

	if err != nil {
		r.Log.Error(err, "Failed to write handler script: %s", pathOutsideChroot(scriptname))
		return
	}
	r.Log.Info("Handler script written")

	cmd := fmt.Sprintf("%s --seed-image %s --progress-file %s", scriptname, ibu.Spec.SeedImageRef.Image, progressfile)
	go utils.ExecuteChrootCmd(utils.Host, cmd)
	result = requeueWithShortInterval()

	return
}

func (r *ImageBasedUpgradeReconciler) launchPullImages(
	ibu *ranv1alpha1.ImageBasedUpgrade, progressfile string) (result ctrl.Result, err error) {
	if err = os.Chdir("/"); err != nil {
		return
	}

	// Write the script
	scriptname := filepath.Join(utils.Path, utils.PrepPullImages)
	scriptcontent, _ := generated.Asset(utils.PrepPullImages)
	err = os.WriteFile(pathOutsideChroot(scriptname), scriptcontent, 0o700)

	if err != nil {
		r.Log.Error(err, "Failed to write handler script: %s", pathOutsideChroot(scriptname))
		return
	}
	r.Log.Info("Handler script written")

	cmd := fmt.Sprintf("%s --seed-image %s --progress-file %s", scriptname, ibu.Spec.SeedImageRef.Image, progressfile)
	go utils.ExecuteChrootCmd(utils.Host, cmd)
	result = requeueWithShortInterval()

	return
}

func (r *ImageBasedUpgradeReconciler) launchSetupStateroot(
	ibu *ranv1alpha1.ImageBasedUpgrade, progressfile string) (result ctrl.Result, err error) {
	if err = os.Chdir("/"); err != nil {
		return
	}

	// Write the script
	scriptname := filepath.Join(utils.Path, utils.PrepSetupStateroot)
	scriptcontent, _ := generated.Asset(utils.PrepSetupStateroot)
	err = os.WriteFile(pathOutsideChroot(scriptname), scriptcontent, 0o700)

	if err != nil {
		r.Log.Error(err, "Failed to write handler script: %s", pathOutsideChroot(scriptname))
		return
	}
	r.Log.Info("Handler script written")

	cmd := fmt.Sprintf("%s --seed-image %s --progress-file %s", scriptname, ibu.Spec.SeedImageRef.Image, progressfile)
	go utils.ExecuteChrootCmd(utils.Host, cmd)
	result = requeueWithShortInterval()

	return
}

func (r *ImageBasedUpgradeReconciler) runCleanup(
	ibu *ranv1alpha1.ImageBasedUpgrade) {
	if err := os.Chdir("/"); err != nil {
		return
	}

	// Write the script
	scriptname := filepath.Join(utils.Path, utils.PrepCleanup)
	scriptcontent, _ := generated.Asset(utils.PrepCleanup)
	err := os.WriteFile(pathOutsideChroot(scriptname), scriptcontent, 0o700)

	if err != nil {
		r.Log.Error(err, "Failed to write handler script: %s", pathOutsideChroot(scriptname))
		return
	}
	r.Log.Info("Handler script written")

	cmd := fmt.Sprintf("%s --seed-image %s", scriptname, ibu.Spec.SeedImageRef.Image)
	// This should be a quick operation, so we can run it within the handler thread
	utils.ExecuteChrootCmd(utils.Host, cmd)

	return
}

//nolint:unparam
func (r *ImageBasedUpgradeReconciler) handlePrep(ctx context.Context, ibu *ranv1alpha1.ImageBasedUpgrade) (result ctrl.Result, err error) {
	result = doNotRequeue()

	_, err = os.Stat(utils.Host)
	if err != nil {
		// fail without /host
		return
	}

	if _, err = os.Stat(pathOutsideChroot(utils.Path)); os.IsNotExist(err) {
		err = os.Mkdir(pathOutsideChroot(utils.Path), 0o700)
	}

	if err != nil {
		return
	}

	progressfile := filepath.Join(utils.Path, "prep-progress")

	_, err = os.Stat(pathOutsideChroot(progressfile))

	if err == nil {
		// in progress
		var content []byte
		content, err = os.ReadFile(pathOutsideChroot(progressfile))
		if err != nil {
			return
		}

		progress := strings.TrimSpace(string(content))
		r.Log.Info("Prep progress: " + progress)

		if progress == "Failed" {
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
		} else if progress == "completed-seed-image-pull" {
			result, err = r.launchSetupStateroot(ibu, progressfile)
			if err != nil {
				r.Log.Error(err, "Failed to launch get-seed-image phase")
				return
			}
		} else if progress == "completed-stateroot" {
			result, err = r.launchPullImages(ibu, progressfile)
			if err != nil {
				r.Log.Error(err, "Failed to launch get-seed-image phase")
				return
			}
		} else if progress == "completed-precache" {
			r.runCleanup(ibu)

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
		} else {
			result = requeueWithShortInterval()
		}
	} else if os.IsNotExist(err) {
		result, err = r.launchGetSeedImage(ibu, progressfile)
		if err != nil {
			r.Log.Error(err, "Failed to launch get-seed-image phase")
			return
		}
	}

	return
}
