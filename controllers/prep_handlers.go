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

	"github.com/openshift-kni/lifecycle-agent/internal/common"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"

	"github.com/openshift-kni/lifecycle-agent/internal/generated"
	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

func (r *ImageBasedUpgradeReconciler) launchGetSeedImage(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade, imageListFile, progressfile string) (result ctrl.Result, err error) {
	if err = os.Chdir("/"); err != nil {
		return
	}

	// Write the script
	scriptname := filepath.Join(utils.IBUWorkspacePath, utils.PrepGetSeedImage)
	scriptcontent, _ := generated.Asset(utils.PrepGetSeedImage)
	err = os.WriteFile(common.PathOutsideChroot(scriptname), scriptcontent, 0o700)

	if err != nil {
		err = fmt.Errorf("failed to write handler script: %s, %w", common.PathOutsideChroot(scriptname), err)
		return
	}
	r.Log.Info("Handler script written")

	// Use cluster wide pull-secret by default
	pullSecretFilename := common.ImageRegistryAuthFile

	if ibu.Spec.SeedImageRef.PullSecretRef != nil {
		var pullSecret string
		pullSecret, err = utils.LoadSecretData(ctx, r.Client, ibu.Spec.SeedImageRef.PullSecretRef.Name, ibu.Namespace, corev1.DockerConfigJsonKey)
		if err != nil {
			err = fmt.Errorf("failed to retrieve pull-secret from secret %s, err: %w", ibu.Spec.SeedImageRef.PullSecretRef.Name, err)
			return
		}

		pullSecretFilename = filepath.Join(utils.IBUWorkspacePath, "seed-pull-secret")
		if err = os.WriteFile(common.PathOutsideChroot(pullSecretFilename), []byte(pullSecret), 0o600); err != nil {
			err = fmt.Errorf("failed to write seed image pull-secret to file %s, err: %w", pullSecretFilename, err)
			return
		}
	}

	go r.Executor.Execute(scriptname, "--seed-image", ibu.Spec.SeedImageRef.Image, "--progress-file", progressfile, "--image-list-file", imageListFile, "--pull-secret", pullSecretFilename)
	result = requeueWithShortInterval()

	return
}

func readPrecachingList(imageListFile string) (imageList []string, err error) {
	var content []byte
	content, err = os.ReadFile(common.PathOutsideChroot(imageListFile))
	if err != nil {
		return
	}

	lines := strings.Split(string(content), "\n")

	// Filter out empty lines
	for _, line := range lines {
		if line != "" {
			imageList = append(imageList, line)
		}
	}

	return imageList, nil
}

func (r *ImageBasedUpgradeReconciler) launchPrecaching(ctx context.Context, imageListFile, progressfile string) (result ctrl.Result, err error) {

	imageList, err := readPrecachingList(imageListFile)
	if err != nil {
		err = fmt.Errorf("failed to read pre-caching image file: %s, %w", common.PathOutsideChroot(imageListFile), err)
		return
	}

	// Create pre-cache config using default values
	config := precache.NewConfig(imageList)
	err = r.Precache.CreateJob(ctx, config)
	if err != nil {
		r.Log.Error(err, "Failed to create precaching job")

		// update progress-file
		progressContent := []byte("Failed")
		err = os.WriteFile(common.PathOutsideChroot(progressfile), progressContent, 0o700)
		if err != nil {
			r.Log.Error(err, "Failed to update progress file for precaching")
			return
		}

		return
	}

	// update progress-file
	progressContent := []byte("precaching-in-progress")
	err = os.WriteFile(common.PathOutsideChroot(progressfile), progressContent, 0o700)
	if err != nil {
		r.Log.Error(err, "Failed to update progress file for precaching")
		return
	}
	r.Log.Info("Precaching progress status updated to in-progress")

	result = requeueWithShortInterval()
	return
}

func (r *ImageBasedUpgradeReconciler) queryPrecachingStatus(ctx context.Context, progressfile string) (result ctrl.Result, err error) {
	// fetch precaching status
	status, err := r.Precache.QueryJobStatus(ctx)
	if err != nil {
		r.Log.Error(err, "Failed to get precaching job status")
		return
	}

	result = requeueImmediately()

	var progressContent []byte
	var logMsg string
	switch {
	case status.Status == "Active":
		logMsg = "Precaching in-progress"
		result = requeueWithMediumInterval()
	case status.Status == "Succeeded":
		progressContent = []byte("completed-precache")
		logMsg = "Precaching completed"
	case status.Status == "Failed":
		progressContent = []byte("Failed")
		logMsg = "Precaching failed"
	}

	// Augment precaching summary data if available
	if status.Message != "" {
		logMsg = fmt.Sprintf("%s: %s", logMsg, status.Message)
	}
	r.Log.Info(logMsg)

	// update progress-file
	if len(progressContent) > 0 {
		if err = os.WriteFile(common.PathOutsideChroot(progressfile), progressContent, 0o700); err != nil {
			r.Log.Error(err, "Failed to update progress file for precaching")
			return
		}
	}

	return
}

