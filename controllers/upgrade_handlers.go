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

	v1 "k8s.io/api/core/v1"

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

var (
	// todo: this value might need adjusting later
	defaultRebootTimeout = 60 * time.Minute
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
		upgradeFailedStatus(ibu, err.Error())
		return doNotRequeue(), nil
	}

	// WARNING: the pod may not know if we are boot loop (for now)
	if prePivot {
		r.Log.Info("Starting pre pivot steps and will pivot to new stateroot with a reboot")
		ctrlResult, err := r.prePivot(ctx, ibu)
		if err != nil {
			//todo: abort handler? e.g delete desired stateroot
			upgradeFailedStatus(ibu, err.Error())
			return doNotRequeue(), nil
		}

		// prePivot requested a requeue
		if ctrlResult != doNotRequeue() {
			return ctrlResult, nil
		}

		// Write an event to indicate reboot attempt
		r.Recorder.Event(ibu, v1.EventTypeNormal, "Reboot", "System wil now reboot")
		err = r.rebootToNewStateRoot()
		if err != nil {
			//todo: abort handler? e.g delete desired stateroot
			r.Log.Error(err, "")
			upgradeFailedStatus(ibu, err.Error())
			return doNotRequeue(), nil
		}
	} else {
		r.Log.Info("Pivot successful, starting post pivot steps")
		// TODO: Post-pivot steps
		/*
				1. oadp restore
				    - platform
				    - acm recovery

			   2. ApplyExtraManifestsFromDir

			   3. health check? readiness check /readyz endpoint?
					- mcp
					- clusteroperator
					- all csv

				4. restore application manifests
		*/

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

	return doNotRequeue(), fmt.Errorf("something we went wrong. upgrade failed")
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
				upgradeFailedStatus(ibu, backupFailedError.Error())
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

	r.Log.Info("PrePivot done")
	return ctrl.Result{}, nil
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

// upgradeFailedStatus call this when upgrade fails
func upgradeFailedStatus(ibu *lcav1alpha1.ImageBasedUpgrade, msg string) {
	utils.SetStatusCondition(&ibu.Status.Conditions,
		utils.GetInProgressConditionType(lcav1alpha1.Stages.Upgrade),
		utils.ConditionReasons.Failed,
		metav1.ConditionFalse,
		msg,
		ibu.Generation)
	utils.SetStatusCondition(&ibu.Status.Conditions,
		utils.GetCompletedConditionType(lcav1alpha1.Stages.Upgrade),
		utils.ConditionReasons.Failed,
		metav1.ConditionFalse,
		"Upgrade failed",
		ibu.Generation)
}
