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

// TestWriteAndLoadRestoreConfig verifies that the per-spec restore configuration
// round-trips through disk and that its checksum entry matches the written file.
func TestWriteAndLoadRestoreConfig(t *testing.T) {
	backupDir := t.TempDir()
	dirName := "000_b1"
	specDir := filepath.Join(backupDir, dirName)
	assert.NoError(t, os.MkdirAll(specDir, 0o700))

	restore := &RestoreSpec{
		Name:                   "r1",
		BackupName:             "b1",
		RestorePVs:             true,
		RestoreStatusResources: []string{"LogicalVolumes", "deployments"},
	}

	checksum, err := writeRestoreConfig(specDir, dirName, restore)
	assert.NoError(t, err)
	assert.Contains(t, checksum, filepath.Join(dirName, restoreConfigFileName))

	relPath := filepath.Join(dirName, restoreConfigFileName)
	cfg, err := loadRestoreConfig(specDir, dirName, map[string]bool{relPath: true})
	assert.NoError(t, err)
	assert.NotNil(t, cfg)
	assert.True(t, cfg.RestorePVs)
	assert.Equal(t, []string{"LogicalVolumes", "deployments"}, cfg.RestoreStatusResources)
	assert.True(t, cfg.statusResourceSet()["logicalvolumes"])
	assert.True(t, cfg.statusResourceSet()["deployments"])

	t.Run("nil restore spec writes a valid empty config", func(t *testing.T) {
		d := t.TempDir()
		sd := filepath.Join(d, "001_b2")
		assert.NoError(t, os.MkdirAll(sd, 0o700))
		_, err := writeRestoreConfig(sd, "001_b2", nil)
		assert.NoError(t, err)
		data, err := os.ReadFile(filepath.Join(sd, restoreConfigFileName))
		assert.NoError(t, err)
		parsed := &specRestoreConfig{}
		assert.NoError(t, json.Unmarshal(data, parsed))
		assert.False(t, parsed.RestorePVs)
		assert.Empty(t, parsed.RestoreStatusResources)
	})

	t.Run("config file not in checksum manifest is rejected", func(t *testing.T) {
		_, err := loadRestoreConfig(specDir, dirName, map[string]bool{})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not listed in checksum manifest")
	})

	t.Run("missing config file yields nil config", func(t *testing.T) {
		emptyDir := t.TempDir()
		cfg, err := loadRestoreConfig(emptyDir, "002_b3", map[string]bool{})
		assert.NoError(t, err)
		assert.Nil(t, cfg)
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
