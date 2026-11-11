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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	backupcontentv1 "github.com/openshift-kni/lifecycle-agent/api/backupcontent/v1"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func int32Ptr(v int32) *int32 { return &v }

// newBackupContentScheme builds a typed-client scheme with the core types and the
// BackupContent CRD registered, so the fake client can serve BackupContent Gets.
func newBackupContentScheme(t *testing.T) *runtime.Scheme {
	s := runtime.NewScheme()
	assert.NoError(t, corev1.AddToScheme(s))
	assert.NoError(t, backupcontentv1.AddToScheme(s))
	return s
}

func newBackupContentTypedClient(t *testing.T, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(newBackupContentScheme(t)).WithObjects(objs...).Build()
}

// backupContentDynScheme registers the core v1 types the BackupContent fetch tests
// exercise (namespaces, configmaps, secrets, persistentvolumes) for the fake
// dynamic client.
func backupContentDynScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, kind := range []string{"Namespace", "ConfigMap", "Secret", "PersistentVolume"} {
		s.AddKnownTypeWithName(schema.GroupVersionKind{Version: "v1", Kind: kind}, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(schema.GroupVersionKind{Version: "v1", Kind: kind + "List"}, &unstructured.UnstructuredList{})
	}
	return s
}

// backupContentDiscovery advertises the core v1 types used by the fetch tests with
// their correct namespaced scope.
func backupContentDiscovery() *fakeDiscovery {
	return &fakeDiscovery{byGV: map[string]*metav1.APIResourceList{
		"v1": {GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "namespaces", SingularName: "namespace", Namespaced: false, Kind: "Namespace"},
			{Name: "configmaps", SingularName: "configmap", Namespaced: true, Kind: "ConfigMap"},
			{Name: "secrets", SingularName: "secret", Namespaced: true, Kind: "Secret"},
			{Name: "persistentvolumes", SingularName: "persistentvolume", Namespaced: false, Kind: "PersistentVolume"},
		}},
	}}
}

func newUnstructuredWithLabels(kind, name, namespace string, labels map[string]interface{}) *unstructured.Unstructured {
	meta := map[string]interface{}{
		"name":            name,
		"resourceVersion": "1",
	}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	if labels != nil {
		meta["labels"] = labels
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       kind,
		"metadata":   meta,
	}}
}

func resourceNames(resources []*unstructured.Unstructured) []string {
	names := make([]string, 0, len(resources))
	for _, r := range resources {
		names = append(names, r.GetName())
	}
	return names
}

