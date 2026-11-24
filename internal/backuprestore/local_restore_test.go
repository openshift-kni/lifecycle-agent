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
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// withFastTransientBackoff shrinks the transient-retry backoff for the duration of a
// test so retry paths do not sleep for seconds, restoring the original on cleanup.
func withFastTransientBackoff(t *testing.T) {
	t.Helper()
	saved := transientRetryBackoff
	transientRetryBackoff = wait.Backoff{Duration: time.Millisecond, Factor: 1.0, Steps: 3}
	t.Cleanup(func() { transientRetryBackoff = saved })
}

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

// TestCreateOrUpdateResourceAlreadyExistsRace verifies that when the initial Get
// returns NotFound but the Create then races another actor and returns
// AlreadyExists, the live object is re-read and the backed-up spec is applied via
// an update, rather than AlreadyExists being treated as success and the spec left
// unapplied.
func TestCreateOrUpdateResourceAlreadyExistsRace(t *testing.T) {
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

	// Seed the object so the re-read after AlreadyExists finds it; the first Get is
	// forced to return NotFound via a reactor to reproduce the race.
	existing := newConfigMap("race-cm", "test-ns", "old")
	dc := dynamicfake.NewSimpleDynamicClient(newConfigMapScheme(), existing)

	var getAttempts, createAttempts, updateAttempts int
	dc.PrependReactor("get", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		getAttempts++
		if getAttempts == 1 {
			// The object does not exist when we first read it.
			return true, nil, k8serrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "race-cm")
		}
		// Subsequent Gets fall through to the tracker (the object now "exists").
		return false, nil, nil
	})
	dc.PrependReactor("create", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		createAttempts++
		// Another actor created the object between our Get and this Create.
		return true, nil, k8serrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, "race-cm")
	})
	dc.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updateAttempts++
		// Fall through to the default tracker so the update is actually applied.
		return false, nil, nil
	})

	h := &BRHandler{DynamicClient: dc, Log: logr.Discard()}

	toApply := newConfigMap("race-cm", "test-ns", "new")
	unstructured.RemoveNestedField(toApply.Object, "metadata", "resourceVersion")

	err := h.applyNamespacedResource(context.Background(), gvr, "test-ns", "race-cm", toApply)
	assert.NoError(t, err)
	assert.Equal(t, 1, createAttempts, "create should be attempted exactly once")
	assert.Equal(t, 2, getAttempts, "expected the initial NotFound Get plus a re-read after AlreadyExists")
	assert.Equal(t, 1, updateAttempts, "the backed-up spec must be applied via update after AlreadyExists")

	got, err := dc.Resource(gvr).Namespace("test-ns").Get(context.Background(), "race-cm", metav1.GetOptions{})
	assert.NoError(t, err)
	val, _, _ := unstructured.NestedString(got.Object, "data", "key")
	assert.Equal(t, "new", val, "the restored spec, not the pre-existing object, must win")
}

// TestStripRestoreMetadata verifies that the cluster-assigned, non-portable
// metadata fields are removed while managedFields and the
// last-applied-configuration annotation are preserved.
func TestStripRestoreMetadata(t *testing.T) {
	resource := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":              "cm1",
			"namespace":         "ns1",
			"generateName":      "cm-",
			"selfLink":          "/api/v1/namespaces/ns1/configmaps/cm1",
			"uid":               "uid-123",
			"resourceVersion":   "42",
			"generation":        int64(7),
			"creationTimestamp": "2020-01-01T00:00:00Z",
			"ownerReferences": []interface{}{
				map[string]interface{}{"kind": "Deployment", "name": "owner"},
			},
			"managedFields": []interface{}{
				map[string]interface{}{"manager": "kubectl"},
			},
			"labels": map[string]interface{}{"app": "demo"},
			"annotations": map[string]interface{}{
				"kubectl.kubernetes.io/last-applied-configuration": "{}",
				"custom.io/keep": "yes",
			},
		},
	}}

	stripRestoreMetadata(resource)

	md, _, _ := unstructured.NestedMap(resource.Object, "metadata")
	for _, removed := range []string{
		"generateName", "selfLink", "uid", "resourceVersion",
		"generation", "creationTimestamp", "ownerReferences",
	} {
		_, present := md[removed]
		assert.Falsef(t, present, "metadata.%s must be removed on restore", removed)
	}

	// Preserved fields.
	_, present := md["managedFields"]
	assert.True(t, present, "metadata.managedFields must be preserved on restore")
	assert.Equal(t, "cm1", resource.GetName())
	annotations := resource.GetAnnotations()
	assert.Equal(t, "{}", annotations["kubectl.kubernetes.io/last-applied-configuration"],
		"last-applied-configuration annotation must be preserved on restore")
	assert.Equal(t, "yes", annotations["custom.io/keep"])
}

