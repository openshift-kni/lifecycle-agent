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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/openshift-kni/lifecycle-agent/utils"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"
)

// crdEstablishTimeout bounds how long restore waits for a freshly-applied
// CustomResourceDefinition to be reported as Established by the API server
// before dependent custom resources are restored.
const crdEstablishTimeout = 60 * time.Second

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

func (h *BRHandler) StartRestore(ctx context.Context, content []ibuv1.ConfigMapRef) (*RestoreTracker, error) {
	rt := &RestoreTracker{}

	backupPath := filepath.Join(hostPath, LocalBackupPath)
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		// A missing backup directory is only acceptable when the user did not
		// configure any backup content. If content was configured but no backup
		// exists on disk, the backup is gone (e.g. never taken, or wiped) and we
		// must fail the restore hard rather than silently succeeding, otherwise
		// the upgrade would complete without the user's application data.
		if len(content) > 0 {
			return rt, NewBRFailedError("Restore",
				fmt.Sprintf("backup content was configured but no local backup found at %s", backupPath))
		}
		h.Log.Info("No backup content configured and no local backup found, skipping restore")
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

	// Flatten the wave groups into a single ordered list, preserving wave-group
	// order and the intra-wave kind ordering.
	var ordered []resourceEntry
	for _, wg := range waveGroups {
		h.Log.Info("Loaded restore wave group", "wave", wg.wave, "resourceCount", len(wg.resources))
		sortResourcesByWave(wg.resources)
		ordered = append(ordered, wg.resources...)
	}

	// Apply CustomResourceDefinitions first, regardless of their configured
	// wave. Dependent custom resources must be sent to a registered, established
	// endpoint; after each CRD is applied we wait for it to become established
	// and refresh discovery (see restoreResourceEntry). Non-CRD resources are
	// then applied in wave order.
	for _, entry := range ordered {
		if !isCRD(entry.resource) {
			continue
		}
		if err := h.restoreResourceEntry(ctx, entry, rt); err != nil {
			return rt, err
		}
	}
	for _, entry := range ordered {
		if isCRD(entry.resource) {
			continue
		}
		if err := h.restoreResourceEntry(ctx, entry, rt); err != nil {
			return rt, err
		}
	}

	h.Log.Info("All resources restored successfully",
		"restoredCount", len(rt.SucceededRestores))
	return rt, nil
}

// restoreResourceEntry applies a single backed-up resource to the cluster and
// records the outcome in the tracker. When the resource is a CRD it waits for
// the CRD to become established and refreshes discovery so that dependent
// custom resources resolve to the correct, registered endpoint.
func (h *BRHandler) restoreResourceEntry(ctx context.Context, entry resourceEntry, rt *RestoreTracker) error {
	resource := entry.resource
	name := resource.GetName()
	namespace := resource.GetNamespace()
	kind := resource.GetKind()

	if shouldSkipRestore(resource) {
		h.Log.Info("Skipping auto-generated resource during restore",
			"kind", kind, "name", name, "namespace", namespace)
		return nil
	}

	savedStatus, hasStatus := resource.Object["status"]
	prepareResourceForRestore(resource)

	unstructured.RemoveNestedField(resource.Object, "status")

	if err := h.applyResource(ctx, resource); err != nil {
		rt.FailedRestores = append(rt.FailedRestores,
			fmt.Sprintf("%s/%s/%s", kind, namespace, name))
		return NewBRFailedError("Restore",
			fmt.Sprintf("failed to restore %s %s/%s: %s", kind, namespace, name, err.Error()))
	}

	// A newly created or updated CRD is not immediately usable: wait until the
	// API server reports it Established and refresh discovery so the dependent
	// custom resources restored afterwards resolve to the right GVR.
	if isCRD(resource) {
		if err := h.waitForCRDEstablished(ctx, name); err != nil {
			rt.FailedRestores = append(rt.FailedRestores,
				fmt.Sprintf("%s/%s/%s", kind, namespace, name))
			return NewBRFailedError("Restore",
				fmt.Sprintf("CRD %s did not become established: %s", name, err.Error()))
		}
		h.invalidateDiscovery()
	}

	if hasStatus && savedStatus != nil {
		if err := h.applyStatusSubresource(ctx, resource, savedStatus); err != nil {
			h.Log.Info("Failed to restore status subresource, continuing",
				"kind", kind, "name", name, "namespace", namespace, "error", err.Error())
		}
	}

	logFields := []interface{}{
		"file", entry.fileName, "resourceType", kind, "name", name,
	}
	if namespace != "" {
		logFields = append(logFields, "namespace", namespace)
	}
	h.Log.Info("Resource restored", logFields...)

	rt.SucceededRestores = append(rt.SucceededRestores,
		fmt.Sprintf("%s/%s/%s", kind, namespace, name))
	return nil
}

