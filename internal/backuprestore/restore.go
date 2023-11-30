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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// ReconcileRestore reconciles the restore CRs
func (h *BRHandler) ReconcileRestore(ctx context.Context, fileDir string,
) (
	result ctrl.Result, status RestoreStatus, err error,
) {
	sortedRestores, err := h.loadRestoresFromDir(fileDir)
	if err != nil {
		return
	}

	return h.triggerRestore(ctx, sortedRestores)
}

func (h *BRHandler) triggerRestore(ctx context.Context, restoreGroups [][]*velerov1.Restore,
) (
	result ctrl.Result, status RestoreStatus, err error,
) {
	for _, restoreGroup := range restoreGroups {
		var (
			missingBackups      []string
			pendingRestores     []string
			progressingRestores []string
			succeededRestores   []string
			failedRestores      []string
		)

		for _, restore := range restoreGroup {
			existingRestore := &velerov1.Restore{}
			if err = h.Get(ctx, types.NamespacedName{
				Name:      restore.Name,
				Namespace: restore.Namespace,
			}, existingRestore); err != nil {

				if !k8serrors.IsNotFound(err) {
					// API error
					return
				}

				// We expect the backup to be auto-created by velero after
				// OADP is running and connects to the object storage.
				// Ensure the backup exists before creating the restore.
				var existingBackup *velerov1.Backup
				existingBackup, err = getBackup(ctx, h, restore.Spec.BackupName, restore.Namespace)
				if err != nil {
					return
				}

				if existingBackup == nil {
					missingBackups = append(missingBackups, restore.Spec.BackupName)
				} else {
					if err = h.Create(ctx, restore); err != nil {
						return
					}
					h.Log.Info("Restore created", "name", restore.Name, "namespace", restore.Namespace)
					progressingRestores = append(progressingRestores, restore.Name)
				}

			} else {
				currentRestoreStatus := h.checkVeleroRestoreProcessStatus(existingRestore)

				switch currentRestoreStatus {
				case RestorePending:
					pendingRestores = append(pendingRestores, existingRestore.Name)
				case RestoreFailed:
					failedRestores = append(failedRestores, existingRestore.Name)
				case RestoreCompleted:
					succeededRestores = append(succeededRestores, existingRestore.Name)
				default:
					progressingRestores = append(progressingRestores, existingRestore.Name)
				}
			}
		}

		if len(succeededRestores) == len(restoreGroup) {
			// The current restore group has done, work on the next group
			continue

		} else if len(failedRestores) != 0 {
			status.Status = RestoreFailed
			status.Message = fmt.Sprintf(
				"Failed restores: %s",
				strings.Join(failedRestores, ","))
			result.Requeue = false
			return

		} else if len(missingBackups) != 0 {
			// Missing required backups, it could be the
			// object storage backend is not available yet
			// or the backup CRs have not been auto-created
			// by velero yet. Requeue to wait for the backups.
			// If the object storage backend is available, but
			// the backups are not created after a long time,
			// they could be deleted in the object storage backend.
			status.Status = RestorePending
			status.Message = fmt.Sprintf(
				"Not found backups: %s.",
				strings.Join(missingBackups, ","),
			)
			result.RequeueAfter = 10 * time.Second
			return

		} else if len(progressingRestores) != 0 {
			status.Status = RestoreInProgress
			status.Message = fmt.Sprintf(
				"Inprogress restores: %s",
				strings.Join(progressingRestores, ","))
			result.RequeueAfter = 30 * time.Second
			return

		} else {
			// Restore doesn't have any status, it's likely
			// that the object storage backend is not available.
			// Requeue to wait for the object storage backend
			// to be recovered.
			status.Status = RestorePending
			status.Message = fmt.Sprintf(
				"Pending restores: %s. %s",
				strings.Join(pendingRestores, ","),
				"Wait for object storage backend to be available.")
			result.RequeueAfter = 1 * time.Minute
			return
		}
	}

	status.Status = RestoreCompleted
	status.Message = "All restores have completed"
	result.Requeue = false

	return
}

// extractRestoreFromConfigmaps extacts Restore CRs from configmaps
func extractRestoreFromConfigmaps(ctx context.Context, c client.Client, configmaps []corev1.ConfigMap) ([]*velerov1.Restore, error) {
	var restores []*velerov1.Restore

	for _, cm := range configmaps {
		for _, value := range cm.Data {
			resource := unstructured.Unstructured{}
			err := yaml.Unmarshal([]byte(value), &resource)
			if err != nil {
				return nil, err
			}

			if resource.GroupVersionKind() != restoreGvk {
				continue
			}

			// Create the restore CR in dry-run mode to detect any validation errors in the CR
			// i.e., missing required fields
			err = c.Create(ctx, &resource, &client.CreateOptions{DryRun: []string{metav1.DryRunAll}})
			if err != nil {
				return nil, err
			}

			restore := velerov1.Restore{}
			err = yaml.Unmarshal([]byte(value), &restore)
			if err != nil {
				return nil, err
			}
			restores = append(restores, &restore)
		}
	}

	return restores, nil
}