// svcPort builds an unstructured Service port entry. A zero nodePort or an empty
// name/protocol is omitted so the fixture matches how such ports are serialized.
func svcPort(name string, port, nodePort int, protocol string) map[string]interface{} {
	p := map[string]interface{}{"port": int64(port)}
	if name != "" {
		p["name"] = name
	}
	if nodePort != 0 {
		p["nodePort"] = int64(nodePort)
	}
	if protocol != "" {
		p["protocol"] = protocol
	}
	return p
}

// lastAppliedConfig renders the kubectl.kubernetes.io/last-applied-configuration
// annotation value for a Service with the given ports.
func lastAppliedConfig(t *testing.T, ports ...map[string]interface{}) string {
	t.Helper()
	raw, err := json.Marshal(map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Service",
		"spec":       map[string]interface{}{"ports": ports},
	})
	assert.NoError(t, err)
	return string(raw)
}

// servicePorts returns the restored spec.ports keyed by port name, falling back to
// "port-<port>" for unnamed ports, so tests can assert per-port nodePort handling.
func servicePorts(t *testing.T, resource *unstructured.Unstructured) map[string]map[string]interface{} {
	t.Helper()
	ports, _, _ := unstructured.NestedSlice(resource.Object, "spec", "ports")
	out := map[string]map[string]interface{}{}
	for _, p := range ports {
		pm, ok := p.(map[string]interface{})
		assert.True(t, ok)
		key, _ := pm["name"].(string)
		if key == "" {
			key = fmt.Sprintf("port-%v", pm["port"])
		}
		out[key] = pm
	}
	return out
}