func (r *ImageBasedUpgradeReconciler) launchSetupStateroot(
	ibu *lcav1alpha1.ImageBasedUpgrade, progressfile string) (result ctrl.Result, err error) {
	if err = os.Chdir("/"); err != nil {
		return
	}

	// Write the script
	scriptname := filepath.Join(utils.IBUWorkspacePath, utils.PrepSetupStateroot)
	scriptcontent, _ := generated.Asset(utils.PrepSetupStateroot)
	err = os.WriteFile(common.PathOutsideChroot(scriptname), scriptcontent, 0o700)

	if err != nil {
		err = fmt.Errorf("failed to write handler script: %s, %w", common.PathOutsideChroot(scriptname), err)
		return
	}

	go r.Executor.Execute(scriptname, "--seed-image", ibu.Spec.SeedImageRef.Image, "--progress-file", progressfile,
		"--os-version", ibu.Spec.SeedImageRef.Version, "--os-name", getDesiredStaterootName(ibu))

	result = requeueWithShortInterval()

	return
}

func (r *ImageBasedUpgradeReconciler) runCleanup(
	ibu *lcav1alpha1.ImageBasedUpgrade) {
	if err := os.Chdir("/"); err != nil {
		return
	}

	// Write the script
	scriptname := filepath.Join(utils.IBUWorkspacePath, utils.PrepCleanup)
	scriptcontent, _ := generated.Asset(utils.PrepCleanup)
	err := os.WriteFile(common.PathOutsideChroot(scriptname), scriptcontent, 0o700)

	if err != nil {
		r.Log.Error(err, fmt.Sprintf("failed to write handler script: %s", common.PathOutsideChroot(scriptname)))
		return
	}
	r.Log.Info("Handler script written")
	// This should be a quick operation, so we can run it within the handler thread
	r.Executor.Execute(scriptname, "--seed-image", ibu.Spec.SeedImageRef.Image)

	return
}

//nolint:unparam
func (r *ImageBasedUpgradeReconciler) handlePrep(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (result ctrl.Result, err error) {
	result = doNotRequeue()

	_, err = os.Stat(utils.Host)
	if err != nil {
		// fail without /host
		return
	}

	if _, err = os.Stat(common.PathOutsideChroot(utils.IBUWorkspacePath)); os.IsNotExist(err) {
		err = os.Mkdir(common.PathOutsideChroot(utils.IBUWorkspacePath), 0o700)
	}

	if err != nil {
		return
	}

	progressfile := filepath.Join(utils.IBUWorkspacePath, "prep-progress")
	imageListFile := filepath.Join(utils.IBUWorkspacePath, "image-list-file")

	_, err = os.Stat(common.PathOutsideChroot(progressfile))

	if err == nil {
		// in progress
		var content []byte
		content, err = os.ReadFile(common.PathOutsideChroot(progressfile))
		if err != nil {
			return
		}

		progress := strings.TrimSpace(string(content))
		r.Log.Info("Prep progress: " + progress)

		if progress == "Failed" {
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.GetCompletedConditionType(lcav1alpha1.Stages.Prep),
				utils.ConditionReasons.Completed,
				metav1.ConditionFalse,
				"Prep failed",
				ibu.Generation)
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.GetInProgressConditionType(lcav1alpha1.Stages.Prep),
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
			result, err = r.launchPrecaching(ctx, imageListFile, progressfile)
			if err != nil {
				r.Log.Error(err, "Failed to launch get-seed-image phase")
				return
			}
		} else if progress == "precaching-in-progress" {
			result, err = r.queryPrecachingStatus(ctx, progressfile)
			if err != nil {
				r.Log.Error(err, "Failed to query get-seed-image phase")
				return
			}
		} else if progress == "completed-precache" {
			r.runCleanup(ibu)

			// Fetch final precaching job report summary
			conditionMessage := "Prep completed"
			status, err := r.Precache.QueryJobStatus(ctx)
			if err == nil && status != nil && status.Message != "" {
				conditionMessage += fmt.Sprintf(": %s", status.Message)
			}

			// If completed, update conditions and return doNotRequeue
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.GetCompletedConditionType(lcav1alpha1.Stages.Prep),
				utils.ConditionReasons.Completed,
				metav1.ConditionTrue,
				conditionMessage,
				ibu.Generation)
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.GetInProgressConditionType(lcav1alpha1.Stages.Prep),
				utils.ConditionReasons.Completed,
				metav1.ConditionFalse,
				"Prep completed",
				ibu.Generation)
		} else {
			result = requeueWithShortInterval()
		}
	} else if os.IsNotExist(err) {
		result, err = r.launchGetSeedImage(ctx, ibu, imageListFile, progressfile)
		if err != nil {
			r.Log.Error(err, "Failed to launch get-seed-image phase")
			return
		}
	}

	return
}
