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

	"github.com/go-logr/logr"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/backuprestore"
	"github.com/openshift-kni/lifecycle-agent/internal/clusterconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/extramanifest"
	"github.com/openshift-kni/lifecycle-agent/internal/healthcheck"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	lcautils "github.com/openshift-kni/lifecycle-agent/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type (
	UpgradeHandler interface {
		HandleBackup(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) (ctrl.Result, error)
		HandleRestore(ctx context.Context) (ctrl.Result, error)
		PostPivot(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) (ctrl.Result, error)
		PrePivot(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) (ctrl.Result, error)
	}

	UpgHandler struct {
		client.Client
		NoncachedClient client.Reader
		Log             logr.Logger
		BackupRestore   backuprestore.BackuperRestorer
		ExtraManifest   extramanifest.EManifestHandler
		ClusterConfig   clusterconfig.UpgradeClusterConfigGatherer
		Executor        ops.Execute
		Ops             ops.Ops
		Recorder        record.EventRecorder
		RPMOstreeClient rpmostreeclient.IClient
		OstreeClient    ostreeclient.IClient
		RebootClient    reboot.RebootIntf
	}
)

// Used to start and end phases in the Upgrade stage. Each of them must be used in exactly two places
var (
	UpgradePhasePrepivot  = "PrePivot"
	UpgradePhasePostpivot = "PostPivot"
)

// handleUpgrade orchestrate main upgrade steps and update status as needed
func (r *ImageBasedUpgradeReconciler) handleUpgrade(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) (ctrl.Result, error) {
	r.Log.Info("Starting handleUpgrade")

	origStaterootBooted, err := r.RebootClient.IsOrigStaterootBooted(ibu.Spec.SeedImageRef.Version)

	if err != nil {
		utils.SetUpgradeStatusFailed(ibu, err.Error())
		return doNotRequeue(), nil
	}

	if origStaterootBooted {
		r.Log.Info("Running PrePivot handler")
		prePivot, err := r.UpgradeHandler.PrePivot(ctx, ibu)
		if err != nil {
			return prePivot, fmt.Errorf("failed to run pre pivots without errors: %w", err)
		}
		return prePivot, nil
	} else {
		if ibu.Status.RollbackAvailabilityExpiration.IsZero() {
			// Set the rollback availability expiration field
			if expiry, err := r.getRollbackAvailabilityExpiration(); err == nil {
				ibu.Status.RollbackAvailabilityExpiration.Time = expiry
			} else {
				r.Log.Error(err, "unable to determine rollback availability expiration")
			}
		}

		r.Log.Info("Running PostPivot handler")
		postPivot, err := r.UpgradeHandler.PostPivot(ctx, ibu)
		if err != nil {
			return postPivot, fmt.Errorf("failed to run post pivot without errors: %w", err)
		}
		return postPivot, nil
	}
}

func (u *UpgHandler) resetProgressMessage(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) {
	// Clear any error status that may have been set
	utils.SetUpgradeStatusInProgress(ibu, utils.InProgress)
	if updateErr := utils.UpdateIBUStatus(ctx, u.Client, ibu); updateErr != nil {
		u.Log.Error(updateErr, "failed to update IBU CR status")
	}
}

