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
	"errors"
	"fmt"
	"os"

	"github.com/openshift-kni/lifecycle-agent/internal/backuprestore"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	ctrl "sigs.k8s.io/controller-runtime"
)

var osStat = os.Stat

//nolint:unparam
func (r *ImageBasedUpgradeReconciler) handleAbort(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {
	r.Log.Info("Starting handleAbort")

	if successful, errMsg := r.cleanup(ctx, false, ibu); successful {
		r.Log.Info("Finished handleAbort")
	} else {

		utils.SetStatusCondition(&ibu.Status.Conditions,
			utils.ConditionTypes.Idle,
			utils.ConditionReasons.AbortFailed,
			metav1.ConditionFalse,
			errMsg,
			ibu.Generation,
		)
	}
	return doNotRequeue(), nil
}

//nolint:unparam
func (r *ImageBasedUpgradeReconciler) handleAbortFailure(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {
	return doNotRequeue(), nil
}

//nolint:unparam
func (r *ImageBasedUpgradeReconciler) handleFinalize(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {
	r.Log.Info("Starting handleFinalize")

	if successful, errMsg := r.cleanup(ctx, true, ibu); successful {
		r.Log.Info("Finished handleFinalize")
	} else {
		utils.SetStatusCondition(&ibu.Status.Conditions,
			utils.ConditionTypes.Idle,
			utils.ConditionReasons.FinalizeFailed,
			metav1.ConditionFalse,
			errMsg,
			ibu.Generation,
		)
	}
	return doNotRequeue(), nil
}

//nolint:unparam
func (r *ImageBasedUpgradeReconciler) handleFinalizeFailure(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {

	// TODO actual steps
	// If succeeds, return doNotRequeue
	return doNotRequeue(), nil
}

// cleanup cleans stateroots, precache, backup, ibu files and OADP operator
// returns true if all cleanup tasks were successful
func (r *ImageBasedUpgradeReconciler) cleanup(
	ctx context.Context, allUnbootedStateroots bool,
	ibu *lcav1alpha1.ImageBasedUpgrade) (bool, string) {
	// try to clean up as much as possible and avoid returning when one of the cleanup tasks fails
	// successful means that all the cleanup tasks completed without any error
	successful := true
	errorMessage := ""

	var handleError = func(err error, msg string) {
		successful = false
		r.Log.Error(err, msg)
		errorMessage += msg + " "
	}
	// Terminate precaching worker thread
	if r.PrepTask.Active && r.PrepTask.Cancel != nil {
		r.PrepTask.Cancel()
		r.PrepTask.Reset()
	}
	if err := r.cleanupStateroots(allUnbootedStateroots, ibu); err != nil {
		handleError(err, "failed to cleanup stateroots.")
	}
	if err := r.Precache.Cleanup(ctx); err != nil {
		handleError(err, "failed to cleanup precaching resources.")
	}
	if allRemoved, err := r.BackupRestore.CleanupBackups(ctx); err != nil {
		handleError(err, "failed to cleanup backups.")
	} else if !allRemoved {
		err := errors.New("failed to delete all the backup CRs.")
		handleError(err, err.Error())
	}
	if err := r.BackupRestore.DeleteOadpOperator(ctx, backuprestore.OadpNs); err != nil {
		handleError(err, "failed to delete OADP operator.")
	}
	if err := cleanupIBUFiles(); err != nil {
		handleError(err, "failed to cleanup ibu files.")
	}

	return successful, errorMessage
}

// cleanupStateroot cleans all unbooted stateroots or desired stateroot
// depending on allUnbootedStateroots argument
func (r *ImageBasedUpgradeReconciler) cleanupStateroots(
	allUnbootedStateroots bool, ibu *lcav1alpha1.ImageBasedUpgrade) error {
	if allUnbootedStateroots {
		return r.cleanupUnbootedStateroots()
	}
	return r.cleanupUnbootedStateroot(common.GetDesiredStaterootName(ibu))
}

func cleanupIBUFiles() error {
	if _, err := os.Stat(common.PathOutsideChroot(utils.IBUWorkspacePath)); err != nil {
		return nil
	}
	if err := os.RemoveAll(common.PathOutsideChroot(utils.IBUWorkspacePath)); err != nil {
		return fmt.Errorf("removing %s failed: %w", utils.IBUWorkspacePath, err)
	}
	return nil
}

func (r *ImageBasedUpgradeReconciler) cleanupUnbootedStateroots() error {
	status, err := r.RPMOstreeClient.QueryStatus()
	if err != nil {
		return err
	}

	bootedStateroot := ""
	staterootsToRemove := make([]string, 0)
	// since undeploy shifts the order, undeploy in the reverse order
	for i := len(status.Deployments) - 1; i >= 0; i-- {
		deployment := &status.Deployments[i]
		if deployment.Booted {
			bootedStateroot = deployment.OSName
			continue
		}
		staterootsToRemove = append(staterootsToRemove, deployment.OSName)
	}

	failures := 0
	for _, stateroot := range staterootsToRemove {
		if stateroot == bootedStateroot {
			continue
		}
		if err := r.cleanupUnbootedStateroot(stateroot); err != nil {
			r.Log.Error(err, "failed to remove stateroot", "stateroot", stateroot)
			failures += 1
		}
	}
	if failures == 0 {
		return nil
	}
	return fmt.Errorf("failed to remove %d stateroots", failures)
}

func (r *ImageBasedUpgradeReconciler) cleanupUnbootedStateroot(stateroot string) error {
	status, err := r.RPMOstreeClient.QueryStatus()
	if err != nil {
		return err
	}

	// since undeploy shifts the order, undeploy in the reverse order
	indicesToUndeploy := make([]int, 0)
	for i := len(status.Deployments) - 1; i >= 0; i-- {
		deployment := &status.Deployments[i]
		if deployment.OSName != stateroot {
			continue
		}
		if deployment.Booted {
			return fmt.Errorf("failed abort: deployment %d in stateroot %s is booted", i, stateroot)
		}
		indicesToUndeploy = append(indicesToUndeploy, i)
	}
	for _, idx := range indicesToUndeploy {
		if err := r.OstreeClient.Undeploy(idx); err != nil {
			return fmt.Errorf("failed to undeploy %s with index %d: %w", stateroot, idx, err)
		}
	}
	staterootPath := common.GetStaterootPath(stateroot)
	if _, err := osStat(common.PathOutsideChroot(staterootPath)); err != nil {
		return nil
	}
	if _, err := r.Executor.Execute("unshare", "-m", "/bin/sh", "-c",
		fmt.Sprintf("\"mount -o remount,rw /sysroot && rm -rf %s\"", staterootPath)); err != nil {
		return fmt.Errorf("removing stateroot %s failed: %w", stateroot, err)
	}
	return nil
}