func isCRD(resource *unstructured.Unstructured) bool {
	return resource.GetKind() == "CustomResourceDefinition"
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

	name := resource.GetName()
	namespace := resource.GetNamespace()

	// Resolve the resource type to its actual GVR. If the type is not yet
	// registered (e.g. a custom resource whose CRD is still becoming
	// established) this refreshes discovery and retries resolution.
	gvr, err := h.resolveGVRWithRetry(ctx, gvk)
	if err != nil {
		return err
	}

	apply := func() error {
		if namespace != "" {
			return h.applyNamespacedResource(ctx, gvr, namespace, name, resource)
		}
		return h.applyClusterScopedResource(ctx, gvr, name, resource)
	}

	err = apply()
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

// resolveGVR resolves a GroupVersionKind to its GroupVersionResource on a
// best-effort basis for callers that cannot fail (e.g. the status subresource
// update). It prefers live discovery, then the REST mapper, and only as a last
// resort falls back to naive pluralization of the kind.
func (h *BRHandler) resolveGVR(gvk schema.GroupVersionKind) schema.GroupVersionResource {
	if gvr, err := h.resolveGVRFromCluster(gvk); err == nil {
		return gvr
	}
	gvr := schema.GroupVersionResource{
		Group:    gvk.Group,
		Version:  gvk.Version,
		Resource: pluralizeKind(gvk.Kind),
	}
	h.Log.Info("GVR resolution failed, using pluralized kind",
		"gvk", gvk.String(), "gvr", gvr.String())
	return gvr
}

// resolveGVRWithRetry resolves a resource's GVK to its GVR, refreshing discovery
// and retrying with a bounded backoff while the type is not yet registered (a
// no-match error — e.g. a custom resource whose CRD is still becoming
// established). Only no-match/not-ready errors are retried; this resolves the
// resource *type* and never an individual object, so ordinary object NotFound
// responses are never involved here.
func (h *BRHandler) resolveGVRWithRetry(ctx context.Context, gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	var gvr schema.GroupVersionResource
	backoff := wait.Backoff{
		Duration: 1 * time.Second,
		Factor:   2.0,
		Steps:    4,
	}
	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) { //nolint:wrapcheck
		var resolveErr error
		gvr, resolveErr = h.resolveGVRFromCluster(gvk)
		if resolveErr == nil {
			return true, nil
		}
		if isNoMatchError(resolveErr) {
			h.Log.Info("Resource type not registered yet, refreshing discovery and retrying",
				"gvk", gvk.String())
			h.invalidateDiscovery()
			return false, nil
		}
		return false, resolveErr
	})
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("failed to resolve GVR for %s: %w", gvk.String(), err)
	}
	return gvr, nil
}

// resolveGVRFromCluster resolves a GVK to its GVR using live discovery first
// (authoritative for custom resources with irregular plurals), then the REST
// mapper. It returns a no-match error when neither source knows the type.
func (h *BRHandler) resolveGVRFromCluster(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	if h.DiscoveryClient != nil {
		list, err := h.DiscoveryClient.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
		if err == nil && list != nil {
			for _, apiResource := range list.APIResources {
				// Skip subresources such as "widgets/status".
				if strings.Contains(apiResource.Name, "/") {
					continue
				}
				if strings.EqualFold(apiResource.Kind, gvk.Kind) {
					return gvk.GroupVersion().WithResource(apiResource.Name), nil
				}
			}
		}
	}

	if h.Client != nil {
		mapping, err := h.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
		if err == nil {
			return mapping.Resource, nil
		}
	}

	return schema.GroupVersionResource{}, &noMatchGVKError{gvk: gvk}
}

// invalidateDiscovery drops any cached discovery data so the next resolution
// sees API resources registered since the last query (e.g. a CRD just
// established). A non-cached discovery client is always live, so this is a
// no-op for it.
func (h *BRHandler) invalidateDiscovery() {
	if cached, ok := h.DiscoveryClient.(discovery.CachedDiscoveryInterface); ok {
		cached.Invalidate()
	}
}

