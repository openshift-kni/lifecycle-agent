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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8syaml "sigs.k8s.io/yaml"
)

func (h *BRHandler) ValidateBackupConfigmaps(ctx context.Context, content []ibuv1.ConfigMapRef) error {
	configmaps, err := common.GetConfigMaps(ctx, h.Client, content)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			errMsg := fmt.Sprintf("Backup configmap not found, error: %s. Please create the configmap.", err.Error())
			h.Log.Error(nil, errMsg)
			return NewBRFailedValidationError("Backup", errMsg)
		}
		return fmt.Errorf("failed to get backup configmaps: %w", err)
	}

	backupSpecs, err := ExtractBackupSpecsFromConfigmaps(configmaps)
	if err != nil {
		return NewBRFailedValidationError("Backup", err.Error())
	}

	if len(backupSpecs) == 0 {
		h.Log.Info("No backup specs found in configmaps")
		return nil
	}

	for _, spec := range backupSpecs {
		objs, err := getObjsFromApplyLabel(spec.ApplyLabel)
		if err != nil {
			return NewBRFailedValidationError("Backup", err.Error())
		}
		payload := []byte(fmt.Sprintf(`{"metadata": {"labels": {"%s": "%s"}}}`, backupLabel, spec.Name))
		for _, obj := range objs {
			err := patchObj(ctx, h.DynamicClient, &obj, true, payload, types.MergePatchType) //nolint:gosec
			if err != nil {
				return NewBRFailedValidationError("Backup",
					fmt.Sprintf("failed to validate apply-label objects: %s", err.Error()))
			}
		}
	}

	h.Log.Info("Backup configmaps validated", "configMaps", content)
	return nil
}

func (h *BRHandler) StartBackup(ctx context.Context, content []ibuv1.ConfigMapRef, targetDir string) (*BackupTracker, error) {
	bt := &BackupTracker{}

	if len(content) == 0 {
		h.Log.Info("No backup content provided, skipping")
		return bt, nil
	}

	configmaps, err := common.GetConfigMaps(ctx, h.Client, content)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			errMsg := fmt.Sprintf("Backup configmap not found: %s", err.Error())
			return bt, NewBRFailedValidationError("Backup", errMsg)
		}
		return bt, fmt.Errorf("failed to get backup configmaps: %w", err)
	}

	backupSpecs, err := ExtractBackupSpecsFromConfigmaps(configmaps)
	if err != nil {
		return bt, NewBRFailedValidationError("Backup", err.Error())
	}

	if len(backupSpecs) == 0 {
		h.Log.Info("No backup specs found, skipping")
		return bt, nil
	}

	sortedGroups, err := SortBackupSpecsByApplyWave(backupSpecs)
	if err != nil {
		return bt, fmt.Errorf("failed to sort backup specs: %w", err)
	}

	backupDir := filepath.Join(targetDir, LocalBackupPath)
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return bt, fmt.Errorf("failed to create backup directory %s: %w", backupDir, err)
	}

	var checksums []string

	for groupIdx, group := range sortedGroups {
		h.Log.Info("Processing backup group", "groupIndex", groupIdx+1, "totalGroups", len(sortedGroups))
		for _, spec := range group {
			if err := validateBackupName(spec.Name); err != nil {
				bt.FailedBackups = append(bt.FailedBackups, spec.Name)
				return bt, NewBRFailedValidationError("Backup", err.Error())
			}

			resources, err := h.fetchResources(ctx, spec)
			if err != nil {
				bt.FailedBackups = append(bt.FailedBackups, spec.Name)
				return bt, NewBRFailedError("Backup",
					fmt.Sprintf("failed to fetch resources for backup %s: %s", spec.Name, err.Error()))
			}

			specDir := filepath.Join(backupDir, spec.Name)
			if err := os.MkdirAll(specDir, 0o700); err != nil {
				bt.FailedBackups = append(bt.FailedBackups, spec.Name)
				return bt, fmt.Errorf("failed to create backup spec directory: %w", err)
			}

			for i, resource := range resources {
				stripTransientMetadata(resource)

				data, err := k8syaml.Marshal(resource.Object)
				if err != nil {
					bt.FailedBackups = append(bt.FailedBackups, spec.Name)
					return bt, fmt.Errorf("failed to marshal resource: %w", err)
				}

				ns := resource.GetNamespace()
				name := resource.GetName()
				kind := resource.GetKind()
				fileName := fmt.Sprintf("%d_%s_%s_%s.yaml", i, strings.ToLower(kind), ns, name)
				filePath := filepath.Join(specDir, fileName)

				if err := os.WriteFile(filePath, data, 0o600); err != nil {
					bt.FailedBackups = append(bt.FailedBackups, spec.Name)
					return bt, fmt.Errorf("failed to write resource file: %w", err)
				}

				hash := sha256.Sum256(data)
				checksums = append(checksums, fmt.Sprintf("%s  %s",
					hex.EncodeToString(hash[:]), filepath.Join(spec.Name, fileName)))
			}

			h.Log.Info("Backup completed for spec", "name", spec.Name, "resourceCount", len(resources))
			bt.SucceededBackups = append(bt.SucceededBackups, spec.Name)
		}
	}

	checksumContent := strings.Join(checksums, "\n") + "\n"
	checksumPath := filepath.Join(backupDir, "checksums.sha256")
	if err := os.WriteFile(checksumPath, []byte(checksumContent), 0o600); err != nil {
		return bt, fmt.Errorf("failed to write checksum manifest: %w", err)
	}

	h.Log.Info("All backups completed successfully",
		"succeeded", bt.SucceededBackups,
		"targetDir", backupDir)
	return bt, nil
}