// TestBackupContentToSpecs verifies the BackupContent -> synthetic BackupSpec /
// RestoreSpec conversion: waves map to the apply-wave strings used by the sort
// machinery (nil -> "" sorts last, explicit 0 -> own group), and restorePVs /
// restoreStatus are always-on on the BackupContent path.
func TestBackupContentToSpecs(t *testing.T) {
	t.Run("explicit waves map to apply-wave strings; restore is always-on", func(t *testing.T) {
		bc := &backupcontentv1.BackupContent{
			ObjectMeta: metav1.ObjectMeta{Name: "bc1"},
			Spec: backupcontentv1.BackupContentSpec{
				BackupWave:  int32Ptr(3),
				RestoreWave: int32Ptr(7),
			},
		}
		backupSpec := backupContentToBackupSpec(bc)
		assert.Equal(t, "bc1", backupSpec.Name)
		assert.Equal(t, "3", backupSpec.ApplyWave)

		restoreSpec := backupContentToRestoreSpec(bc)
		assert.Equal(t, "bc1", restoreSpec.Name)
		assert.Equal(t, "7", restoreSpec.ApplyWave)
		assert.True(t, restoreSpec.RestorePVs, "restorePVs is always-on on the BackupContent path")
		assert.Equal(t, []string{"logicalvolumes"}, restoreSpec.RestoreStatusResources,
			"restoreStatus for logicalvolumes is always-on on the BackupContent path")
	})

	t.Run("omitted wave yields empty apply-wave (sorts last)", func(t *testing.T) {
		bc := &backupcontentv1.BackupContent{ObjectMeta: metav1.ObjectMeta{Name: "bc-nowave"}}
		assert.Equal(t, "", backupContentToBackupSpec(bc).ApplyWave)
		assert.Equal(t, "", backupContentToRestoreSpec(bc).ApplyWave)
	})

	t.Run("explicit wave 0 maps to \"0\" (its own group)", func(t *testing.T) {
		bc := &backupcontentv1.BackupContent{
			ObjectMeta: metav1.ObjectMeta{Name: "bc-zero"},
			Spec:       backupcontentv1.BackupContentSpec{BackupWave: int32Ptr(0), RestoreWave: int32Ptr(0)},
		}
		assert.Equal(t, "0", backupContentToBackupSpec(bc).ApplyWave)
		assert.Equal(t, "0", backupContentToRestoreSpec(bc).ApplyWave)
	})

	// backupWave orders capture via SortBackupSpecsByApplyWave over the synthetic
	// specs: numbered waves ascending, omitted last, explicit 0 in its own group.
	t.Run("backupWave orders capture (omitted last, explicit 0 separate)", func(t *testing.T) {
		specs := []BackupSpec{
			backupContentToBackupSpec(&backupcontentv1.BackupContent{ObjectMeta: metav1.ObjectMeta{Name: "late"}}),
			backupContentToBackupSpec(&backupcontentv1.BackupContent{ObjectMeta: metav1.ObjectMeta{Name: "two"}, Spec: backupcontentv1.BackupContentSpec{BackupWave: int32Ptr(2)}}),
			backupContentToBackupSpec(&backupcontentv1.BackupContent{ObjectMeta: metav1.ObjectMeta{Name: "zero"}, Spec: backupcontentv1.BackupContentSpec{BackupWave: int32Ptr(0)}}),
			backupContentToBackupSpec(&backupcontentv1.BackupContent{ObjectMeta: metav1.ObjectMeta{Name: "one"}, Spec: backupcontentv1.BackupContentSpec{BackupWave: int32Ptr(1)}}),
		}
		groups, err := SortBackupSpecsByApplyWave(specs)
		assert.NoError(t, err)
		var ordered []string
		for _, g := range groups {
			for _, s := range g {
				ordered = append(ordered, s.Name)
			}
		}
		assert.Equal(t, []string{"zero", "one", "two", "late"}, ordered)
	})
}

// TestResolveBackupContentRefs verifies that references are resolved by name from
// LCA's install namespace and that an unresolved reference is a terminal
// BRFailedValidationError (which makes a hub/spoke namespace mismatch loud).
func TestResolveBackupContentRefs(t *testing.T) {
	t.Setenv(common.OperatorNamespaceEnvVar, "lca-ns")

	bc := &backupcontentv1.BackupContent{
		ObjectMeta: metav1.ObjectMeta{Name: "present", Namespace: "lca-ns"},
		Spec:       backupcontentv1.BackupContentSpec{BackupWave: int32Ptr(1)},
	}
	c := newBackupContentTypedClient(t, bc)
	h := &BRHandler{Client: c, Log: logr.Discard()}

	t.Run("present reference resolves", func(t *testing.T) {
		got, err := h.resolveBackupContentRefs(context.Background(), []ibuv1.BackupContentRef{{Name: "present"}})
		assert.NoError(t, err)
		assert.Len(t, got, 1)
		assert.Equal(t, "present", got[0].Name)
	})

	t.Run("missing reference is a terminal validation error", func(t *testing.T) {
		_, err := h.resolveBackupContentRefs(context.Background(), []ibuv1.BackupContentRef{{Name: "absent"}})
		assert.Error(t, err)
		assert.True(t, IsBRFailedValidationError(err), "a missing ref must be a terminal validation error, got: %v", err)
		assert.Contains(t, err.Error(), "absent")
		assert.Contains(t, err.Error(), "lca-ns")
	})

	t.Run("a wrong install namespace makes the reference fail loudly", func(t *testing.T) {
		// The CR exists, but in a different namespace than LCA's install namespace.
		other := &backupcontentv1.BackupContent{ObjectMeta: metav1.ObjectMeta{Name: "spoke-only", Namespace: "other-ns"}}
		hh := &BRHandler{Client: newBackupContentTypedClient(t, other), Log: logr.Discard()}
		_, err := hh.resolveBackupContentRefs(context.Background(), []ibuv1.BackupContentRef{{Name: "spoke-only"}})
		assert.Error(t, err)
		assert.True(t, IsBRFailedValidationError(err))
	})
}