// TestPrepareServiceForRestoreNodePorts verifies that explicitly assigned nodePorts
// are preserved on restore (detected from last-applied-configuration or managedFields)
// while auto-assigned nodePorts are cleared for reallocation.
func TestPrepareServiceForRestoreNodePorts(t *testing.T) {
	t.Run("explicit named nodePort in last-applied-configuration is preserved", func(t *testing.T) {
		resource := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":      "svc",
				"namespace": "ns",
				"annotations": map[string]interface{}{
					"kubectl.kubernetes.io/last-applied-configuration": lastAppliedConfig(t,
						svcPort("https", 443, 30443, "TCP")),
				},
			},
			"spec": map[string]interface{}{
				"clusterIP": "10.0.0.1",
				"type":      "NodePort",
				"ports": []interface{}{
					svcPort("https", 443, 30443, "TCP"),
				},
			},
		}}

		prepareServiceForRestore(resource)

		ports := servicePorts(t, resource)
		assert.EqualValues(t, int64(30443), ports["https"]["nodePort"],
			"explicit nodePort from last-applied-configuration must be preserved")
	})

	t.Run("explicit named nodePort recorded only in managedFields is preserved", func(t *testing.T) {
		resource := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":      "svc",
				"namespace": "ns",
				"managedFields": []interface{}{
					map[string]interface{}{
						"manager": "kubectl",
						"fieldsV1": map[string]interface{}{
							"f:spec": map[string]interface{}{
								"f:ports": map[string]interface{}{
									".": map[string]interface{}{},
									`k:{"port":443,"protocol":"TCP"}`: map[string]interface{}{
										".":          map[string]interface{}{},
										"f:name":     map[string]interface{}{},
										"f:nodePort": map[string]interface{}{},
										"f:port":     map[string]interface{}{},
										"f:protocol": map[string]interface{}{},
									},
								},
							},
						},
					},
				},
			},
			"spec": map[string]interface{}{
				"clusterIP": "10.0.0.1",
				"type":      "NodePort",
				"ports": []interface{}{
					svcPort("https", 443, 30443, "TCP"),
				},
			},
		}}

		prepareServiceForRestore(resource)

		ports := servicePorts(t, resource)
		assert.EqualValues(t, int64(30443), ports["https"]["nodePort"],
			"explicit nodePort from managedFields must be preserved")
	})

	t.Run("nodePort in neither source is removed", func(t *testing.T) {
		resource := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata":   map[string]interface{}{"name": "svc", "namespace": "ns"},
			"spec": map[string]interface{}{
				"clusterIP": "10.0.0.1",
				"type":      "NodePort",
				"ports": []interface{}{
					svcPort("https", 443, 30443, "TCP"),
				},
			},
		}}

		prepareServiceForRestore(resource)

		ports := servicePorts(t, resource)
		_, present := ports["https"]["nodePort"]
		assert.False(t, present, "auto-assigned nodePort must be removed for reallocation")
	})

	t.Run("unnamed explicit port preserved by value, unnamed auto-assigned removed", func(t *testing.T) {
		resource := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":      "svc",
				"namespace": "ns",
				"annotations": map[string]interface{}{
					"kubectl.kubernetes.io/last-applied-configuration": lastAppliedConfig(t,
						svcPort("", 80, 30080, "TCP")),
				},
			},
			"spec": map[string]interface{}{
				"clusterIP": "10.0.0.1",
				"type":      "NodePort",
				"ports": []interface{}{
					svcPort("", 80, 30080, "TCP"),
					svcPort("", 81, 31081, "TCP"),
				},
			},
		}}

		prepareServiceForRestore(resource)

		ports := servicePorts(t, resource)
		assert.EqualValues(t, int64(30080), ports["port-80"]["nodePort"],
			"unnamed explicit nodePort must be preserved by value")
		_, present := ports["port-81"]["nodePort"]
		assert.False(t, present, "unnamed auto-assigned nodePort must be removed")
	})

	t.Run("unnamed port explicit only in managedFields is preserved", func(t *testing.T) {
		resource := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":      "svc",
				"namespace": "ns",
				"managedFields": []interface{}{
					map[string]interface{}{
						"manager": "kubectl",
						"fieldsV1": map[string]interface{}{
							"f:spec": map[string]interface{}{
								"f:ports": map[string]interface{}{
									".": map[string]interface{}{},
									`k:{"port":80,"protocol":"TCP"}`: map[string]interface{}{
										".":          map[string]interface{}{},
										"f:nodePort": map[string]interface{}{},
										"f:port":     map[string]interface{}{},
										"f:protocol": map[string]interface{}{},
									},
								},
							},
						},
					},
				},
			},
			"spec": map[string]interface{}{
				"clusterIP": "10.0.0.1",
				"type":      "NodePort",
				"ports": []interface{}{
					svcPort("", 80, 30080, "TCP"),
				},
			},
		}}

		prepareServiceForRestore(resource)

		ports := servicePorts(t, resource)
		assert.EqualValues(t, int64(30080), ports["port-80"]["nodePort"],
			"unnamed explicit nodePort from managedFields must be preserved")
	})

	t.Run("managedFields port without f:nodePort is removed", func(t *testing.T) {
		resource := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":      "svc",
				"namespace": "ns",
				"managedFields": []interface{}{
					map[string]interface{}{
						"manager": "kubectl",
						"fieldsV1": map[string]interface{}{
							"f:spec": map[string]interface{}{
								"f:ports": map[string]interface{}{
									".": map[string]interface{}{},
									// Managed but without f:nodePort: the port was declared
									// by the user but its nodePort was auto-assigned.
									`k:{"port":443,"protocol":"TCP"}`: map[string]interface{}{
										".":      map[string]interface{}{},
										"f:name": map[string]interface{}{},
										"f:port": map[string]interface{}{},
									},
								},
							},
						},
					},
				},
			},
			"spec": map[string]interface{}{
				"clusterIP": "10.0.0.1",
				"type":      "NodePort",
				"ports": []interface{}{
					svcPort("https", 443, 30443, "TCP"),
				},
			},
		}}

		prepareServiceForRestore(resource)

		ports := servicePorts(t, resource)
		_, present := ports["https"]["nodePort"]
		assert.False(t, present, "nodePort not tracked in managedFields must be removed")
	})

	t.Run("duplicate port number distinguished by protocol", func(t *testing.T) {
		resource := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":      "svc",
				"namespace": "ns",
				"managedFields": []interface{}{
					map[string]interface{}{
						"manager": "kubectl",
						"fieldsV1": map[string]interface{}{
							"f:spec": map[string]interface{}{
								"f:ports": map[string]interface{}{
									".": map[string]interface{}{},
									// Only the TCP port's nodePort is explicitly managed.
									`k:{"port":53,"protocol":"TCP"}`: map[string]interface{}{
										".":          map[string]interface{}{},
										"f:name":     map[string]interface{}{},
										"f:nodePort": map[string]interface{}{},
										"f:port":     map[string]interface{}{},
										"f:protocol": map[string]interface{}{},
									},
								},
							},
						},
					},
				},
			},
			"spec": map[string]interface{}{
				"clusterIP": "10.0.0.1",
				"type":      "NodePort",
				"ports": []interface{}{
					svcPort("dns-tcp", 53, 30053, "TCP"),
					svcPort("dns-udp", 53, 31053, "UDP"),
				},
			},
		}}

		prepareServiceForRestore(resource)

		ports := servicePorts(t, resource)
		assert.EqualValues(t, int64(30053), ports["dns-tcp"]["nodePort"],
			"the TCP port with a managed nodePort must be preserved")
		_, present := ports["dns-udp"]["nodePort"]
		assert.False(t, present, "the UDP port sharing the port number must be reallocated")
	})

	t.Run("last-applied-configuration takes precedence over managedFields", func(t *testing.T) {
		// Annotation present but does not declare a nodePort for the port, while
		// managedFields does. Velero consults managedFields only when the annotation
		// is absent, so the nodePort must still be removed.
		resource := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":      "svc",
				"namespace": "ns",
				"annotations": map[string]interface{}{
					"kubectl.kubernetes.io/last-applied-configuration": lastAppliedConfig(t,
						svcPort("https", 443, 0, "TCP")),
				},
				"managedFields": []interface{}{
					map[string]interface{}{
						"manager": "kubectl",
						"fieldsV1": map[string]interface{}{
							"f:spec": map[string]interface{}{
								"f:ports": map[string]interface{}{
									`k:{"port":443,"protocol":"TCP"}`: map[string]interface{}{
										"f:nodePort": map[string]interface{}{},
									},
								},
							},
						},
					},
				},
			},
			"spec": map[string]interface{}{
				"clusterIP": "10.0.0.1",
				"type":      "NodePort",
				"ports": []interface{}{
					svcPort("https", 443, 30443, "TCP"),
				},
			},
		}}

		prepareServiceForRestore(resource)

		ports := servicePorts(t, resource)
		_, present := ports["https"]["nodePort"]
		assert.False(t, present,
			"managedFields must be ignored when the annotation is present")
	})

	t.Run("malformed metadata degrades to strip-all without error", func(t *testing.T) {
		// Invalid annotation JSON.
		badAnnotation := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":      "svc",
				"namespace": "ns",
				"annotations": map[string]interface{}{
					"kubectl.kubernetes.io/last-applied-configuration": "{not-json",
				},
			},
			"spec": map[string]interface{}{
				"clusterIP": "10.0.0.1",
				"type":      "NodePort",
				"ports":     []interface{}{svcPort("https", 443, 30443, "TCP")},
			},
		}}

		assert.NotPanics(t, func() { prepareServiceForRestore(badAnnotation) })
		ports := servicePorts(t, badAnnotation)
		_, present := ports["https"]["nodePort"]
		assert.False(t, present, "malformed annotation must fall back to removing nodePorts")

		// Malformed managedFields key.
		badManagedFields := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":      "svc",
				"namespace": "ns",
				"managedFields": []interface{}{
					map[string]interface{}{
						"manager": "kubectl",
						"fieldsV1": map[string]interface{}{
							"f:spec": map[string]interface{}{
								"f:ports": map[string]interface{}{
									`k:{bad-json`: map[string]interface{}{
										"f:nodePort": map[string]interface{}{},
									},
								},
							},
						},
					},
				},
			},
			"spec": map[string]interface{}{
				"clusterIP": "10.0.0.1",
				"type":      "NodePort",
				"ports":     []interface{}{svcPort("https", 443, 30443, "TCP")},
			},
		}}

		assert.NotPanics(t, func() { prepareServiceForRestore(badManagedFields) })
		ports = servicePorts(t, badManagedFields)
		_, present = ports["https"]["nodePort"]
		assert.False(t, present, "malformed managedFields key must be skipped, not preserved")
	})

	t.Run("mixed service keeps only the explicit port", func(t *testing.T) {
		resource := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":      "svc",
				"namespace": "ns",
				"annotations": map[string]interface{}{
					"kubectl.kubernetes.io/last-applied-configuration": lastAppliedConfig(t,
						svcPort("https", 443, 30443, "TCP")),
				},
			},
			"spec": map[string]interface{}{
				"clusterIP": "10.0.0.1",
				"type":      "NodePort",
				"ports": []interface{}{
					svcPort("https", 443, 30443, "TCP"),
					svcPort("metrics", 9090, 32090, "TCP"),
				},
			},
		}}

		prepareServiceForRestore(resource)

		ports := servicePorts(t, resource)
		assert.EqualValues(t, int64(30443), ports["https"]["nodePort"],
			"explicit nodePort must be preserved")
		_, present := ports["metrics"]["nodePort"]
		assert.False(t, present, "auto-assigned nodePort on the same Service must be removed")
	})
}