func (h *BRHandler) CleanupBackups(_ context.Context) error {
	backupPath := filepath.Join(hostPath, LocalBackupPath)
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		h.Log.Info("No local backup files to cleanup")
		return nil
	}

	if err := os.RemoveAll(backupPath); err != nil {
		return fmt.Errorf("failed to cleanup local backup files at %s: %w", backupPath, err)
	}

	h.Log.Info("Local backup files cleaned up", "path", backupPath)
	return nil
}

func (h *BRHandler) fetchResources(ctx context.Context, spec BackupSpec) ([]*unstructured.Unstructured, error) {
	var resources []*unstructured.Unstructured

	applyLabelObjs, err := getObjsFromApplyLabel(spec.ApplyLabel)
	if err != nil {
		return nil, fmt.Errorf("failed to parse apply-label: %w", err)
	}
	for _, obj := range applyLabelObjs {
		resource, err := h.getResourceByMetadata(ctx, &obj)
		if err != nil {
			return nil, fmt.Errorf("failed to get resource from apply-label %s/%s: %w", obj.Resource, obj.Name, err)
		}
		if resource != nil {
			resources = append(resources, resource)
		}
	}

	for _, ns := range spec.IncludedNamespaces {
		nsResources, err := h.fetchNamespacedResources(ctx, ns, spec)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch resources in namespace %s: %w", ns, err)
		}
		resources = append(resources, nsResources...)
	}

	for _, gvr := range spec.IncludedClusterScopedResources {
		clusterResources, err := h.fetchClusterScopedResources(ctx, gvr, spec)
		if err != nil {
			h.Log.Info("Skipping cluster-scoped resource", "resource", gvr, "reason", err.Error())
			continue
		}
		resources = append(resources, clusterResources...)
	}

	return deduplicateResources(resources), nil
}

func (h *BRHandler) getResourceByMetadata(ctx context.Context, obj *ObjMetadata) (*unstructured.Unstructured, error) {
	gvr := schema.GroupVersionResource{
		Group:    obj.Group,
		Version:  obj.Version,
		Resource: obj.Resource,
	}

	var result *unstructured.Unstructured
	var err error

	if obj.Namespace != "" {
		result, err = h.DynamicClient.Resource(gvr).Namespace(obj.Namespace).Get(ctx, obj.Name, metav1.GetOptions{})
	} else {
		result, err = h.DynamicClient.Resource(gvr).Get(ctx, obj.Name, metav1.GetOptions{})
	}

	if err != nil {
		if k8serrors.IsNotFound(err) {
			h.Log.Info("Resource not found, skipping", "resource", obj.Resource, "name", obj.Name)
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get resource: %w", err)
	}
	return result, nil
}

func (h *BRHandler) fetchNamespacedResources(ctx context.Context, namespace string, spec BackupSpec) ([]*unstructured.Unstructured, error) {
	var resources []*unstructured.Unstructured

	resourceTypes := spec.IncludedNamespaceScopedResources
	if len(resourceTypes) == 0 {
		resourceTypes = []string{
			"configmaps", "secrets", "services", "serviceaccounts",
			"persistentvolumeclaims",
			"deployments.apps", "statefulsets.apps", "daemonsets.apps",
			"roles.rbac.authorization.k8s.io", "rolebindings.rbac.authorization.k8s.io",
			"routes.route.openshift.io",
		}
	}

	listOpts := metav1.ListOptions{}
	if spec.LabelSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(spec.LabelSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to convert label selector: %w", err)
		}
		listOpts.LabelSelector = selector.String()
	}

	for _, resourceType := range resourceTypes {
		if isExcluded(resourceType, spec.ExcludedResources) {
			continue
		}

		gvr := parseResourceType(resourceType)
		list, err := h.DynamicClient.Resource(gvr).Namespace(namespace).List(ctx, listOpts)
		if err != nil {
			if k8serrors.IsNotFound(err) || isResourceNotRegistered(err) {
				continue
			}
			h.Log.Info("Failed to list resources, skipping",
				"resource", resourceType, "namespace", namespace, "error", err.Error())
			continue
		}
		for i := range list.Items {
			resources = append(resources, &list.Items[i])
		}
	}

	return resources, nil
}

