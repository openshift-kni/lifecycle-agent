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
	"strings"
	"time"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/utils"

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
)

type BackupTracker struct {
	PendingBackups          []string
	ProgressingBackups      []string
	SucceededBackups        []string
	FailedBackups           []string
	FailedValidationBackups []string
}

const (
	yamlExt = ".yaml"
)

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
		return nil, fmt.Errorf("failed to get oadp configMaps : %w", err)
	}

	// extract backup CRs from configmaps
	backupCRs, err := common.ExtractResourcesFromConfigmaps[*velerov1.Backup](ctx, oadpConfigmaps, common.BackupGvk)
	if err != nil {
		return nil, err
	}

	sortedBackupGroups, err := common.SortAndGroupByApplyWave[*velerov1.Backup](backupCRs)
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

// getObjsFromAnnotations goes through a backup annotations and returns the list
// of objects that backup label should be applied to them, example backup CR:
//
//	apiVersion: velero.io/v1
//	kind: Backup
//	metadata:
//	  name: acm-klusterlet
//	  namespace: openshift-adp
//	annotations:
//	  lca.openshift.io/apply-label: "rbac.authorization.k8s.io/v1/clusterroles/klusterlet,apps/v1/deployments/open-cluster-management-agent/klusterlet"
func getObjsFromAnnotations(backup *velerov1.Backup) ([]ObjMetadata, error) {
	result := []ObjMetadata{}
	for k, v := range backup.GetAnnotations() {
		if k != applyLabelAnn || v == "" {
			continue
		}
		objStrings := common.RemoveDuplicates[string](strings.Split(v, ","))
		for _, objString := range objStrings {
			objStringSplitted := strings.Split(objString, "/")
			if len(objStringSplitted) < 3 || len(objStringSplitted) > 5 {
				return result, fmt.Errorf("invalid apply-label obj in annotation value: %s", objString)
			}
			var obj ObjMetadata
			switch len(objStringSplitted) {
			case 3:
				obj = ObjMetadata{Version: objStringSplitted[0], Resource: objStringSplitted[1], Name: objStringSplitted[2]}
			case 4:
				if objStringSplitted[0] == "v1" {
					obj = ObjMetadata{
						Version: objStringSplitted[0], Resource: objStringSplitted[1],
						Namespace: objStringSplitted[2], Name: objStringSplitted[3],
					}
				} else {
					obj = ObjMetadata{
						Group: objStringSplitted[0], Version: objStringSplitted[1],
						Resource: objStringSplitted[2], Name: objStringSplitted[3],
					}
				}
			case 5:
				obj = ObjMetadata{
					Group: objStringSplitted[0], Version: objStringSplitted[1],
					Resource: objStringSplitted[2], Namespace: objStringSplitted[3], Name: objStringSplitted[4],
				}
			default:
				return result, fmt.Errorf("invalid apply-label obj in annotation value: %s", objString)
			}
			result = append(result, obj)
		}
	}
	return result, nil
}

// applyBackupLabels patches objects that are in apply-backup annotation with backup label
// and adds label selector to backup CR
//
// The objects could be passed using the "lca.openshift.io/apply-label" annotation.
// The value should be a list of comma separated objects in
// "<group>/<version>/<resource>/<name>" format for cluster-scoped resources
// and "<group>/<version>/<resource>/<namespace>/<name> format for namespace-scoped resources.
func (h *BRHandler) applyBackupLabels(ctx context.Context, backup *velerov1.Backup) error {
	objs, err := getObjsFromAnnotations(backup)
	if err != nil {
		return fmt.Errorf("failed to get objs from apply-label annotations: %w", err)
	}
	payload := []byte(fmt.Sprintf(`[{"op":"add","path":"/metadata/labels","value":{"%s":"%s"}}]`, backupLabel, backup.GetName()))
	for _, obj := range objs {
		err := patchObj(ctx, h.DynamicClient, &obj, false, payload) //nolint:gosec
		if err != nil {
			return fmt.Errorf("failed to apply backup label on object name:%s namespace:%s resource:%s group:%s version:%s err:%w",
				obj.Name, obj.Namespace, obj.Resource, obj.Group, obj.Version, err)
		}
	}
	if len(objs) != 0 {
		setBackupLabelSelector(backup)
	}
	return nil
}