// TestFetchResourcesFromBackupContentNamedUnnamedMix verifies that a single
// BackupContent can mix named and unnamed entries: an unnamed entry is listed, a
// named entry is fetched per name, and both are backed up together (plus the
// namespace objects).
func TestFetchResourcesFromBackupContentNamedUnnamedMix(t *testing.T) {
	ns := newUnstructuredWithLabels("Namespace", "app-ns", "", nil)
	cm1 := newUnstructuredWithLabels("ConfigMap", "cm1", "app-ns", nil)
	cm2 := newUnstructuredWithLabels("ConfigMap", "cm2", "app-ns", nil)
	s1 := newUnstructuredWithLabels("Secret", "s1", "app-ns", nil)
	s2 := newUnstructuredWithLabels("Secret", "s2", "app-ns", nil)

	dc := dynamicfake.NewSimpleDynamicClient(backupContentDynScheme(), ns, cm1, cm2, s1, s2)
	h := &BRHandler{DynamicClient: dc, DiscoveryClient: backupContentDiscovery(), Log: logr.Discard()}

	bc := &backupcontentv1.BackupContent{
		ObjectMeta: metav1.ObjectMeta{Name: "mix"},
		Spec: backupcontentv1.BackupContentSpec{
			Namespaces: []string{"app-ns"},
			NamespacedResources: []backupcontentv1.ResourceSelector{
				{Kind: "configmaps"},                     // unnamed -> List
				{Kind: "secrets", Names: []string{"s1"}}, // named -> Get only s1
			},
		},
	}

	resources, err := h.fetchResourcesFromBackupContent(context.Background(), bc)
	assert.NoError(t, err)
	names := resourceNames(resources)
	assert.Contains(t, names, "app-ns", "the included namespace must be backed up")
	assert.Contains(t, names, "cm1")
	assert.Contains(t, names, "cm2")
	assert.Contains(t, names, "s1", "a named secret must be fetched")
	assert.NotContains(t, names, "s2", "an unnamed secret must not be fetched when the entry is named")
}

// TestFetchResourcesFromBackupContentLabelSelectorScope verifies that the
// top-level labelSelector filters only unnamed included entries, and never named
// entries.
func TestFetchResourcesFromBackupContentLabelSelectorScope(t *testing.T) {
	ns := newUnstructuredWithLabels("Namespace", "app-ns", "", nil)
	cmKeep := newUnstructuredWithLabels("ConfigMap", "cm-keep", "app-ns", map[string]interface{}{"app": "keep"})
	cmDrop := newUnstructuredWithLabels("ConfigMap", "cm-drop", "app-ns", nil)
	sNoLabel := newUnstructuredWithLabels("Secret", "s-nolabel", "app-ns", nil)

	dc := dynamicfake.NewSimpleDynamicClient(backupContentDynScheme(), ns, cmKeep, cmDrop, sNoLabel)
	h := &BRHandler{DynamicClient: dc, DiscoveryClient: backupContentDiscovery(), Log: logr.Discard()}

	bc := &backupcontentv1.BackupContent{
		ObjectMeta: metav1.ObjectMeta{Name: "selected"},
		Spec: backupcontentv1.BackupContentSpec{
			Namespaces:    []string{"app-ns"},
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "keep"}},
			NamespacedResources: []backupcontentv1.ResourceSelector{
				{Kind: "configmaps"},                            // unnamed -> selector applies
				{Kind: "secrets", Names: []string{"s-nolabel"}}, // named -> selector ignored
			},
		},
	}

	resources, err := h.fetchResourcesFromBackupContent(context.Background(), bc)
	assert.NoError(t, err)
	names := resourceNames(resources)
	assert.Contains(t, names, "cm-keep", "an unnamed configmap matching the selector is backed up")
	assert.NotContains(t, names, "cm-drop", "an unnamed configmap not matching the selector is filtered out")
	assert.Contains(t, names, "s-nolabel", "a named secret is backed up regardless of the selector")
}

