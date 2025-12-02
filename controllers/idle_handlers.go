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

	"github.com/openshift-kni/lifecycle-agent/internal/prep"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/internal/extramanifest"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"

	"github.com/openshift-kni/lifecycle-agent/internal/healthcheck"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	ctrl "sigs.k8s.io/controller-runtime"
)

var osStat = os.Stat
var osReadDir = os.ReadDir
var osRemoveAll = os.RemoveAll

func (r *ImageBasedUpgradeReconciler) resetStatusFields(ibu *ibuv1.ImageBasedUpgrade) {
	ibu.Status.RollbackAvailabilityExpiration.Reset()
	utils.ResetStatusConditions(&ibu.Status.Conditions, ibu.Generation)
}

//nolint:unparam
func (r *ImageBasedUpgradeReconciler) handleAbort(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) (ctrl.Result, error) {
	r.Log.Info("Starting handleAbort")

	if successful, errMsg := r.cleanup(ctx, ibu); successful {
		r.Log.Info("Finished handleAbort successfully")
		r.resetStatusFields(ibu)
		return doNotRequeue(), nil
	} else {
		utils.SetStatusCondition(&ibu.Status.Conditions,
			utils.ConditionTypes.Idle,
			utils.ConditionReasons.AbortFailed,
			metav1.ConditionFalse,
			errMsg+fmt.Sprintf("Perform cleanup manually then add '%s' annotation to ibu CR to transition back to Idle",
				utils.ManualCleanupAnnotation),
			ibu.Generation,
		)
	}
	return requeueWithLongInterval(), nil
}

func (r *ImageBasedUpgradeReconciler) handleFinalizeFailure(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) (ctrl.Result, error) {
	if done, err := r.checkManualCleanup(ctx, ibu); err != nil {
		return requeueWithShortInterval(), err
	} else if done {
		r.Log.Info("Manual cleanup annotation is found, removed annotation and running handleFinalize again for verification")
		return r.handleFinalize(ctx, ibu)
	}
	r.Log.Info("Manual cleanup annotation is not set, requeue again")
	return requeueWithLongInterval(), nil
}

func (r *ImageBasedUpgradeReconciler) handleAbortFailure(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) (ctrl.Result, error) {
	if done, err := r.checkManualCleanup(ctx, ibu); err != nil {
		return requeueWithShortInterval(), err
	} else if done {
		r.Log.Info("Manual cleanup annotation is found, removed annotation and running handleAbort again for verification")
		return r.handleAbort(ctx, ibu)
	}
	r.Log.Info("Manual cleanup annotation is not set, requeue again")
	return requeueWithLongInterval(), nil
}

// checkManualCleanup looks for ManualCleanupAnnotation in the ibu CR, if it is present removes the annotation and returns true
// if it is not present returns false
func (r *ImageBasedUpgradeReconciler) checkManualCleanup(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) (bool, error) {
	if _, ok := ibu.Annotations[utils.ManualCleanupAnnotation]; ok {
		delete(ibu.Annotations, utils.ManualCleanupAnnotation)
		if err := r.Client.Update(ctx, ibu); err != nil {
			return false, fmt.Errorf("failed to remove manual cleanup annotation from ibu: %w", err)
		}
		return true, nil
	}
	return false, nil
}

func (r *ImageBasedUpgradeReconciler) handleFinalize(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) (ctrl.Result, error) {
	r.Log.Info("Starting handleFinalize")

	r.Log.Info("Running health check for finalize (Idle) stage")
	if err := healthcheck.HealthChecks(ctx, r.NoncachedClient, r.Log); err != nil {
		msg := fmt.Sprintf("Waiting for system to stabilize before finalize (idle) stage can continue: %s", err.Error())
		r.Log.Info(msg)
		utils.SetStatusCondition(&ibu.Status.Conditions,
			utils.ConditionTypes.Idle,
			utils.ConditionReasons.Finalizing,
			metav1.ConditionFalse,
			msg,
			ibu.Generation,
		)
		return requeueWithHealthCheckInterval(), nil
	}

	if successful, errMsg := r.cleanup(ctx, ibu); successful {
		r.Log.Info("Finished handleFinalize successfully")
		r.resetStatusFields(ibu)
		return doNotRequeue(), nil
	} else {
		utils.SetStatusCondition(&ibu.Status.Conditions,
			utils.ConditionTypes.Idle,
			utils.ConditionReasons.FinalizeFailed,
			metav1.ConditionFalse,
			errMsg+fmt.Sprintf("Perform cleanup manually then add '%s' annotation to ibu CR to transition back to Idle",
				utils.ManualCleanupAnnotation),
			ibu.Generation,
		)
	}
	return requeueWithLongInterval(), nil
}

