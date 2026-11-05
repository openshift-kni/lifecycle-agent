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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func newNamespaceScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Version: "v1", Kind: "NamespaceList"}, &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Version: "v1", Kind: "ConfigMapList"}, &unstructured.UnstructuredList{})
	return scheme
}

// coreV1Discovery exposes the core v1 namespaced types used by the backup fetch
// tests (namespaces, configmaps) so the strict GVR-resolution path resolves them.
func coreV1Discovery() *fakeDiscovery {
	return &fakeDiscovery{byGV: map[string]*metav1.APIResourceList{
		"v1": {
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "namespaces", SingularName: "namespace", Namespaced: false, Kind: "Namespace"},
				{Name: "configmaps", SingularName: "configmap", Namespaced: true, Kind: "ConfigMap"},
			},
		},
	}}
}

func newNamespace(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]interface{}{
			"name":            name,
			"resourceVersion": "1",
		},
	}}
}

// TestFetchResourcesIncludedNamespaceExistence verifies that fetchResources fails
// the backup when an explicitly included namespace does not exist, while an
// existing included namespace is backed up normally. This mirrors Velero's
// behavior of not silently reporting a successful backup for configured
// resources that are missing.
func TestFetchResourcesIncludedNamespaceExistence(t *testing.T) {
	disc := coreV1Discovery()

	t.Run("existing included namespace is backed up", func(t *testing.T) {
		dc := dynamicfake.NewSimpleDynamicClient(newNamespaceScheme(), newNamespace("exists-ns"))
		h := &BRHandler{DynamicClient: dc, DiscoveryClient: disc, Log: logr.Discard()}

		resources, err := h.fetchResources(context.Background(), BackupSpec{
			Name:                             "backup-ok",
			IncludedNamespaces:               []string{"exists-ns"},
			IncludedNamespaceScopedResources: []string{"configmaps"},
		})
		assert.NoError(t, err)
		assert.Len(t, resources, 1, "the included namespace object must be backed up")
		assert.Equal(t, "exists-ns", resources[0].GetName())
		assert.Equal(t, "Namespace", resources[0].GetKind())
	})

	t.Run("missing included namespace fails the backup", func(t *testing.T) {
		dc := dynamicfake.NewSimpleDynamicClient(newNamespaceScheme())
		h := &BRHandler{DynamicClient: dc, DiscoveryClient: disc, Log: logr.Discard()}

		resources, err := h.fetchResources(context.Background(), BackupSpec{
			Name:               "backup-missing-ns",
			IncludedNamespaces: []string{"missing-ns"},
		})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "missing-ns")
		assert.Contains(t, err.Error(), "does not exist")
		assert.Nil(t, resources, "no resources must be returned when an included namespace is missing")
	})
}