func (h *BRHandler) cleanupBackupLabels(ctx context.Context, backup *velerov1.Backup) error {
	objs, err := getObjsFromAnnotations(backup)
	if err != nil {
		return fmt.Errorf("failed to get objs from apply-label annotations: %w", err)
	}
	// json patch escape info: https://jsonpatch.com/#json-pointer
	escaped := strings.Replace(backupLabel, "/", "~1", 1)
	payload := []byte(fmt.Sprintf(`[{"op":"remove","path":"/metadata/labels/%s"}]`, escaped))
	for _, obj := range objs {
		err := patchObj(ctx, h.DynamicClient, &obj, false, payload) //nolint:gosec
		if err != nil {
			h.Log.Error(err, "failed to remove backup label", "name", obj.Name, "namespace", obj.Namespace,
				"resource", obj.Resource, "group", obj.Group, "version", obj.Version)
		}
	}
	return nil
}

func (h *BRHandler) createNewBackupCr(ctx context.Context, backup *velerov1.Backup) error {
	clusterID, err := getClusterID(ctx, h.Client)
	if err != nil {
		return err
	}

	setBackupLabel(backup, map[string]string{clusterIDLabel: clusterID})
	if err := h.applyBackupLabels(ctx, backup); err != nil {
		return fmt.Errorf("failed to apply backup labels: %w", err)
	}
	if err := h.Create(ctx, backup); err != nil {
		return fmt.Errorf("failed to create backup: %w", err)
	}

	h.Log.Info("Backup created", "name", backup.Name, "namespace", backup.Namespace)
	return nil
}