func (h *BRHandler) fetchClusterScopedResources(ctx context.Context, resourceType string, spec BackupSpec) ([]*unstructured.Unstructured, error) {
	if isExcluded(resourceType, spec.ExcludedResources) {
		return nil, nil
	}

	gvr := parseResourceType(resourceType)

	listOpts := metav1.ListOptions{}
	if spec.LabelSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(spec.LabelSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to convert label selector: %w", err)
		}
		listOpts.LabelSelector = selector.String()
	}

	list, err := h.DynamicClient.Resource(gvr).List(ctx, listOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to list cluster-scoped resource %s: %w", resourceType, err)
	}

	var resources []*unstructured.Unstructured
	for i := range list.Items {
		resources = append(resources, &list.Items[i])
	}
	return resources, nil
}

func stripTransientMetadata(resource *unstructured.Unstructured) {
	resource.SetUID("")
	resource.SetResourceVersion("")
	resource.SetGeneration(0)
	resource.SetCreationTimestamp(metav1.Time{})
	resource.SetManagedFields(nil)

	annotations := resource.GetAnnotations()
	if annotations != nil {
		delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
		if len(annotations) == 0 {
			resource.SetAnnotations(nil)
		} else {
			resource.SetAnnotations(annotations)
		}
	}

	unstructured.RemoveNestedField(resource.Object, "status")
	unstructured.RemoveNestedField(resource.Object, "metadata", "deletionTimestamp")
	unstructured.RemoveNestedField(resource.Object, "metadata", "deletionGracePeriodSeconds")
	unstructured.RemoveNestedField(resource.Object, "metadata", "ownerReferences")
	unstructured.RemoveNestedField(resource.Object, "metadata", "finalizers")
	unstructured.RemoveNestedField(resource.Object, "metadata", "selfLink")
}

func isExcluded(resource string, excludedResources []string) bool {
	for _, excluded := range excludedResources {
		if strings.EqualFold(resource, excluded) {
			return true
		}
		parts := strings.SplitN(resource, ".", 2)
		if len(parts) > 0 && strings.EqualFold(parts[0], excluded) {
			return true
		}
	}
	return false
}

func parseResourceType(resourceType string) schema.GroupVersionResource {
	parts := strings.SplitN(resourceType, ".", 2)
	resource := parts[0]
	group := ""
	if len(parts) > 1 {
		group = parts[1]
	}
	return schema.GroupVersionResource{
		Group:    group,
		Version:  "v1",
		Resource: resource,
	}
}

func isResourceNotRegistered(err error) bool {
	return strings.Contains(err.Error(), "the server could not find the requested resource") ||
		strings.Contains(err.Error(), "no matches for kind")
}

func validateBackupName(name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("invalid backup name %q: must not contain path separators or '..'", name)
	}
	return nil
}

func deduplicateResources(resources []*unstructured.Unstructured) []*unstructured.Unstructured {
	seen := make(map[string]bool)
	var result []*unstructured.Unstructured
	for _, r := range resources {
		key := fmt.Sprintf("%s/%s/%s/%s",
			r.GetAPIVersion(), r.GetKind(), r.GetNamespace(), r.GetName())
		if !seen[key] {
			seen[key] = true
			result = append(result, r)
		}
	}
	return result
}