// TestFetchResourcesUnresolvedTypeFails verifies that a configured namespace-scoped
// resource type that cannot be resolved via discovery fails the backup rather than
// being silently skipped.
func TestFetchResourcesUnresolvedTypeFails(t *testing.T) {
	disc := coreV1Discovery()
	dc := dynamicfake.NewSimpleDynamicClient(newNamespaceScheme(), newNamespace("app-ns"))
	h := &BRHandler{DynamicClient: dc, DiscoveryClient: disc, Log: logr.Discard()}

	_, err := h.fetchResources(context.Background(), BackupSpec{
		Name:                             "backup-unresolved",
		IncludedNamespaces:               []string{"app-ns"},
		IncludedNamespaceScopedResources: []string{"widgets.example.io"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "widgets.example.io")
	assert.Contains(t, err.Error(), "not found")
}

// TestFetchClusterScopedResourceScopeMismatch verifies that a resource configured
// as cluster-scoped but reported namespace-scoped by discovery fails the backup.
func TestFetchClusterScopedResourceScopeMismatch(t *testing.T) {
	disc := coreV1Discovery() // configmaps are namespace-scoped
	dc := dynamicfake.NewSimpleDynamicClient(newNamespaceScheme())
	h := &BRHandler{DynamicClient: dc, DiscoveryClient: disc, Log: logr.Discard()}

	_, err := h.fetchResources(context.Background(), BackupSpec{
		Name:                           "backup-scope",
		IncludedClusterScopedResources: []string{"configmaps"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "namespace-scoped per discovery")
}

// TestIsJobCompletedOrFailed verifies that only Jobs with a Complete or Failed
// condition set to True are reported as terminated.
func TestIsJobCompletedOrFailed(t *testing.T) {
	newJob := func(conditions []interface{}) *unstructured.Unstructured {
		obj := map[string]interface{}{
			"apiVersion": "batch/v1",
			"kind":       "Job",
			"metadata":   map[string]interface{}{"name": "j", "namespace": "ns"},
		}
		if conditions != nil {
			obj["status"] = map[string]interface{}{"conditions": conditions}
		}
		return &unstructured.Unstructured{Object: obj}
	}
	cond := func(t, s string) interface{} {
		return map[string]interface{}{"type": t, "status": s}
	}

	t.Run("Complete condition True is terminated", func(t *testing.T) {
		assert.True(t, isJobCompletedOrFailed(newJob([]interface{}{cond("Complete", "True")})))
	})
	t.Run("Failed condition True is terminated", func(t *testing.T) {
		assert.True(t, isJobCompletedOrFailed(newJob([]interface{}{cond("Failed", "True")})))
	})
	t.Run("Complete condition False is not terminated", func(t *testing.T) {
		assert.False(t, isJobCompletedOrFailed(newJob([]interface{}{cond("Complete", "False")})))
	})
	t.Run("running Job with no conditions is not terminated", func(t *testing.T) {
		assert.False(t, isJobCompletedOrFailed(newJob(nil)))
	})
	t.Run("non-Job resource is never terminated", func(t *testing.T) {
		pod := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"status":     map[string]interface{}{"conditions": []interface{}{cond("Complete", "True")}},
		}}
		assert.False(t, isJobCompletedOrFailed(pod))
	})
}

// TestWriteAndLoadGroupRestoreConfig verifies that a restore group's per-backup
// configuration round-trips through disk, maps each resource file to its own
// configuration (so backups sharing a group keep distinct settings), and that its
// checksum entry matches the written file.
func TestWriteAndLoadGroupRestoreConfig(t *testing.T) {
	backupDir := t.TempDir()
	dirName := "restore-1"
	groupDir := filepath.Join(backupDir, dirName)
	assert.NoError(t, os.MkdirAll(groupDir, 0o700))

	entries := []groupRestoreConfigEntry{
		{
			RestoreStatusResources: []string{"LogicalVolumes", "deployments"},
			RestorePVs:             true,
			Files:                  []string{"1_persistentvolume__pv1.yaml", "2_deployment_app-ns_d1.yaml"},
		},
		{
			RestorePVs: false,
			Files:      []string{"3_configmap_app-ns_cm1.yaml"},
		},
	}

	checksum, err := writeGroupRestoreConfig(groupDir, dirName, entries)
	assert.NoError(t, err)
	assert.Contains(t, checksum, filepath.Join(dirName, restoreConfigFileName))

	relPath := filepath.Join(dirName, restoreConfigFileName)
	fileConfig, err := loadGroupRestoreConfig(groupDir, dirName, map[string]bool{relPath: true})
	assert.NoError(t, err)

	// The first backup's files carry its status/PV configuration.
	cfg := fileConfig["1_persistentvolume__pv1.yaml"]
	assert.NotNil(t, cfg)
	assert.True(t, cfg.RestorePVs)
	assert.Equal(t, []string{"LogicalVolumes", "deployments"}, cfg.RestoreStatusResources)
	assert.True(t, cfg.statusResourceSet()["logicalvolumes"])
	assert.True(t, cfg.statusResourceSet()["deployments"])
	assert.Same(t, cfg, fileConfig["2_deployment_app-ns_d1.yaml"])

	// The second backup in the same group keeps its own (different) configuration.
	cfg2 := fileConfig["3_configmap_app-ns_cm1.yaml"]
	assert.NotNil(t, cfg2)
	assert.False(t, cfg2.RestorePVs)
	assert.Empty(t, cfg2.RestoreStatusResources)

	t.Run("empty entries write a valid empty config", func(t *testing.T) {
		d := t.TempDir()
		gd := filepath.Join(d, "restore-2")
		assert.NoError(t, os.MkdirAll(gd, 0o700))
		_, err := writeGroupRestoreConfig(gd, "restore-2", nil)
		assert.NoError(t, err)
		data, err := os.ReadFile(filepath.Join(gd, restoreConfigFileName))
		assert.NoError(t, err)
		parsed := &groupRestoreConfig{}
		assert.NoError(t, json.Unmarshal(data, parsed))
		assert.Empty(t, parsed.Entries)
	})

	t.Run("config file not in checksum manifest is rejected", func(t *testing.T) {
		_, err := loadGroupRestoreConfig(groupDir, dirName, map[string]bool{})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not listed in checksum manifest")
	})

	t.Run("missing config file yields an empty map", func(t *testing.T) {
		emptyDir := t.TempDir()
		fc, err := loadGroupRestoreConfig(emptyDir, "restore-3", map[string]bool{})
		assert.NoError(t, err)
		assert.Empty(t, fc)
	})
}

// TestAssignRestoreOrdinals verifies that backups are mapped to 1-based restore
// ordinal groups ordered by the paired Restore CR's apply-wave (missing last),
// that duplicate waves share an ordinal, and that the Backup CR's apply-wave never
// affects the grouping.
func TestAssignRestoreOrdinals(t *testing.T) {
	// Backup apply-waves are set opposite to the restore apply-waves to prove they
	// do not influence the restore grouping.
	collected := []collectedBackup{
		{spec: BackupSpec{Name: "e", ApplyWave: "1"}, restore: nil},                               // missing restore wave -> final group
		{spec: BackupSpec{Name: "c", ApplyWave: "99"}, restore: &RestoreSpec{ApplyWave: "0"}},     // wave 0
		{spec: BackupSpec{Name: "a", ApplyWave: "-999"}, restore: &RestoreSpec{ApplyWave: "-10"}}, // wave -10
		{spec: BackupSpec{Name: "d", ApplyWave: "0"}, restore: &RestoreSpec{ApplyWave: "50"}},     // wave 50
		{spec: BackupSpec{Name: "b", ApplyWave: ""}, restore: &RestoreSpec{ApplyWave: "-1"}},      // wave -1
		{spec: BackupSpec{Name: "c2"}, restore: &RestoreSpec{ApplyWave: "0"}},                     // duplicate wave 0
	}

	buckets, numGroups := assignRestoreOrdinals(collected)
	assert.Equal(t, 5, numGroups, "five distinct restore waves (-10, -1, 0, 50, missing)")

	// restore-1: wave -10, restore-2: wave -1, restore-3: wave 0 (shared),
	// restore-4: wave 50, restore-5: missing.
	assert.Equal(t, []string{"a"}, specNames(buckets[1]))
	assert.Equal(t, []string{"b"}, specNames(buckets[2]))
	assert.Equal(t, []string{"c", "c2"}, specNames(buckets[3]))
	assert.Equal(t, []string{"d"}, specNames(buckets[4]))
	assert.Equal(t, []string{"e"}, specNames(buckets[5]))
}

func specNames(bucket []collectedBackup) []string {
	names := make([]string, 0, len(bucket))
	for _, c := range bucket {
		names = append(names, c.spec.Name)
	}
	return names
}

// TestLoadBackupResourcesByWaveNumericOrder verifies that restore ordinal group
// directories are loaded in numeric ordinal order regardless of zero-padding, that
// the resource files within a group are ordered by their numeric index prefix
// (not lexically), that the per-group config is associated with the right files,
// and that an unprefixed resource file is rejected.
func TestLoadBackupResourcesByWaveNumericOrder(t *testing.T) {
	backupPath := t.TempDir()
	validatedFiles := map[string]bool{}

	writeCM := func(dir, fileName, name string) {
		content := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\n  namespace: ns\n"
		assert.NoError(t, os.WriteFile(filepath.Join(backupPath, dir, fileName), []byte(content), 0o600))
		validatedFiles[filepath.Join(dir, fileName)] = true
	}

	// Two groups written with two-digit zero-padding to exercise numeric parsing.
	assert.NoError(t, os.MkdirAll(filepath.Join(backupPath, "restore-01"), 0o700))
	assert.NoError(t, os.MkdirAll(filepath.Join(backupPath, "restore-02"), 0o700))

	// Group 1 holds ten files; "10_" must sort after "2_" (numeric), not before it
	// (lexical).
	var g1Files []string
	for i := 1; i <= 10; i++ {
		fileName := fmt.Sprintf("%d_configmap_ns_cm-%02d.yaml", i, i)
		writeCM("restore-01", fileName, fmt.Sprintf("cm-%02d", i))
		g1Files = append(g1Files, fileName)
	}
	g1Cfg, err := writeGroupRestoreConfig(
		filepath.Join(backupPath, "restore-01"), "restore-01",
		[]groupRestoreConfigEntry{{RestorePVs: true, Files: g1Files}})
	assert.NoError(t, err)
	validatedFiles[filepath.Join("restore-01", restoreConfigFileName)] = true
	assert.Contains(t, g1Cfg, filepath.Join("restore-01", restoreConfigFileName))

	// Group 2 holds a single file.
	writeCM("restore-02", "1_configmap_ns_late.yaml", "late")
	_, err = writeGroupRestoreConfig(
		filepath.Join(backupPath, "restore-02"), "restore-02",
		[]groupRestoreConfigEntry{{Files: []string{"1_configmap_ns_late.yaml"}}})
	assert.NoError(t, err)
	validatedFiles[filepath.Join("restore-02", restoreConfigFileName)] = true

	groups, err := loadBackupResourcesByWave(backupPath, validatedFiles, logr.Discard())
	assert.NoError(t, err)
	assert.Len(t, groups, 2)

	// Group 1 comes before group 2 (ordinal order) and holds its ten files in
	// numeric-index order.
	assert.Equal(t, 1, groups[0].wave)
	assert.Len(t, groups[0].resources, 10)
	var loadedOrder []string
	for _, r := range groups[0].resources {
		loadedOrder = append(loadedOrder, r.fileName)
		assert.NotNil(t, r.restoreConfig, "every group-1 file must carry its config")
		assert.True(t, r.restoreConfig.RestorePVs)
	}
	assert.Equal(t, g1Files, loadedOrder, "resources must load in numeric-index order")

	assert.Equal(t, 2, groups[1].wave)
	assert.Len(t, groups[1].resources, 1)

	t.Run("resource file without a numeric prefix is rejected", func(t *testing.T) {
		bad := t.TempDir()
		assert.NoError(t, os.MkdirAll(filepath.Join(bad, "restore-1"), 0o700))
		assert.NoError(t, os.WriteFile(
			filepath.Join(bad, "restore-1", "noindex.yaml"),
			[]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"), 0o600))
		vf := map[string]bool{filepath.Join("restore-1", "noindex.yaml"): true}
		_, err := loadBackupResourcesByWave(bad, vf, logr.Discard())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "no numeric index prefix")
	})
}

// TestFetchResourcesApplyLabelMissingFails verifies that an apply-label entry
// naming an object that does not exist fails the backup.
func TestFetchResourcesApplyLabelMissingFails(t *testing.T) {
	disc := coreV1Discovery()
	dc := dynamicfake.NewSimpleDynamicClient(newNamespaceScheme())
	h := &BRHandler{DynamicClient: dc, DiscoveryClient: disc, Log: logr.Discard()}

	_, err := h.fetchResources(context.Background(), BackupSpec{
		Name:       "backup-applylabel",
		ApplyLabel: "v1/configmaps/app-ns/missing-cm",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing-cm")
	assert.Contains(t, err.Error(), "not found")
}