// TestFetchResourcesFromBackupContentExclusions verifies kind-and-name exclusions
// for both namespaced and cluster-scoped resources: a kind-only exclusion drops the
// whole kind; a kind+name exclusion drops only those objects, including explicitly
// named included objects; exclusions are independent of the labelSelector.
func TestFetchResourcesFromBackupContentExclusions(t *testing.T) {
	t.Run("namespaced kind-only exclusion drops the whole kind", func(t *testing.T) {
		ns := newUnstructuredWithLabels("Namespace", "app-ns", "", nil)
		cm := newUnstructuredWithLabels("ConfigMap", "cm", "app-ns", nil)
		s := newUnstructuredWithLabels("Secret", "s", "app-ns", nil)
		dc := dynamicfake.NewSimpleDynamicClient(backupContentDynScheme(), ns, cm, s)
		h := &BRHandler{DynamicClient: dc, DiscoveryClient: backupContentDiscovery(), Log: logr.Discard()}

		bc := &backupcontentv1.BackupContent{
			ObjectMeta: metav1.ObjectMeta{Name: "excl-kind"},
			Spec: backupcontentv1.BackupContentSpec{
				Namespaces: []string{"app-ns"},
				NamespacedResources: []backupcontentv1.ResourceSelector{
					{Kind: "configmaps"},
					{Kind: "secrets"},
				},
				ExcludedNamespacedResources: []backupcontentv1.ResourceSelector{
					{Kind: "secrets"}, // kind-only -> all secrets excluded
				},
			},
		}
		resources, err := h.fetchResourcesFromBackupContent(context.Background(), bc)
		assert.NoError(t, err)
		names := resourceNames(resources)
		assert.Contains(t, names, "cm")
		assert.NotContains(t, names, "s", "a kind-only exclusion drops the whole kind")
	})

	t.Run("namespaced kind+name exclusion drops only the named objects, including named includes", func(t *testing.T) {
		ns := newUnstructuredWithLabels("Namespace", "app-ns", "", nil)
		cmKeep := newUnstructuredWithLabels("ConfigMap", "cm-keep", "app-ns", nil)
		cmExcluded := newUnstructuredWithLabels("ConfigMap", "cm-excluded", "app-ns", nil)
		dc := dynamicfake.NewSimpleDynamicClient(backupContentDynScheme(), ns, cmKeep, cmExcluded)
		h := &BRHandler{DynamicClient: dc, DiscoveryClient: backupContentDiscovery(), Log: logr.Discard()}

		bc := &backupcontentv1.BackupContent{
			ObjectMeta: metav1.ObjectMeta{Name: "excl-name"},
			Spec: backupcontentv1.BackupContentSpec{
				Namespaces: []string{"app-ns"},
				NamespacedResources: []backupcontentv1.ResourceSelector{
					// Explicitly named includes; the excluded name must still be removed.
					{Kind: "configmaps", Names: []string{"cm-keep", "cm-excluded"}},
				},
				ExcludedNamespacedResources: []backupcontentv1.ResourceSelector{
					{Kind: "configmaps", Names: []string{"cm-excluded"}},
				},
			},
		}
		resources, err := h.fetchResourcesFromBackupContent(context.Background(), bc)
		assert.NoError(t, err)
		names := resourceNames(resources)
		assert.Contains(t, names, "cm-keep")
		assert.NotContains(t, names, "cm-excluded", "a named-object exclusion removes even an explicitly named include")
	})

	t.Run("cluster-scoped kind+name exclusion drops only the named objects", func(t *testing.T) {
		pvKeep := newUnstructuredWithLabels("PersistentVolume", "pv-keep", "", nil)
		pvExcluded := newUnstructuredWithLabels("PersistentVolume", "pv-excluded", "", nil)
		dc := dynamicfake.NewSimpleDynamicClient(backupContentDynScheme(), pvKeep, pvExcluded)
		h := &BRHandler{DynamicClient: dc, DiscoveryClient: backupContentDiscovery(), Log: logr.Discard()}

		bc := &backupcontentv1.BackupContent{
			ObjectMeta: metav1.ObjectMeta{Name: "excl-cluster"},
			Spec: backupcontentv1.BackupContentSpec{
				ClusterScopedResources: []backupcontentv1.ResourceSelector{
					{Kind: "persistentvolumes"}, // unnamed -> List all
				},
				ExcludedClusterScopedResources: []backupcontentv1.ResourceSelector{
					{Kind: "persistentvolumes", Names: []string{"pv-excluded"}},
				},
			},
		}
		resources, err := h.fetchResourcesFromBackupContent(context.Background(), bc)
		assert.NoError(t, err)
		names := resourceNames(resources)
		assert.Contains(t, names, "pv-keep")
		assert.NotContains(t, names, "pv-excluded")
	})
}