// prePivot executes all the pre-upgrade steps and initiates a cluster reboot.
//
// Note: All decisions, including reconciles and failures, should be made within this function.
// The caller will simply return what this function returns.
func (u *UpgHandler) PrePivot(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) (ctrl.Result, error) {
	// start pre-pivot phase timer
	utils.StartPhase(u.Client, u.Log, ibu, UpgradePhasePrepivot)

	if prog := utils.GetInProgressCondition(ibu, ibuv1.Stages.Upgrade); prog == nil {
		// Set in-progress status
		u.resetProgressMessage(ctx, ibu)
	}

	u.Log.Info("Running health check for Upgrade (pre-pivot)")
	if err := CheckHealth(ctx, u.NoncachedClient, u.Log); err != nil {
		msg := fmt.Sprintf("Waiting for system to stabilize before Upgrade (pre-pivot) stage can continue: %s", err.Error())
		u.Log.Info(msg)
		utils.SetUpgradeStatusInProgress(ibu, msg)
		return requeueWithHealthCheckInterval(), nil
	}

	utils.SetUpgradeStatusInProgress(ibu, "Backing up Application Data")
	if updateErr := utils.UpdateIBUStatus(ctx, u.Client, ibu); updateErr != nil {
		u.Log.Error(updateErr, "failed to update IBU CR status")
	}

	u.Log.Info("Handling backups with OADP operator")
	ctrlResult, err := u.HandleBackup(ctx, ibu)
	if err != nil {
		if backuprestore.IsBRFailedValidationError(err) ||
			backuprestore.IsBRFailedError(err) {

			u.Log.Error(err, "Failed to handle backups")
			utils.SetUpgradeStatusFailed(ibu, err.Error())
			return doNotRequeue(), nil
		}
		return requeueWithError(fmt.Errorf("error while handling backup: %w", err))
	}
	if !ctrlResult.IsZero() {
		// The backup process has not been completed yet, requeue
		utils.SetUpgradeStatusInProgress(ibu, "Backup of Application Data is in progress")
		return ctrlResult, nil
	}

	u.Log.Info("Remounting sysroot")
	if err := u.Ops.RemountSysroot(); err != nil {
		return requeueWithError(fmt.Errorf("error while remounting sysroot: %w", err))
	}

	stateroot := common.GetDesiredStaterootName(ibu)
	staterootPath := getStaterootPath(stateroot)
	staterootVarPath := getStaterootVarPath(stateroot)

	utils.SetUpgradeStatusInProgress(ibu, "Exporting Application Configuration")
	if updateErr := utils.UpdateIBUStatus(ctx, u.Client, ibu); updateErr != nil {
		u.Log.Error(updateErr, "failed to update IBU CR status")
	}

	if err := u.exportOadpConfigurationAndRestore(ctx, ibu, staterootVarPath); err != nil {
		if backuprestore.IsBRFailedError(err) || backuprestore.IsBRFailedValidationError(err) {
			u.Log.Error(err, "Failed to export OADP configuration and restores")
			utils.SetUpgradeStatusFailed(ibu, err.Error())
			return doNotRequeue(), nil
		}
		return requeueWithError(fmt.Errorf("error while exporting OADP configuration and restores: %w", err))
	}

	utils.SetUpgradeStatusInProgress(ibu, "Exporting Policy and Config Manifests")
	if updateErr := utils.UpdateIBUStatus(ctx, u.Client, ibu); updateErr != nil {
		u.Log.Error(updateErr, "failed to update IBU CR status")
	}

	u.Log.Info("Writing extra-manifests into new stateroot")
	if err := u.extractAndExportExtraManifests(ctx, ibu, staterootVarPath); err != nil {
		if extramanifest.IsEMFailedError(err) {
			u.Log.Error(err, "Failed to export manifests")
			utils.SetUpgradeStatusFailed(ibu, err.Error())
			return doNotRequeue(), nil
		}
		return requeueWithError(fmt.Errorf("error while exporting manifests: %w", err))
	}

	utils.SetUpgradeStatusInProgress(ibu, "Exporting Cluster and LVM configuration")
	if updateErr := utils.UpdateIBUStatus(ctx, u.Client, ibu); updateErr != nil {
		u.Log.Error(updateErr, "failed to update IBU CR status")
	}

	u.Log.Info("Writing cluster-configuration into new stateroot")
	if err := u.ClusterConfig.FetchClusterConfig(ctx, staterootVarPath); err != nil {
		return requeueWithError(fmt.Errorf("error while fetching cluster configuration: %w", err))
	}

	u.Log.Info("Writing lvm-configuration into new stateroot")
	if err := u.ClusterConfig.FetchLvmConfig(ctx, staterootVarPath); err != nil {
		return requeueWithError(fmt.Errorf("error while fetching LVM configuration: %w", err))
	}

	// Clear any error status that may have been previously set
	u.resetProgressMessage(ctx, ibu)

	// close pre-pivot phase timer
	utils.StopPhase(u.Client, u.Log, ibu, UpgradePhasePrepivot)

	u.Log.Info("Save the IBU CR to the new state root before pivot")
	if err := exportIBUToNewStateroot(ibu, staterootPath); err != nil {
		return requeueWithError(fmt.Errorf("error while exporting IBU CR to the new state root: %w", err))
	}

	u.Log.Info("Save a copy of the IBU in the current stateroot for rollback")
	if err := exportForUncontrolledRollback(ibu); err != nil {
		return requeueWithError(fmt.Errorf("error while exporting for uncontrolled rollback: %w", err))
	}

	if err := u.setDefaultDeploymentToNewStateroot(stateroot); err != nil {
		return requeueWithError(fmt.Errorf("error while setting default deployment: %w", err))
	}

	// Write an event to indicate reboot attempt
	u.Recorder.Event(ibu, v1.EventTypeNormal, "Reboot", "System will now reboot for upgrade")
	err = u.RebootClient.RebootToNewStateRoot("upgrade")
	if err != nil {
		u.Log.Error(err, "Failed to reboot to new stateroot")
		utils.SetUpgradeStatusFailed(ibu, err.Error())
		return doNotRequeue(), nil
	}
	return doNotRequeue(), nil
}

