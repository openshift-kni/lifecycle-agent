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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/openshift-kni/lifecycle-agent/utils"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type RestoreTracker struct {
	MissingBackups      []string
	PendingRestores     []string
	ProgressingRestores []string
	SucceededRestores   []string
	FailedRestores      []string
}

// StartOrTrackRestore start restore or track restore status
func (h *BRHandler) StartOrTrackRestore(ctx context.Context, restores []*velerov1.Restore,
) (
	*RestoreTracker, error,
) {
	rt := &RestoreTracker{}

	for _, restore := range restores {
		// Check if the restore CR already exists
		existingRestore := &velerov1.Restore{}
		if err := h.Get(ctx, types.NamespacedName{
			Name:      restore.Name,
			Namespace: restore.Namespace,
		}, existingRestore); err != nil {
			// Restore CR has not been created yet
			if !k8serrors.IsNotFound(err) {
				// API error
				return rt, fmt.Errorf("failed to get restore: %w", err)
			}

			// We expect the backup to be auto-created by velero after
			// OADP is running and connects to the object storage.
			// Ensure the backup exists before creating the restore.
			var existingBackup *velerov1.Backup
			existingBackup, err = getBackup(ctx, h, restore.Spec.BackupName, restore.Namespace)
			if err != nil {
				return rt, err
			}

			if existingBackup == nil {
				// The backup CR has not been auto-created by velero yet.
				rt.MissingBackups = append(rt.MissingBackups, restore.Spec.BackupName)
			} else {
				if err := h.Create(ctx, restore); err != nil {
					return rt, fmt.Errorf("failed to create restore: %w", err)
				}
				h.Log.Info("Restore created", "name", restore.Name, "namespace", restore.Namespace)
				rt.ProgressingRestores = append(rt.ProgressingRestores, restore.Name)
			}

		} else {
			// Restore CR already exists, check its status
			h.Log.Info("Restore CR status",
				"name", existingRestore.Name,
				"phase", existingRestore.Status.Phase,
				"warnings", existingRestore.Status.Warnings,
				"errors", existingRestore.Status.Errors,
				"failure", existingRestore.Status.FailureReason,
				"validation errors", existingRestore.Status.ValidationErrors,
			)

			switch existingRestore.Status.Phase {
			case velerov1.RestorePhaseCompleted:
				rt.SucceededRestores = append(rt.SucceededRestores, existingRestore.Name)
			case velerov1.RestorePhaseFailedValidation,
				velerov1.RestorePhasePartiallyFailed,
				velerov1.RestorePhaseFailed:
				rt.FailedRestores = append(rt.FailedRestores, existingRestore.Name)
			case "":
				// Restore CR has no status
				rt.PendingRestores = append(rt.PendingRestores, existingRestore.Name)
			default:
				rt.ProgressingRestores = append(rt.ProgressingRestores, existingRestore.Name)
			}
		}
	}

	h.Log.Info("Restores status",
		"missing backups", rt.MissingBackups,
		"pending restores", rt.PendingRestores,
		"progressing restores", rt.ProgressingRestores,
		"succeeded restores", rt.SucceededRestores,
		"failed restores", rt.FailedRestores,
	)
	return rt, nil
}

// extractRestoreFromConfigmaps extacts Restore CRs from configmaps
func (h *BRHandler) extractRestoreFromConfigmaps(ctx context.Context, configmaps []corev1.ConfigMap) ([]*velerov1.Restore, error) {
	var restores []*velerov1.Restore

	for _, cm := range configmaps {
		for _, value := range cm.Data {
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewBufferString(value), 4096)
			for {
				resource := unstructured.Unstructured{}
				err := decoder.Decode(&resource)
				if err != nil {
					if errors.Is(err, io.EOF) {
						// Reach the end of the data, exit the loop
						break
					}
					errMsg := fmt.Sprintf("Failed to decode yaml in configmap: %v", err.Error())
					h.Log.Error(nil, errMsg)
					return nil, NewBRFailedValidationError(resource.GetKind(), errMsg)
				}

				if resource.GroupVersionKind() != restoreGvk {
					continue
				}

				err = h.createObjectWithDryRun(ctx, &resource, cm.Name)
				if err != nil {
					return nil, fmt.Errorf("failed to create resource obj with dry-run: %w", err)
				}

				restore := velerov1.Restore{}
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, &restore); err != nil {
					return nil, fmt.Errorf("failed to convert to type restore from unstructure: %w", err)
				}
				restores = append(restores, &restore)
			}
		}
	}

	return restores, nil
}