// ExportOadpConfigurationToDir exports the OADP DataProtectionApplication CR and required storage creds to a given location
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
		return fmt.Errorf("failed to list oadp: %w", err)
	}
	if len(dpaList.Items) == 0 {
		return nil
	}
	if len(dpaList.Items) != 1 {
		errMsg := fmt.Sprintf("Only one DataProtectionApplication CR is allowed in the %s", OadpNs)
		h.Log.Error(nil, errMsg)
		return NewBRFailedError("OADP", errMsg)
	}

	// Create the directory for DPA
	dpaDir := filepath.Join(toDir, OadpDpaPath)
	if err := os.MkdirAll(dpaDir, 0o700); err != nil {
		return fmt.Errorf("failed to make dir for DPA in %s: %w", dpaDir, err)
	}

	var secrets []string
	for _, dpa := range dpaList.Items {
		// Unset uid and resource version
		dpa.SetUID("")
		dpa.SetResourceVersion("")

		filePath := filepath.Join(toDir, OadpDpaPath, dpa.GetName()+yamlExt)
		if err := utils.MarshalToYamlFile(&dpa, filePath); err != nil { //nolint:gosec
			return fmt.Errorf("failed to delete %s: %w", filePath, err)
		}
		h.Log.Info("Exported DataProtectionApplication CR to file", "path", filePath)

		// Make sure the required storage creds are exported
		dpaSpec := dpa.Object["spec"].(map[string]any)
		backupLocations := dpaSpec["backupLocations"].([]any)
		for _, backupLocation := range backupLocations {
			if backupLocation.(map[string]any)["velero"] == nil {
				continue
			}
			velero := backupLocation.(map[string]any)["velero"].(map[string]any)
			if velero["credential"] == nil {
				continue
			}

			creds := velero["credential"].(map[string]any)
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
	if err := os.MkdirAll(filepath.Join(toDir, OadpSecretPath), 0o700); err != nil {
		return fmt.Errorf("failed to make oadp secret path in %s: %w", OadpDpaPath, err)
	}

	// Write secrets
	for _, secretName := range secrets {
		storageSecret := &corev1.Secret{}
		if err := h.Get(ctx, types.NamespacedName{
			Name:      secretName,
			Namespace: oadpNamespace,
		}, storageSecret); err != nil {
			return fmt.Errorf("failed to get storageSecret: %w", err)
		}

		storageSecret.SetUID("")
		storageSecret.SetResourceVersion("")

		filePath := filepath.Join(toDir, OadpSecretPath, secretName+yamlExt)
		if err := utils.MarshalToYamlFile(storageSecret, filePath); err != nil {
			return fmt.Errorf("failed to marshal oadp secret %s: %w", filePath, err)
		}
		h.Log.Info("Exported secret to file", "path", filePath)
	}
	return nil
}

// CleanupBackups deletes all backups for this cluster from object storage
func (h *BRHandler) CleanupBackups(ctx context.Context) error {
	// Get the cluster ID
	clusterID, err := getClusterID(ctx, h.Client)
	if err != nil {
		return err
	}

	// List all backups created for this cluster
	backupList := &velerov1.BackupList{}
	if err := h.List(ctx, backupList, client.MatchingLabels{
		clusterIDLabel: clusterID,
	}); err != nil {
		var groupDiscoveryErr *discovery.ErrGroupDiscoveryFailed
		if errors.As(err, &groupDiscoveryErr) {
			h.Log.Info("Backup CR is not installed, nothing to cleanup")
			return nil
		}
		return fmt.Errorf("failed to list Backup: %w", err)
	}

	// Create deleteBackupRequest CR to delete the backup in the object storage
	for _, backup := range backupList.Items {
		deleteBackupRequest := &velerov1.DeleteBackupRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      backup.Name,
				Namespace: backup.Namespace,
				Labels:    map[string]string{clusterIDLabel: clusterID},
			},
			Spec: velerov1.DeleteBackupRequestSpec{
				BackupName: backup.Name,
			},
		}

		if err := h.Create(ctx, deleteBackupRequest); err != nil {
			return fmt.Errorf("could not apply deleteBackupRequest CR: %w", err)
		}
		h.Log.Info("Backup deletion request has sent", "backup", backup.Name)
	}

	for _, backup := range backupList.Items {
		err := h.cleanupBackupLabels(ctx, &backup) //nolint:gosec
		if err != nil {
			h.Log.Error(err, "failed to clean backup labels")
		}
	}

	return h.ensureBackupsDeleted(ctx, backupList.Items)
}

// CleanupStaleBackups checks and deletes if there are any stale Backups (with the same name) that
// may be available in the object storage but do not belong to this cluster.
// returns: true if all stale backups have been deleted, error
func (h *BRHandler) CleanupStaleBackups(ctx context.Context, backups []*velerov1.Backup) error {
	// Get the cluster ID
	clusterID, err := getClusterID(ctx, h.Client)
	if err != nil {
		return err
	}

	// List of stale backups present in this cluster
	staleBackupList := &velerov1.BackupList{}

	for _, backup := range backups {

		// If the target cluster is reinstalled before cleaning up any previous Backups from
		// the object storage, OADP may sync those stale Backup CRs back to this cluster, even
		// though they are labeled with a different cluster ID.
		existingBackup, err := getBackup(ctx, h.Client, backup.Name, backup.Namespace)
		if err != nil {
			return err
		} else if existingBackup == nil {
			// Backup isn't found, continue to the next one
			continue
		}

		// Check cluster ID label of existingBackup
		labels := existingBackup.GetLabels()
		if labels != nil && labels[clusterIDLabel] != clusterID {
			staleBackupList.Items = append(staleBackupList.Items, *existingBackup)

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
				return fmt.Errorf("could not apply DeleteBackupRequest CR: %w", err)
			}
			h.Log.Info("Found stale Backup, DeleteBackupRequest has been sent", "backup", backup.Name)
		}
	}

	if len(staleBackupList.Items) == 0 {
		h.Log.Info("No stale Backups found in the cluster, skipping")
		return nil
	}

	// Ensure all backups are deleted
	return h.ensureBackupsDeleted(ctx, staleBackupList.Items)
}

