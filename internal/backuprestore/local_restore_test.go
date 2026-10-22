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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// fakeDiscovery is a minimal discovery.DiscoveryInterface that answers only
// ServerResourcesForGroupVersion from a static map. Embedding the interface
// leaves the other methods unimplemented (they panic if called, which the
// tests never do).
type fakeDiscovery struct {
	discovery.DiscoveryInterface
	byGV map[string]*metav1.APIResourceList
}

func (f *fakeDiscovery) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	if list, ok := f.byGV[groupVersion]; ok {
		return list, nil
	}
	return nil, k8serrors.NewNotFound(schema.GroupResource{Resource: "APIResourceList"}, groupVersion)
}

// ServerPreferredResources returns every APIResourceList the fake knows about so
// callers that discover preferred resources (e.g. the backup resource fetcher)
// can run against the fake. An empty map yields an empty (but non-error) result.
func (f *fakeDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	lists := make([]*metav1.APIResourceList, 0, len(f.byGV))
	for _, list := range f.byGV {
		lists = append(lists, list)
	}
	return lists, nil
}

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

// TestResolveGVRIrregularPlural verifies that a custom resource whose plural
// does not follow the naive pluralization rules is resolved to the correct GVR
// using live discovery rather than a derived plural.
func TestResolveGVRIrregularPlural(t *testing.T) {
	octopusGVK := schema.GroupVersionKind{Group: "zoo.example.io", Version: "v1", Kind: "Octopus"}
	octopiGVR := schema.GroupVersionResource{Group: "zoo.example.io", Version: "v1", Resource: "octopi"}

	disc := &fakeDiscovery{byGV: map[string]*metav1.APIResourceList{
		"zoo.example.io/v1": {
			GroupVersion: "zoo.example.io/v1",
			APIResources: []metav1.APIResource{
				{Name: "octopi", Namespaced: false, Kind: "Octopus"},
				{Name: "octopi/status", Namespaced: false, Kind: "Octopus"},
			},
		},
	}}
	h := &BRHandler{DiscoveryClient: disc, Log: logr.Discard()}

	gvr, err := h.resolveGVRFromCluster(octopusGVK)
	assert.NoError(t, err)
	assert.Equal(t, octopiGVR, gvr)

	// Sanity check: the naive pluralizer would send the resource to the wrong
	// endpoint, which is exactly what discovery-based resolution avoids.
	assert.NotEqual(t, "octopi", pluralizeKind("Octopus"))
}

// TestApplyResourceIrregularPluralEndpoint verifies that restoring a custom
// resource with an irregular plural creates it at the discovery-reported
// endpoint (octopi), not the naively pluralized one (octopuses).
func TestApplyResourceIrregularPluralEndpoint(t *testing.T) {
	octopiGVR := schema.GroupVersionResource{Group: "zoo.example.io", Version: "v1", Resource: "octopi"}
	octopusesGVR := schema.GroupVersionResource{Group: "zoo.example.io", Version: "v1", Resource: "octopuses"}

	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "zoo.example.io", Version: "v1", Kind: "Octopus"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "zoo.example.io", Version: "v1", Kind: "OctopusList"}, &unstructured.UnstructuredList{})

	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{octopiGVR: "OctopusList"})

	disc := &fakeDiscovery{byGV: map[string]*metav1.APIResourceList{
		"zoo.example.io/v1": {
			GroupVersion: "zoo.example.io/v1",
			APIResources: []metav1.APIResource{{Name: "octopi", Namespaced: false, Kind: "Octopus"}},
		},
	}}

	h := &BRHandler{DynamicClient: dc, DiscoveryClient: disc, Log: logr.Discard()}

	cr := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "zoo.example.io/v1",
		"kind":       "Octopus",
		"metadata":   map[string]interface{}{"name": "inky"},
	}}

	err := h.applyResource(context.Background(), cr)
	assert.NoError(t, err)

	got, err := dc.Resource(octopiGVR).Get(context.Background(), "inky", metav1.GetOptions{})
	assert.NoError(t, err, "custom resource must be created at the octopi endpoint")
	assert.Equal(t, "inky", got.GetName())

	_, err = dc.Resource(octopusesGVR).Get(context.Background(), "inky", metav1.GetOptions{})
	assert.Error(t, err, "custom resource must not be created at the naively pluralized endpoint")
}
