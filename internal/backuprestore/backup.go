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

package backuprestore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// ReconcileBackup reconciles the backup CRs
func (h *BRHandler) ReconcileBackup(ctx context.Context, oadpContent []lcav1alpha1.ConfigMapRef,
) (
	result ctrl.Result, status BackupStatus, err error,
) {
	if len(oadpContent) == 0 {
		status.Status = BackupFailedValidation
		status.Message = "No oadp configmap is provided."
		result.RequeueAfter = 1 * time.Minute
		return
	}

	oadpConfigmaps, err := getConfigMaps(ctx, h.Client, oadpContent)
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return
		}

		status.Status = BackupFailedValidation
		status.Message = fmt.Sprintf("The oadp configmap is not found: %s", err.Error())
		result.RequeueAfter = 1 * time.Minute
		return
	}

	backupCrs, err := extractBackupFromConfigmaps(ctx, h.Client, oadpConfigmaps)
	if err != nil {
		if k8serrors.IsInvalid(err) {
			h.Log.Error(err, "Invalid backup CR")

			status.Status = BackupFailedValidation
			status.Message = fmt.Sprintf("Invalid backup CR: %s", err.Error())
			result.RequeueAfter = 1 * time.Minute

			err = nil
		}
		return
	}

	sortedBackupGroups, err := sortBackupCrs(backupCrs)
	if err != nil {
		return
	}

	return h.triggerBackup(ctx, sortedBackupGroups)
}

func (h *BRHandler) triggerBackup(ctx context.Context, backupGroups [][]*velerov1.Backup,
) (
	result ctrl.Result, status BackupStatus, err error,
) {
	for _, backupGroup := range backupGroups {
		var (
			pendingBackups          []string
			progressingBackups      []string
			succeededBackups        []string
			failedBackups           []string
			failedValidationBackups []string
		)

		for _, backup := range backupGroup {
			var existingBackup *velerov1.Backup
			existingBackup, err = getBackup(ctx, h.Client, backup.Name, backup.Namespace)
			if err != nil {
				return
			}

			if existingBackup == nil {
				// Backup has not been applied. Applying backup
				var clusterID string
				clusterID, err = getClusterID(ctx, h.Client)
				if err != nil {
					return
				}

				setBackupLabel(backup, map[string]string{clusterIDLabel: clusterID})
				if err = h.Create(ctx, backup); err != nil {
					return
				}
				h.Log.Info("Backup created", "name", backup.Name, "namespace", backup.Namespace)
				progressingBackups = append(progressingBackups, backup.Name)

			} else {
				currentBackupStatus := h.checkVeleroBackupProcessStatus(existingBackup)

				switch currentBackupStatus {
				case BackupPending:
					pendingBackups = append(pendingBackups, existingBackup.Name)
				case BackupFailed:
					failedBackups = append(failedBackups, existingBackup.Name)
				case BackupCompleted:
					succeededBackups = append(succeededBackups, existingBackup.Name)
				case BackupInProgress:
					progressingBackups = append(progressingBackups, existingBackup.Name)
				case BackupFailedValidation:
					// Re-create the failed validation backup if it has been updated
					if !equivalentBackups(existingBackup, backup) {
						if err = h.Delete(ctx, existingBackup); err != nil {
							return
						}

						var clusterID string
						clusterID, err = getClusterID(ctx, h.Client)
						if err != nil {
							return
						}
						setBackupLabel(backup, map[string]string{clusterIDLabel: clusterID})
						if err = h.Create(ctx, backup); err != nil {
							return
						}

						progressingBackups = append(progressingBackups, existingBackup.Name)
					} else {
						failedValidationBackups = append(failedValidationBackups, existingBackup.Name)
					}
				}
			}
		}

		if len(succeededBackups) == len(backupGroup) {
			// The current backup group has done, work on the next group
			continue

		} else if len(failedBackups) != 0 {
			status.Status = BackupFailed
			status.Message = fmt.Sprintf(
				"Failed backups: %s",
				strings.Join(failedBackups, ","))
			result.Requeue = false
			return

		} else if len(progressingBackups) != 0 {
			status.Status = BackupInProgress
			status.Message = fmt.Sprintf(
				"Inprogress backups: %s",
				strings.Join(progressingBackups, ","))
			result.RequeueAfter = 2 * time.Second
			return

		} else if len(failedValidationBackups) != 0 {
			status.Status = BackupFailedValidation
			status.Message = fmt.Sprintf(
				"Failed validation backups: %s. %s",
				strings.Join(failedValidationBackups, ","),
				"Please update the invalid backups.")
			result.RequeueAfter = 1 * time.Minute
			return

		} else {
			// Backup doesn't have any status, it's likely
			// that the object storage backend is not available.
			// Requeue to wait for the object storage backend
			// to be ready.
			status.Status = BackupPending
			status.Message = fmt.Sprintf(
				"Pending backups: %s. %s",
				strings.Join(pendingBackups, ","),
				"Wait for object storage backend to be available.")
			result.RequeueAfter = 1 * time.Minute
			return
		}
	}

	status.Status = BackupCompleted
	status.Message = "All Backups have completed"
	result.Requeue = false
	return
}