func (h *BRHandler) waitForDeleteBackupRequests(ctx context.Context, backups []velerov1.Backup) error {
	return wait.PollUntilContextTimeout(ctx, 1*time.Second, 5*time.Minute, true, //nolint:wrapcheck
		func(ctx context.Context) (bool, error) {
			for _, backup := range backups {
				backupRequest := &velerov1.DeleteBackupRequest{}
				if err := h.Get(ctx, types.NamespacedName{
					Name:      backup.Name,
					Namespace: backup.Namespace,
				}, backupRequest); err != nil {
					if !k8serrors.IsNotFound(err) {
						return false, nil
					}
				} else {
					if backupRequest.Status.Phase != velerov1.DeleteBackupRequestPhaseProcessed {
						h.Log.Info("Waiting for DeleteBackupRequest to be processed", "deleteBackupRequest", backupRequest.GetName(), "phase", backupRequest.Status.Phase)
						return false, nil
					}
					if len(backupRequest.Status.Errors) != 0 {
						return true, fmt.Errorf("deleteBackupRequest %s failed with errors: %v", backupRequest.GetName(), backupRequest.Status.Errors)
					}
				}
			}
			return true, nil
		})
}

func (h *BRHandler) ensureBackupsDeleted(ctx context.Context, backups []velerov1.Backup) error {
	if err := h.waitForDeleteBackupRequests(ctx, backups); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			h.Log.Error(err, "Timeout waiting for backups to be deleted")
			return err
		}
		return fmt.Errorf("failed deleting backups: %w", err)
	}

	for _, backup := range backups {
		err := common.RetryOnRetriable(common.RetryBackoffTwoMinutes, func() error {
			return h.Get(ctx, types.NamespacedName{ //nolint:wrapcheck
				Name:      backup.Name,
				Namespace: backup.Namespace,
			}, &velerov1.Backup{})
		})
		if err != nil {
			if k8serrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("failed to ensure backup %s is deleted: %w", backup.GetName(), err)
		} else {
			return fmt.Errorf("all DeleteBackupRequests are processed, but backup %s is not deleted", backup.GetName())
		}
	}
	h.Log.Info("All Backup CRs have been deleted successfully")
	return nil
}

// CleanupDeleteBackupRequests deletes all DeleteBackupRequest for this cluster from object storage
func (h *BRHandler) CleanupDeleteBackupRequests(ctx context.Context) error {
	// Get the cluster ID
	clusterID, err := getClusterID(ctx, h.Client)
	if err != nil {
		return err
	}

	// List all DeleteBackupRequest CRs created for this cluster
	deleteBackupRequestList := &velerov1.DeleteBackupRequestList{}
	if err := h.List(ctx, deleteBackupRequestList, client.MatchingLabels{
		clusterIDLabel: clusterID,
	}); err != nil {
		var groupDiscoveryErr *discovery.ErrGroupDiscoveryFailed
		if errors.As(err, &groupDiscoveryErr) {
			h.Log.Info("DeleteBackupRequest CR is not installed, nothing to cleanup")
			return nil
		}
		return fmt.Errorf("failed to list DeleteBackupRequest CRs: %w", err)
	}

	// Cleanup all DeleteBackupRequest CRs
	for _, deleteBackupRequest := range deleteBackupRequestList.Items {
		deleteBackupRequestName := deleteBackupRequest.GetName()

		h.Log.Info(fmt.Sprintf("Deleting DeleteBackupRequest CR %s", deleteBackupRequestName))
		if err := h.Delete(ctx, deleteBackupRequest.DeepCopy()); err != nil {
			return fmt.Errorf("failed to delete DeleteBackupRequest CR %s: %w", deleteBackupRequestName, err)
		}
	}

	h.Log.Info("All DeleteBackupRequest CRs have been deleted")
	return nil
}
