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
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/utils"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
)

var resourceWaveOrder = map[string]int{
	"Namespace":                0,
	"CustomResourceDefinition": 1,
	"ClusterRole":              2,
	"ClusterRoleBinding":       2,
	"PersistentVolume":         3,
	"StorageClass":             3,
	"PersistentVolumeClaim":    4,
	"ConfigMap":                5,
	"Secret":                   5,
	"ServiceAccount":           5,
	"Role":                     6,
	"RoleBinding":              6,
	"Service":                  7,
	"Deployment":               8,
	"StatefulSet":              8,
	"DaemonSet":                8,
	"Job":                      8,
	"CronJob":                  8,
	"Route":                    9,
	"Ingress":                  9,
}

func (h *BRHandler) StartRestore(ctx context.Context) (*RestoreTracker, error) {
	rt := &RestoreTracker{}

	backupPath := filepath.Join(hostPath, LocalBackupPath)
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		h.Log.Info("No local backup found, skipping restore")
		return rt, nil
	}

	if err := validateChecksums(backupPath); err != nil {
		return rt, NewBRFailedError("Restore",
			fmt.Sprintf("backup checksum validation failed: %s", err.Error()))
	}

	resources, err := loadBackupResources(backupPath, h.Log)
	if err != nil {
		return rt, NewBRFailedError("Restore",
			fmt.Sprintf("failed to load backup resources: %s", err.Error()))
	}

	if len(resources) == 0 {
		h.Log.Info("No resources to restore")
		return rt, nil
	}

	sortResourcesByWave(resources)

	for _, resource := range resources {
		name := resource.GetName()
		namespace := resource.GetNamespace()
		kind := resource.GetKind()

		err := h.applyResource(ctx, resource)
		if err != nil {
			rt.FailedRestores = append(rt.FailedRestores,
				fmt.Sprintf("%s/%s/%s", kind, namespace, name))
			return rt, NewBRFailedError("Restore",
				fmt.Sprintf("failed to restore %s %s/%s: %s", kind, namespace, name, err.Error()))
		}

		rt.SucceededRestores = append(rt.SucceededRestores,
			fmt.Sprintf("%s/%s/%s", kind, namespace, name))
	}

	h.Log.Info("All resources restored successfully",
		"restoredCount", len(rt.SucceededRestores))
	return rt, nil
}

func (h *BRHandler) applyResource(ctx context.Context, resource *unstructured.Unstructured) error {
	gvk := resource.GroupVersionKind()
	gvr := h.resolveGVR(gvk)

	name := resource.GetName()
	namespace := resource.GetNamespace()

	apply := func() error {
		if namespace != "" {
			return h.applyNamespacedResource(ctx, gvr, namespace, name, resource)
		}
		return h.applyClusterScopedResource(ctx, gvr, name, resource)
	}

	err := apply()
	if err == nil {
		return nil
	}

	if !isTransientError(err) {
		return err
	}

	h.Log.Info("Transient error during restore, retrying with backoff",
		"kind", gvk.Kind, "name", name, "namespace", namespace, "error", err.Error())
	return retryWithBackoff(ctx, apply)
}

func (h *BRHandler) resolveGVR(gvk schema.GroupVersionKind) schema.GroupVersionResource {
	mapping, err := h.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		gvr := schema.GroupVersionResource{
			Group:    gvk.Group,
			Version:  gvk.Version,
			Resource: pluralizeKind(gvk.Kind),
		}
		h.Log.Info("REST mapper lookup failed, using pluralized kind",
			"gvk", gvk.String(), "gvr", gvr.String(), "error", err.Error())
		return gvr
	}
	return mapping.Resource
}

func (h *BRHandler) applyNamespacedResource(ctx context.Context, gvr schema.GroupVersionResource,
	namespace, name string, resource *unstructured.Unstructured) error {

	existing, err := h.DynamicClient.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("failed to get existing resource: %w", err)
		}
		_, err = h.DynamicClient.Resource(gvr).Namespace(namespace).Create(ctx, resource, metav1.CreateOptions{})
		if err != nil && !k8serrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create resource: %w", err)
		}
		return nil
	}

	resource.SetResourceVersion(existing.GetResourceVersion())
	_, err = h.DynamicClient.Resource(gvr).Namespace(namespace).Update(ctx, resource, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update resource: %w", err)
	}
	return nil
}

