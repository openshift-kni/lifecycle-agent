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
	"path/filepath"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
	"github.com/openshift-kni/lifecycle-agent/internal/backuprestore"
	"github.com/openshift-kni/lifecycle-agent/internal/common"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	ctrl "sigs.k8s.io/controller-runtime"
)

func (r *ImageBasedUpgradeReconciler) handleAbort(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {
	r.Log.Info("Starting handleAbort")

	if r.cleanup(ctx, false, ibu) {
		r.Log.Info("Finished handleAbort")
		return doNotRequeue(), nil
	}
	return requeueWithError(fmt.Errorf("failed handelAbort"))
}

//nolint:unparam
func (r *ImageBasedUpgradeReconciler) handleAbortFailure(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {
	return doNotRequeue(), nil
}

func (r *ImageBasedUpgradeReconciler) handleFinalize(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {
	r.Log.Info("Starting handleFinalize")

	if r.cleanup(ctx, true, ibu) {
		r.Log.Info("Finished handleFinalize")
		return doNotRequeue(), nil
	}
	return requeueWithError(fmt.Errorf("failed handleFinalize"))
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
	ctx context.Context, allUnbootedStateroots bool, ibu *lcav1alpha1.ImageBasedUpgrade) bool {
	// try to clean up as much as possible and avoid returning when one of the cleanup tasks fails
	// successful means that all the cleanup tasks completed without any error
	successful := true

	if err := r.cleanupStateroots(allUnbootedStateroots, ibu); err != nil {
		successful = false
		r.Log.Error(err, "failed to cleanup stateroots")
	}
	if err := r.Precache.Cleanup(ctx); err != nil {
		successful = false
		r.Log.Error(err, "failed to cleanup precaching resources")
	}
	if allRemoved, err := r.BackupRestore.CleanupBackups(ctx); err != nil {
		successful = false
		r.Log.Error(err, "failed to cleanup backups")
	} else if !allRemoved {
		successful = false
		r.Log.Error(errors.New("failed to delete all the backup CRs"), "")
	}
	if err := r.BackupRestore.DeleteOadpOperator(ctx, backuprestore.OadpNs); err != nil {
		successful = false
		r.Log.Error(err, "failed to delete OADP operator")
	}
	if err := cleanupIBUFiles(); err != nil {
		successful = false
		r.Log.Error(err, "failed to cleanup ibu files")
	}

	return successful
}

// cleanupStateroot cleans all unbooted stateroots or desired stateroot
// depending on allUnbootedStateroots argumennt
func (r *ImageBasedUpgradeReconciler) cleanupStateroots(
	allUnbootedStateroots bool, ibu *lcav1alpha1.ImageBasedUpgrade) error {
	if allUnbootedStateroots {
		return r.cleanupUnbootedStateroots()
	}
	return r.cleanupUnbootedStateroot(getDesiredStaterootName(ibu))
}

func cleanupIBUFiles() error {
	if _, err := os.Stat(common.PathOutsideChroot(utils.IBUWorkspacePath)); err != nil {
		return nil
	}
	if err := os.RemoveAll(filepath.Join(utils.Host, utils.IBUWorkspacePath)); err != nil {
		return fmt.Errorf("removing %s failed: %w", utils.IBUWorkspacePath, err)
	}
	return nil
}

func (r *ImageBasedUpgradeReconciler) cleanupUnbootedStateroots() error {
	status, err := r.RPMOstreeClient.QueryStatus()
	if err != nil {
		return err
	}

	undeployIndices := make([]int, 0)
	undeployStateroots := make([]string, 0)
	for index, deployment := range status.Deployments {
		if deployment.Booted {
			continue
		}
		undeployIndices = append(undeployIndices, index)
		undeployStateroots = append(undeployStateroots, deployment.OSName)
	}
	// since undeploy shifts the order, undeploy in the reverse order
	for i := len(undeployIndices) - 1; i >= 0; i-- {
		_, err = r.Executor.Execute("ostree", "admin", "undeploy", fmt.Sprint(i))
		if err != nil {
			return fmt.Errorf("ostree undeploy %d failed: %w", i, err)
		}
	}

	failures := 0
	for _, stateroot := range undeployStateroots {
		if err = removeStateroot(r.Executor, stateroot); err != nil {
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

	undeployIndices := make([]int, 0)
	for index, deployment := range status.Deployments {
		if deployment.OSName != stateroot {
			continue
		}
		// make sure none of the deployments in the stateroot is booted
		if deployment.Booted {
			return fmt.Errorf("failed abort: deployment %d in stateroot %s is booted", index, stateroot)
		}
		undeployIndices = append(undeployIndices, index)
	}
	// since undeploy shifts the order, undeploy in the reverse order
	for i := len(undeployIndices) - 1; i >= 0; i-- {
		_, err = r.Executor.Execute("ostree", "admin", "undeploy", fmt.Sprint(i))
		if err != nil {
			return fmt.Errorf("ostree undeploy %d failed: %w", i, err)
		}
	}

	return removeStateroot(r.Executor, stateroot)
}

func removeStateroot(executor ops.Execute, stateroot string) error {
	staterootPath := fmt.Sprintf("/sysroot/ostree/deploy/%s", stateroot)
	if _, err := os.Stat(common.PathOutsideChroot(staterootPath)); err != nil {
		return nil
	}

	if _, err := executor.Execute("unshare", "-m", "/bin/sh", "-c",
		fmt.Sprintf("\"mount -o remount,rw /sysroot && rm -rf %s\"", staterootPath)); err != nil {
		return fmt.Errorf("removing stateroot %s failed: %w", stateroot, err)
	}
	return nil
}
