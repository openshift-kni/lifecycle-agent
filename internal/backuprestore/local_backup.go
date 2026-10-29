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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
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

	// Parse the Restore docs to surface malformed configuration early; the
	// specs themselves are not needed for backup validation.
	if _, err := ExtractRestoreSpecsFromConfigmaps(configmaps); err != nil {
		return NewBRFailedValidationError("Backup", err.Error())
	}

	if err := ValidateBackupRestorePairing(configmaps); err != nil {
		return NewBRFailedValidationError("Backup", err.Error())
	}

	if err := ValidateBackupSpecs(backupSpecs); err != nil {
		return NewBRFailedValidationError("Backup", err.Error())
	}

	if len(backupSpecs) == 0 {
		h.Log.Info("No backup specs found in configmaps")
		return nil
	}

	for _, spec := range backupSpecs {
		if _, err := getObjsFromApplyLabel(spec.ApplyLabel); err != nil {
			return NewBRFailedValidationError("Backup", err.Error())
		}
	}

	h.Log.Info("Backup configmaps validated", "configMaps", content)
	return nil
}

func (h *BRHandler) StartBackup(ctx context.Context, content []ibuv1.ConfigMapRef) (*BackupTracker, error) {
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

	restoreSpecs, err := ExtractRestoreSpecsFromConfigmaps(configmaps)
	if err != nil {
		return bt, NewBRFailedValidationError("Backup", err.Error())
	}

	// Re-check the Backup/Restore pairing here in case the referenced ConfigMaps
	// changed between Prep (ValidateBackupConfigmaps) and this pre-pivot backup.
	if err := ValidateBackupRestorePairing(configmaps); err != nil {
		return bt, NewBRFailedValidationError("Backup", err.Error())
	}

	if err := ValidateBackupSpecs(backupSpecs); err != nil {
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

	backupDir := filepath.Join(hostPath, LocalBackupPath)
	// Clear any stale contents from a previous backup attempt before recreating
	// the directory. PrePivot can re-enter and re-run the backup after a partial
	// failure; if the resource set shrank between attempts, orphaned YAML files
	// would otherwise survive while checksums.sha256 is rewritten without them,
	// causing the post-pivot restore to hard-fail on the unlisted files.
	// Filesystem errors below are returned as BRFailedError so the Upgrade stage
	// fails terminally instead of requeuing forever: a backup that cannot write
	// to local node storage will not succeed on retry, so human intervention
	// (moving the IBU back to Idle and restarting) is required.
	if err := os.RemoveAll(backupDir); err != nil {
		return bt, NewBRFailedError("Backup",
			fmt.Sprintf("failed to clear stale backup directory %s: %s", backupDir, err.Error()))
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return bt, NewBRFailedError("Backup",
			fmt.Sprintf("failed to create backup directory %s: %s", backupDir, err.Error()))
	}

	var checksums []string

	for groupIdx, group := range sortedGroups {
		h.Log.Info("Processing backup group", "groupIndex", groupIdx+1, "totalGroups", len(sortedGroups))
		for _, spec := range group {
			if err := validateBackupName(spec.Name); err != nil {
				bt.FailedBackups = append(bt.FailedBackups, spec.Name)
				return bt, NewBRFailedValidationError("Backup", err.Error())
			}

			restoreSpec := FindRestoreForBackup(spec.Name, restoreSpecs)
			statusResources := resolveStatusResources(restoreSpec)
			restorePVs := restoreSpec != nil && restoreSpec.RestorePVs

			resources, err := h.fetchResources(ctx, spec)
			if err != nil {
				bt.FailedBackups = append(bt.FailedBackups, spec.Name)
				return bt, NewBRFailedError("Backup",
					fmt.Sprintf("failed to fetch resources for backup %s: %s", spec.Name, err.Error()))
			}

			wave := resolveWave(spec, restoreSpec)
			dirName := fmt.Sprintf("%03d_%s", wave, spec.Name)
			specDir := filepath.Join(backupDir, dirName)
			if err := os.MkdirAll(specDir, 0o700); err != nil {
				bt.FailedBackups = append(bt.FailedBackups, spec.Name)
				return bt, NewBRFailedError("Backup",
					fmt.Sprintf("failed to create backup spec directory %s: %s", specDir, err.Error()))
			}

			for i, resource := range resources {
				stripTransientMetadata(resource, statusResources)

				// When the matching Restore CR sets restorePVs: true, strip
				// spec.claimRef from PersistentVolumes so that when they are
				// (re)applied during the post-pivot restore they are not bound
				// to a stale PVC reference and can rebind to their restored PVC.
				if restorePVs {
					removePVClaimRef(resource)
				}

				data, err := k8syaml.Marshal(resource.Object)
				if err != nil {
					bt.FailedBackups = append(bt.FailedBackups, spec.Name)
					return bt, fmt.Errorf("failed to marshal resource: %w", err)
				}

				ns := resource.GetNamespace()
				name := resource.GetName()
				kind := resource.GetKind()
				fileName := fmt.Sprintf("%05d_%s_%s_%s.yaml", i, strings.ToLower(kind), ns, name)
				filePath := filepath.Join(specDir, fileName)

				if err := os.WriteFile(filePath, data, 0o600); err != nil {
					bt.FailedBackups = append(bt.FailedBackups, spec.Name)
					return bt, NewBRFailedError("Backup",
						fmt.Sprintf("failed to write resource file %s: %s", filePath, err.Error()))
				}

				logFields := []interface{}{
					"file", fileName, "resourceType", kind, "name", name,
				}
				if ns != "" {
					logFields = append(logFields, "namespace", ns)
				}
				h.Log.Info("Resource backed up", logFields...)

				hash := sha256.Sum256(data)
				checksums = append(checksums, fmt.Sprintf("%s  %s",
					hex.EncodeToString(hash[:]), filepath.Join(dirName, fileName)))
			}

			h.Log.Info("Backup completed for spec", "name", spec.Name, "resourceCount", len(resources))
			bt.SucceededBackups = append(bt.SucceededBackups, spec.Name)
		}
	}

	checksumContent := strings.Join(checksums, "\n") + "\n"
	checksumPath := filepath.Join(backupDir, "checksums.sha256")
	if err := os.WriteFile(checksumPath, []byte(checksumContent), 0o600); err != nil {
		return bt, NewBRFailedError("Backup",
			fmt.Sprintf("failed to write checksum manifest %s: %s", checksumPath, err.Error()))
	}

	h.Log.Info("All backups completed successfully",
		"succeeded", bt.SucceededBackups,
		"targetDir", backupDir)
	return bt, nil
}

// resolveWave determines the wave number for a backup spec.
// Priority: Restore CR's apply-wave > Backup CR's apply-wave > default 999.
func resolveWave(backup BackupSpec, restore *RestoreSpec) int {
	if restore != nil && restore.ApplyWave != "" {
		if w, err := strconv.Atoi(restore.ApplyWave); err == nil {
			return w
		}
	}
	if backup.ApplyWave != "" {
		if w, err := strconv.Atoi(backup.ApplyWave); err == nil {
			return w
		}
	}
	return 999
}

// resolveStatusResources returns the list of resource kinds/plurals for which
// status should be preserved during backup, based on the matching Restore CR.
func resolveStatusResources(restore *RestoreSpec) map[string]bool {
	if restore == nil || len(restore.RestoreStatusResources) == 0 {
		return nil
	}
	result := make(map[string]bool, len(restore.RestoreStatusResources))
	for _, r := range restore.RestoreStatusResources {
		result[strings.ToLower(r)] = true
	}
	return result
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

	// Always include the Namespace objects for the included namespaces so they
	// are recreated (in wave 0) before their contained resources during restore,
	// regardless of whether the apply-label annotation is used.
	nsGVR := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	for _, ns := range spec.IncludedNamespaces {
		nsObj, err := h.DynamicClient.Resource(nsGVR).Get(ctx, ns, metav1.GetOptions{})
		if err != nil {
			// An explicitly included namespace that does not exist is a
			// configuration error: fail the backup instead of silently skipping
			// it (and the resources it should contain), matching Velero's
			// behavior for non-existing configured resources. Do not continue
			// listing resources from a missing namespace.
			if k8serrors.IsNotFound(err) {
				return nil, fmt.Errorf("included namespace %q does not exist", ns)
			}
			return nil, fmt.Errorf("failed to get namespace %s: %w", ns, err)
		}
		resources = append(resources, nsObj)
	}

	// When the apply-label annotation is present, the backup scope is limited
	// exclusively to the resources named in the annotation, regardless of the
	// resource types listed in the spec (see docs/backuprestore-with-oadp.md,
	// section "LCA apply label annotation"). Fetch each named object directly
	// instead of listing every resource of the included types.
	if spec.ApplyLabel != "" {
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
		return deduplicateResources(resources), nil
	}

	// Discover the server's preferred API resources on every fetch so that CRDs
	// installed after the controller started (e.g. lvmclusters) are visible and
	// resources configured by name only can be resolved to their preferred GVR.
	apiResourceLists, failedGroups, err := h.getServerPreferredResources()
	if err != nil {
		return nil, err
	}

	// No apply-label: back up all resources of the included types in each
	// included namespace and every included cluster-scoped resource type.
	for _, ns := range spec.IncludedNamespaces {
		nsResources, err := h.fetchNamespacedResources(ctx, ns, spec, apiResourceLists, failedGroups)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch resources in namespace %s: %w", ns, err)
		}
		resources = append(resources, nsResources...)
	}

	for _, resourceType := range spec.IncludedClusterScopedResources {
		clusterResources, err := h.fetchClusterScopedResources(ctx, resourceType, spec, apiResourceLists, failedGroups)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch cluster-scoped resource %s: %w", resourceType, err)
		}
		resources = append(resources, clusterResources...)
	}

	return deduplicateResources(resources), nil
}