func (h *BRHandler) applyClusterScopedResource(ctx context.Context, gvr schema.GroupVersionResource,
	name string, resource *unstructured.Unstructured) error {

	existing, err := h.DynamicClient.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("failed to get existing resource: %w", err)
		}
		_, err = h.DynamicClient.Resource(gvr).Create(ctx, resource, metav1.CreateOptions{})
		if err != nil && !k8serrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create resource: %w", err)
		}
		return nil
	}

	resource.SetResourceVersion(existing.GetResourceVersion())
	_, err = h.DynamicClient.Resource(gvr).Update(ctx, resource, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update resource: %w", err)
	}
	return nil
}

func isTransientError(err error) bool {
	return k8serrors.IsServerTimeout(err) || k8serrors.IsTooManyRequests(err) || k8serrors.IsServiceUnavailable(err)
}

func retryWithBackoff(ctx context.Context, fn func() error) error {
	backoff := wait.Backoff{
		Duration: 1 * time.Second,
		Factor:   2.0,
		Steps:    3,
	}

	return wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) { //nolint:wrapcheck
		err := fn()
		if err == nil {
			return true, nil
		}
		if isTransientError(err) {
			return false, nil
		}
		return false, err
	})
}

func validateChecksums(backupPath string) error {
	checksumFile := filepath.Join(backupPath, "checksums.sha256")
	data, err := os.ReadFile(checksumFile) //nolint:gosec
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read checksum file: %w", err)
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid checksum line: %s", line)
		}
		expectedHash := parts[0]
		relPath := parts[1]

		filePath := filepath.Join(backupPath, relPath)
		fileData, err := os.ReadFile(filePath) //nolint:gosec
		if err != nil {
			return fmt.Errorf("failed to read file %s for checksum validation: %w", relPath, err)
		}

		actualHash := sha256.Sum256(fileData)
		if hex.EncodeToString(actualHash[:]) != expectedHash {
			return fmt.Errorf("checksum mismatch for file %s", relPath)
		}
	}

	return nil
}

func loadBackupResources(backupPath string, log logr.Logger) ([]*unstructured.Unstructured, error) {
	var resources []*unstructured.Unstructured

	entries, err := os.ReadDir(backupPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read backup directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		specDir := filepath.Join(backupPath, entry.Name())
		yamlFiles, err := os.ReadDir(specDir)
		if err != nil {
			return nil, fmt.Errorf("failed to read spec directory %s: %w", entry.Name(), err)
		}

		count := 0
		for _, yamlFile := range yamlFiles {
			if yamlFile.IsDir() || !strings.HasSuffix(yamlFile.Name(), ".yaml") {
				continue
			}

			filePath := filepath.Join(specDir, yamlFile.Name())
			resource := &unstructured.Unstructured{}
			if err := utils.ReadYamlOrJSONFile(filePath, resource); err != nil {
				return nil, fmt.Errorf("failed to read resource file %s: %w", filePath, err)
			}

			resources = append(resources, resource)
			count++
		}

		log.Info("Loaded backup resources from spec", "spec", entry.Name(), "count", count)
	}

	return resources, nil
}

func sortResourcesByWave(resources []*unstructured.Unstructured) {
	sort.SliceStable(resources, func(i, j int) bool {
		return getWaveOrder(resources[i]) < getWaveOrder(resources[j])
	})
}

func getWaveOrder(resource *unstructured.Unstructured) int {
	kind := resource.GetKind()
	if order, ok := resourceWaveOrder[kind]; ok {
		return order
	}
	return 100
}

func pluralizeKind(kind string) string {
	lower := strings.ToLower(kind)
	switch {
	case strings.HasSuffix(lower, "s"),
		strings.HasSuffix(lower, "sh"),
		strings.HasSuffix(lower, "ch"),
		strings.HasSuffix(lower, "x"),
		strings.HasSuffix(lower, "z"):
		return lower + "es"
	case len(lower) > 1 && strings.HasSuffix(lower, "y") && !strings.ContainsAny(string(lower[len(lower)-2]), "aeiou"):
		return lower[:len(lower)-1] + "ies"
	default:
		return lower + "s"
	}
}
