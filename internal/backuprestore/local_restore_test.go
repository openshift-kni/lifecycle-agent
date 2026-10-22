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
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func newConfigMapScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Version: "v1", Kind: "ConfigMapList"}, &unstructured.UnstructuredList{})
	return scheme
}

func newConfigMap(name, namespace, dataValue string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":            name,
			"namespace":       namespace,
			"resourceVersion": "1",
		},
		"data": map[string]interface{}{"key": dataValue},
	}}
}

// TestCreateOrUpdateResourceConflictRetry verifies that an existing-resource
// update which fails with a 409 Conflict is retried after re-fetching the live
// object and re-applying its resourceVersion, and then succeeds.
func TestCreateOrUpdateResourceConflictRetry(t *testing.T) {
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

	t.Run("initial 409 conflict then successful update", func(t *testing.T) {
		existing := newConfigMap("test-cm", "test-ns", "old")
		dc := dynamicfake.NewSimpleDynamicClient(newConfigMapScheme(), existing)

		var updateAttempts int
		dc.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
			updateAttempts++
			if updateAttempts == 1 {
				// Simulate the live object changing between Get and Update.
				return true, nil, k8serrors.NewConflict(
					schema.GroupResource{Resource: "configmaps"}, "test-cm",
					fmt.Errorf("the object has been modified; please apply your changes to the latest version"))
			}
			// Fall through to the default tracker for subsequent attempts.
			return false, nil, nil
		})

		h := &BRHandler{DynamicClient: dc, Log: logr.Discard()}

		toApply := newConfigMap("test-cm", "test-ns", "new")
		unstructured.RemoveNestedField(toApply.Object, "metadata", "resourceVersion")

		err := h.applyNamespacedResource(context.Background(), gvr, "test-ns", "test-cm", toApply)
		assert.NoError(t, err)
		assert.Equal(t, 2, updateAttempts, "expected one conflict retry followed by a successful update")
	})

	t.Run("non-conflict error is returned without retry", func(t *testing.T) {
		existing := newConfigMap("test-cm", "test-ns", "old")
		dc := dynamicfake.NewSimpleDynamicClient(newConfigMapScheme(), existing)

		var updateAttempts int
		dc.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
			updateAttempts++
			return true, nil, k8serrors.NewInternalError(fmt.Errorf("boom"))
		})

		h := &BRHandler{DynamicClient: dc, Log: logr.Discard()}

		toApply := newConfigMap("test-cm", "test-ns", "new")
		unstructured.RemoveNestedField(toApply.Object, "metadata", "resourceVersion")

		err := h.applyNamespacedResource(context.Background(), gvr, "test-ns", "test-cm", toApply)
		assert.Error(t, err)
		assert.Equal(t, 1, updateAttempts, "a non-conflict error must not be retried")
	})
}
