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

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/ibu-imager/ostreeclient"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

func updateProgressFile(progressfile, progress string) (err error) {
	f, err := os.Create(pathOutsideChroot(progressfile))
	if err != nil {
		return
	}
	defer f.Close()

	_, err = f.WriteString(progress)
	return
}

func (r *ImageBasedUpgradeReconciler) handlePreReboot(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade, progressfile string) (err error) {
	if err = updateProgressFile(progressfile, "started-pre-reboot"); err != nil {
		r.Log.Error(err, "Failed to create progress file")
		return
	}

	stateroot := fmt.Sprintf("rhcos_%s", ibu.Spec.SeedImageRef.Version)

	utils.ExecuteChrootCmd(utils.Host, "mount /sysroot -o remount,rw")
	stateRootRepo := fmt.Sprintf("/host/ostree/deploy/%s/var", stateroot)

	// TODO: Pre-pivot steps
	//
	// TODO: Call r.BackupRestore.ExportRestoresToDir to write restore CRs to the new stateroot.
	//       If there is invalid type of error returned, we should update the ibu status and
	//       requeue with short interval.
	//
	if err = r.ExtraManifest.ExportExtraManifestToDir(ctx, ibu.Spec.ExtraManifests, stateRootRepo); err != nil {
		r.Log.Error(err, "Failed to export extra manifests")
		updateProgressFile(progressfile, "Failed")
		return
	}

	if err = r.ClusterConfig.FetchClusterConfig(ctx, stateRootRepo); err != nil {
		r.Log.Error(err, "failed fetching cluster config")
		updateProgressFile(progressfile, "Failed")
		return
	}

	if err = r.NetworkConfig.FetchNetworkConfig(ctx, stateRootRepo); err != nil {
		r.Log.Error(err, "failed fetching Network config")
		updateProgressFile(progressfile, "Failed")
		return
	}

	//  Pivot to new stateroot
	r.Log.Info("Pivoting to new stateroot")
	if err = rpmostreeclient.PivotToStaterootChroot(utils.Host, stateroot); err != nil {
		r.Log.Error(err, "failed pivoting to new stateroot")
		updateProgressFile(progressfile, "Failed")
		return
	}

	updateProgressFile(progressfile, "completed-pre-reboot")

	// TODO: Post-pivot steps
	//
	// err = r.ExtraManifest.ApplyExtraManifestsFromDir(ctx, stateRootRepo)
	// if err != nil {
	// 	 r.Log.Error(err, "Failed to apply extra manifests")
	//	 return ctrl.Result{}, err
	// }

	return
}

func (r *ImageBasedUpgradeReconciler) launchPreReboot(
	ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade, progressfile string) (result ctrl.Result) {
	go r.handlePreReboot(ctx, ibu, progressfile)
	result = requeueWithShortInterval()

	return
}

func (r *ImageBasedUpgradeReconciler) handleUpgrade(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (result ctrl.Result, err error) {
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

	progressfile := filepath.Join(utils.Path, "upgrade-progress")

	_, err = os.Stat(pathOutsideChroot(progressfile))

	if err == nil {
		// in progress
		var content []byte
		content, err = os.ReadFile(pathOutsideChroot(progressfile))
		if err != nil {
			return
		}

		progress := strings.TrimSpace(string(content))
		r.Log.Info("Upgrade progress: " + progress)

		if progress == "Failed" {
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.GetCompletedConditionType(lcav1alpha1.Stages.Upgrade),
				utils.ConditionReasons.Completed,
				metav1.ConditionFalse,
				"Upgrade failed",
				ibu.Generation)
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.GetInProgressConditionType(lcav1alpha1.Stages.Upgrade),
				utils.ConditionReasons.Completed,
				metav1.ConditionFalse,
				"Upgrade failed",
				ibu.Generation)
		} else if progress == "completed-pre-reboot" {
			// If completed, update conditions and return doNotRequeue
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.GetCompletedConditionType(lcav1alpha1.Stages.Upgrade),
				utils.ConditionReasons.Completed,
				metav1.ConditionTrue,
				"Upgrade completed",
				ibu.Generation)
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.GetInProgressConditionType(lcav1alpha1.Stages.Upgrade),
				utils.ConditionReasons.Completed,
				metav1.ConditionFalse,
				"Upgrade completed",
				ibu.Generation)
			return doNotRequeue(), nil
		}
	} else if os.IsNotExist(err) {
		result = r.launchPreReboot(ctx, ibu, progressfile)
	}

	return
}