// exportOadpConfigurationAndRestore exports OADP configuration and restore CRs to the new stateroot
func (u *UpgHandler) exportOadpConfigurationAndRestore(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade, ostreeVarDir string) error {
	if len(ibu.Spec.OADPContent) == 0 {
		u.Log.Info("spec.oadpContent is empty. Skipping exporting OADP configuration and restore CRs")
		return nil
	}

	u.Log.Info("Writing OadpConfiguration CRs into new stateroot")
	if err := u.BackupRestore.ExportOadpConfigurationToDir(ctx, ostreeVarDir, backuprestore.OadpNs); err != nil {
		return fmt.Errorf("failed to export OADP configuration: %w", err)
	}

	u.Log.Info("Writing Restore CRs into new stateroot")
	if err := u.BackupRestore.ExportRestoresToDir(ctx, ibu.Spec.OADPContent, ostreeVarDir); err != nil {
		return fmt.Errorf("failed to export restores: %w", err)
	}

	return nil
}

// extractAndExportExtraManifests extracts extra manifest from policies and/or configmaps and export them to the new stateroot
func (u *UpgHandler) extractAndExportExtraManifests(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade, ostreeVarDir string) error {
	var validationAnns = map[string]string{}
	if count, exists := ibu.GetAnnotations()[extramanifest.TargetOcpVersionManifestCountAnnotation]; exists {
		validationAnns[extramanifest.TargetOcpVersionManifestCountAnnotation] = count
	}

	versions, err := extramanifest.GetMatchingTargetOcpVersionLabelVersions(ibu.Spec.SeedImageRef.Version)
	if err != nil {
		return fmt.Errorf("failed to export manifests from policies: %w", err)
	}

	// Extract from policies can be done by matching labels on the policy or the CR itself
	// Currently we expect user to properly label CRs with site specific content
	// as those policies must not be applied on the seed
	labels := map[string]string{extramanifest.TargetOcpVersionLabel: strings.Join(versions, ",")}
	if err := u.ExtraManifest.ExtractAndExportManifestFromPoliciesToDir(ctx, nil, labels, validationAnns, ostreeVarDir); err != nil {
		return fmt.Errorf("failed to export manifests from policies: %w", err)
	}

	if err := u.ExtraManifest.ExportExtraManifestToDir(ctx, ibu.Spec.ExtraManifests, ostreeVarDir); err != nil {
		return fmt.Errorf("failed to export manifests from configmaps: %w", err)
	}
	return nil
}