// cleanup cleans stateroots, precache, backup, ibu files
// returns true if all cleanup tasks were successful
func (r *ImageBasedUpgradeReconciler) cleanup(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) (bool, string) {
	// try to clean up as much as possible and avoid returning when one of the cleanup tasks fails
	// successful means that all the cleanup tasks completed without any error
	successful := true
	errorMessage := ""

	var handleError = func(err error, msg string) {
		successful = false
		r.Log.Error(err, msg)
		errorMessage += err.Error() + " "
	}

	r.Log.Info("Cleaning up stateroot")
	if err := r.cleanupStateroot(ctx); err != nil {
		handleError(err, "failed to cleanup stateroot")
	}

	r.Log.Info("Cleaning up precache")
	if err := r.Precache.Cleanup(ctx); err != nil {
		handleError(err, "failed to cleanup precaching resources.")
	}

	r.Log.Info("Removing annotation with warning")
	if err := extramanifest.RemoveAnnotationEMWarningValidation(r.Client, r.Log, ibu); err != nil {
		handleError(err, "failed to remove extra manifest warning annotation from IBU")
	}

	r.Log.Info("Cleaning up OADP resources")
	if err := r.cleanupOADPResources(ctx); err != nil {
		handleError(err, "failed to cleanup OADP resources")
	}

	r.Log.Info("Cleaning up IBU files")
	if err := cleanupIBUFiles(); err != nil {
		handleError(err, "failed to cleanup ibu files.")
	}

	return successful, errorMessage
}

// cleanupOADPResources clean resources from backup/restore as long as OADP is present
func (r *ImageBasedUpgradeReconciler) cleanupOADPResources(ctx context.Context) error {
	if !r.BackupRestore.IsOadpInstalled(ctx) {
		r.Log.Info("OADP not installed, nothing to cleanup")
		return nil
	}

	r.Log.Info("Cleaning up DeleteBackupRequest")
	if err := r.BackupRestore.CleanupDeleteBackupRequests(ctx); err != nil {
		return fmt.Errorf("failed to cleanup DeleteBackupRequest CRs: %w", err)
	}

	r.Log.Info("Cleaning up Backup")
	if err := r.BackupRestore.CleanupBackups(ctx); err != nil {
		return fmt.Errorf("failed to cleanup backups: %w", err)
	}

	r.Log.Info("Restoring PV reclaim policy")
	if err := r.BackupRestore.RestorePVsReclaimPolicy(ctx); err != nil {
		return fmt.Errorf("failed to restore persistentVolumeReclaimPolicy in PVs created by LVMS: %w", err)
	}

	r.Log.Info("Successfully cleaned all resources related to backup and restore (OADP)")
	return nil
}

func (r *ImageBasedUpgradeReconciler) cleanupStateroot(ctx context.Context) error {
	r.Log.Info("Cleaning up cluster stateroot resources")
	if err := prep.DeleteStaterootSetupJob(ctx, r.Client, r.Log); err != nil {
		return fmt.Errorf("failed to cleanup cluster stateroot resources: %w", err)
	}

	r.Log.Info("Cleaning up unbooted stateroot resources")
	if err := CleanupUnbootedStateroots(r.Log, r.Ops, r.OstreeClient, r.RPMOstreeClient); err != nil {
		return fmt.Errorf("failed to clean up host stateroot resources: %w", err)
	}

	r.Log.Info("Successfully cleaned all resources related to stateroot setup")
	return nil
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

func CleanupUnbootedStateroots(log logr.Logger, ops ops.Ops, ostreeClient ostreeclient.IClient, rpmOstreeClient rpmostreeclient.IClient) error {
	status, err := rpmOstreeClient.QueryStatus()
	if err != nil {
		return fmt.Errorf("failed to query status with rpmostree: %w", err)
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
		if err := cleanupUnbootedStateroot(stateroot, ops, ostreeClient, rpmOstreeClient); err != nil {
			log.Error(err, "failed to remove stateroot", "stateroot", stateroot)
			failures += 1
		}
	}

	// clear temporary files and reclaim disk space
	if err := rpmOstreeClient.RpmOstreeCleanup(); err != nil {
		log.Error(err, "failed rpm-ostree cleanup -b")
		failures += 1
	}

	// remove stateroots that are not listed in rpm-ostree, e.g failed deployments
	files, err := osReadDir(getStaterootPath(""))
	if err != nil {
		return fmt.Errorf("failed to list stateroots: %w", err)
	}
	for _, fileInfo := range files {
		if fileInfo.IsDir() {
			if fileInfo.Name() == bootedStateroot {
				continue
			}
			err := osRemoveAll(getStaterootPath(fileInfo.Name()))
			if err != nil {
				log.Error(err, "failed to remove undeployed stateroot", "stateroot", fileInfo.Name())
				failures += 1
			}
		}
	}

	if failures == 0 {
		log.Info("Unbooted stateroot cleanup completed successfully")
		return nil
	}
	return fmt.Errorf("failed to remove %d stateroots", failures)
}

func cleanupUnbootedStateroot(stateroot string, ops ops.Ops, ostreeClient ostreeclient.IClient, rpmOstreeClient rpmostreeclient.IClient) error {
	status, err := rpmOstreeClient.QueryStatus()
	if err != nil {
		return fmt.Errorf("failed to query status with rpmostree during stateroot cleanup: %w", err)
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
		if err := ostreeClient.Undeploy(idx); err != nil {
			return fmt.Errorf("failed to undeploy %s with index %d: %w", stateroot, idx, err)
		}
	}
	staterootPath := common.GetStaterootPath(stateroot)
	if _, err := osStat(common.PathOutsideChroot(staterootPath)); err != nil {
		return nil
	}
	if _, err := ops.RunBashInHostNamespace("unshare", "-m", "/bin/sh", "-c",
		fmt.Sprintf("\"mount -o remount,rw /sysroot && rm -rf %s\"", staterootPath)); err != nil {
		return fmt.Errorf("removing stateroot %s failed: %w", stateroot, err)
	}

	return nil
}