// TestFetchResourcesFromBackupContentMissingNamespace verifies that an included
// namespace that does not exist fails the backup (matching the oadpContent path).
func TestFetchResourcesFromBackupContentMissingNamespace(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClient(backupContentDynScheme())
	h := &BRHandler{DynamicClient: dc, DiscoveryClient: backupContentDiscovery(), Log: logr.Discard()}

	bc := &backupcontentv1.BackupContent{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-ns"},
		Spec:       backupcontentv1.BackupContentSpec{Namespaces: []string{"ghost"}},
	}
	_, err := h.fetchResourcesFromBackupContent(context.Background(), bc)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ghost")
	assert.Contains(t, err.Error(), "does not exist")
}

// TestFetchResourcesFromBackupContentNamedMissing verifies that a named include
// that does not exist fails the backup so it is not silently lost on restore.
func TestFetchResourcesFromBackupContentNamedMissing(t *testing.T) {
	ns := newUnstructuredWithLabels("Namespace", "app-ns", "", nil)
	dc := dynamicfake.NewSimpleDynamicClient(backupContentDynScheme(), ns)
	h := &BRHandler{DynamicClient: dc, DiscoveryClient: backupContentDiscovery(), Log: logr.Discard()}

	bc := &backupcontentv1.BackupContent{
		ObjectMeta: metav1.ObjectMeta{Name: "named-missing"},
		Spec: backupcontentv1.BackupContentSpec{
			Namespaces: []string{"app-ns"},
			NamespacedResources: []backupcontentv1.ResourceSelector{
				{Kind: "configmaps", Names: []string{"nope"}},
			},
		},
	}
	_, err := h.fetchResourcesFromBackupContent(context.Background(), bc)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nope")
	assert.Contains(t, err.Error(), "not found")
}