func (u *UpgHandler) setDefaultDeploymentToNewStateroot(stateroot string) error {
	// Set the new default deployment
	if u.OstreeClient.IsOstreeAdminSetDefaultFeatureEnabled() {
		deploymentIndex, err := u.RPMOstreeClient.GetDeploymentIndex(stateroot)
		if err != nil {
			return fmt.Errorf("failed to get deployment index for stateroot %s: %w", stateroot, err)
		}
		if err := u.OstreeClient.SetDefaultDeployment(deploymentIndex); err != nil {
			return fmt.Errorf("failed to set default deployment at index %d: %w", deploymentIndex, err)
		}
	}
	return nil
}

func exportIBUToNewStateroot(ibu *ibuv1.ImageBasedUpgrade, staterootPath string) error {
	lcaConfigDir := filepath.Join(staterootPath, common.LCAConfigDir)
	if err := os.MkdirAll(lcaConfigDir, 0o700); err != nil {
		return fmt.Errorf("failed to mkdir: %w", err)
	}

	filePath := filepath.Join(staterootPath, utils.IBUFilePath)
	if err := lcautils.MarshalToFile(ibu, filePath); err != nil {
		return fmt.Errorf("error while saving IBU CR to the new state root: %w", err)
	}
	return nil
}

// exportForUncontrolledRollback Save a copy of the IBU in the current stateroot in case of uncontrolled rollback, with Upgrade set to failed
var ibuPreStaterootPath = common.PathOutsideChroot(utils.IBUFilePath)

func exportForUncontrolledRollback(ibu *ibuv1.ImageBasedUpgrade) error {
	ibuCopy := ibu.DeepCopy()
	utils.SetUpgradeStatusFailed(ibuCopy, "Uncontrolled rollback")
	if err := lcautils.MarshalToFile(ibuCopy, ibuPreStaterootPath); err != nil {
		return fmt.Errorf("failed to save copy of IBU CR for rollback: %w", err)
	}
	return nil
}

var getStaterootPath = func(stateroot string) string {
	return common.PathOutsideChroot(common.GetStaterootPath(stateroot))
}

var getStaterootVarPath = func(stateroot string) string {
	return common.PathOutsideChroot(filepath.Join(common.GetStaterootPath(stateroot), "/var"))
}

// CheckHealth helper func to call HealthChecks
var CheckHealth = healthcheck.HealthChecks

func (u *UpgHandler) autoRollbackIfEnabled(ibu *ibuv1.ImageBasedUpgrade, msg string) {
	// Check whether auto-rollback is disabled using annotation
	if val, exists := ibu.GetAnnotations()[common.AutoRollbackOnFailureUpgradeCompletionAnnotation]; exists {
		if val == common.AutoRollbackDisableValue {
			u.Log.Info("Auto-rollback upgrade completion is disabled")
			return
		}
	}

	u.Log.Info("Automatically rolling back due to failure")

	if err := u.RebootClient.InitiateRollback(msg); err != nil {
		u.Log.Info(fmt.Sprintf("Unable to auto rollback: %s", err))
		return
	}

	// Should never get here
	return
}

