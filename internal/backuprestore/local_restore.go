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
	"strconv"
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
	"Secret":                   5, //nolint:goconst
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

	validatedFiles, err := validateChecksums(backupPath)
	if err != nil {
		return rt, NewBRFailedError("Restore",
			fmt.Sprintf("backup checksum validation failed: %s", err.Error()))
	}

	waveGroups, err := loadBackupResourcesByWave(backupPath, validatedFiles, h.Log)
	if err != nil {
		return rt, NewBRFailedError("Restore",
			fmt.Sprintf("failed to load backup resources: %s", err.Error()))
	}

	if len(waveGroups) == 0 {
		h.Log.Info("No resources to restore")
		return rt, nil
	}

	for _, wg := range waveGroups {
		h.Log.Info("Processing restore wave group", "wave", wg.wave, "resourceCount", len(wg.resources))

		sortResourcesByWave(wg.resources)

		for _, resource := range wg.resources {
			name := resource.GetName()
			namespace := resource.GetNamespace()
			kind := resource.GetKind()

			if shouldSkipRestore(resource) {
				h.Log.Info("Skipping auto-generated resource during restore",
					"kind", kind, "name", name, "namespace", namespace)
				continue
			}

			savedStatus, hasStatus := resource.Object["status"]
			prepareResourceForRestore(resource)

			unstructured.RemoveNestedField(resource.Object, "status")

			err := h.applyResource(ctx, resource)
			if err != nil {
				rt.FailedRestores = append(rt.FailedRestores,
					fmt.Sprintf("%s/%s/%s", kind, namespace, name))
				return rt, NewBRFailedError("Restore",
					fmt.Sprintf("failed to restore %s %s/%s: %s", kind, namespace, name, err.Error()))
			}

			if hasStatus && savedStatus != nil {
				if err := h.applyStatusSubresource(ctx, resource, savedStatus); err != nil {
					h.Log.Info("Failed to restore status subresource, continuing",
						"kind", kind, "name", name, "namespace", namespace, "error", err.Error())
				}
			}

			rt.SucceededRestores = append(rt.SucceededRestores,
				fmt.Sprintf("%s/%s/%s", kind, namespace, name))
		}
	}

	h.Log.Info("All resources restored successfully",
		"restoredCount", len(rt.SucceededRestores))
	return rt, nil
}

func (h *BRHandler) applyStatusSubresource(ctx context.Context, resource *unstructured.Unstructured, status interface{}) error {
	gvk := resource.GroupVersionKind()
	gvr := h.resolveGVR(gvk)

	name := resource.GetName()
	namespace := resource.GetNamespace()

	var existing *unstructured.Unstructured
	var err error
	if namespace != "" {
		existing, err = h.DynamicClient.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	} else {
		existing, err = h.DynamicClient.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		return fmt.Errorf("failed to get resource for status update: %w", err)
	}

	existing.Object["status"] = status

	if namespace != "" {
		_, err = h.DynamicClient.Resource(gvr).Namespace(namespace).UpdateStatus(ctx, existing, metav1.UpdateOptions{})
	} else {
		_, err = h.DynamicClient.Resource(gvr).UpdateStatus(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("failed to update status subresource: %w", err)
	}
	return nil
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

func validateChecksums(backupPath string) (map[string]bool, error) {
	checksumFile := filepath.Join(backupPath, "checksums.sha256")
	data, err := os.ReadFile(checksumFile) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("checksum manifest checksums.sha256 is missing or unreadable: %w", err)
	}

	validatedFiles := make(map[string]bool)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid checksum line: %s", line)
		}
		expectedHash := parts[0]
		relPath := parts[1]

		filePath := filepath.Join(backupPath, relPath)
		fileData, err := os.ReadFile(filePath) //nolint:gosec
		if err != nil {
			return nil, fmt.Errorf("failed to read file %s for checksum validation: %w", relPath, err)
		}

		actualHash := sha256.Sum256(fileData)
		if hex.EncodeToString(actualHash[:]) != expectedHash {
			return nil, fmt.Errorf("checksum mismatch for file %s", relPath)
		}
		validatedFiles[relPath] = true
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to scan checksum manifest: %w", err)
	}

	return validatedFiles, nil
}