// sortRestoreCrs sorts the restore CRs by the apply-wave annotation
func sortRestoreCrs(resources []*velerov1.Restore) ([][]*velerov1.Restore, error) {
	var resourcesApplyWaveMap = make(map[int][]*velerov1.Restore)
	var sortedResources [][]*velerov1.Restore

	// sort restore CRs by annotation lca.openshift.io/apply-wave
	for _, resource := range resources {
		applyWave, _ := resource.GetAnnotations()[applyWaveAnn]
		if applyWave == "" {
			// Empty apply-wave annotation or no annotation
			resourcesApplyWaveMap[defaultApplyWave] = append(resourcesApplyWaveMap[defaultApplyWave], resource)
			continue
		}

		applyWaveInt, err := strconv.Atoi(applyWave)
		if err != nil {
			return nil, fmt.Errorf("failed to convert %s in Backup CR %s to interger: %w", applyWave, resource.GetName(), err)
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
		sortRestoresByName(sortedResources[index])
	}

	return sortedResources, nil
}

func sortRestoresByName(resources []*velerov1.Restore) {
	sort.Slice(resources, func(i, j int) bool {
		nameI := resources[i].GetName()
		nameJ := resources[j].GetName()
		return nameI < nameJ
	})
}

func (h *BRHandler) loadRestoresFromDir(fromDir string) ([][]*velerov1.Restore, error) {
	var sortedRestores [][]*velerov1.Restore

	// The returned list of entries are sorted by name alphabetically
	restoreSubDirs, err := os.ReadDir(filepath.Join(fromDir, oadpRestorePath))
	if err != nil {
		return nil, err
	}

	for _, restoreSubDir := range restoreSubDirs {
		if !restoreSubDir.IsDir() {
			// Unexpected
			h.Log.Info("Unexpected file found, skipping...", "file",
				filepath.Join(fromDir, oadpRestorePath, restoreSubDir.Name()))
			continue
		}

		// The returned list of entries are sorted by name alphabetically
		restoreDirPath := filepath.Join(fromDir, oadpRestorePath, restoreSubDir.Name())
		restoreYamls, err := os.ReadDir(restoreDirPath)
		if err != nil {
			return nil, err
		}

		var restores []*velerov1.Restore
		for _, restoreYaml := range restoreYamls {
			if restoreYaml.IsDir() {
				// Unexpected
				h.Log.Info("Unexpected directory found, skipping...", "directory",
					filepath.Join(restoreDirPath, restoreYaml.Name()))
				continue
			}

			restoreFilePath := filepath.Join(restoreDirPath, restoreYaml.Name())
			restoreBytes, err := os.ReadFile(restoreFilePath)
			if err != nil {
				return nil, err
			}

			restore := &velerov1.Restore{}
			if err := yaml.Unmarshal(restoreBytes, restore); err != nil {
				return nil, err
			}

			restores = append(restores, restore)
		}

		sortedRestores = append(sortedRestores, restores)
	}

	return sortedRestores, nil
}

func (h *BRHandler) checkVeleroRestoreProcessStatus(restore *velerov1.Restore) RestorePhase {
	h.Log.Info("Restore",
		"name", restore.Name,
		"phase", restore.Status.Phase,
		"warnings", restore.Status.Warnings,
		"errors", restore.Status.Errors,
		"failure", restore.Status.FailureReason,
		"validation errors", restore.Status.ValidationErrors,
	)

	switch restore.Status.Phase {
	case "":
		// Restore has no status
		return RestorePending
	case velerov1.RestorePhaseCompleted:
		return RestoreCompleted
	case velerov1.RestorePhaseFailedValidation,
		velerov1.RestorePhasePartiallyFailed,
		velerov1.RestorePhaseFailed:
		return RestoreFailed
	default:
		return RestoreInProgress
	}
}

// RestoreOadpConfigurationsFromDir restores the backed up OADP DataProtectionApplication CRs and storage secrets
// from the given location
// returns true if the storage backend is available after the restore of OADP configurations, otherwise false
func (h *BRHandler) RestoreOadpConfigurationsFromDir(ctx context.Context, fromDir string) (bool, error) {
	if err := h.restoreSecrets(ctx, fromDir); err != nil {
		return false, err
	}

	return h.restoreDataProtectionApplications(ctx, fromDir)
}

// restoreSecrets restores the previous backed up secrets for object storage backend
// from the given location
func (h *BRHandler) restoreSecrets(ctx context.Context, fromDir string) error {
	secretDir := filepath.Join(fromDir, oadpSecretPath)
	secretYamls, err := os.ReadDir(secretDir)
	if err != nil {
		return err
	}

	for _, secretYaml := range secretYamls {
		if secretYaml.IsDir() {
			// Unexpected
			h.Log.Info("Unexpected directory found, skipping...", "directory",
				filepath.Join(secretDir, secretYaml.Name()))
			continue
		}

		secretBytes, err := os.ReadFile(filepath.Join(secretDir, secretYaml.Name()))
		if err != nil {
			return err
		}

		secret := &corev1.Secret{}
		if err := yaml.Unmarshal(secretBytes, secret); err != nil {
			return err
		}

		if err := h.Create(ctx, secret); err != nil {
			if !k8serrors.IsAlreadyExists(err) {
				return err
			}

			// Update if it already exists
			existingSecret := &corev1.Secret{}
			if err := h.Get(ctx, types.NamespacedName{
				Name:      secret.Name,
				Namespace: secret.Namespace,
			}, existingSecret); err != nil {
				return err
			}

			secret.SetResourceVersion(existingSecret.GetResourceVersion())
			secret.SetUID(existingSecret.GetUID())
			if err := h.Update(ctx, secret); err != nil {
				return err
			}
		}
		h.Log.Info("Secret restored", "name", secret.GetName(), "namespace", secret.GetNamespace())
	}
	return nil
}

// restoreDataProtectionApplications restores the previous backed up DataProtectionApplication CRs
// from the given location
func (h *BRHandler) restoreDataProtectionApplications(ctx context.Context, fromDir string) (bool, error) {
	dpaDir := filepath.Join(fromDir, oadpDpaPath)
	dpaYamls, err := os.ReadDir(dpaDir)
	if err != nil {
		return false, err
	}

	var oadpNs string
	for _, dpaYaml := range dpaYamls {
		if dpaYaml.IsDir() {
			// Unexpected
			h.Log.Info("Unexpected directory found, skipping...", "directory",
				filepath.Join(dpaDir, dpaYaml.Name()))
			continue
		}

		dpaBytes, err := os.ReadFile(filepath.Join(dpaDir, dpaYaml.Name()))
		if err != nil {
			return false, err
		}

		dpa := &unstructured.Unstructured{}
		dpa.SetGroupVersionKind(dpaGvk)
		if err := yaml.Unmarshal(dpaBytes, dpa); err != nil {
			return false, err
		}

		if err := h.Create(ctx, dpa); err != nil {
			if !k8serrors.IsAlreadyExists(err) {
				return false, err
			}

			// Update if it already exists
			existingDpa := &unstructured.Unstructured{}
			existingDpa.SetGroupVersionKind(dpaGvk)
			if err := h.Get(ctx, types.NamespacedName{
				Name:      dpa.GetName(),
				Namespace: dpa.GetNamespace(),
			}, existingDpa); err != nil {
				return false, err
			}

			dpa.SetResourceVersion(existingDpa.GetResourceVersion())
			dpa.SetUID(existingDpa.GetUID())
			if err := h.Update(ctx, dpa); err != nil {
				return false, err
			}
		}
		h.Log.Info("DataProtectionApplication restored", "name", dpa.GetName(), "namespace", dpa.GetNamespace())
		oadpNs = dpa.GetNamespace()
	}

	// Ensure the storage backends are created and available
	// after the restore of DataProtectionApplications
	return h.ensureStorageBackendAvailable(ctx, oadpNs)
}

// ensureStorageBackendAvailable ensures the storage backend is available.
// It returns false if the storage backend is not available because of an error,
// no storage backend is created after a minute, or it's timed out waiting for
// the storage backend to be available due to unknown reasons
func (h *BRHandler) ensureStorageBackendAvailable(ctx context.Context, lookupNs string) (bool, error) {
	err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 1*time.Minute, true,
		func(ctx context.Context) (done bool, err error) {
			var succeededBsls []string
			backupStorageLocation := &velerov1.BackupStorageLocationList{}
			err = h.List(ctx, backupStorageLocation, client.InNamespace(lookupNs))
			if err != nil {
				return false, err
			}

			for _, bsl := range backupStorageLocation.Items {
				if bsl.Status.Phase == velerov1.BackupStorageLocationPhaseUnavailable {
					err := fmt.Errorf("backupStorageLocation is unavailable. Name: %s, Error: %s", bsl.Name, bsl.Status.Message)
					return false, err
				} else if bsl.Status.Phase == velerov1.BackupStorageLocationPhaseAvailable {
					succeededBsls = append(succeededBsls, bsl.Name)
				}
			}

			if len(succeededBsls) == len(backupStorageLocation.Items) {
				h.Log.Info("All backup storage locations are available")
				return true, nil
			}
			return false, nil
		})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "backupStorageLocation is unavailable") {
			h.Log.Error(err, "Backup storage locations are not available")
			return false, nil
		}
	}

	backupStorageLocation := &velerov1.BackupStorageLocationList{}
	err = h.List(ctx, backupStorageLocation, client.InNamespace(lookupNs))
	if err != nil {
		return false, err
	}
	if len(backupStorageLocation.Items) == 0 {
		h.Log.Error(nil, "No backup storage location found")
		return false, nil
	}

	return true, nil
}