// PostPivot executes all the post-upgrade steps after the cluster is rebooted to the new stateroot.
//
// Note: All decisions, including reconciles and failures, should be made within this function.
// The caller will simply return what this function returns.
func (u *UpgHandler) PostPivot(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) (ctrl.Result, error) {
	// start post-pivot phase timer
	utils.StartPhase(u.Client, u.Log, ibu, UpgradePhasePostpivot)

	u.Log.Info("Starting health check for different components")
	if err := CheckHealth(ctx, u.NoncachedClient, u.Log); err != nil {
		utils.SetUpgradeStatusInProgress(ibu, fmt.Sprintf("Waiting for system to stabilize: %s", err.Error()))
		return requeueWithHealthCheckInterval(), nil
	}

	err := u.BackupRestore.EnsureOadpConfiguration(ctx)
	if err != nil {
		if backuprestore.IsBRStorageBackendUnavailableError(err) {
			u.Log.Error(err, "Failed to ensure OADP configuration")
			utils.SetUpgradeStatusFailed(ibu, err.Error())
			u.autoRollbackIfEnabled(ibu, fmt.Sprintf("Rollback due to missing DataProtectionApplication: %s", err))
			return doNotRequeue(), nil
		}
		utils.SetUpgradeStatusInProgress(ibu, fmt.Sprintf("Checking Application Configuration: Failure occurred: %s", err.Error()))
		return requeueWithError(fmt.Errorf("error while checking OADP configuration: %w", err))
	}

	// Applying extra manifests
	utils.SetUpgradeStatusInProgress(ibu, "Applying Policy Manifests")
	if updateErr := utils.UpdateIBUStatus(ctx, u.Client, ibu); updateErr != nil {
		u.Log.Error(updateErr, "failed to update IBU CR status")
	}

	err = u.ExtraManifest.ApplyExtraManifests(ctx, common.PathOutsideChroot(extramanifest.PolicyManifestPath))
	if err != nil {
		if extramanifest.IsEMFailedError(err) {
			u.Log.Error(err, "Failed to apply policy manifests")
			utils.SetUpgradeStatusFailed(ibu, err.Error())
			u.autoRollbackIfEnabled(ibu, fmt.Sprintf("Rollback due to failure applying policy manifests: %s", err))
			return doNotRequeue(), nil
		}
		utils.SetUpgradeStatusInProgress(ibu, fmt.Sprintf("Applying Policy Manifests: Failure occurred: %s", err.Error()))
		return requeueWithError(fmt.Errorf("error while applying policy manifests: %w", err))
	}

	utils.SetUpgradeStatusInProgress(ibu, "Applying Config Manifests")
	if updateErr := utils.UpdateIBUStatus(ctx, u.Client, ibu); updateErr != nil {
		u.Log.Error(updateErr, "failed to update IBU CR status")
	}

	err = u.ExtraManifest.ApplyExtraManifests(ctx, common.PathOutsideChroot(extramanifest.CmManifestPath))
	if err != nil {
		if extramanifest.IsEMFailedError(err) {
			u.Log.Error(err, "Failed to apply config manifests")
			utils.SetUpgradeStatusFailed(ibu, err.Error())
			u.autoRollbackIfEnabled(ibu, fmt.Sprintf("Rollback due to failure applying config manifests: %s", err))
			return doNotRequeue(), nil
		}
		utils.SetUpgradeStatusInProgress(ibu, fmt.Sprintf("Applying Config Manifests: Failure occurred: %s", err.Error()))
		return requeueWithError(fmt.Errorf("error while applying config manifests: %w", err))
	}

	// Handling restores with OADP operator
	utils.SetUpgradeStatusInProgress(ibu, "Restoring Application Data")
	if updateErr := utils.UpdateIBUStatus(ctx, u.Client, ibu); updateErr != nil {
		u.Log.Error(updateErr, "failed to update IBU CR status")
	}

	result, err := u.HandleRestore(ctx)
	if err != nil {
		// Restore failed
		if backuprestore.IsBRFailedError(err) {
			u.Log.Error(err, "Failed to handle restore")
			utils.SetUpgradeStatusFailed(ibu, err.Error())
			u.autoRollbackIfEnabled(ibu, fmt.Sprintf("Rollback due to restore failure: %s", err))
			return doNotRequeue(), nil
		}
		utils.SetUpgradeStatusInProgress(ibu, fmt.Sprintf("Restoring Application Data: Failure occurred: %s", err))
		return requeueWithError(fmt.Errorf("error while handling restore: %w", err))
	}
	if !result.IsZero() {
		// The restore process has not been completed yet, requeue
		utils.SetUpgradeStatusInProgress(ibu, "Restore of Application Data is in progress")
		return result, nil
	}

	if err := u.RebootClient.DisableInitMonitor(); err != nil {
		// Don't fail the upgrade on failure here, just log it
		u.Log.Error(err, "Unable to disable LCA init monitor")
	}

	// stop post-pivot phase timer
	utils.StopPhase(u.Client, u.Log, ibu, UpgradePhasePostpivot)
	// stop Upgrade stage timer
	utils.StopStageHistory(u.Client, u.Log, ibu)

	u.Log.Info("Done handleUpgrade")
	utils.SetUpgradeStatusCompleted(ibu)
	return doNotRequeue(), nil
}