type waveGroup struct {
	wave      int
	resources []*unstructured.Unstructured
}

func parseWavePrefix(dirName string) (int, string) {
	if idx := strings.Index(dirName, "_"); idx > 0 {
		if w, err := strconv.Atoi(dirName[:idx]); err == nil {
			return w, dirName[idx+1:]
		}
	}
	return 999, dirName
}

func loadBackupResourcesByWave(backupPath string, validatedFiles map[string]bool, log logr.Logger) ([]waveGroup, error) {
	entries, err := os.ReadDir(backupPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read backup directory: %w", err)
	}

	type dirEntry struct {
		wave    int
		dirName string
	}
	var dirs []dirEntry
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		wave, _ := parseWavePrefix(entry.Name())
		dirs = append(dirs, dirEntry{wave: wave, dirName: entry.Name()})
	}

	sort.SliceStable(dirs, func(i, j int) bool {
		return dirs[i].wave < dirs[j].wave
	})

	waveMap := make(map[int]*waveGroup)
	var waveOrder []int

	for _, d := range dirs {
		specDir := filepath.Join(backupPath, d.dirName)
		yamlFiles, err := os.ReadDir(specDir)
		if err != nil {
			return nil, fmt.Errorf("failed to read spec directory %s: %w", d.dirName, err)
		}

		count := 0
		for _, yamlFile := range yamlFiles {
			if yamlFile.IsDir() || !strings.HasSuffix(yamlFile.Name(), ".yaml") {
				continue
			}

			relPath := filepath.Join(d.dirName, yamlFile.Name())
			if !validatedFiles[relPath] {
				return nil, fmt.Errorf("file %s is not listed in checksum manifest, refusing to restore", relPath)
			}

			filePath := filepath.Join(specDir, yamlFile.Name())
			resource := &unstructured.Unstructured{}
			if err := utils.ReadYamlOrJSONFile(filePath, resource); err != nil {
				return nil, fmt.Errorf("failed to read resource file %s: %w", filePath, err)
			}

			wg, ok := waveMap[d.wave]
			if !ok {
				wg = &waveGroup{wave: d.wave}
				waveMap[d.wave] = wg
				waveOrder = append(waveOrder, d.wave)
			}
			wg.resources = append(wg.resources, resource)
			count++
		}

		log.Info("Loaded backup resources from spec", "spec", d.dirName, "count", count)
	}

	var result []waveGroup
	for _, w := range waveOrder {
		result = append(result, *waveMap[w])
	}
	return result, nil
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

func shouldSkipRestore(resource *unstructured.Unstructured) bool {
	if resource.GetKind() == "Secret" {
		return skipAutoGeneratedSecret(resource)
	}
	return false
}

// prepareResourceForRestore applies Velero-compatible transformations to
// resources before they are applied to the cluster, matching the behavior
// of Velero's built-in restore item actions and the openshift-velero-plugin.
func prepareResourceForRestore(resource *unstructured.Unstructured) {
	kind := resource.GetKind()
	switch kind {
	case "Service":
		prepareServiceForRestore(resource)
	case "ServiceAccount":
		prepareServiceAccountForRestore(resource)
	case "Secret":
		prepareSecretForRestore(resource)
	case "PersistentVolumeClaim":
		preparePVCForRestore(resource)
	case "Job":
		prepareJobForRestore(resource)
	case "Route":
		prepareRouteForRestore(resource)
	}
}

func prepareServiceForRestore(resource *unstructured.Unstructured) {
	clusterIP, _, _ := unstructured.NestedString(resource.Object, "spec", "clusterIP")
	if clusterIP != "None" {
		unstructured.RemoveNestedField(resource.Object, "spec", "clusterIP")
		unstructured.RemoveNestedField(resource.Object, "spec", "clusterIPs")
	}

	unstructured.RemoveNestedField(resource.Object, "spec", "healthCheckNodePort")

	ports, found, _ := unstructured.NestedSlice(resource.Object, "spec", "ports")
	if found {
		for i, p := range ports {
			port, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			delete(port, "nodePort")
			ports[i] = port
		}
		_ = unstructured.SetNestedSlice(resource.Object, ports, "spec", "ports")
	}

	svcType, _, _ := unstructured.NestedString(resource.Object, "spec", "type")
	if svcType == "LoadBalancer" {
		unstructured.RemoveNestedField(resource.Object, "spec", "externalIPs")
	}
}