// TestFetchResourcesFromBackupContentNamedAcrossNamespaces verifies that a named
// namespaced entry is backed up from whichever in-scope namespace it exists in (it
// need not exist in every in-scope namespace), while a name absent from all in-scope
// namespaces fails the backup.
func TestFetchResourcesFromBackupContentNamedAcrossNamespaces(t *testing.T) {
	nsA := newUnstructuredWithLabels("Namespace", "ns-a", "", nil)
	nsB := newUnstructuredWithLabels("Namespace", "ns-b", "", nil)
	// cm-only-a exists only in ns-a; it must still be backed up without ns-b failing.
	cmOnlyA := newUnstructuredWithLabels("ConfigMap", "cm-only-a", "ns-a", nil)
	dc := dynamicfake.NewSimpleDynamicClient(backupContentDynScheme(), nsA, nsB, cmOnlyA)
	h := &BRHandler{DynamicClient: dc, DiscoveryClient: backupContentDiscovery(), Log: logr.Discard()}

	t.Run("named object present in only one in-scope namespace is backed up", func(t *testing.T) {
		bc := &backupcontentv1.BackupContent{
			ObjectMeta: metav1.ObjectMeta{Name: "named-span"},
			Spec: backupcontentv1.BackupContentSpec{
				Namespaces: []string{"ns-a", "ns-b"},
				NamespacedResources: []backupcontentv1.ResourceSelector{
					{Kind: "configmaps", Names: []string{"cm-only-a"}},
				},
			},
		}
		resources, err := h.fetchResourcesFromBackupContent(context.Background(), bc)
		assert.NoError(t, err)
		assert.Contains(t, resourceNames(resources), "cm-only-a")
	})

	t.Run("named object absent from every in-scope namespace fails the backup", func(t *testing.T) {
		bc := &backupcontentv1.BackupContent{
			ObjectMeta: metav1.ObjectMeta{Name: "named-ghost"},
			Spec: backupcontentv1.BackupContentSpec{
				Namespaces: []string{"ns-a", "ns-b"},
				NamespacedResources: []backupcontentv1.ResourceSelector{
					{Kind: "configmaps", Names: []string{"nowhere"}},
				},
			},
		}
		_, err := h.fetchResourcesFromBackupContent(context.Background(), bc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "nowhere")
		assert.Contains(t, err.Error(), "not found")
	})
}

// startBackupTestHandler wires a BRHandler for StartBackup BackupContent-path tests,
// pointing hostPath at a temp directory and OperatorNamespace() at the namespace the
// BackupContent CRs live in.
func startBackupTestHandler(t *testing.T, ns string, dynObjs []runtime.Object, bcs ...client.Object) (*BRHandler, string) {
	t.Setenv(common.OperatorNamespaceEnvVar, ns)

	tmp := t.TempDir()
	origHostPath := hostPath
	hostPath = tmp
	t.Cleanup(func() { hostPath = origHostPath })

	dc := dynamicfake.NewSimpleDynamicClient(backupContentDynScheme(), dynObjs...)
	c := newBackupContentTypedClient(t, bcs...)
	h := &BRHandler{Client: c, DynamicClient: dc, DiscoveryClient: backupContentDiscovery(), Log: logr.Discard()}
	return h, tmp
}

// TestStartBackupSourceSelection verifies that StartBackup selects the input source
// from the two slices: backupContent -> BackupContent path, oadpContent -> the
// (existing) Velero-ConfigMap path, neither -> skip.
func TestStartBackupSourceSelection(t *testing.T) {
	t.Run("neither source skips", func(t *testing.T) {
		h := &BRHandler{Log: logr.Discard()}
		bt, err := h.StartBackup(context.Background(), nil, nil)
		assert.NoError(t, err)
		assert.Empty(t, bt.SucceededBackups)
		assert.Empty(t, bt.FailedBackups)
	})

	t.Run("backupContent source drives the BackupContent path", func(t *testing.T) {
		ns := newUnstructuredWithLabels("Namespace", "app-ns", "", nil)
		cm := newUnstructuredWithLabels("ConfigMap", "cm", "app-ns", nil)
		bc := &backupcontentv1.BackupContent{
			ObjectMeta: metav1.ObjectMeta{Name: "bc", Namespace: "lca-ns"},
			Spec: backupcontentv1.BackupContentSpec{
				Namespaces:          []string{"app-ns"},
				NamespacedResources: []backupcontentv1.ResourceSelector{{Kind: "configmaps"}},
			},
		}
		h, tmp := startBackupTestHandler(t, "lca-ns", []runtime.Object{ns, cm}, bc)

		bt, err := h.StartBackup(context.Background(), nil, []ibuv1.BackupContentRef{{Name: "bc"}})
		assert.NoError(t, err)
		assert.Contains(t, bt.SucceededBackups, "bc")

		// The on-disk layout is produced under the local backup path.
		_, err = os.Stat(filepath.Join(tmp, LocalBackupPath, backupLayoutFileName))
		assert.NoError(t, err, "a BackupContent backup must write the layout manifest")
	})

	t.Run("missing backupContent reference fails terminally before the pivot", func(t *testing.T) {
		h, _ := startBackupTestHandler(t, "lca-ns", nil)
		_, err := h.StartBackup(context.Background(), nil, []ibuv1.BackupContentRef{{Name: "ghost"}})
		assert.Error(t, err)
		assert.True(t, IsBRFailedValidationError(err),
			"a missing BackupContent must fail the Upgrade terminally, got: %v", err)
	})
}

