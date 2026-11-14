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
	"encoding/json"
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

	// Parse the Restore docs to surface malformed configuration early and to
	// validate the Restore apply-wave annotation during Prep.
	restoreSpecs, err := ExtractRestoreSpecsFromConfigmaps(configmaps)
	if err != nil {
		return NewBRFailedValidationError("Backup", err.Error())
	}

	if err := ValidateBackupRestorePairing(configmaps); err != nil {
		return NewBRFailedValidationError("Backup", err.Error())
	}

	if err := ValidateBackupSpecs(backupSpecs); err != nil {
		return NewBRFailedValidationError("Backup", err.Error())
	}

	if err := ValidateRestoreSpecs(restoreSpecs); err != nil {
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

func (h *BRHandler) StartBackup(ctx context.Context, oadpContent []ibuv1.ConfigMapRef, backupContent []ibuv1.BackupContentRef) (*BackupTracker, error) {
	bt := &BackupTracker{}

	// At most one of backupContent/oadpContent is set (guaranteed by the IBU CEL
	// mutual-exclusivity rule). The BackupContent path is the replacement for the
	// deprecated oadpContent path; both converge on the shared []collectedBackup
	// representation and the same on-disk layout written by writeCollectedBackups.
	switch {
	case len(backupContent) > 0:
		return h.startBackupFromBackupContent(ctx, backupContent, bt)
	case len(oadpContent) > 0:
		return h.startBackupFromConfigmaps(ctx, oadpContent, bt)
	default:
		h.Log.Info("No backup content provided, skipping")
		return bt, nil
	}
}

// startBackupFromConfigmaps runs the deprecated oadpContent backup path: it parses
// the Velero Backup/Restore CRs out of the referenced ConfigMaps, validates them,
// fetches the configured resources ordered by Backup apply-wave, and hands the
// collected backups to the shared write pass (writeCollectedBackups). Its
// behaviour is unchanged from before the BackupContent path was added.
func (h *BRHandler) startBackupFromConfigmaps(ctx context.Context, content []ibuv1.ConfigMapRef, bt *BackupTracker) (*BackupTracker, error) {
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

	if err := ValidateRestoreSpecs(restoreSpecs); err != nil {
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

	backupDir, err := prepareLocalBackupDir()
	if err != nil {
		return bt, err
	}

	// Fetch every backup's resources, processing lower Backup apply-waves first.
	// The on-disk layout is driven separately by the paired Restore CR's apply-wave
	// (see assignRestoreOrdinals below): the Backup apply-wave never affects restore
	// ordering.
	collected := make([]collectedBackup, 0, len(backupSpecs))
	for groupIdx, group := range sortedGroups {
		h.Log.Info("Processing backup group", "groupIndex", groupIdx+1, "totalGroups", len(sortedGroups))
		for _, spec := range group {
			if err := validateBackupName(spec.Name); err != nil {
				bt.FailedBackups = append(bt.FailedBackups, spec.Name)
				return bt, NewBRFailedValidationError("Backup", err.Error())
			}

			restoreSpec := FindRestoreForBackup(spec.Name, restoreSpecs)

			resources, err := h.fetchResources(ctx, spec)
			if err != nil {
				bt.FailedBackups = append(bt.FailedBackups, spec.Name)
				return bt, NewBRFailedError("Backup",
					fmt.Sprintf("failed to fetch resources for backup %s: %s", spec.Name, err.Error()))
			}

			collected = append(collected, collectedBackup{
				spec:      spec,
				restore:   restoreSpec,
				resources: h.selectBackupResources(resources),
			})
		}
	}

	if err := h.writeCollectedBackups(backupDir, collected, bt); err != nil {
		return bt, err
	}
	return bt, nil
}

// prepareLocalBackupDir clears any stale contents from a previous backup attempt
// and recreates the local backup directory, returning its path.
//
// PrePivot can re-enter and re-run the backup after a partial failure; if the
// resource set shrank between attempts, orphaned YAML files would otherwise survive
// while checksums.sha256 is rewritten without them, causing the post-pivot restore
// to hard-fail on the unlisted files. Filesystem errors are returned as
// BRFailedError so the Upgrade stage fails terminally instead of requeuing forever:
// a backup that cannot write to local node storage will not succeed on retry, so
// human intervention (moving the IBU back to Idle and restarting) is required.
func prepareLocalBackupDir() (string, error) {
	backupDir := filepath.Join(hostPath, LocalBackupPath)
	if err := os.RemoveAll(backupDir); err != nil {
		return "", NewBRFailedError("Backup",
			fmt.Sprintf("failed to clear stale backup directory %s: %s", backupDir, err.Error()))
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", NewBRFailedError("Backup",
			fmt.Sprintf("failed to create backup directory %s: %s", backupDir, err.Error()))
	}
	return backupDir, nil
}

// writeCollectedBackups is the shared write pass for both the oadpContent and
// BackupContent input paths. Given the collected backups (each carrying its
// resources and synthetic/parsed restore settings), it assigns restore ordinal
// groups, writes the per-group resource files and restore config, the root-level
// layout manifest, and the checksum manifest. Both paths produce the identical
// layoutVersion 1.0.0 on-disk layout, consumable by the same post-pivot reader
// without input-source-specific handling.
func (h *BRHandler) writeCollectedBackups(backupDir string, collected []collectedBackup, bt *BackupTracker) error {
	// Assign each backup to a 1-based restore ordinal group, ordered by the paired
	// Restore CR's apply-wave (missing or empty last). Backups that share a restore
	// wave share an ordinal. The ordinals are written as zero-padded directory names
	// (restore-1, restore-01, ...) wide enough for the final ordinal, so lexical and
	// numeric ordering agree and no raw wave value appears in a directory name.
	buckets, numGroups := assignRestoreOrdinals(collected)
	ordinalWidth := numDigits(numGroups)

	var checksums []string
	for ordinal := 1; ordinal <= numGroups; ordinal++ {
		bucket := buckets[ordinal]

		// Number the resource files 1-based across the whole group, padded wide
		// enough for the group's total file count so lexical and numeric ordering
		// agree even past nine files.
		total := 0
		for _, c := range bucket {
			total += len(c.resources)
		}

		groupDirName := fmt.Sprintf("restore-%0*d", ordinalWidth, ordinal)
		groupDir := filepath.Join(backupDir, groupDirName)
		if err := os.MkdirAll(groupDir, 0o700); err != nil {
			return NewBRFailedError("Backup",
				fmt.Sprintf("failed to create restore group directory %s: %s", groupDir, err.Error()))
		}

		groupChecksums, err := h.writeRestoreGroup(groupDir, groupDirName, bucket, numDigits(total), bt)
		if err != nil {
			return err
		}
		checksums = append(checksums, groupChecksums...)
	}

	// Write the root-level version manifest beside checksums.sha256 and include it
	// in the checksum manifest. The resource count and backup spec names are taken
	// from the collected backups (every written resource file corresponds to one
	// collected resource). See the layout contract in local_layout.go.
	totalResources := 0
	specNames := make([]string, 0, len(collected))
	for _, c := range collected {
		totalResources += len(c.resources)
		specNames = append(specNames, c.spec.Name)
	}
	layoutChecksum, err := writeBackupLayoutManifest(backupDir, totalResources, specNames)
	if err != nil {
		return err
	}
	checksums = append(checksums, layoutChecksum)

	checksumContent := strings.Join(checksums, "\n") + "\n"
	checksumPath := filepath.Join(backupDir, "checksums.sha256")
	if err := os.WriteFile(checksumPath, []byte(checksumContent), 0o600); err != nil {
		return NewBRFailedError("Backup",
			fmt.Sprintf("failed to write checksum manifest %s: %s", checksumPath, err.Error()))
	}

	h.Log.Info("All backups completed successfully",
		"succeeded", bt.SucceededBackups,
		"targetDir", backupDir)
	return nil
}

// collectedBackup pairs a backup spec with its paired Restore CR and the already
// filtered resources to persist, carried between the fetch pass (ordered by Backup
// apply-wave) and the write pass (ordered by restore ordinal group).
type collectedBackup struct {
	spec      BackupSpec
	restore   *RestoreSpec
	resources []*unstructured.Unstructured
}

// selectBackupResources returns the resources to persist, in their fetched order,
// dropping the two classes that must never be recreated on restore:
//   - resources with metadata.deletionTimestamp set: they are being deleted, so
//     restoring them would recreate objects the cluster was actively tearing down.
//   - Jobs that have already run to termination (Complete or Failed condition
//     True): recreating them on restore would re-run finished work.
//
// Resources are otherwise kept in full fidelity (including status); the transient,
// cluster-assigned metadata is stripped later, at restore time (see
// stripRestoreMetadata), matching Velero's model.
func (h *BRHandler) selectBackupResources(resources []*unstructured.Unstructured) []*unstructured.Unstructured {
	selected := make([]*unstructured.Unstructured, 0, len(resources))
	for _, resource := range resources {
		if resource.GetDeletionTimestamp() != nil {
			h.Log.Info("Skipping resource being deleted",
				"kind", resource.GetKind(), "name", resource.GetName(), "namespace", resource.GetNamespace())
			continue
		}
		if isJobCompletedOrFailed(resource) {
			h.Log.Info("Skipping completed/failed Job",
				"name", resource.GetName(), "namespace", resource.GetNamespace())
			continue
		}
		selected = append(selected, resource)
	}
	return selected
}

// writeRestoreGroup writes one restore ordinal group's resources and its per-group
// restore config into groupDir, numbering the resource files 1-based across the
// whole group with fileWidth-padded prefixes so lexical and numeric ordering agree.
// It records each contributing backup in the tracker and returns the checksum
// manifest lines for every file written (resources and the config). The per-group
// config travels with the backup because the post-pivot restore reads only from
// disk (the Restore CR ConfigMaps are not guaranteed to exist in the upgraded
// cluster), so the status-resource selection and restorePVs flag must be persisted.
func (h *BRHandler) writeRestoreGroup(groupDir, groupDirName string, bucket []collectedBackup, fileWidth int, bt *BackupTracker) ([]string, error) {
	var checksums []string
	idx := 1
	var cfgEntries []groupRestoreConfigEntry

	for _, c := range bucket {
		files := make([]string, 0, len(c.resources))
		for _, resource := range c.resources {
			data, err := k8syaml.Marshal(resource.Object)
			if err != nil {
				bt.FailedBackups = append(bt.FailedBackups, c.spec.Name)
				return nil, fmt.Errorf("failed to marshal resource: %w", err)
			}

			ns := resource.GetNamespace()
			name := resource.GetName()
			kind := resource.GetKind()
			fileName := fmt.Sprintf("%0*d_%s_%s_%s.yaml", fileWidth, idx, strings.ToLower(kind), ns, name)
			filePath := filepath.Join(groupDir, fileName)

			if err := os.WriteFile(filePath, data, 0o600); err != nil {
				bt.FailedBackups = append(bt.FailedBackups, c.spec.Name)
				return nil, NewBRFailedError("Backup",
					fmt.Sprintf("failed to write resource file %s: %s", filePath, err.Error()))
			}

			logFields := []interface{}{
				"file", filepath.Join(groupDirName, fileName), "resourceType", kind, "name", name,
			}
			if ns != "" {
				logFields = append(logFields, "namespace", ns)
			}
			h.Log.Info("Resource backed up", logFields...)

			hash := sha256.Sum256(data)
			checksums = append(checksums, fmt.Sprintf("%s  %s",
				hex.EncodeToString(hash[:]), filepath.Join(groupDirName, fileName)))
			files = append(files, fileName)
			idx++
		}

		cfgEntries = append(cfgEntries, groupRestoreConfigEntry{
			RestoreStatusResources: restoreStatusResourcesOf(c.restore),
			RestorePVs:             restorePVsOf(c.restore),
			Files:                  files,
		})

		h.Log.Info("Backup completed for spec", "name", c.spec.Name, "resourceCount", len(c.resources))
		bt.SucceededBackups = append(bt.SucceededBackups, c.spec.Name)
	}

	cfgChecksum, err := writeGroupRestoreConfig(groupDir, groupDirName, cfgEntries)
	if err != nil {
		return nil, err
	}
	checksums = append(checksums, cfgChecksum)

	return checksums, nil
}

// assignRestoreOrdinals buckets the collected backups into 1-based restore ordinal
// groups. The distinct restore waves are ordered ascending, with backups that have
// no paired Restore apply-wave forming the final group; backups sharing a wave
// share an ordinal. It returns the buckets indexed by ordinal (1..numGroups; index
// 0 is unused) and the number of groups.
func assignRestoreOrdinals(collected []collectedBackup) ([][]collectedBackup, int) {
	type groupKey struct {
		wave    int
		hasWave bool
	}

	keyOf := func(c collectedBackup) groupKey {
		wave, hasWave := resolveRestoreWave(c.restore)
		return groupKey{wave: wave, hasWave: hasWave}
	}

	var keys []groupKey
	seen := map[groupKey]bool{}
	for _, c := range collected {
		k := keyOf(c)
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}

	// Explicit waves first, ascending; the no-wave group sorts last.
	sort.SliceStable(keys, func(i, j int) bool {
		if keys[i].hasWave != keys[j].hasWave {
			return keys[i].hasWave
		}
		return keys[i].wave < keys[j].wave
	})

	ordinalOf := make(map[groupKey]int, len(keys))
	for i, k := range keys {
		ordinalOf[k] = i + 1
	}

	buckets := make([][]collectedBackup, len(keys)+1)
	for _, c := range collected {
		o := ordinalOf[keyOf(c)]
		buckets[o] = append(buckets[o], c)
	}
	return buckets, len(keys)
}

// resolveRestoreWave returns the apply-wave of a backup's paired Restore CR and
// whether one was set. The Backup CR's apply-wave never affects restore ordering,
// so a backup without a paired Restore apply-wave reports hasWave=false and is
// placed in the final restore group. The value is parsed defensively;
// ValidateRestoreSpecs already rejects a non-numeric Restore apply-wave during Prep.
func resolveRestoreWave(restore *RestoreSpec) (int, bool) {
	if restore != nil && restore.ApplyWave != "" {
		if w, err := strconv.Atoi(restore.ApplyWave); err == nil {
			return w, true
		}
	}
	return 0, false
}

// numDigits returns the number of decimal digits needed to represent n, and at
// least 1 so an empty group still yields a usable padding width.
func numDigits(n int) int {
	if n <= 0 {
		return 1
	}
	d := 0
	for n > 0 {
		d++
		n /= 10
	}
	return d
}

// restoreStatusResourcesOf returns the status-resource selection of a paired
// Restore CR, or nil when there is none.
func restoreStatusResourcesOf(restore *RestoreSpec) []string {
	if restore == nil {
		return nil
	}
	return restore.RestoreStatusResources
}

// restorePVsOf returns the restorePVs flag of a paired Restore CR, or false when
// there is none.
func restorePVsOf(restore *RestoreSpec) bool {
	return restore != nil && restore.RestorePVs
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

// specRestoreConfig is the restore-time configuration persisted alongside a
// backed-up spec's resources. It is derived from the paired Restore CR at backup
// time and consumed by the post-pivot restore, which reads only from disk.
type specRestoreConfig struct {
	// RestoreStatusResources lists the resource types (as written in the Restore
	// CR's spec.restoreStatus.includedResources) whose status must be restored.
	RestoreStatusResources []string `json:"restoreStatusResources,omitempty"`
	// RestorePVs mirrors the Restore CR's spec.restorePVs: when true, the restore
	// resets PersistentVolume binding so the PV can rebind to its restored PVC.
	RestorePVs bool `json:"restorePVs,omitempty"`
}

// statusResourceSet returns the lowercased set of pluralized resource names whose
// status should be restored, or nil when none are configured.
func (c *specRestoreConfig) statusResourceSet() map[string]bool {
	if c == nil {
		return nil
	}
	return resolveStatusResources(&RestoreSpec{RestoreStatusResources: c.RestoreStatusResources})
}

// groupRestoreConfig is persisted once per restore ordinal group directory. It
// records, for each backup that contributed resources to the group, the
// restore-time configuration derived from its paired Restore CR and the resource
// files in the group that configuration applies to. Multiple backups can land in
// the same group (when their Restore CRs share an apply-wave), so the per-backup
// configuration is kept distinct and associated with its own files.
type groupRestoreConfig struct {
	Entries []groupRestoreConfigEntry `json:"entries"`
}

// groupRestoreConfigEntry is one backup's restore-time configuration within a
// restore group, together with the resource file names (within that group
// directory) it applies to.
type groupRestoreConfigEntry struct {
	// RestoreStatusResources lists the resource types (as written in the Restore
	// CR's spec.restoreStatus.includedResources) whose status must be restored.
	RestoreStatusResources []string `json:"restoreStatusResources,omitempty"`
	// RestorePVs mirrors the Restore CR's spec.restorePVs: when true, the restore
	// resets PersistentVolume binding so the PV can rebind to its restored PVC.
	RestorePVs bool `json:"restorePVs,omitempty"`
	// Files are the resource file names in the group directory this configuration
	// applies to.
	Files []string `json:"files,omitempty"`
}

// writeGroupRestoreConfig persists the restore-time configuration for every backup
// in a restore group into the group directory and returns the checksum manifest
// line for the written file. A group with no configured backups still produces a
// valid (empty) configuration so the restore always finds a config file to read.
func writeGroupRestoreConfig(groupDir, groupDirName string, entries []groupRestoreConfigEntry) (string, error) {
	cfg := groupRestoreConfig{Entries: entries}

	data, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("failed to marshal restore config: %w", err)
	}

	cfgPath := filepath.Join(groupDir, restoreConfigFileName)
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		return "", NewBRFailedError("Backup",
			fmt.Sprintf("failed to write restore config %s: %s", cfgPath, err.Error()))
	}

	hash := sha256.Sum256(data)
	return fmt.Sprintf("%s  %s", hex.EncodeToString(hash[:]), filepath.Join(groupDirName, restoreConfigFileName)), nil
}

// isJobCompletedOrFailed reports whether the resource is a Job that has already
// run to termination, i.e. it has a Complete or Failed condition set to True.
// Such Jobs are skipped during backup: recreating them on restore would re-run
// finished work. A Pod launched by a Job (or a Job created by a CronJob) is a
// backup-configuration concern, not handled here.
func isJobCompletedOrFailed(resource *unstructured.Unstructured) bool {
	if resource.GetKind() != kindJob {
		return false
	}
	conditions, found, err := unstructured.NestedSlice(resource.Object, "status", "conditions")
	if !found || err != nil {
		return false
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		condType, _ := cond["type"].(string)
		condStatus, _ := cond["status"].(string)
		if (condType == "Complete" || condType == "Failed") && condStatus == "True" {
			return true
		}
	}
	return false
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