// getServerPreferredResources returns the server's preferred API resources via
// the discovery client. A partial discovery failure (e.g. a single aggregated
// API group being unavailable) is tolerated: the successfully discovered
// resources are returned and the error is logged, matching how kubectl behaves.
// The names of the groups that failed discovery are returned as well so callers
// can distinguish "resource type genuinely not registered" from "resource type
// unknown because its API group could not be discovered" and avoid silently
// omitting requested resources from the backup.
func (h *BRHandler) getServerPreferredResources() ([]*metav1.APIResourceList, map[string]bool, error) {
	if h.DiscoveryClient == nil {
		return nil, nil, fmt.Errorf("discovery client is not initialized")
	}

	failedGroups := map[string]bool{}
	lists, err := h.DiscoveryClient.ServerPreferredResources()
	if err != nil {
		var groupErr *discovery.ErrGroupDiscoveryFailed
		if errors.As(err, &groupErr) {
			for gv := range groupErr.Groups {
				failedGroups[gv.Group] = true
			}
			h.Log.Info("Partial API discovery failure, continuing with available resources",
				"failedGroups", failedGroups, "error", err.Error())
		} else {
			return nil, nil, fmt.Errorf("failed to discover server preferred resources: %w", err)
		}
	}
	return lists, failedGroups, nil
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
		// An apply-label names an exact object the backup must capture. If it does
		// not exist, the configuration is wrong: fail the backup instead of
		// silently skipping it, so the missing object is not lost on restore.
		if k8serrors.IsNotFound(err) {
			return nil, fmt.Errorf("apply-label resource %s %q (group %q version %q namespace %q) not found",
				obj.Resource, obj.Name, obj.Group, obj.Version, obj.Namespace)
		}
		return nil, fmt.Errorf("failed to get apply-label resource %s %q: %w", obj.Resource, obj.Name, err)
	}
	return result, nil
}