// TestPrepareServiceForRestoreUnchangedFields verifies that the nodePort change does
// not regress the existing clusterIP, headless, healthCheckNodePort, and
// LoadBalancer externalIPs handling.
func TestPrepareServiceForRestoreUnchangedFields(t *testing.T) {
	resource := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   map[string]interface{}{"name": "svc", "namespace": "ns"},
		"spec": map[string]interface{}{
			"clusterIP":           "10.0.0.1",
			"clusterIPs":          []interface{}{"10.0.0.1"},
			"healthCheckNodePort": int64(32000),
			"externalIPs":         []interface{}{"1.2.3.4"},
			"type":                "LoadBalancer",
			"ports":               []interface{}{svcPort("https", 443, 30443, "TCP")},
		},
	}}

	prepareServiceForRestore(resource)

	_, present, _ := unstructured.NestedString(resource.Object, "spec", "clusterIP")
	assert.False(t, present, "non-headless clusterIP must be cleared on restore")
	_, present, _ = unstructured.NestedSlice(resource.Object, "spec", "clusterIPs")
	assert.False(t, present, "clusterIPs must be cleared on restore")
	_, present, _ = unstructured.NestedInt64(resource.Object, "spec", "healthCheckNodePort")
	assert.False(t, present, "healthCheckNodePort must still be removed on restore")
	_, present, _ = unstructured.NestedSlice(resource.Object, "spec", "externalIPs")
	assert.False(t, present, "LoadBalancer externalIPs must be removed on restore")

	// Headless services keep their clusterIP/clusterIPs.
	headless := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   map[string]interface{}{"name": "svc", "namespace": "ns"},
		"spec": map[string]interface{}{
			"clusterIP":  "None",
			"clusterIPs": []interface{}{"None"},
			"type":       "ClusterIP",
		},
	}}

	prepareServiceForRestore(headless)

	clusterIP, present, _ := unstructured.NestedString(headless.Object, "spec", "clusterIP")
	assert.True(t, present)
	assert.Equal(t, "None", clusterIP, "headless clusterIP must be preserved")
}

