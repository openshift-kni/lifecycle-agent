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
	"strconv"
	"time"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/utils"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
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

func (h *BRHandler) LoadRestoresFromOadpRestorePath() ([][]*velerov1.Restore, error) {
	var sortedRestores [][]*velerov1.Restore

	// The returned list of entries are sorted by name alphabetically
	basePath := filepath.Join(hostPath, OadpRestorePath)
	manifests, err := utils.LoadGroupedManifestsFromPath(basePath, &h.Log)
	if err != nil {
		return nil, fmt.Errorf("failed to read restore manifests from path: %w", err)
	}

	for _, group := range manifests {
		restores := []*velerov1.Restore{}
		for _, manifest := range group {
			restore := &velerov1.Restore{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(manifest.Object, restore); err != nil {
				return nil, fmt.Errorf("failed to convert from unstructured to Restore: %w", err)
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

// ExportRestoresToDir extracts all restore CRs from oadp configmaps and write them to a given location
// returns: error
func (h *BRHandler) ExportRestoresToDir(ctx context.Context, configMaps []lcav1alpha1.ConfigMapRef, toDir string) error {
	configmaps, err := common.GetConfigMaps(ctx, h.Client, configMaps)
	if err != nil {
		return fmt.Errorf("failed to get configMaps: %w", err)
	}

	restores, err := common.ExtractResourcesFromConfigmaps[*velerov1.Restore](ctx, configmaps, common.RestoreGvk)
	if err != nil {
		return fmt.Errorf("failed to get restore CR from configmaps: %w", err)
	}

	sortedRestores, err := common.SortAndGroupByApplyWave[*velerov1.Restore](restores)
	if err != nil {
		return fmt.Errorf("failed to sort restore CRs: %w", err)
	}

	for i, restoreGroup := range sortedRestores {
		// Create a directory for each group
		group := filepath.Join(toDir, OadpRestorePath, "restore"+strconv.Itoa(i+1))
		// If the directory already exists, it does nothing
		if err := os.MkdirAll(group, 0o700); err != nil {
			return fmt.Errorf("failed make dir in %s: %w", group, err)
		}

		for j, restore := range restoreGroup {
			restoreFileName := strconv.Itoa(j+1) + "_" + restore.Name + "_" + restore.Namespace + yamlExt
			filePath := filepath.Join(group, restoreFileName)
			if err := utils.MarshalToYamlFile(restore, filePath); err != nil {
				return fmt.Errorf("failed marshal file %s: %w", filePath, err)
			}
			h.Log.Info("Exported restore CR to file", "path", filePath)
		}
	}

	return nil
}