// resolveRequiredGVR resolves a configured (user-requested or default) resource
// type to its preferred GroupVersionResource, treating anything that prevents a
// unique, confirmed resolution as a hard error. A configured resource selection
// must be fetched reliably or fail the Upgrade before pivot, so:
//   - no match (the type is absent from discovery) fails;
//   - an ambiguous match (the bare name maps to more than one API group) fails;
//   - a match that cannot be confirmed because the relevant API group failed
//     discovery fails.
//
// An unrelated API group failing discovery does not fail a type that still
// resolves uniquely from the successfully discovered resources.
func resolveRequiredGVR(resourceType string, apiResourceLists []*metav1.APIResourceList, failedGroups map[string]bool) (schema.GroupVersionResource, bool, error) {
	gvr, namespaced, found, err := resolveResourceGVR(resourceType, apiResourceLists)
	if err != nil {
		// Ambiguous match across multiple API groups.
		return schema.GroupVersionResource{}, false, err
	}
	if found {
		return gvr, namespaced, nil
	}

	// Not found. Distinguish "its API group failed discovery" (cannot confirm)
	// from "genuinely absent" so the error names the right cause.
	group := schema.ParseGroupResource(resourceType).Group
	if group != "" && failedGroups[group] {
		return schema.GroupVersionResource{}, false, fmt.Errorf(
			"cannot resolve resource type %q: its API group %q failed discovery", resourceType, group)
	}
	if group == "" && len(failedGroups) > 0 {
		// A bare name that did not resolve while discovery was incomplete cannot be
		// confirmed absent: it may live in a group that failed discovery.
		return schema.GroupVersionResource{}, false, fmt.Errorf(
			"cannot confirm resource type %q: API discovery was incomplete (failed groups: %v)",
			resourceType, sortedGroupNames(failedGroups))
	}
	return schema.GroupVersionResource{}, false, fmt.Errorf(
		"resource type %q not found in cluster API discovery", resourceType)
}