// TestRestoreResourceEntryStatusGating verifies that status is restored only when
// the paired Restore CR configured the resource's kind, and never otherwise.
func TestRestoreResourceEntryStatusGating(t *testing.T) {
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

	newCMWithStatus := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "cm1", "namespace": "ns1"},
			"data":       map[string]interface{}{"k": "v"},
			"status":     map[string]interface{}{"phase": "Ready"},
		}}
	}

	disc := &fakeDiscovery{byGV: map[string]*metav1.APIResourceList{
		"v1": {GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "configmaps", SingularName: "configmap", Namespaced: true, Kind: "ConfigMap"},
		}},
	}}

	t.Run("status restored when configured", func(t *testing.T) {
		dc := dynamicfake.NewSimpleDynamicClient(newConfigMapScheme())
		h := &BRHandler{DynamicClient: dc, DiscoveryClient: disc, Log: logr.Discard()}

		rt := &RestoreTracker{}
		err := h.restoreResourceEntry(context.Background(), resourceEntry{
			resource:      newCMWithStatus(),
			fileName:      "cm1.yaml",
			restoreConfig: &specRestoreConfig{RestoreStatusResources: []string{"configmaps"}},
		}, rt)
		assert.NoError(t, err)

		got, err := dc.Resource(gvr).Namespace("ns1").Get(context.Background(), "cm1", metav1.GetOptions{})
		assert.NoError(t, err)
		phase, _, _ := unstructured.NestedString(got.Object, "status", "phase")
		assert.Equal(t, "Ready", phase, "status must be restored for a configured kind")
	})

	t.Run("status not restored when not configured", func(t *testing.T) {
		dc := dynamicfake.NewSimpleDynamicClient(newConfigMapScheme())
		h := &BRHandler{DynamicClient: dc, DiscoveryClient: disc, Log: logr.Discard()}

		rt := &RestoreTracker{}
		err := h.restoreResourceEntry(context.Background(), resourceEntry{
			resource:      newCMWithStatus(),
			fileName:      "cm1.yaml",
			restoreConfig: nil,
		}, rt)
		assert.NoError(t, err)

		got, err := dc.Resource(gvr).Namespace("ns1").Get(context.Background(), "cm1", metav1.GetOptions{})
		assert.NoError(t, err)
		_, found, _ := unstructured.NestedMap(got.Object, "status")
		assert.False(t, found, "status must not be restored when no kind is configured")
	})
}

