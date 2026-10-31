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
