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
	"time"

	"github.com/openshift-kni/lifecycle-agent/internal/common"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

type BackupTracker struct {
	PendingBackups          []string
	ProgressingBackups      []string
	SucceededBackups        []string
	FailedBackups           []string
	FailedValidationBackups []string
}

// GetSortedBackupsFromConfigmap returns a list of sorted backup CRs extracted from configmap
func (h *BRHandler) GetSortedBackupsFromConfigmap(ctx context.Context, content []lcav1alpha1.ConfigMapRef) ([][]*velerov1.Backup, error) {
	// no CM listed
	if len(content) == 0 {
		h.Log.Info("no configMap CR provided")
		return nil, nil
	}

	oadpConfigmaps, err := common.GetConfigMaps(ctx, h.Client, content)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			errMsg := fmt.Sprintf("OADP configmap not found, error: %s. Please create the configmap.", err.Error())
			h.Log.Error(nil, errMsg)
			return nil, NewBRNotFoundError(errMsg)
		}
		return nil, err
	}

	// extract backup CRs from configmaps
	backupCRs, err := h.extractBackupFromConfigmaps(ctx, oadpConfigmaps)
	if err != nil {
		return nil, err
	}

	// sort backup CRs by wave-apply
	sortedBackupGroups, err := sortByApplyWaveBackupCrs(backupCRs)
	if err != nil {
		return nil, err
	}

	return sortedBackupGroups, nil
}

// StartOrTrackBackup start backup or track backup status
func (h *BRHandler) StartOrTrackBackup(ctx context.Context, backups []*velerov1.Backup) (*BackupTracker, error) {
	// track backup status
	bt := BackupTracker{}

	for _, backup := range backups {
		var existingBackup *velerov1.Backup
		existingBackup, err := getBackup(ctx, h.Client, backup.Name, backup.Namespace)
		if err != nil {
			return &bt, err
		}

		if existingBackup == nil {
			// Backup has not been applied. Applying backup
			err := h.createNewBackupCr(ctx, backup)
			if err != nil {
				return &bt, err
			}
			bt.ProgressingBackups = append(bt.ProgressingBackups, backup.Name)

		} else {
			h.Log.Info("Backup CR status",
				"name", existingBackup.Name,
				"phase", existingBackup.Status.Phase,
				"warnings", existingBackup.Status.Warnings,
				"errors", existingBackup.Status.Errors,
				"failure", existingBackup.Status.FailureReason,
				"validation errors", existingBackup.Status.ValidationErrors,
			)

			switch existingBackup.Status.Phase {
			case velerov1.BackupPhaseCompleted:
				bt.SucceededBackups = append(bt.SucceededBackups, existingBackup.Name)
			case velerov1.BackupPhaseFailedValidation,
				velerov1.BackupPhasePartiallyFailed,
				velerov1.BackupPhaseFailed:
				bt.FailedBackups = append(bt.FailedBackups, existingBackup.Name)
			case "":
				// Backup has no status
				bt.PendingBackups = append(bt.PendingBackups, existingBackup.Name)
			default:
				bt.ProgressingBackups = append(bt.ProgressingBackups, existingBackup.Name)
			}

		}
	}

	h.Log.Info("Backups status",
		"pending backups", bt.PendingBackups,
		"progressing backups", bt.ProgressingBackups,
		"succeeded backups", bt.SucceededBackups,
		"failed backups", bt.FailedBackups,
	)
	return &bt, nil
}

func (h *BRHandler) createNewBackupCr(ctx context.Context, backup *velerov1.Backup) error {
	clusterID, err := getClusterID(ctx, h.Client)
	if err != nil {
		return err
	}

	setBackupLabel(backup, map[string]string{clusterIDLabel: clusterID})
	if err := h.Create(ctx, backup); err != nil {
		return err
	}

	h.Log.Info("Backup created", "name", backup.Name, "namespace", backup.Namespace)
	return nil
}

