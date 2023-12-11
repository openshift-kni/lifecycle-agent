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
	"time"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/backuprestore"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/extramanifest"
	"github.com/openshift-kni/lifecycle-agent/internal/healthcheck"
	v1 "k8s.io/api/core/v1"

	lcautils "github.com/openshift-kni/lifecycle-agent/utils"
	ctrl "sigs.k8s.io/controller-runtime"
)

var (
	// todo: this value might need adjusting later
	defaultRebootTimeout = 60 * time.Minute
)

var isPrePivot = func(r *ImageBasedUpgradeReconciler, ibu *lcav1alpha1.ImageBasedUpgrade) (bool, error) {
	currentStaterootName, err := r.RPMOstreeClient.GetCurrentStaterootName()
	if err != nil {
		return false, err
	}
	r.Log.Info("stateroots", "current stateroot:", currentStaterootName, "desired stateroot", getDesiredStaterootName(ibu))
	return currentStaterootName != getDesiredStaterootName(ibu), nil
}

// handleUpgrade orchestrate main upgrade steps and update status as needed
func (r *ImageBasedUpgradeReconciler) handleUpgrade(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {
	r.Log.Info("Starting handleUpgrade")

	prePivot, err := isPrePivot(r, ibu)
	if err != nil {
		//todo: abort handler? e.g delete desired stateroot
		utils.SetUpgradeStatusFailed(ibu, err.Error())
		return doNotRequeue(), nil
	}

	// WARNING: the pod may not know if we are boot loop (for now)
	if prePivot {
		r.Log.Info("Starting pre pivot steps and will pivot to new stateroot with a reboot")
		return r.prePivot(ctx, ibu)
	} else {
		r.Log.Info("Pivot successful, starting post pivot steps")
		return r.postPivot(ctx, ibu)
	}
}

func (r *ImageBasedUpgradeReconciler) rebootToNewStateRoot() error {
	r.Log.Info("rebooting to a new stateroot")

	_, err := r.Executor.Execute("systemctl", "--message=\"Image Based Upgrade\"", "reboot")
	if err != nil {
		return err
	}

	r.Log.Info(fmt.Sprintf("Wait for %s to be killed via SIGTERM", defaultRebootTimeout.String()))
	time.Sleep(defaultRebootTimeout)

	return fmt.Errorf("failed to reboot. This should never happen! Please check the system")
}

// prePivot all the funcs needed to be called before a pivot
func (r *ImageBasedUpgradeReconciler) prePivot(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {
	// backup with OADP
	r.Log.Info("Handling backups with OADP operator")
	ctrlResult, err := r.handleBackup(ctx, ibu)
	if err != nil {
		if backuprestore.IsBRFailedError(err) {
			utils.SetUpgradeStatusFailed(ibu, err.Error())
			return doNotRequeue(), nil
		}
		if backuprestore.IsBRFailedValidationError(err) || backuprestore.IsBRNotFoundError(err) {
			utils.SetUpgradeStatusInProgress(ibu, err.Error())
			return requeueWithMediumInterval(), nil
		}
		return requeueWithError(err)
	}
	if !ctrlResult.IsZero() {
		return ctrlResult, nil
	}

	if _, err := r.Executor.Execute("mount", "/sysroot", "-o", "remount,rw"); err != nil {
		return requeueWithError(err)
	}
	stateRootRepo := fmt.Sprintf("/host/ostree/deploy/rhcos_%s/var", ibu.Spec.SeedImageRef.Version)

	r.Log.Info("Writing OadpConfiguration CRs into new stateroot")
	if err := r.BackupRestore.ExportOadpConfigurationToDir(ctx, stateRootRepo, backuprestore.OadpNs); err != nil {
		return requeueWithError(err)
	}

	r.Log.Info("Writing Restore CRs into new stateroot")
	if err := r.BackupRestore.ExportRestoresToDir(ctx, ibu.Spec.OADPContent, stateRootRepo); err != nil {
		if backuprestore.IsBRFailedValidationError(err) {
			utils.SetUpgradeStatusInProgress(ibu, err.Error())
			return requeueWithMediumInterval(), nil
		}
		return requeueWithError(err)
	}

	r.Log.Info("Writing extra-manifests into new stateroot")
	if err := r.ExtraManifest.ExportExtraManifestToDir(ctx, ibu.Spec.ExtraManifests, stateRootRepo); err != nil {
		return requeueWithError(err)
	}

	r.Log.Info("Writing cluster-configuration into new stateroot")
	if err := r.ClusterConfig.FetchClusterConfig(ctx, stateRootRepo); err != nil {
		return requeueWithError(err)
	}

	r.Log.Info("Writing lvm-configuration into new stateroot")
	if err := r.ClusterConfig.FetchLvmConfig(ctx, stateRootRepo); err != nil {
		return requeueWithError(err)
	}

	r.Log.Info("Save the IBU CR to the new state root before pivot")
	filePath := filepath.Join(stateRootRepo, utils.IBUFilePath)
	// Temporarily empty resource version so the file can be used to restore status
	rv := ibu.ResourceVersion
	ibu.ResourceVersion = ""
	if err := lcautils.MarshalToFile(ibu, filePath); err != nil {
		return requeueWithError(err)
	}
	ibu.ResourceVersion = rv

	// Write an event to indicate reboot attempt
	r.Recorder.Event(ibu, v1.EventTypeNormal, "Reboot", "System will now reboot")
	err = r.rebootToNewStateRoot()
	if err != nil {
		//todo: abort handler? e.g delete desired stateroot
		r.Log.Error(err, "")
		utils.SetUpgradeStatusFailed(ibu, err.Error())
		return doNotRequeue(), nil
	}
	return doNotRequeue(), nil
}

func (r *ImageBasedUpgradeReconciler) postPivot(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {
	r.Log.Info("Starting health check for different components")
	err := healthcheck.HealthChecks(r.Client, r.Log)
	if err != nil {
		utils.SetUpgradeStatusFailed(ibu, err.Error())
		return doNotRequeue(), nil
	}

	r.Log.Info("Applying extra manifests")
	err = r.ExtraManifest.ApplyExtraManifests(ctx, common.PathOutsideChroot(extramanifest.ExtraManifestPath))
	if err != nil {
		if extramanifest.IsEMFailedError(err) {
			utils.SetUpgradeStatusFailed(ibu, err.Error())
			return doNotRequeue(), nil
		}
		return requeueWithError(err)
	}

	r.Log.Info("Recovering OADP configuration")
	err = r.BackupRestore.RestoreOadpConfigurations(ctx)
	if err != nil {
		if backuprestore.IsBRStorageBackendUnavailableError(err) {
			utils.SetUpgradeStatusFailed(ibu, err.Error())
			return doNotRequeue(), nil
		}
		return requeueWithError(err)
	}

	r.Log.Info("Handling restores with OADP operator")
	result, err := r.handleRestore(ctx)
	if err != nil {
		// Restore failed
		if backuprestore.IsBRFailedError(err) {
			utils.SetUpgradeStatusFailed(ibu, err.Error())
			return doNotRequeue(), nil
		}
		return requeueWithError(err)
	}
	if !result.IsZero() {
		return result, nil
	}

	r.Log.Info("Done handleUpgrade")
	utils.SetUpgradeStatusCompleted(ibu)
	return doNotRequeue(), nil
}

// handleBackup manages backup flow and returns with possible requeue
func (r *ImageBasedUpgradeReconciler) handleBackup(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {
	sortedBackupGroups, err := r.BackupRestore.GetSortedBackupsFromConfigmap(ctx, ibu.Spec.OADPContent)
	if err != nil {
		return requeueWithError(err)
	}

	if len(sortedBackupGroups) == 0 {
		r.Log.Info("No backup requests, skipping")
		return doNotRequeue(), nil
	}

	// trigger and track each group
	for index, backups := range sortedBackupGroups {
		r.Log.Info("Processing backup", "groupIndex", index+1, "totalGroups", len(sortedBackupGroups))
		backupTracker, err := r.BackupRestore.StartOrTrackBackup(ctx, backups)
		if err != nil {
			return requeueWithError(err)
		}

		// The current backup group has done, work on the next group
		if len(backupTracker.SucceededBackups) == len(backups) {
			continue
		}

		// Backup CRs failed
		if len(backupTracker.FailedBackups) > 0 {
			errMsg := fmt.Sprintf("Failed backup CRs: %s", strings.Join(backupTracker.FailedBackups, ","))
			return requeueWithError(backuprestore.NewBRFailedError("Backup", errMsg))
		}

		// Backups are in progress
		if len(backupTracker.ProgressingBackups) > 0 {
			return requeueWithCustomInterval(5 * time.Second), nil
		}

		// Backups are waiting for condition
		return requeueWithMediumInterval(), nil
	}

	r.Log.Info("All backups succeeded")
	return doNotRequeue(), nil
}

func (r *ImageBasedUpgradeReconciler) handleRestore(ctx context.Context) (ctrl.Result, error) {
	// Load restore CRs from files
	sortedRestoreGroups, err := r.BackupRestore.LoadRestoresFromOadpRestorePath()
	if err != nil {
		return requeueWithError(err)
	}

	if len(sortedRestoreGroups) == 0 {
		r.Log.Info("No restore requests, skipping")
		return doNotRequeue(), nil
	}

	for index, restores := range sortedRestoreGroups {
		r.Log.Info("Processing restore", "groupIndex", index+1, "totalGroups", len(sortedRestoreGroups))
		restoreTracker, err := r.BackupRestore.StartOrTrackRestore(ctx, restores)
		if err != nil {
			return requeueWithError(err)
		}

		// The current restore group has done, work on the next group
		if len(restoreTracker.SucceededRestores) == len(restores) {
			continue
		}

		// Restore CRs failed
		if len(restoreTracker.FailedRestores) > 0 {
			errMsg := fmt.Sprintf("Failed restore CRs: %s", strings.Join(restoreTracker.FailedRestores, ","))
			return requeueWithError(backuprestore.NewBRFailedError("Restore", errMsg))
		}

		// Restores CRs are in progress
		if len(restoreTracker.ProgressingRestores) > 0 {
			return requeueWithShortInterval(), nil
		}

		// Restores are waiting for condition
		return requeueWithMediumInterval(), nil
	}

	r.Log.Info("All restores succeeded")
	if err := os.RemoveAll(common.PathOutsideChroot(backuprestore.OadpRestorePath)); err != nil {
		return requeueWithError(err)
	}
	r.Log.Info("OADP restore path removed", "path", backuprestore.OadpRestorePath)
	return doNotRequeue(), nil
}
