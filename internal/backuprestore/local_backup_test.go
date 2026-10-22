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
	return scheme
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
	// Empty discovery: the default namespaced resource types resolve to nothing,
	// so the test isolates the namespace-existence check without needing to stock
	// the fake with every default type.
	disc := &fakeDiscovery{}

	t.Run("existing included namespace is backed up", func(t *testing.T) {
		dc := dynamicfake.NewSimpleDynamicClient(newNamespaceScheme(), newNamespace("exists-ns"))
		h := &BRHandler{DynamicClient: dc, DiscoveryClient: disc, Log: logr.Discard()}

		resources, err := h.fetchResources(context.Background(), BackupSpec{
			Name:               "backup-ok",
			IncludedNamespaces: []string{"exists-ns"},
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
