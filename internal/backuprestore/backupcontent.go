/*
Copyright 2026.

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
	"fmt"
	"strconv"

	backupcontentv1 "github.com/openshift-kni/lifecycle-agent/api/backupcontent/v1"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// logicalVolumesStatusResource is the resource whose status the BackupContent path
// always restores (restoreStatus is always-on on this path). It mirrors the
// spec.restoreStatus.includedResources entry the oadpContent path carries for
// TopoLVM logical volumes.
const logicalVolumesStatusResource = "logicalvolumes"

// startBackupFromBackupContent runs the BackupContent backup path: it resolves the
// referenced BackupContent CRs from LCA's install namespace, fetches the configured
// resources per entry (ordered by backupWave), and hands the collected backups to
// the shared write pass (writeCollectedBackups), which produces the identical
// layoutVersion 1.0.0 on-disk layout as the oadpContent path.
func (h *BRHandler) startBackupFromBackupContent(ctx context.Context, refs []ibuv1.BackupContentRef, bt *BackupTracker) (*BackupTracker, error) {
	backupContents, err := h.resolveBackupContentRefs(ctx, refs)
	if err != nil {
		return bt, err
	}
	if len(backupContents) == 0 {
		h.Log.Info("No backup content found, skipping")
		return bt, nil
	}

	// Build a synthetic BackupSpec per BackupContent so capture is ordered by
	// backupWave (omitted waves last), reusing SortBackupSpecsByApplyWave. The
	// BackupContent is looked up by name to fetch its resources below.
	specs := make([]BackupSpec, 0, len(backupContents))
	byName := make(map[string]*backupcontentv1.BackupContent, len(backupContents))
	for i := range backupContents {
		bc := &backupContents[i]
		specs = append(specs, backupContentToBackupSpec(bc))
		byName[bc.Name] = bc
	}

	sortedGroups, err := SortBackupSpecsByApplyWave(specs)
	if err != nil {
		return bt, fmt.Errorf("failed to sort backup content: %w", err)
	}

	// Fetch every BackupContent's resources, processing lower backupWaves first.
	// The on-disk layout is driven separately by the restoreWave (see
	// writeCollectedBackups -> assignRestoreOrdinals): backupWave never affects
	// restore ordering.
	collected := make([]collectedBackup, 0, len(specs))
	for groupIdx, group := range sortedGroups {
		h.Log.Info("Processing backup content group", "groupIndex", groupIdx+1, "totalGroups", len(sortedGroups))
		for _, spec := range group {
			if err := validateBackupName(spec.Name); err != nil {
				bt.FailedBackups = append(bt.FailedBackups, spec.Name)
				return bt, NewBRFailedValidationError("Backup", err.Error())
			}

			bc := byName[spec.Name]
			resources, err := h.fetchResourcesFromBackupContent(ctx, bc)
			if err != nil {
				bt.FailedBackups = append(bt.FailedBackups, spec.Name)
				return bt, NewBRFailedError("Backup",
					fmt.Sprintf("failed to fetch resources for backup content %s: %s", spec.Name, err.Error()))
			}

			collected = append(collected, collectedBackup{
				spec:      spec,
				restore:   backupContentToRestoreSpec(bc),
				resources: h.selectBackupResources(resources),
			})
		}
	}

	backupDir, err := prepareLocalBackupDir()
	if err != nil {
		return bt, err
	}

	if err := h.writeCollectedBackups(backupDir, collected, bt); err != nil {
		return bt, err
	}
	return bt, nil
}

// resolveBackupContentRefs resolves each name-only BackupContent reference in LCA's
// install namespace (common.OperatorNamespace()), the same way seedImageRef's
// pullSecretRef is resolved. A reference that does not resolve there is a terminal
// Upgrade failure (BRFailedValidationError, surfaced through PrePivot's existing
// IsBRFailedValidationError branch), which is also what makes a hub/spoke namespace
// mismatch loud.
func (h *BRHandler) resolveBackupContentRefs(ctx context.Context, refs []ibuv1.BackupContentRef) ([]backupcontentv1.BackupContent, error) {
	namespace := common.OperatorNamespace()
	result := make([]backupcontentv1.BackupContent, 0, len(refs))
	for _, ref := range refs {
		bc := &backupcontentv1.BackupContent{}
		if err := h.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: namespace}, bc); err != nil {
			if k8serrors.IsNotFound(err) {
				return nil, NewBRFailedValidationError("Backup",
					fmt.Sprintf("BackupContent %q not found in namespace %q", ref.Name, namespace))
			}
			return nil, fmt.Errorf("failed to get BackupContent %q in namespace %q: %w", ref.Name, namespace, err)
		}
		result = append(result, *bc)
	}
	return result, nil
}

// backupContentToBackupSpec builds the synthetic BackupSpec used to order capture by
// backupWave. An omitted backupWave yields an empty ApplyWave, which
// SortBackupSpecsByApplyWave places in the final (last) wave.
func backupContentToBackupSpec(bc *backupcontentv1.BackupContent) BackupSpec {
	return BackupSpec{
		Name:      bc.Name,
		ApplyWave: waveString(bc.Spec.BackupWave),
	}
}

// backupContentToRestoreSpec builds the synthetic RestoreSpec that drives the
// restore ordinal group (via restoreWave) and the per-group restore-config.json.
// restorePVs and restoreStatus are always-on on the BackupContent path, so the
// written restore-config.json is interpreted by a 1.0.0 reader as "rebind PVs +
// restore logicalvolumes status" with no new shape.
func backupContentToRestoreSpec(bc *backupcontentv1.BackupContent) *RestoreSpec {
	return &RestoreSpec{
		Name:                   bc.Name,
		ApplyWave:              waveString(bc.Spec.RestoreWave),
		RestorePVs:             true,
		RestoreStatusResources: []string{logicalVolumesStatusResource},
	}
}

// waveString renders an optional wave pointer as the decimal string used by the
// apply-wave sort machinery; a nil wave yields "" (sorts last).
func waveString(wave *int32) string {
	if wave == nil {
		return ""
	}
	return strconv.Itoa(int(*wave))
}

// fetchResourcesFromBackupContent fetches the resources selected by a single
// BackupContent, honouring the CNF-26659 field semantics:
//   - the spec.Namespaces Namespace objects are always fetched first, so they
//     restore in wave 0 before their contained resources;
//   - an included entry without names lists the kind (per namespace for namespaced
//     resources) with the top-level labelSelector applied; an entry with names Gets
//     each named object and the labelSelector does NOT apply to it. One BackupContent
//     may mix named and unnamed entries, handled independently per entry;
//   - kind-and-name exclusions are applied independently of the labelSelector: an
//     excluded entry without names excludes the whole kind; one with names excludes
//     only those objects (including explicitly named included objects).
//
// List/Get failures fail closed, matching the oadpContent path, so an incomplete
// backup is never recorded as successful.
func (h *BRHandler) fetchResourcesFromBackupContent(ctx context.Context, bc *backupcontentv1.BackupContent) ([]*unstructured.Unstructured, error) {
	spec := bc.Spec
	var resources []*unstructured.Unstructured

	// Always include the Namespace objects so they are recreated (in wave 0) before
	// their contained resources during restore. A missing included namespace is a
	// configuration error (matching the oadpContent path).
	nsGVR := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	for _, ns := range spec.Namespaces {
		nsObj, err := h.DynamicClient.Resource(nsGVR).Get(ctx, ns, metav1.GetOptions{})
		if err != nil {
			if k8serrors.IsNotFound(err) {
				return nil, fmt.Errorf("included namespace %q does not exist", ns)
			}
			return nil, fmt.Errorf("failed to get namespace %s: %w", ns, err)
		}
		resources = append(resources, nsObj)
	}

	// Discover the server's preferred API resources on every fetch so that CRDs
	// installed after the controller started are visible and entry kinds resolve to
	// their preferred GVR.
	apiResourceLists, failedGroups, err := h.getServerPreferredResources()
	if err != nil {
		return nil, err
	}

	// The labelSelector applies only to unnamed included entries (never to named
	// entries and never to the excluded lists).
	selectorString, err := labelSelectorString(spec.LabelSelector)
	if err != nil {
		return nil, err
	}

	// Namespaced entries are fetched across the in-scope namespaces.
	nsExcluded := h.buildExclusionSet(spec.ExcludedNamespacedResources, apiResourceLists)
	for _, entry := range spec.NamespacedResources {
		gvr, namespaced, err := resolveRequiredGVR(entry.Kind, apiResourceLists, failedGroups)
		if err != nil {
			return nil, err
		}
		if !namespaced {
			return nil, fmt.Errorf("resource %q configured as namespaced is cluster-scoped per discovery; "+
				"move it to spec.clusterScopedResources", entry.Kind)
		}
		gr := gvr.GroupResource().String()
		if nsExcluded.excludesKind(gr) {
			continue
		}
		entryResources, err := h.fetchEntry(ctx, gvr, spec.Namespaces, entry, selectorString, nsExcluded, gr)
		if err != nil {
			return nil, err
		}
		resources = append(resources, entryResources...)
	}

	// Cluster-scoped entries are fetched in the single cluster scope (the empty
	// namespace).
	csExcluded := h.buildExclusionSet(spec.ExcludedClusterScopedResources, apiResourceLists)
	for _, entry := range spec.ClusterScopedResources {
		gvr, namespaced, err := resolveRequiredGVR(entry.Kind, apiResourceLists, failedGroups)
		if err != nil {
			return nil, err
		}
		if namespaced {
			return nil, fmt.Errorf("resource %q configured as cluster-scoped is namespace-scoped per discovery; "+
				"move it to spec.namespacedResources", entry.Kind)
		}
		gr := gvr.GroupResource().String()
		if csExcluded.excludesKind(gr) {
			continue
		}
		entryResources, err := h.fetchEntry(ctx, gvr, []string{""}, entry, selectorString, csExcluded, gr)
		if err != nil {
			return nil, err
		}
		resources = append(resources, entryResources...)
	}

	return deduplicateResources(resources), nil
}

// fetchEntry fetches a single included resource entry across the given scopes: the
// in-scope namespaces for a namespaced resource, or a single empty scope ("") for a
// cluster-scoped resource.
//
//   - An unnamed entry is listed per scope with the (already-rendered) labelSelector,
//     and any object excluded by name is dropped from the result.
//   - A named entry Gets each name across the scopes (the labelSelector does not
//     apply), skipping any name excluded by the exclusion set. A named object must
//     exist in at least one scope, else the backup fails so the configured object is
//     not silently lost on restore (matching the apply-label path). For a
//     cluster-scoped entry the single scope collapses this to "must exist".
//
// gr is the entry's canonical GroupResource string, used to look up name exclusions.
func (h *BRHandler) fetchEntry(ctx context.Context, gvr schema.GroupVersionResource,
	scopes []string, entry backupcontentv1.ResourceSelector, selectorString string,
	excluded exclusionSet, gr string) ([]*unstructured.Unstructured, error) {

	var out []*unstructured.Unstructured

	if len(entry.Names) == 0 {
		for _, scope := range scopes {
			list, err := h.resourceInterface(gvr, scope).List(ctx, metav1.ListOptions{LabelSelector: selectorString})
			if err != nil {
				return nil, fmt.Errorf("failed to list %s%s: %w", entry.Kind, scopeSuffix(scope), err)
			}
			for i := range list.Items {
				item := &list.Items[i]
				if excluded.excludesName(gr, item.GetName()) {
					continue
				}
				out = append(out, item)
			}
		}
		return out, nil
	}

	for _, name := range entry.Names {
		if excluded.excludesName(gr, name) {
			continue
		}
		found := false
		for _, scope := range scopes {
			obj, err := h.resourceInterface(gvr, scope).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				if k8serrors.IsNotFound(err) {
					// Not in this scope; it may be in another in-scope namespace.
					continue
				}
				return nil, fmt.Errorf("failed to get resource %s %q%s: %w", entry.Kind, name, scopeSuffix(scope), err)
			}
			out = append(out, obj)
			found = true
		}
		if !found {
			return nil, fmt.Errorf("resource %s %q not found in any in-scope namespace", entry.Kind, name)
		}
	}
	return out, nil
}

// scopeSuffix renders a namespace scope as a short error-message suffix, or "" for
// the cluster scope.
func scopeSuffix(scope string) string {
	if scope == "" {
		return ""
	}
	return fmt.Sprintf(" in namespace %s", scope)
}

// resourceInterface returns the dynamic client's resource interface scoped to a
// namespace when one is given, or cluster-scoped otherwise.
func (h *BRHandler) resourceInterface(gvr schema.GroupVersionResource, namespace string) dynamic.ResourceInterface {
	if namespace != "" {
		return h.DynamicClient.Resource(gvr).Namespace(namespace)
	}
	return h.DynamicClient.Resource(gvr)
}

// labelSelectorString renders a label selector to its string form, or "" when no
// selector is configured.
func labelSelectorString(selector *metav1.LabelSelector) (string, error) {
	if selector == nil {
		return "", nil
	}
	s, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return "", fmt.Errorf("failed to convert label selector: %w", err)
	}
	return s.String(), nil
}

// exclusionSet is a resolved view of a BackupContent's excluded resource list. An
// entry without names excludes the whole kind (kinds); an entry with names excludes
// only those objects (names). It is independent of the labelSelector.
type exclusionSet struct {
	// kinds holds the canonical GroupResource strings excluded in full.
	kinds map[string]bool
	// names maps a canonical GroupResource string to the set of excluded object
	// names for that kind.
	names map[string]map[string]bool
}

// buildExclusionSet resolves each excluded selector through the discovery-backed
// resolver and records whether the whole kind or only specific names are excluded.
// An exclusion that cannot be resolved (an ambiguous bare name, or a kind absent
// from discovery) is not applied, but it is logged: a resource absent from discovery
// is never fetched (so it cannot be included either), yet an ambiguous bare-name
// exclusion paired with a differently-qualified include of the same resource would
// otherwise be silently ineffective. Logging keeps that diagnosable.
func (h *BRHandler) buildExclusionSet(selectors []backupcontentv1.ResourceSelector, apiResourceLists []*metav1.APIResourceList) exclusionSet {
	es := exclusionSet{kinds: map[string]bool{}, names: map[string]map[string]bool{}}
	for _, sel := range selectors {
		gvr, _, found, err := resolveResourceGVR(sel.Kind, apiResourceLists)
		if err != nil || !found {
			reason := "kind not found in cluster API discovery"
			if err != nil {
				reason = err.Error()
			}
			h.Log.Info("WARNING: BackupContent exclusion could not be resolved; it will not be applied",
				"kind", sel.Kind, "names", sel.Names, "reason", reason)
			continue
		}
		gr := gvr.GroupResource().String()
		if len(sel.Names) == 0 {
			es.kinds[gr] = true
			continue
		}
		if es.names[gr] == nil {
			es.names[gr] = map[string]bool{}
		}
		for _, name := range sel.Names {
			es.names[gr][name] = true
		}
	}
	return es
}

// excludesKind reports whether the whole kind (canonical GroupResource) is excluded.
func (es exclusionSet) excludesKind(gr string) bool {
	return es.kinds[gr]
}

// excludesName reports whether a specific object of a kind is excluded, either
// because the whole kind is excluded or because the name is listed for that kind.
func (es exclusionSet) excludesName(gr, name string) bool {
	if es.kinds[gr] {
		return true
	}
	if names := es.names[gr]; names != nil {
		return names[name]
	}
	return false
}
