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
	"encoding/json"
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
	kindJob:                    8,
	"CronJob":                  8,
	"Route":                    9,
	"Ingress":                  9,
}

func (h *BRHandler) StartRestore(ctx context.Context, oadpContent []ibuv1.ConfigMapRef, backupContent []ibuv1.BackupContentRef) (*RestoreTracker, error) {
	rt := &RestoreTracker{}

	// Backup content is configured when either input source is set. At most one of
	// them is populated (guaranteed by the IBU CEL mutual-exclusivity rule); the
	// restore walks only the on-disk backup regardless of which source produced it,
	// so the only thing the sources decide here is whether a missing backup
	// directory is an error (configured) or expected (nothing configured).
	configured := len(oadpContent) > 0 || len(backupContent) > 0

	backupPath := filepath.Join(hostPath, LocalBackupPath)
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		// A missing backup directory is only acceptable when the user did not
		// configure any backup content. If content was configured but no backup
		// exists on disk, the backup is gone (e.g. never taken, or wiped) and we
		// must fail the restore hard rather than silently succeeding, otherwise
		// the upgrade would complete without the user's application data.
		if configured {
			return rt, NewBRFailedError("Restore",
				fmt.Sprintf("backup content was configured but no local backup found at %s", backupPath))
		}
		h.Log.Info("No backup content configured and no local backup found, skipping restore")
		// Signal the caller that there is no backup directory, so it bypasses the
		// restore-completed marker (which would otherwise fail to be created inside the
		// absent directory and spuriously fail the upgrade).
		rt.NoBackupToRestore = true
		return rt, nil
	}

	// If a previous PostPivot run already completed the restore (all resources
	// applied and the PV reclaim policy restored), a restore-completed marker is
	// present inside the backup directory. A later re-entry — e.g. a deferred
	// health check that requeues PostPivot after a successful restore — must not
	// re-apply the resources, so skip restoration successfully. A stat error other
	// than "not exists" is a hard failure so the Upgrade fails terminally (human
	// intervention) rather than silently re-restoring.
	markerPath := filepath.Join(backupPath, restoreCompletedMarker)
	if _, err := os.Stat(markerPath); err == nil {
		h.Log.Info("Restore already completed on a previous run, skipping restore", "marker", markerPath)
		return rt, nil
	} else if !os.IsNotExist(err) {
		return rt, NewBRFailedError("Restore",
			fmt.Sprintf("failed to stat restore-completed marker %s: %s", markerPath, err.Error()))
	}

	validatedFiles, err := validateChecksums(backupPath)
	if err != nil {
		return rt, NewBRFailedError("Restore",
			fmt.Sprintf("backup checksum validation failed: %s", err.Error()))
	}

	// A local backup is expected at this point (the directory exists and its
	// checksums validated), so the version manifest must be present, intact, and a
	// layout version this LCA can read. This runs before loading any resources and
	// returns a terminal BRFailedError on any incompatibility.
	if _, err := loadAndValidateBackupLayout(backupPath, validatedFiles, h.Log); err != nil {
		return rt, err
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

// MarkRestoreCompleted atomically creates the restore-completed marker inside the
// local backup directory, recording that the restore fully completed (all
// resources applied and the PV reclaim policy restored). HandleRestore calls this
// after RestorePVsReclaimPolicy so that a re-entrant PostPivot run skips
// re-applying resources (see StartRestore).
//
// The marker is written to a temporary file that is fsync'd and atomically
// renamed into place, after which the parent directory is fsync'd so the new
// directory entry survives a crash/reboot — otherwise a lost marker would trigger
// a spurious re-restore. All failures are returned as BRFailedError so the
// existing PostPivot failure and rollback path handles them.
func (h *BRHandler) MarkRestoreCompleted(_ context.Context) error {
	backupPath := filepath.Join(hostPath, LocalBackupPath)
	markerPath := filepath.Join(backupPath, restoreCompletedMarker)

	tmp, err := os.CreateTemp(backupPath, restoreCompletedMarker+".tmp-*")
	if err != nil {
		return NewBRFailedError("Restore",
			fmt.Sprintf("failed to create restore-completed marker temp file in %s: %s", backupPath, err.Error()))
	}
	tmpPath := tmp.Name()
	// Best-effort removal of the temp file; a no-op once the rename below succeeds.
	defer func() { _ = os.Remove(tmpPath) }()

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return NewBRFailedError("Restore",
			fmt.Sprintf("failed to sync restore-completed marker temp file %s: %s", tmpPath, err.Error()))
	}
	if err := tmp.Close(); err != nil {
		return NewBRFailedError("Restore",
			fmt.Sprintf("failed to close restore-completed marker temp file %s: %s", tmpPath, err.Error()))
	}

	if err := os.Rename(tmpPath, markerPath); err != nil {
		return NewBRFailedError("Restore",
			fmt.Sprintf("failed to rename restore-completed marker into place at %s: %s", markerPath, err.Error()))
	}

	// fsync the parent directory so the rename (the marker's new directory entry)
	// is durable across a crash/reboot.
	dir, err := os.Open(backupPath) //nolint:gosec
	if err != nil {
		return NewBRFailedError("Restore",
			fmt.Sprintf("failed to open backup directory %s to sync restore-completed marker: %s", backupPath, err.Error()))
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return NewBRFailedError("Restore",
			fmt.Sprintf("failed to sync backup directory %s after writing restore-completed marker: %s", backupPath, err.Error()))
	}

	h.Log.Info("Restore-completed marker written", "marker", markerPath)
	return nil
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

	// Strip the cluster-assigned, non-portable metadata fields that must not be
	// recreated on restore (matching Velero's reset of these fields).
	// metadata.managedFields and the kubectl.kubernetes.io/last-applied-configuration
	// annotation are deliberately preserved.
	stripRestoreMetadata(resource)

	// Resource-specific restore actions (Service, ServiceAccount, Secret, PVC, Job, Route).
	prepareResourceForRestore(resource)

	// When the paired Restore CR set restorePVs, reset PersistentVolume binding so
	// the restored PV can rebind to its restored PVC.
	if entry.restoreConfig != nil && entry.restoreConfig.RestorePVs {
		removePVClaimRef(resource)
	}

	// Status is applied via the status subresource after the object exists, and
	// only for the resource kinds the paired Restore CR configured.
	restoreStatus := shouldPreserveStatus(resource, entry.restoreConfig.statusResourceSet())
	savedStatus, hasStatus := resource.Object["status"]

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

	if restoreStatus && hasStatus && savedStatus != nil {
		if err := h.applyStatusSubresource(ctx, resource, savedStatus); err != nil {
			h.Log.Info("Failed to restore status subresource, continuing",
				"kind", kind, "name", name, "namespace", namespace, "error", err.Error())
		}
	}

	logFields := []interface{}{
		"file", entry.fileName, "restoreWave", entry.wave, "resourceType", kind, "name", name,
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
	// wave is the 1-based restore group ordinal (the "restore-<n>" directory the
	// resource was loaded from) the resource belongs to. It is logged on restore
	// so operators can see which restore wave each resource was applied in.
	wave int
	// restoreConfig is the restore-time configuration persisted with the spec this
	// resource was backed up under (status-resource selection and restorePVs). It
	// may be nil for a spec that persisted no configuration.
	restoreConfig *specRestoreConfig
}

type waveGroup struct {
	wave      int
	resources []resourceEntry
}

// restoreGroupDirPrefix is the fixed prefix of a restore ordinal group directory
// name written by the backup (restore-1, restore-01, ...).
const restoreGroupDirPrefix = "restore-"

// parseGroupOrdinal parses the 1-based ordinal from a restore group directory name
// of the form "restore-<n>" (with any amount of zero-padding). It returns the
// ordinal and true, or false when the name is not a restore group directory.
func parseGroupOrdinal(dirName string) (int, bool) {
	if !strings.HasPrefix(dirName, restoreGroupDirPrefix) {
		return 0, false
	}
	n, err := strconv.Atoi(dirName[len(restoreGroupDirPrefix):])
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseResourceIndex parses the leading "<n>_" numeric prefix of a resource file
// name. It returns the index and true, or false when the name carries no numeric
// prefix.
func parseResourceIndex(fileName string) (int, bool) {
	if idx := strings.Index(fileName, "_"); idx > 0 {
		if n, err := strconv.Atoi(fileName[:idx]); err == nil {
			return n, true
		}
	}
	return 0, false
}

func loadBackupResourcesByWave(backupPath string, validatedFiles map[string]bool, log logr.Logger) ([]waveGroup, error) {
	entries, err := os.ReadDir(backupPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read backup directory: %w", err)
	}

	type groupDir struct {
		ordinal int
		dirName string
	}
	var dirs []groupDir
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		ordinal, ok := parseGroupOrdinal(entry.Name())
		if !ok {
			// Ignore any directory that is not a restore ordinal group.
			continue
		}
		dirs = append(dirs, groupDir{ordinal: ordinal, dirName: entry.Name()})
	}

	// Apply the groups in ascending ordinal order. The ordinal is parsed
	// numerically so padding never changes the order.
	sort.SliceStable(dirs, func(i, j int) bool {
		return dirs[i].ordinal < dirs[j].ordinal
	})

	var result []waveGroup
	for _, d := range dirs {
		groupDirPath := filepath.Join(backupPath, d.dirName)
		files, err := os.ReadDir(groupDirPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read restore group directory %s: %w", d.dirName, err)
		}

		fileConfig, err := loadGroupRestoreConfig(groupDirPath, d.dirName, validatedFiles)
		if err != nil {
			return nil, err
		}

		// Collect the resource files and sort them by their leading numeric index
		// rather than relying on lexical ordering, so the backup's intended
		// within-group order is honored even past nine files.
		type resourceFile struct {
			index int
			name  string
		}
		var resourceFiles []resourceFile
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".yaml") {
				continue
			}
			relPath := filepath.Join(d.dirName, f.Name())
			if !validatedFiles[relPath] {
				return nil, fmt.Errorf("file %s is not listed in checksum manifest, refusing to restore", relPath)
			}
			index, ok := parseResourceIndex(f.Name())
			if !ok {
				return nil, fmt.Errorf("resource file %s has no numeric index prefix", relPath)
			}
			resourceFiles = append(resourceFiles, resourceFile{index: index, name: f.Name()})
		}
		sort.SliceStable(resourceFiles, func(i, j int) bool {
			return resourceFiles[i].index < resourceFiles[j].index
		})

		wg := waveGroup{wave: d.ordinal}
		for _, rf := range resourceFiles {
			filePath := filepath.Join(groupDirPath, rf.name)
			resource := &unstructured.Unstructured{}
			if err := utils.ReadYamlOrJSONFile(filePath, resource); err != nil {
				return nil, fmt.Errorf("failed to read resource file %s: %w", filePath, err)
			}
			wg.resources = append(wg.resources, resourceEntry{
				resource:      resource,
				fileName:      rf.name,
				wave:          d.ordinal,
				restoreConfig: fileConfig[rf.name],
			})
		}

		log.Info("Loaded backup resources from restore group", "group", d.dirName, "count", len(wg.resources))
		result = append(result, wg)
	}

	return result, nil
}