// sortRestoreCrs sorts the restore CRs by the apply-wave annotation
func sortByApplyWaveRestoreCrs(resources []*velerov1.Restore) ([][]*velerov1.Restore, error) {
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

func (h *BRHandler) LoadRestoresFromOadpRestorePath() ([][]*velerov1.Restore, error) {
	var sortedRestores [][]*velerov1.Restore

	// The returned list of entries are sorted by name alphabetically
	oP := filepath.Join(hostPath, OadpRestorePath)
	restoreSubDirs, err := os.ReadDir(oP)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read oadp restore subdirs in %s: %w", oP, err)
	}

	for _, restoreSubDir := range restoreSubDirs {
		if !restoreSubDir.IsDir() {
			// Unexpected
			h.Log.Info("Unexpected file found, skipping...", "file",
				filepath.Join(OadpRestorePath, restoreSubDir.Name()))
			continue
		}

		// The returned list of entries are sorted by name alphabetically
		restoreDirPath := filepath.Join(OadpRestorePath, restoreSubDir.Name())
		restoreYamls, err := os.ReadDir(filepath.Join(hostPath, restoreDirPath))
		if err != nil {
			return nil, fmt.Errorf("failed get restore yamls in %s: %w", restoreYamls, err)
		}

		var restores []*velerov1.Restore
		for _, restoreYaml := range restoreYamls {
			if restoreYaml.IsDir() {
				// Unexpected
				h.Log.Info("Unexpected directory found, skipping...", "directory",
					filepath.Join(restoreDirPath, restoreYaml.Name()))
				continue
			}
			restoreFilePath := filepath.Join(hostPath, restoreDirPath, restoreYaml.Name())

			restore := &velerov1.Restore{}
			err := utils.ReadYamlOrJSONFile(restoreFilePath, restore)
			if err != nil {
				return nil, fmt.Errorf("failed to read restore file in %s: %w", restoreFilePath, err)
			}
			restores = append(restores, restore)
		}

		sortedRestores = append(sortedRestores, restores)
	}

	return sortedRestores, nil
}

// EnsureOadpConfiguration checks OADP configuration
func (h *BRHandler) EnsureOadpConfiguration(ctx context.Context) error {
	h.Log.Info("Checking OADP configuration")
	dpaYamlDir := filepath.Join(hostPath, OadpDpaPath)
	dpa, err := ReadOadpDataProtectionApplication(dpaYamlDir)
	if err != nil {
		return fmt.Errorf("failed to get stored DataProtectionApplication: %w", err)
	}
	if dpa == nil {
		h.Log.Info("No OADP configuration applied, skipping")
		return nil
	}

	// Ensure the DPA is reconciled successfully
	err = h.ensureDPAReconciled(ctx, dpa.GetName(), dpa.GetNamespace())
	if err != nil {
		return fmt.Errorf("failed to ensure DataProtectionApplication reconcile successfully: %w", err)
	}

	// Ensure the storage backends are created and available
	if err := h.ensureStorageBackendAvailable(ctx, OadpNs); err != nil {
		return fmt.Errorf("failed to ensure StorageBackend availability: %w", err)
	}
	return nil
}

// ensureDPAReconciled ensures the DataProtectionApplication CR is reconciled successfully
func (h *BRHandler) ensureDPAReconciled(ctx context.Context, name, namespace string) error {
	err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 3*time.Minute, true,
		func(ctx context.Context) (done bool, err error) {
			dpa := &unstructured.Unstructured{}
			dpa.SetGroupVersionKind(dpaGvk)
			err = h.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, dpa)
			if err != nil {
				if k8serrors.IsNotFound(err) {
					errMsg := fmt.Sprintf("DataProtectionApplication %s is not found", name)
					h.Log.Error(err, errMsg)
					return false, NewBRStorageBackendUnavailableError(errMsg)
				}
				return false, nil
			}

			ok := isDPAReconciled(dpa)
			if ok {
				h.Log.Info("DataProtectionApplication CR is reconciled", "name", name, "namespace", namespace)
				return true, nil
			}
			return false, nil
		})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			errMsg := fmt.Sprintf("Timeout waiting for DataProtectionApplication CR %s to be reconciled", name)
			h.Log.Error(err, errMsg)
			return NewBRStorageBackendUnavailableError(errMsg)
		}
		return err //nolint:wrapcheck
	}
	return nil
}

// ensureStorageBackendAvaialble ensures the storage backend is available.
// It returns NewBRStorageBackendUnavailableError if the storage backend
// is not available because of an error, no storage backend is created after
// 5 minutes, or it's timed out waiting for the storage backend to be available
// due to unknown reasons.
// Since DPA has been restored earlier during node startup following the reboot,
// at this point, all storage backends should already be available.
func (h *BRHandler) ensureStorageBackendAvailable(ctx context.Context, lookupNs string) error {
	err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 5*time.Minute, true,
		func(ctx context.Context) (done bool, err error) {
			var succeededBsls []string
			backupStorageLocation := &velerov1.BackupStorageLocationList{}
			err = h.List(ctx, backupStorageLocation, client.InNamespace(lookupNs))
			if err != nil {
				return false, nil
			}

			if len(backupStorageLocation.Items) == 0 {
				h.Log.Info("Waiting for backup storage locations to be created")
				return false, nil
			}

			for _, bsl := range backupStorageLocation.Items {
				if bsl.Status.Phase == velerov1.BackupStorageLocationPhaseUnavailable {
					errMsg := fmt.Sprintf("BackupStorageLocation is unavailable. Name: %s, Error: %s", bsl.Name, bsl.Status.Message)
					h.Log.Error(nil, errMsg)
					return false, NewBRStorageBackendUnavailableError(errMsg)
				} else if bsl.Status.Phase == velerov1.BackupStorageLocationPhaseAvailable {
					succeededBsls = append(succeededBsls, bsl.Name)
				}
			}

			if len(succeededBsls) == len(backupStorageLocation.Items) {
				h.Log.Info("All backup storage locations are available")
				return true, nil
			}

			h.Log.Info("Waiting for backup storage locations to be available")
			return false, nil
		})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			errMsg := "Timeout waiting for backup storage locations to be available"
			h.Log.Error(err, errMsg)
			return NewBRStorageBackendUnavailableError(errMsg)
		}
		return err //nolint:wrapcheck
	}

	return nil
}
