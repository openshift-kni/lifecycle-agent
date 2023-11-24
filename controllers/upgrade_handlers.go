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
	"strings"
	"time"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"path/filepath"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/backuprestore"

	lcautils "github.com/openshift-kni/lifecycle-agent/utils"

	ctrl "sigs.k8s.io/controller-runtime"
)

// simple custom error types to handle backup cases
type (
	BackupFailedError     string
	BackupValidationError string
)

func (b BackupFailedError) Error() string {
	return string(b)
}

func (b BackupValidationError) Error() string {
	return string(b)
}

// handleUpgrade orchestrate main upgrade steps and update status as needed
func (r *ImageBasedUpgradeReconciler) handleUpgrade(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {

	r.Log.Info("Starting handleUpgrade")

	// backup with OADP
	r.Log.Info("Backup with Oadp operator")
	reqBackup, backupCRs, err := r.BackupRestore.CheckIfBackupRequested(ctx, ibu.Spec.OADPContent)
	if err != nil {
		if k8serrors.IsInvalid(err) {
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.GetInProgressConditionType(lcav1alpha1.Stages.Upgrade),
				utils.ConditionReasons.InProgress,
				metav1.ConditionTrue,
				fmt.Sprintf("Invalid backup resource detected in configMap, please update"),
				ibu.Generation)
			return requeueWithShortInterval(), nil
		}
		return requeueWithError(err)
	}
	if reqBackup {
		r.Log.Info("Backup requested")
		ctrlResult, err := r.startBackup(ctx, backupCRs)
		if err != nil {
			var (
				backupFailedError     *BackupFailedError
				backupValidationError *BackupValidationError
			)
			switch {
			case errors.As(err, &backupFailedError):
				utils.SetStatusCondition(&ibu.Status.Conditions,
					utils.GetInProgressConditionType(lcav1alpha1.Stages.Upgrade),
					utils.ConditionReasons.Failed,
					metav1.ConditionFalse,
					backupFailedError.Error(),
					ibu.Generation)
				utils.SetStatusCondition(&ibu.Status.Conditions,
					utils.GetCompletedConditionType(lcav1alpha1.Stages.Upgrade),
					utils.ConditionReasons.Failed,
					metav1.ConditionFalse,
					"Upgrade failed",
					ibu.Generation)
				return ctrlResult, nil
			case errors.As(err, &backupValidationError):
				utils.SetStatusCondition(&ibu.Status.Conditions,
					utils.GetInProgressConditionType(lcav1alpha1.Stages.Upgrade),
					utils.ConditionReasons.InProgress,
					metav1.ConditionTrue,
					backupValidationError.Error(),
					ibu.Generation)
				return ctrlResult, nil
			default:
				return requeueWithError(err)
			}
		}
		if !ctrlResult.IsZero() {
			return ctrlResult, nil
		}
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
		if k8serrors.IsInvalid(err) {
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.GetInProgressConditionType(lcav1alpha1.Stages.Upgrade),
				utils.ConditionReasons.InProgress,
				metav1.ConditionTrue,
				fmt.Sprintf("Invalid restore resource detected in configMap, please update"),
				ibu.Generation)
			return requeueWithShortInterval(), nil
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

	r.Log.Info("Writing network-configuration into new stateroot")
	if err := r.NetworkConfig.FetchNetworkConfig(ctx, stateRootRepo); err != nil {
		return requeueWithError(err)
	}

	// Save the IBU CR to the new state root before pivot
	filePath := filepath.Join(stateRootRepo, utils.IBUFilePath)
	// Temporarily empty resource version so the file can be used to restore status
	rv := ibu.ResourceVersion
	ibu.ResourceVersion = ""
	if err := lcautils.WriteToFile(ibu, filePath); err != nil {
		return doNotRequeue(), err
	}
	ibu.ResourceVersion = rv
	// TODO: Pivot to new stateroot

	// TODO: Post-pivot steps
	//
	// err = r.ExtraManifest.ApplyExtraManifestsFromDir(ctx, stateRootRepo)
	// if err != nil {
	// 	 r.Log.Error(err, "Failed to apply extra manifests")
	//	 return ctrl.Result{}, err
	// }
	r.Log.Info("Done handleUpgrade")

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

// startBackup manages backup flow and returns with possible requeue
func (r *ImageBasedUpgradeReconciler) startBackup(ctx context.Context, backupCRs []*velerov1.Backup) (ctrl.Result, error) {
	// sort backupCRs by wave-apply
	sortedBackupGroups, err := backuprestore.SortByApplyWaveBackupCrs(backupCRs)
	if err != nil {
		return requeueWithError(err)
	}

	// trigger and track each group
	for _, backups := range sortedBackupGroups {
		backupTracker, err := r.BackupRestore.TriggerBackup(ctx, backups)
		if err != nil {
			return requeueWithError(err)
		}
		if len(backupTracker.SucceededBackups) == len(backups) {
			// The current backup group has done, work on the next group
			continue
		} else if len(backupTracker.FailedBackups) != 0 {
			// not recoverable
			msg := fmt.Sprintf("Failed backups: %s", strings.Join(backupTracker.FailedBackups, ","))
			err := BackupFailedError(msg)
			return doNotRequeue(), &err
		} else if len(backupTracker.FailedValidationBackups) != 0 {
			msg := fmt.Sprintf("Failed validation backups reported by Oadp: %s. %s", strings.Join(backupTracker.FailedValidationBackups, ","), "Please update the invalid backups.")
			err := BackupValidationError(msg)
			return requeueWithShortInterval(), &err
		} else if len(backupTracker.ProgressingBackups) != 0 {
			r.Log.Info(fmt.Sprintf("Inprogress backups: %s", strings.Join(backupTracker.ProgressingBackups, ",")))
			return requeueWithCustomInterval(2 * time.Second), nil
		} else {
			// Backup doesn't have any status, it's likely
			// that the object storage backend is not available.
			// Requeue to wait for the object storage backend
			// to be ready.
			r.Log.Info(fmt.Sprintf("Pending backups: %s. %s", strings.Join(backupTracker.PendingBackups, ","), "Wait for object storage backend to be available."))
			return requeueWithMediumInterval(), nil
		}
	}

	return doNotRequeue(), nil
}