// TestStartBackupBackupContentIndependentWaves verifies that restoreWave assigns the
// restore ordinal groups independently of backupWave: backupWave is set opposite to
// restoreWave to prove it does not influence restore ordering, and an omitted
// restoreWave forms the final group.
func TestStartBackupBackupContentIndependentWaves(t *testing.T) {
	// Each BackupContent captures a distinct namespace + configmap so the group each
	// landed in can be identified by the configmap name on disk.
	nsA := newUnstructuredWithLabels("Namespace", "ns-a", "", nil)
	nsB := newUnstructuredWithLabels("Namespace", "ns-b", "", nil)
	nsC := newUnstructuredWithLabels("Namespace", "ns-c", "", nil)
	cmA := newUnstructuredWithLabels("ConfigMap", "cm-a", "ns-a", nil)
	cmB := newUnstructuredWithLabels("ConfigMap", "cm-b", "ns-b", nil)
	cmC := newUnstructuredWithLabels("ConfigMap", "cm-c", "ns-c", nil)

	mkBC := func(name, ns string, backupWave, restoreWave *int32) *backupcontentv1.BackupContent {
		return &backupcontentv1.BackupContent{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "lca-ns"},
			Spec: backupcontentv1.BackupContentSpec{
				BackupWave:          backupWave,
				RestoreWave:         restoreWave,
				Namespaces:          []string{ns},
				NamespacedResources: []backupcontentv1.ResourceSelector{{Kind: "configmaps"}},
			},
		}
	}
	// restoreWave: bcB=5 -> restore-1, bcA=10 -> restore-2, bcC=omitted -> restore-3.
	// backupWave is opposite so it cannot be what produced that ordering.
	bcA := mkBC("bc-a", "ns-a", int32Ptr(1), int32Ptr(10))
	bcB := mkBC("bc-b", "ns-b", int32Ptr(99), int32Ptr(5))
	bcC := mkBC("bc-c", "ns-c", int32Ptr(0), nil)

	h, tmp := startBackupTestHandler(t, "lca-ns", []runtime.Object{nsA, nsB, nsC, cmA, cmB, cmC}, bcA, bcB, bcC)

	bt, err := h.StartBackup(context.Background(), nil, []ibuv1.BackupContentRef{
		{Name: "bc-a"}, {Name: "bc-b"}, {Name: "bc-c"},
	})
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"bc-a", "bc-b", "bc-c"}, bt.SucceededBackups)

	root := filepath.Join(tmp, LocalBackupPath)
	assert.Contains(t, groupFileNames(t, root, "restore-1"), "cm-b", "restoreWave 5 -> first restore group")
	assert.Contains(t, groupFileNames(t, root, "restore-2"), "cm-a", "restoreWave 10 -> second restore group")
	assert.Contains(t, groupFileNames(t, root, "restore-3"), "cm-c", "omitted restoreWave -> final restore group")
}

// groupFileNames returns the concatenated resource file names in a restore group
// directory, for asserting which backup landed in which group.
func groupFileNames(t *testing.T, root, group string) string {
	entries, err := os.ReadDir(filepath.Join(root, group))
	assert.NoError(t, err)
	var joined string
	for _, e := range entries {
		joined += e.Name() + "\n"
	}
	return joined
}