// HandleBackup manages backup flow and returns with possible requeue
func (u *UpgHandler) HandleBackup(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) (ctrl.Result, error) {
	sortedBackupGroups, err := u.BackupRestore.GetSortedBackupsFromConfigmap(ctx, ibu.Spec.OADPContent)
	if err != nil {
		return requeueWithError(fmt.Errorf("error while getting sorted backups from configmap: %w", err))
	}

	if len(sortedBackupGroups) == 0 {
		u.Log.Info("No backup requests, skipping")
		return doNotRequeue(), nil
	}

	if err := u.BackupRestore.PatchPVsReclaimPolicy(ctx); err != nil {
		return requeueWithError(fmt.Errorf("failed to patch LVMS PVs with Retain as persistentVolumeReclaimPolicy: %w", err))
	}

	// trigger and track each group
	for index, backups := range sortedBackupGroups {
		u.Log.Info("Processing backup", "groupIndex", index+1, "totalGroups", len(sortedBackupGroups))

		// check for any stale backup in the group
		if err := u.BackupRestore.CleanupStaleBackups(ctx, backups); err != nil {
			return requeueWithError(fmt.Errorf("failed to cleanup stale Backups: %w", err))
		}

		backupTracker, err := u.BackupRestore.StartOrTrackBackup(ctx, backups)
		if err != nil {
			return requeueWithError(fmt.Errorf("error while starting or tracking backup: %w", err))
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
			return requeueWithShortInterval(), nil
		}

		// Backups are waiting for condition
		return requeueWithMediumInterval(), nil
	}

	u.Log.Info("All backups succeeded")
	return doNotRequeue(), nil
}

func (u *UpgHandler) HandleRestore(ctx context.Context) (ctrl.Result, error) {
	u.Log.Info("Handling restores with OADP operator")
	// Load restore CRs from files
	sortedRestoreGroups, err := u.BackupRestore.LoadRestoresFromOadpRestorePath()
	if err != nil {
		return requeueWithError(fmt.Errorf("error while loading restores from OADP restore path: %w", err))
	}

	if len(sortedRestoreGroups) == 0 {
		u.Log.Info("No restore requests, skipping")
		return doNotRequeue(), nil
	}

	for index, restores := range sortedRestoreGroups {
		u.Log.Info("Processing restore", "groupIndex", index+1, "totalGroups", len(sortedRestoreGroups))
		restoreTracker, err := u.BackupRestore.StartOrTrackRestore(ctx, restores)
		if err != nil {
			return requeueWithError(fmt.Errorf("error while starting or tracking restore: %w", err))
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
	u.Log.Info("All restores succeeded")

	if err := u.BackupRestore.RestorePVsReclaimPolicy(ctx); err != nil {
		return requeueWithError(fmt.Errorf("failed to restore persistentVolumeReclaimPolicy in PVs created by LVMS: %w", err))
	}

	if err := os.RemoveAll(common.PathOutsideChroot(backuprestore.OadpPath)); err != nil {
		return requeueWithError(fmt.Errorf("error while removing OADP path: %w", err))
	}
	u.Log.Info("OADP path removed", "path", backuprestore.OadpPath)

	return doNotRequeue(), nil
}