func equivalentBackups(backup1, backup2 *velerov1.Backup) bool {
	// Compare the specs only
	return equality.Semantic.DeepEqual(backup1.Spec, backup2.Spec)
}

func (h *BRHandler) checkVeleroBackupProcessStatus(backup *velerov1.Backup) BackupPhase {
	h.Log.Info("Backup",
		"name", backup.Name,
		"phase", backup.Status.Phase,
		"warnings", backup.Status.Warnings,
		"errors", backup.Status.Errors,
		"failure", backup.Status.FailureReason,
		"validation errors", backup.Status.ValidationErrors,
	)

	switch backup.Status.Phase {
	case "":
		// Backup has no status
		return BackupPending
	case velerov1.BackupPhaseCompleted:
		return BackupCompleted
	case velerov1.BackupPhaseFailedValidation:
		return BackupFailedValidation
	case velerov1.BackupPhasePartiallyFailed,
		velerov1.BackupPhaseFailed:
		return BackupFailed
	default:
		return BackupInProgress
	}
}

// sortBackupOrRestoreCrs sorts the backups by the apply-wave annotation
func sortBackupCrs(resources []*velerov1.Backup) ([][]*velerov1.Backup, error) {
	var resourcesApplyWaveMap = make(map[int][]*velerov1.Backup)
	var sortedResources [][]*velerov1.Backup

	// sort backups by annotation lca.openshift.io/apply-wave
	for _, resource := range resources {
		applyWave, _ := resource.GetAnnotations()[applyWaveAnn]
		if applyWave == "" {
			// Empty apply-wave annotation or no annotation
			resourcesApplyWaveMap[defaultApplyWave] = append(resourcesApplyWaveMap[defaultApplyWave], resource)
			continue
		}

		applyWaveInt, err := strconv.Atoi(applyWave)
		if err != nil {
			return nil, fmt.Errorf("Failed to convert %s in Backup CR %s to interger: %s", applyWave, resource.GetName(), err)
		}
		resourcesApplyWaveMap[applyWaveInt] = append(resourcesApplyWaveMap[applyWaveInt], resource)
	}

	var sortedApplyWaves []int
	for applyWave := range resourcesApplyWaveMap {
		sortedApplyWaves = append(sortedApplyWaves, applyWave)
	}
	sort.Ints(sortedApplyWaves)

	for index, applyWave := range sortedApplyWaves {
		sortedResources = append(sortedResources, resourcesApplyWaveMap[applyWave])
		sortBackupsByName(sortedResources[index])
	}

	return sortedResources, nil
}

// sortBackupsByName sorts a list of backups by name alphebetically
func sortBackupsByName(resources []*velerov1.Backup) {
	sort.Slice(resources, func(i, j int) bool {
		nameI := resources[i].GetName()
		nameJ := resources[j].GetName()
		return nameI < nameJ
	})
}

// extractBackupFromConfigmaps extacts Backup CRs from configmaps
func extractBackupFromConfigmaps(ctx context.Context, c client.Client, configmaps []corev1.ConfigMap) ([]*velerov1.Backup, error) {
	var backups []*velerov1.Backup

	for _, cm := range configmaps {
		for _, value := range cm.Data {
			resource := unstructured.Unstructured{}
			err := yaml.Unmarshal([]byte(value), &resource)
			if err != nil {
				return nil, err
			}

			if resource.GroupVersionKind() != backupGvk {
				continue
			}

			// Create the backup CR in dry-run mode to detect any validation errors in the CR
			// i.e., missing required fields
			err = c.Create(ctx, &resource, &client.CreateOptions{DryRun: []string{metav1.DryRunAll}})
			if err != nil {
				return nil, err
			}

			backup := velerov1.Backup{}
			err = yaml.Unmarshal([]byte(value), &backup)
			if err != nil {
				return nil, err
			}
			backups = append(backups, &backup)
		}
	}

	return backups, nil
}