// TestStartBackupBackupContentLayoutParity verifies that the BackupContent path
// writes the identical layoutVersion 1.0.0 layout as the oadpContent path, with a
// restore-config.json a 1.0.0 reader interprets as always-on restorePVs + restore
// logicalvolumes status, and that the backup validates end to end.
func TestStartBackupBackupContentLayoutParity(t *testing.T) {
	ns := newUnstructuredWithLabels("Namespace", "app-ns", "", nil)
	cm := newUnstructuredWithLabels("ConfigMap", "cm", "app-ns", nil)
	bc := &backupcontentv1.BackupContent{
		ObjectMeta: metav1.ObjectMeta{Name: "bc", Namespace: "lca-ns"},
		Spec: backupcontentv1.BackupContentSpec{
			RestoreWave:         int32Ptr(1),
			Namespaces:          []string{"app-ns"},
			NamespacedResources: []backupcontentv1.ResourceSelector{{Kind: "configmaps"}},
		},
	}
	h, tmp := startBackupTestHandler(t, "lca-ns", []runtime.Object{ns, cm}, bc)

	_, err := h.StartBackup(context.Background(), nil, []ibuv1.BackupContentRef{{Name: "bc"}})
	assert.NoError(t, err)

	root := filepath.Join(tmp, LocalBackupPath)

	// The root-level layout manifest is 1.0.0.
	manifestData, err := os.ReadFile(filepath.Join(root, backupLayoutFileName))
	assert.NoError(t, err)
	manifest := &backupLayoutManifest{}
	assert.NoError(t, json.Unmarshal(manifestData, manifest))
	assert.Equal(t, currentLayoutVersion, manifest.LayoutVersion)
	assert.Equal(t, []string{"bc"}, manifest.BackupSpecNames)

	// The per-group restore-config.json carries always-on restorePVs + logicalvolumes
	// status, in the existing 1.0.0 shape.
	cfgData, err := os.ReadFile(filepath.Join(root, "restore-1", restoreConfigFileName))
	assert.NoError(t, err)
	cfg := &groupRestoreConfig{}
	assert.NoError(t, json.Unmarshal(cfgData, cfg))
	assert.Len(t, cfg.Entries, 1)
	assert.True(t, cfg.Entries[0].RestorePVs, "restorePVs must be always-on on the BackupContent path")
	assert.Equal(t, []string{"logicalvolumes"}, cfg.Entries[0].RestoreStatusResources)

	// The produced backup is readable by the existing 1.0.0 reader without any
	// source-specific handling: checksums verify and the layout is accepted.
	validated, err := validateChecksums(root)
	assert.NoError(t, err)
	_, err = loadAndValidateBackupLayout(root, validated, logr.Discard())
	assert.NoError(t, err)
}

// TestStartRestoreMissingBackupWithBackupContent verifies that with spec.backupContent
// configured and the backup directory missing, the restore fails terminally, exactly
// as it does for oadpContent.
func TestStartRestoreMissingBackupWithBackupContent(t *testing.T) {
	tmp := t.TempDir()
	origHostPath := hostPath
	hostPath = tmp
	defer func() { hostPath = origHostPath }()

	h := &BRHandler{Log: logr.Discard()}

	t.Run("configured backupContent but no backup on disk fails hard", func(t *testing.T) {
		rt, err := h.StartRestore(context.Background(), nil, []ibuv1.BackupContentRef{{Name: "bc"}})
		assert.Error(t, err)
		assert.True(t, IsBRFailedError(err), "a configured-but-missing backup must fail terminally, got: %v", err)
		assert.Empty(t, rt.SucceededRestores)
	})

	t.Run("no source configured and no backup on disk skips", func(t *testing.T) {
		rt, err := h.StartRestore(context.Background(), nil, nil)
		assert.NoError(t, err)
		assert.Empty(t, rt.FailedRestores)
	})
}