// TestRestoreResourceEntryStatusRestorationRetries verifies that status restoration
// is mandatory and resilient: a transient status-update error is retried with
// backoff and then succeeds, a 409 Conflict is retried after re-reading the live
// object, and a status update that keeps failing after the bounded retries fails the
// restore terminally and records the resource as a failed restore (rather than being
// logged and ignored).
func TestRestoreResourceEntryStatusRestorationRetries(t *testing.T) {
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

	newCMWithStatus := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "cm1", "namespace": "ns1"},
			"data":       map[string]interface{}{"k": "v"},
			"status":     map[string]interface{}{"phase": "Ready"},
		}}
	}

	disc := &fakeDiscovery{byGV: map[string]*metav1.APIResourceList{
		"v1": {GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "configmaps", SingularName: "configmap", Namespaced: true, Kind: "ConfigMap"},
		}},
	}}

	statusCfg := &specRestoreConfig{RestoreStatusResources: []string{"configmaps"}}

	assertStatusRestored := func(t *testing.T, dc *dynamicfake.FakeDynamicClient) {
		t.Helper()
		got, err := dc.Resource(gvr).Namespace("ns1").Get(context.Background(), "cm1", metav1.GetOptions{})
		assert.NoError(t, err)
		phase, _, _ := unstructured.NestedString(got.Object, "status", "phase")
		assert.Equal(t, "Ready", phase, "the saved status must be restored")
	}

	t.Run("transient status update error is retried then succeeds", func(t *testing.T) {
		withFastTransientBackoff(t)
		dc := dynamicfake.NewSimpleDynamicClient(newConfigMapScheme())

		var statusUpdates int
		dc.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
			if action.GetSubresource() != "status" {
				return false, nil, nil
			}
			statusUpdates++
			if statusUpdates == 1 {
				return true, nil, k8serrors.NewServiceUnavailable("kube-apiserver is temporarily unavailable")
			}
			return false, nil, nil
		})

		h := &BRHandler{DynamicClient: dc, DiscoveryClient: disc, Log: logr.Discard()}
		rt := &RestoreTracker{}
		err := h.restoreResourceEntry(context.Background(), resourceEntry{
			resource: newCMWithStatus(), fileName: "cm1.yaml", restoreConfig: statusCfg,
		}, rt)
		assert.NoError(t, err)
		assert.Equal(t, 2, statusUpdates, "a transient status update must be retried once then succeed")
		assert.Empty(t, rt.FailedRestores)
		assert.Contains(t, rt.SucceededRestores, "ConfigMap/ns1/cm1")
		assertStatusRestored(t, dc)
	})

	t.Run("status update conflict is retried then succeeds", func(t *testing.T) {
		dc := dynamicfake.NewSimpleDynamicClient(newConfigMapScheme())

		var statusUpdates int
		dc.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
			if action.GetSubresource() != "status" {
				return false, nil, nil
			}
			statusUpdates++
			if statusUpdates == 1 {
				return true, nil, k8serrors.NewConflict(
					schema.GroupResource{Resource: "configmaps"}, "cm1",
					fmt.Errorf("the object has been modified; please apply your changes to the latest version"))
			}
			return false, nil, nil
		})

		h := &BRHandler{DynamicClient: dc, DiscoveryClient: disc, Log: logr.Discard()}
		rt := &RestoreTracker{}
		err := h.restoreResourceEntry(context.Background(), resourceEntry{
			resource: newCMWithStatus(), fileName: "cm1.yaml", restoreConfig: statusCfg,
		}, rt)
		assert.NoError(t, err)
		assert.Equal(t, 2, statusUpdates, "a status update conflict must be retried after re-reading the live object")
		assert.Empty(t, rt.FailedRestores)
		assertStatusRestored(t, dc)
	})

	t.Run("status restoration failing after retries fails the restore terminally", func(t *testing.T) {
		withFastTransientBackoff(t)
		dc := dynamicfake.NewSimpleDynamicClient(newConfigMapScheme())

		var statusUpdates int
		dc.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
			if action.GetSubresource() != "status" {
				return false, nil, nil
			}
			statusUpdates++
			return true, nil, k8serrors.NewServiceUnavailable("kube-apiserver is unavailable")
		})

		h := &BRHandler{DynamicClient: dc, DiscoveryClient: disc, Log: logr.Discard()}
		rt := &RestoreTracker{}
		err := h.restoreResourceEntry(context.Background(), resourceEntry{
			resource: newCMWithStatus(), fileName: "cm1.yaml", restoreConfig: statusCfg,
		}, rt)
		assert.Error(t, err)
		assert.True(t, IsBRFailedError(err), "exhausted status retries must yield a terminal restore error")
		assert.Greater(t, statusUpdates, 1, "the status update must be retried before the restore fails")
		assert.Contains(t, rt.FailedRestores, "ConfigMap/ns1/cm1",
			"a resource whose status cannot be restored must be recorded as a failed restore")
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