// loadGroupRestoreConfig reads the per-group restore configuration persisted in a
// restore ordinal group directory and returns a map from each resource file name
// in the group to the restore configuration that applies to it. The file is
// covered by checksums.sha256, so when present it must appear in the validated
// manifest. A group directory without a config file yields an empty map, under
// which the restore applies no status and no PV rebinding.
func loadGroupRestoreConfig(groupDir, dirName string, validatedFiles map[string]bool) (map[string]*specRestoreConfig, error) {
	fileConfig := map[string]*specRestoreConfig{}

	configPath := filepath.Join(groupDir, restoreConfigFileName)
	data, err := os.ReadFile(configPath) //nolint:gosec
	if err != nil {
		if os.IsNotExist(err) {
			return fileConfig, nil
		}
		return nil, fmt.Errorf("failed to read restore config %s: %w", configPath, err)
	}

	relPath := filepath.Join(dirName, restoreConfigFileName)
	if !validatedFiles[relPath] {
		return nil, fmt.Errorf("file %s is not listed in checksum manifest, refusing to restore", relPath)
	}

	cfg := &groupRestoreConfig{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse restore config %s: %w", relPath, err)
	}

	for i := range cfg.Entries {
		entry := cfg.Entries[i]
		sc := &specRestoreConfig{
			RestoreStatusResources: entry.RestoreStatusResources,
			RestorePVs:             entry.RestorePVs,
		}
		for _, f := range entry.Files {
			fileConfig[f] = sc
		}
	}
	return fileConfig, nil
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

// stripRestoreMetadata removes the cluster-assigned, non-portable metadata fields
// that must not be recreated when a backed-up resource is applied to the cluster,
// matching the set of fields Velero resets on restore. metadata.managedFields and
// the kubectl.kubernetes.io/last-applied-configuration annotation are intentionally
// left in place.
func stripRestoreMetadata(resource *unstructured.Unstructured) {
	for _, field := range []string{
		"generateName",
		"selfLink",
		"uid",
		"resourceVersion",
		"generation",
		"creationTimestamp",
		"ownerReferences",
	} {
		unstructured.RemoveNestedField(resource.Object, "metadata", field)
	}
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
	case kindJob:
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