// waitForCRDEstablished polls a CustomResourceDefinition until the API server
// reports it Established, bounded by crdEstablishTimeout.
func (h *BRHandler) waitForCRDEstablished(ctx context.Context, name string) error {
	crdGVR := schema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	}
	err := wait.PollUntilContextTimeout(ctx, 1*time.Second, crdEstablishTimeout, true,
		func(ctx context.Context) (bool, error) {
			crd, err := h.DynamicClient.Resource(crdGVR).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				if k8serrors.IsNotFound(err) {
					return false, nil
				}
				return false, fmt.Errorf("failed to get CRD %s: %w", name, err)
			}
			conditions, _, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
			for _, c := range conditions {
				cond, ok := c.(map[string]interface{})
				if !ok {
					continue
				}
				condType, _ := cond["type"].(string)
				condStatus, _ := cond["status"].(string)
				if condType == "Established" && condStatus == "True" {
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		return fmt.Errorf("timed out waiting for CRD %s to become established: %w", name, err)
	}
	return nil
}

// noMatchGVKError reports that no registered resource exists for a GVK.
type noMatchGVKError struct {
	gvk schema.GroupVersionKind
}

func (e *noMatchGVKError) Error() string {
	return fmt.Sprintf("no registered resource found for %s", e.gvk.String())
}

// isNoMatchError reports whether err indicates the resource type is not (yet)
// registered, as opposed to an individual object being missing.
func isNoMatchError(err error) bool {
	var nm *noMatchGVKError
	if errors.As(err, &nm) {
		return true
	}
	return meta.IsNoMatchError(err)
}

func (h *BRHandler) applyNamespacedResource(ctx context.Context, gvr schema.GroupVersionResource,
	namespace, name string, resource *unstructured.Unstructured) error {

	ri := h.DynamicClient.Resource(gvr).Namespace(namespace)
	return h.createOrUpdateResource(ctx, ri, name, resource)
}

func (h *BRHandler) applyClusterScopedResource(ctx context.Context, gvr schema.GroupVersionResource,
	name string, resource *unstructured.Unstructured) error {

	ri := h.DynamicClient.Resource(gvr)
	return h.createOrUpdateResource(ctx, ri, name, resource)
}

// createOrUpdateResource creates the resource if it does not exist yet, or
// updates it in place otherwise.
//
// Updates use a bounded conflict retry: a resource can change between the Get
// that reads its resourceVersion and the Update, so if the Update fails with a
// 409 Conflict the live object is re-fetched, its latest resourceVersion is
// re-applied to the resource being restored, and the Update is retried (bounded
// by retry.DefaultRetry). The existing create, AlreadyExists, success, and
// non-conflict error handling are preserved.
func (h *BRHandler) createOrUpdateResource(ctx context.Context, ri dynamic.ResourceInterface,
	name string, resource *unstructured.Unstructured) error {

	existing, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("failed to get existing resource: %w", err)
		}
		_, err = ri.Create(ctx, resource, metav1.CreateOptions{})
		if err != nil && !k8serrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create resource: %w", err)
		}
		return nil
	}

	resource.SetResourceVersion(existing.GetResourceVersion())
	// updateErr is always either nil or an error already wrapped inside the
	// closure below (RetryOnConflict returns the closure's own error verbatim),
	// so it does not need re-wrapping here.
	updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, err := ri.Update(ctx, resource, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if !k8serrors.IsConflict(err) {
			return fmt.Errorf("failed to update resource: %w", err)
		}
		// The live object changed since we read its resourceVersion. Re-fetch
		// it, re-apply the latest resourceVersion, and let RetryOnConflict
		// retry the Update. The returned error stays conflict-typed (errors.As
		// unwraps it) so RetryOnConflict keeps retrying.
		latest, getErr := ri.Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("failed to re-fetch resource during conflict retry: %w", getErr)
		}
		resource.SetResourceVersion(latest.GetResourceVersion())
		return fmt.Errorf("update conflict, retrying: %w", err)
	})
	return updateErr //nolint:wrapcheck
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

type resourceEntry struct {
	resource *unstructured.Unstructured
	fileName string
}

type waveGroup struct {
	wave      int
	resources []resourceEntry
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
			wg.resources = append(wg.resources, resourceEntry{
				resource: resource,
				fileName: yamlFile.Name(),
			})
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

func sortResourcesByWave(resources []resourceEntry) {
	sort.SliceStable(resources, func(i, j int) bool {
		return getWaveOrder(resources[i].resource) < getWaveOrder(resources[j].resource)
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