// ExportOadpConfigurationToDir exports the OADP DataProtectionApplication CRs and required storage creds to a given location
func (h *BRHandler) ExportOadpConfigurationToDir(ctx context.Context, toDir, oadpNamespace string) error {
	// Create the directories for OADP and secret
	if err := os.MkdirAll(filepath.Join(toDir, oadpSecretPath), 0o700); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Join(toDir, oadpDpaPath), 0o700); err != nil {
		return err
	}

	// Write the object storage secret to a given directory if found
	storageSecret := &corev1.Secret{}
	if err := h.Get(ctx, types.NamespacedName{
		Name:      defaultStorageSecret,
		Namespace: oadpNamespace,
	}, storageSecret); err != nil {
		if !k8serrors.IsNotFound(err) {
			return err
		}
	} else {
		// Unset uid and resource version
		storageSecret.SetUID("")
		storageSecret.SetResourceVersion("")

		filePath := filepath.Join(toDir, oadpSecretPath, defaultStorageSecret+".yaml")
		if err := writeSecretToFile(storageSecret, filePath); err != nil {
			return err
		}
	}

	dpaList := &unstructured.UnstructuredList{}
	dpaList.SetGroupVersionKind(dpaGvkList)
	opts := []client.ListOption{
		client.InNamespace(oadpNamespace),
	}
	if err := h.List(ctx, dpaList, opts...); err != nil {
		return err
	}

	// Write DPAs to a given directory
	for _, dpa := range dpaList.Items {
		// Unset uid and resource version
		dpa.SetUID("")
		dpa.SetResourceVersion("")

		filePath := filepath.Join(toDir, oadpDpaPath, dpa.GetName()+".yaml")
		if err := writeDpaToFile(&dpa, filePath); err != nil {
			return err
		}

		// Make sure the required storage creds are exported
		dpaSpec := dpa.Object["spec"].(map[string]interface{})
		backupLocations := dpaSpec["backupLocations"].([]interface{})
		for _, backupLocation := range backupLocations {
			if backupLocation.(map[string]interface{})["velero"] == nil {
				continue
			}
			velero := backupLocation.(map[string]interface{})["velero"].(map[string]interface{})
			if velero["credential"] == nil {
				continue
			}

			creds := velero["credential"].(map[string]interface{})
			if creds["name"] == nil {
				continue
			}

			secretName := creds["name"].(string)
			if secretName == defaultStorageSecret {
				// default storage secret has been exported already, continue
				continue
			}

			storageSecret := &corev1.Secret{}
			if err := h.Get(ctx, types.NamespacedName{
				Name:      secretName,
				Namespace: oadpNamespace,
			}, storageSecret); err != nil {
				return err
			}

			storageSecret.SetUID("")
			storageSecret.SetResourceVersion("")

			filePath := filepath.Join(toDir, oadpSecretPath, secretName+".yaml")
			if err := writeSecretToFile(storageSecret, filePath); err != nil {
				return err
			}
		}
	}

	return nil
}

// ExportRestoresToDir extracts all restore CRs from oadp configmaps and write them to a given location
// returns: error
func (h *BRHandler) ExportRestoresToDir(ctx context.Context, configMaps []lcav1alpha1.ConfigMapRef, toDir string) error {
	configmaps, err := getConfigMaps(ctx, h.Client, configMaps)
	if err != nil {
		return err
	}

	restores, err := extractRestoreFromConfigmaps(ctx, h.Client, configmaps)
	if err != nil {
		return err
	}

	sortedRestores, err := sortRestoreCrs(restores)
	if err != nil {
		return err
	}

	for i, restoreGroup := range sortedRestores {
		// Create a directory for each group
		group := filepath.Join(toDir, oadpRestorePath, "restore"+strconv.Itoa(i+1))
		// If the directory already exists, it does nothing
		if err := os.MkdirAll(group, 0o700); err != nil {
			return err
		}

		for j, restore := range restoreGroup {
			filePath := filepath.Join(group, "restore"+strconv.Itoa(j+1)+".yaml")
			if err := writeRestoreToFile(restore, filePath); err != nil {
				return err
			}
		}
	}

	return nil
}

// CleanupBackups deletes all backups for this cluster from object storage
// returns: true if all backups have been deleted, error
func (h *BRHandler) CleanupBackups(ctx context.Context) (bool, error) {
	// Get the cluster ID
	clusterID, err := getClusterID(ctx, h.Client)
	if err != nil {
		return false, err
	}

	// List all backups created for this cluster
	backupList := &velerov1.BackupList{}
	if err := h.List(ctx, backupList, client.MatchingLabels{
		clusterIDLabel: clusterID,
	}); err != nil {
		return false, err
	}

	// Create deleteBackupRequest CR to delete the backup in the object storage
	for _, backup := range backupList.Items {
		deleteBackupRequest := &velerov1.DeleteBackupRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      backup.Name,
				Namespace: backup.Namespace,
			},
			Spec: velerov1.DeleteBackupRequestSpec{
				BackupName: backup.Name,
			},
		}

		if err := h.Create(ctx, deleteBackupRequest); err != nil {
			return false, err
		}
		h.Log.Info("Backup deletion request has sent", "backup", backup.Name)
	}

	// Ensure all backups are deleted
	return h.ensureBackupsDeleted(ctx, backupList.Items)
}
func (h *BRHandler) ensureBackupsDeleted(ctx context.Context, backups []velerov1.Backup) (bool, error) {
	err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 5*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			var remainingBackups []string
			for _, backup := range backups {
				err := h.Get(ctx, types.NamespacedName{
					Name:      backup.Name,
					Namespace: backup.Namespace,
				}, &velerov1.Backup{})
				if err != nil {
					if !k8serrors.IsNotFound(err) {
						return false, err
					}
				} else {
					// Backup still exists
					remainingBackups = append(remainingBackups, backup.Name)
				}
			}

			if len(remainingBackups) == 0 {
				h.Log.Info("All backups have been deleted")
				return true, nil
			}

			h.Log.Info("Waiting for backups to be deleted", "backups", remainingBackups)
			return false, nil
		})
	if err != nil {
		if err == context.DeadlineExceeded {
			h.Log.Error(err, "Timeout waiting for backups to be deleted")
			return false, nil
		}
		// API call errors
		return false, err
	}
	return true, nil
}