// sortedGroupNames returns the keys of the set as a deterministically ordered
// slice, for stable error messages.
func sortedGroupNames(groups map[string]bool) []string {
	names := make([]string, 0, len(groups))
	for g := range groups {
		names = append(names, g)
	}
	sort.Strings(names)
	return names
}

func (h *BRHandler) fetchNamespacedResources(ctx context.Context, namespace string, spec BackupSpec, apiResourceLists []*metav1.APIResourceList, failedGroups map[string]bool) ([]*unstructured.Unstructured, error) {
	var resources []*unstructured.Unstructured

	// The namespace-scoped resource types to back up are taken verbatim from the
	// spec; ValidateBackupSpecs guarantees they are set whenever namespaces are
	// included without the apply-label annotation, so there is no built-in default
	// list to fall back to.
	resourceTypes := spec.IncludedNamespaceScopedResources

	excluded := excludedGroupResources(apiResourceLists, spec.ExcludedNamespaceScopedResources)

	listOpts := metav1.ListOptions{}
	if spec.LabelSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(spec.LabelSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to convert label selector: %w", err)
		}
		listOpts.LabelSelector = selector.String()
	}

	for _, resourceType := range resourceTypes {
		gvr, namespaced, err := resolveRequiredGVR(resourceType, apiResourceLists, failedGroups)
		if err != nil {
			return nil, fmt.Errorf("namespace %s: %w", namespace, err)
		}
		if excluded[gvr.GroupResource().String()] {
			continue
		}
		if !namespaced {
			return nil, fmt.Errorf("resource %q configured as namespace-scoped is cluster-scoped per discovery (namespace %s); "+
				"move it to spec.includedClusterScopedResources", resourceType, namespace)
		}

		// The GVR was confirmed via discovery above, so any List failure here —
		// including NotFound or "not registered" — is a real error, not an absent
		// type. A successful List that returns zero items is fine. Failing closed
		// prevents recording an incomplete backup as successful.
		list, err := h.DynamicClient.Resource(gvr).Namespace(namespace).List(ctx, listOpts)
		if err != nil {
			return nil, fmt.Errorf("failed to list %s in namespace %s: %w", resourceType, namespace, err)
		}
		for i := range list.Items {
			resources = append(resources, &list.Items[i])
		}
	}

	return resources, nil
}