func prepareServiceAccountForRestore(resource *unstructured.Unstructured) {
	saName := resource.GetName()

	secrets, found, _ := unstructured.NestedSlice(resource.Object, "secrets")
	if found {
		filtered := filterObjectRefsByPrefix(secrets, saName+"-token-", saName+"-dockercfg-")
		_ = unstructured.SetNestedSlice(resource.Object, filtered, "secrets")
	}

	imagePullSecrets, found, _ := unstructured.NestedSlice(resource.Object, "imagePullSecrets")
	if found {
		filtered := filterObjectRefsByPrefix(imagePullSecrets, saName+"-dockercfg-")
		_ = unstructured.SetNestedSlice(resource.Object, filtered, "imagePullSecrets")
	}
}

func filterObjectRefsByPrefix(refs []interface{}, prefixes ...string) []interface{} {
	var result []interface{}
	for _, ref := range refs {
		refMap, ok := ref.(map[string]interface{})
		if !ok {
			result = append(result, ref)
			continue
		}
		name, _ := refMap["name"].(string)
		skip := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				skip = true
				break
			}
		}
		if !skip {
			result = append(result, ref)
		}
	}
	return result
}

func skipAutoGeneratedSecret(resource *unstructured.Unstructured) bool {
	secretType, _, _ := unstructured.NestedString(resource.Object, "type")
	if secretType != "kubernetes.io/service-account-token" { //nolint:gosec // K8s secret type constant, not credentials
		return false
	}
	name := resource.GetName()
	annotations := resource.GetAnnotations()
	if annotations == nil {
		return false
	}
	saName := annotations["kubernetes.io/service-account.name"]
	if saName == "" {
		return false
	}
	return strings.HasPrefix(name, saName+"-token-")
}

func prepareSecretForRestore(resource *unstructured.Unstructured) {
	secretType, _, _ := unstructured.NestedString(resource.Object, "type")
	if secretType != "kubernetes.io/service-account-token" { //nolint:gosec // K8s secret type constant, not credentials
		return
	}
	annotations := resource.GetAnnotations()
	if annotations != nil {
		delete(annotations, "kubernetes.io/service-account.uid")
		resource.SetAnnotations(annotations)
	}
	data, found, _ := unstructured.NestedMap(resource.Object, "data")
	if found {
		delete(data, "token")
		delete(data, "ca.crt")
		_ = unstructured.SetNestedField(resource.Object, data, "data")
	}
}

func preparePVCForRestore(resource *unstructured.Unstructured) {
	annotations := resource.GetAnnotations()
	if annotations != nil {
		delete(annotations, "pv.kubernetes.io/bind-completed")
		delete(annotations, "pv.kubernetes.io/bound-by-controller")
		delete(annotations, "volume.kubernetes.io/storage-provisioner")
		delete(annotations, "volume.beta.kubernetes.io/storage-provisioner")
		delete(annotations, "volume.kubernetes.io/selected-node")
		if len(annotations) == 0 {
			resource.SetAnnotations(nil)
		} else {
			resource.SetAnnotations(annotations)
		}
	}
}

func prepareJobForRestore(resource *unstructured.Unstructured) {
	controllerUIDKeys := []string{"controller-uid", "batch.kubernetes.io/controller-uid"}
	for _, key := range controllerUIDKeys {
		unstructured.RemoveNestedField(resource.Object, "spec", "selector", "matchLabels", key)
		unstructured.RemoveNestedField(resource.Object, "spec", "template", "metadata", "labels", key)
	}
}

func prepareRouteForRestore(resource *unstructured.Unstructured) {
	annotations := resource.GetAnnotations()
	if annotations != nil && annotations["openshift.io/host.generated"] == "true" { //nolint:goconst
		unstructured.RemoveNestedField(resource.Object, "spec", "host")
	}
}