// sortByApplyWaveBackupCrs sorts the backups by the apply-wave annotation
func sortByApplyWaveBackupCrs(resources []*velerov1.Backup) ([][]*velerov1.Backup, error) {
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
func (h *BRHandler) extractBackupFromConfigmaps(ctx context.Context, configmaps []corev1.ConfigMap) ([]*velerov1.Backup, error) {
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
			err = h.Create(ctx, &resource, &client.CreateOptions{DryRun: []string{metav1.DryRunAll}})
			if err != nil {
				if k8serrors.IsInvalid(err) {
					errMsg := fmt.Sprintf("Invalid backup %s detected in configmap %s, error: %s. Please update the invalid CRs in configmap.",
						resource.GetName(), cm.GetName(), err.Error())
					h.Log.Error(err, errMsg)
					return nil, NewBRFailedValidationError("Backup", errMsg)
				}

				if !k8serrors.IsAlreadyExists(err) {
					return nil, err
				}

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
	dpaList := &unstructured.UnstructuredList{}
	dpaList.SetGroupVersionKind(dpaGvkList)
	opts := []client.ListOption{
		client.InNamespace(oadpNamespace),
	}
	if err := h.List(ctx, dpaList, opts...); err != nil {
		var groupDiscoveryErr *discovery.ErrGroupDiscoveryFailed
		if errors.As(err, &groupDiscoveryErr) {
			// If the CRD is not discovered, it means that OADP is not installed
			return nil
		}
		return err
	}
	if len(dpaList.Items) == 0 {
		return nil
	}

	// Create the directory for DPAs
	if err := os.MkdirAll(filepath.Join(toDir, oadpDpaPath), 0o700); err != nil {
		return err
	}

	var secrets []string
	// Write DPAs to a given directory
	for _, dpa := range dpaList.Items {
		// Unset uid and resource version
		dpa.SetUID("")
		dpa.SetResourceVersion("")

		filePath := filepath.Join(toDir, oadpDpaPath, dpa.GetName()+".yaml")
		if err := writeDpaToFile(&dpa, filePath); err != nil {
			return err
		}
		h.Log.Info("Exported DPA CR to file", "path", filePath)

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
			secrets = append(secrets, secretName)
		}
	}

	if len(secrets) == 0 {
		return nil
	}
	// Create the directory for secrets
	if err := os.MkdirAll(filepath.Join(toDir, oadpSecretPath), 0o700); err != nil {
		return err
	}

	// Write secrets
	for _, secretName := range secrets {
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
		h.Log.Info("Exported secret to file", "path", filePath)
	}
	return nil
}

// ExportRestoresToDir extracts all restore CRs from oadp configmaps and write them to a given location
// returns: error
func (h *BRHandler) ExportRestoresToDir(ctx context.Context, configMaps []lcav1alpha1.ConfigMapRef, toDir string) error {
	configmaps, err := common.GetConfigMaps(ctx, h.Client, configMaps)
	if err != nil {
		return err
	}

	restores, err := extractRestoreFromConfigmaps(ctx, h.Client, configmaps)
	if err != nil {
		return err
	}

	sortedRestores, err := sortByApplyWaveRestoreCrs(restores)
	if err != nil {
		return err
	}

	for i, restoreGroup := range sortedRestores {
		// Create a directory for each group
		group := filepath.Join(toDir, OadpRestorePath, "restore"+strconv.Itoa(i+1))
		// If the directory already exists, it does nothing
		if err := os.MkdirAll(group, 0o700); err != nil {
			return err
		}

		for j, restore := range restoreGroup {
			restoreFileName := strconv.Itoa(j+1) + "_" + restore.Name + "_" + restore.Namespace + ".yaml"
			filePath := filepath.Join(group, restoreFileName)
			if err := writeRestoreToFile(restore, filePath); err != nil {
				return err
			}
			h.Log.Info("Exported restore CR to file", "path", filePath)
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
		if errors.Is(err, context.DeadlineExceeded) {
			h.Log.Error(err, "Timeout waiting for backups to be deleted")
			return false, nil
		}
		// API call errors
		return false, err
	}
	return true, nil
}