func (h *BRHandler) fetchClusterScopedResources(ctx context.Context, resourceType string, spec BackupSpec, apiResourceLists []*metav1.APIResourceList, failedGroups map[string]bool) ([]*unstructured.Unstructured, error) {
	gvr, namespaced, err := resolveRequiredGVR(resourceType, apiResourceLists, failedGroups)
	if err != nil {
		return nil, err
	}

	excluded := excludedGroupResources(apiResourceLists, spec.ExcludedClusterScopedResources)
	if excluded[gvr.GroupResource().String()] {
		return nil, nil
	}

	if namespaced {
		return nil, fmt.Errorf("resource %q configured as cluster-scoped is namespace-scoped per discovery; "+
			"move it to spec.includedNamespaceScopedResources", resourceType)
	}

	listOpts := metav1.ListOptions{}
	if spec.LabelSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(spec.LabelSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to convert label selector: %w", err)
		}
		listOpts.LabelSelector = selector.String()
	}

	// The GVR was confirmed via discovery above, so any List failure here —
	// including NotFound or "not registered" — is a real error, not an absent
	// type. A successful List returning zero items is fine.
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

// stripTransientMetadata removes transient fields from a resource before backup.
// If statusResources is non-nil, status is preserved for resource kinds whose
// pluralized name appears in the set (matching Velero Restore CR restoreStatus behavior).
func stripTransientMetadata(resource *unstructured.Unstructured, statusResources map[string]bool) {
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

	if !shouldPreserveStatus(resource, statusResources) {
		unstructured.RemoveNestedField(resource.Object, "status")
	}
	unstructured.RemoveNestedField(resource.Object, "metadata", "deletionTimestamp")
	unstructured.RemoveNestedField(resource.Object, "metadata", "deletionGracePeriodSeconds")
	unstructured.RemoveNestedField(resource.Object, "metadata", "ownerReferences")
	unstructured.RemoveNestedField(resource.Object, "metadata", "finalizers")
	unstructured.RemoveNestedField(resource.Object, "metadata", "selfLink")
}

// removePVClaimRef clears spec.claimRef from a PersistentVolume. It is invoked
// during backup when the matching Velero Restore CR sets restorePVs: true, so
// that the stored PV manifest is applied without a claimRef during the
// post-pivot restore and the PV can rebind to its restored PVC. This mirrors
// the volume-binding reset that OADP/Velero performs when restorePVs is set.
// For any non-PersistentVolume resource this is a no-op.
func removePVClaimRef(resource *unstructured.Unstructured) {
	if resource.GetKind() != "PersistentVolume" {
		return
	}
	unstructured.RemoveNestedField(resource.Object, "spec", "claimRef")
}

func shouldPreserveStatus(resource *unstructured.Unstructured, statusResources map[string]bool) bool {
	if len(statusResources) == 0 {
		return false
	}
	kind := resource.GetKind()
	return statusResources[pluralizeKind(kind)]
}

// excludedGroupResources resolves each configured exclusion through the resolver
// and returns the set of canonical GroupResource strings to exclude, so that
// inclusions and exclusions are compared in a normalized form regardless of
// whether they were written as plural, singular, Kind-like, short-name, bare or
// qualified. Entries that cannot be resolved are skipped: a resource type absent
// from discovery is never fetched, so it cannot be included either.
func excludedGroupResources(apiResourceLists []*metav1.APIResourceList, excludedLists ...[]string) map[string]bool {
	excluded := make(map[string]bool)
	for _, list := range excludedLists {
		for _, entry := range list {
			gvr, _, found, err := resolveResourceGVR(entry, apiResourceLists)
			if err != nil || !found {
				continue
			}
			excluded[gvr.GroupResource().String()] = true
		}
	}
	return excluded
}

// resourceMatches reports whether an APIResource is referred to by the given
// name, matching its plural Name, SingularName, Kind or any advertised
// ShortName, case-insensitively.
func resourceMatches(apiResource metav1.APIResource, name string) bool {
	if strings.EqualFold(apiResource.Name, name) ||
		strings.EqualFold(apiResource.SingularName, name) ||
		strings.EqualFold(apiResource.Kind, name) {
		return true
	}
	for _, short := range apiResource.ShortNames {
		if strings.EqualFold(short, name) {
			return true
		}
	}
	return false
}

// resolveResourceGVR resolves a user-provided resource type string to its
// canonical, preferred GroupVersionResource using the supplied server-preferred
// API resource lists (from discovery). The resource portion is parsed with
// schema.ParseGroupResource and matched against an APIResource's plural Name,
// SingularName, Kind (case-insensitively) and advertised ShortNames, so bare,
// singular, Kind-like and short-name forms all resolve (e.g.
// "dataprotectionapplications", "dataprotectionapplication",
// "DataProtectionApplication", "dpa"). The returned GVR is always the canonical
// preferred one, groupVersion.WithResource(APIResource.Name).
//
// For qualified names like "deployments.apps" or "routes.route.openshift.io"
// the explicit group is honored. For bare names like "lvmclusters" the matching
// API group is discovered. If a bare name matches more than one API group, an
// ambiguity error is returned instead of guessing, so the caller can surface
// the conflict and fail the backup. No pluralization or core/v1 fallback is
// performed.
//
// The found return value reports whether the resource type was found in
// discovery at all; a value of false (with a nil error) means the resource is
// absent from the cluster and should be skipped. The namespaced return value
// reflects APIResource.Namespaced so the caller can validate scope and select
// the correct dynamic-client operation.
func resolveResourceGVR(resourceType string, apiResourceLists []*metav1.APIResourceList) (schema.GroupVersionResource, bool, bool, error) {
	parsed := schema.ParseGroupResource(resourceType)
	wanted := parsed.Resource
	explicitGroup := parsed.Group

	type candidate struct {
		gvr        schema.GroupVersionResource
		namespaced bool
	}
	// Collect one candidate per matching API group (ServerPreferredResources
	// already returns only the preferred version for each group).
	candidatesByGroup := make(map[string]candidate)
	var groups []string
	for _, list := range apiResourceLists {
		if list == nil {
			continue
		}
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		if explicitGroup != "" && gv.Group != explicitGroup {
			continue
		}
		for _, apiResource := range list.APIResources {
			// Skip subresources such as "pods/status".
			if strings.Contains(apiResource.Name, "/") {
				continue
			}
			if !resourceMatches(apiResource, wanted) {
				continue
			}
			if _, ok := candidatesByGroup[gv.Group]; !ok {
				candidatesByGroup[gv.Group] = candidate{
					gvr:        gv.WithResource(apiResource.Name),
					namespaced: apiResource.Namespaced,
				}
				groups = append(groups, gv.Group)
			}
		}
	}

	switch len(candidatesByGroup) {
	case 0:
		return schema.GroupVersionResource{}, false, false, nil
	case 1:
		c := candidatesByGroup[groups[0]]
		return c.gvr, c.namespaced, true, nil
	default:
		sort.Strings(groups)
		return schema.GroupVersionResource{}, false, false, fmt.Errorf(
			"ambiguous resource %q matches multiple API groups %v; qualify it as <resource>.<group>",
			resourceType, groups)
	}
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
