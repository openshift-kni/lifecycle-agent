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

package extramanifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	policiesv1 "open-cluster-management.io/governance-policy-propagator/api/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	testscheme = scheme.Scheme
)

func init() {
	testscheme.AddKnownTypes(policiesv1.GroupVersion, &policiesv1.Policy{})
	testscheme.AddKnownTypes(policiesv1.GroupVersion, &policiesv1.PolicyList{})
}

const sriovnodepolicies = `
apiVersion: sriovnetwork.openshift.io/v1
kind: SriovNetworkNodePolicy
metadata:
  name: sriov-nnp-mh
  namespace: openshift-sriov-network-operator
  spec:
    deviceType: netdevice
    isRdma: false
---
apiVersion: sriovnetwork.openshift.io/v1
kind: SriovNetworkNodePolicy
metadata:
  name: sriov-nnp-fh
  namespace: openshift-sriov-network-operator
  spec:
    deviceType: netdevice
    isRdma: true
`

const sriovnetwork1 = `
apiVersion: sriovnetwork.openshift.io/v1
kind: SriovNetwork
metadata:
  name: sriov-nw-mh
  namespace: openshift-sriov-network-operator
  spec:
    resourceName: mh
`

const sriovnetwork2 = `
apiVersion: sriovnetwork.openshift.io/v1
kind: SriovNetwork
metadata:
  name: sriov-nw-fh
  namespace: openshift-sriov-network-operator
  spec:
    resourceName: fh
`

func TestExportExtraManifests(t *testing.T) {
	fakeClient := fake.NewClientBuilder().Build()

	// Create a temporary directory for testing
	toDir, err := os.MkdirTemp("", "staterootB")
	if err != nil {
		t.Errorf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(toDir)

	// Create two configmaps
	cms := []*corev1.ConfigMap{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "extra-manifest-cm1",
				Namespace: "default",
			},
			Data: map[string]string{
				"sriovnetwork1.yaml": sriovnetwork1,
				"sriovnetwork2.yaml": sriovnetwork2,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "extra-manifest-cm2",
				Namespace: "default",
			},
			Data: map[string]string{
				"sriovnodepolicies.yaml": sriovnodepolicies,
			},
		},
	}

	for _, cm := range cms {
		err = fakeClient.Create(context.Background(), cm)
		if err != nil {
			t.Errorf("Unexpected error: %v", err)
		}
	}

	handler := &EMHandler{
		Client: fakeClient,
		Log:    ctrl.Log.WithName("ExtraManifest"),
	}

	// Export the manifests to the temporary directory
	err = handler.ExportExtraManifestToDir(context.Background(),
		[]lcav1alpha1.ConfigMapRef{
			{Name: "extra-manifest-cm1", Namespace: "default"},
			{Name: "extra-manifest-cm2", Namespace: "default"},
		}, toDir)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// Check that the manifests were exported to the correct files
	expectedFilePaths := []string{
		filepath.Join(toDir, ExtraManifestPath, "0_sriov-nw-mh_openshift-sriov-network-operator.yaml"),
		filepath.Join(toDir, ExtraManifestPath, "0_sriov-nw-fh_openshift-sriov-network-operator.yaml"),
		filepath.Join(toDir, ExtraManifestPath, "1_sriov-nnp-mh_openshift-sriov-network-operator.yaml"),
		filepath.Join(toDir, ExtraManifestPath, "1_sriov-nnp-fh_openshift-sriov-network-operator.yaml"),
	}

	for _, expectedFile := range expectedFilePaths {
		if _, err := os.Stat(expectedFile); os.IsNotExist(err) {
			t.Fatalf("Expected file %s does not exist", expectedFile)
		}
	}
}

const policyWithLabelAndAnnotation = `---
kind: Policy
apiVersion: policy.open-cluster-management.io/v1
metadata:
  name: ztp-common.p1
  namespace: spoke
  labels:
    lca.openshift.io/for-ocp-version: "4.16.1"
  annotations:
    ran.openshift.io/ztp-deploy-wave: "1"
spec:
  disabled: false
  policy-templates:
    - objectDefinition:
        apiVersion: policy.open-cluster-management.io/v1
        kind: ConfigurationPolicy
        metadata:
          name: common-cnfdf22-new-config-policy-config
        spec:
          evaluationInterval:
            compliant: 10m
            noncompliant: 10s
          namespaceselector:
            exclude:
              - kube-*
            include:
              - "*"
          object-templates:
            - complianceType: musthave
              objectDefinition:
                apiVersion: operators.coreos.com/v1alpha1
                kind: CatalogSource
                metadata:
                  name: redhat-operators-new
                  namespace: openshift-marketplace
                  annotations:
                    target.workload.openshift.io/management: '{"effect": "PreferredDuringScheduling"}'
                spec:
                  displayName: Red Hat Operators Catalog
                  image: registry.redhat.io/redhat/redhat-operator-index:v4.16
                  publisher: Red Hat
                  sourceType: grpc
                  updateStrategy:
                    registryPoll:
                      interval: 1h
                status:
                  connectionState:
                    lastObservedState: READY
          remediationAction: inform
          severity: low
  remediationAction: inform
`

const policyWithOutLabel = `---
kind: Policy
apiVersion: policy.open-cluster-management.io/v1
metadata:
  name: ztp-group.p2
  namespace: spoke
spec:
  disabled: false
  policy-templates:
    - objectDefinition:
        apiVersion: policy.open-cluster-management.io/v1
        kind: ConfigurationPolicy
        metadata:
          name: common-cnfdf22-new-config-policy-config
        spec:
          evaluationInterval:
            compliant: 10m
            noncompliant: 10s
          namespaceselector:
            exclude:
              - kube-*
            include:
              - "*"
          object-templates:
            - complianceType: musthave
              objectDefinition:
                apiVersion: operators.coreos.com/v1alpha1
                kind: CatalogSource
                metadata:
                  name: redhat-operators
                  namespace: openshift-marketplace
                  annotations:
                    target.workload.openshift.io/management: '{"effect": "PreferredDuringScheduling"}'
                spec:
                  displayName: Red Hat Operators Catalog
                  image: registry.redhat.io/redhat/redhat-operator-index:v4.14
                  publisher: Red Hat
                  sourceType: grpc
                  updateStrategy:
                    registryPoll:
                      interval: 1h
                status:
                  connectionState:
                    lastObservedState: READY
          remediationAction: inform
          severity: low
  remediationAction: inform
`

func TestExportPolicyManifests(t *testing.T) {
	fakeClient := fake.NewClientBuilder().Build()

	// Create a temporary directory for testing
	toDir, err := os.MkdirTemp("", "staterootB")
	if err != nil {
		t.Errorf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(toDir)

	labels := map[string]string{"lca.openshift.io/for-ocp-version": "4.16.1"}
	// Create policies
	policies := []*unstructured.Unstructured{mustConvertYamlStrToUnstructured(policyWithLabelAndAnnotation), mustConvertYamlStrToUnstructured(policyWithOutLabel)}

	for _, p := range policies {
		err = fakeClient.Create(context.Background(), p)
		if err != nil {
			t.Errorf("Unexpected error: %v", err)
		}
	}

	handler := &EMHandler{
		Client: fakeClient,
		Log:    ctrl.Log.WithName("ExtraManifest"),
	}

	// Export the manifests to the temporary directory
	err = handler.ExtractAndExportManifestFromPoliciesToDir(context.Background(),
		labels, toDir)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// Check that the manifests were exported to the correct files
	expectedFilePaths := []string{
		filepath.Join(toDir, PolicyManifestPath, "0_redhat-operators-new_openshift-marketplace.yaml"),
	}

	unexpectedFilePaths := []string{
		filepath.Join(toDir, PolicyManifestPath, "1_redhat-operators_openshift-marketplace.yaml"),
	}

	expectedObjects := []unstructured.Unstructured{
		{
			Object: map[string]interface{}{
				"apiVersion": "operators.coreos.com/v1alpha1",
				"kind":       "CatalogSource",
				"metadata": map[string]interface{}{
					"annotations": map[string]interface{}{
						"target.workload.openshift.io/management": "{\"effect\": \"PreferredDuringScheduling\"}",
					},
					"name":      "redhat-operators-new",
					"namespace": "openshift-marketplace",
				},
				"spec": map[string]interface{}{
					"displayName": "Red Hat Operators Catalog",
					"image":       "registry.redhat.io/redhat/redhat-operator-index:v4.16",
					"publisher":   "Red Hat",
					"sourceType":  "grpc",
					"updateStrategy": map[string]interface{}{
						"registryPoll": map[string]interface{}{
							"interval": "1h",
						},
					},
				},
				// status should be empty
				"status": map[string]interface{}{},
			},
		},
	}

	for i, expectedFile := range expectedFilePaths {
		if _, err := os.Stat(expectedFile); os.IsNotExist(err) {
			t.Fatalf("Expected file %s does not exist", expectedFile)
		}
		object := &unstructured.Unstructured{}
		err := utils.ReadYamlOrJSONFile(expectedFile, object)
		if err != nil {
			t.Fatalf("Failed to read expected file %s, err: %v", expectedFile, err)
		}
		if !equality.Semantic.DeepEqual(object, &expectedObjects[i]) {
			t.Fatalf("exported CR: \n%v does not match expected: \n%v", object, expectedObjects[i])
		}

	}
	for _, unexpectedFile := range unexpectedFilePaths {
		if _, err := os.Stat(unexpectedFile); !os.IsNotExist(err) {
			t.Fatalf("Unexpected file %s should not exist", unexpectedFile)
		}
	}
}

// convertYamlStrToUnstructured helper func to convert a CR in Yaml string to Unstructured
func mustConvertYamlStrToUnstructured(cr string) *unstructured.Unstructured {
	jCr, err := yaml.ToJSON([]byte(cr))
	if err != nil {
		panic(err.Error())
	}

	object, err := runtime.Decode(unstructured.UnstructuredJSONScheme, jCr)
	if err != nil {
		panic(err.Error())
	}

	uCr, ok := object.(*unstructured.Unstructured)
	if !ok {
		panic("unstructured.Unstructured expected")
	}
	return uCr
}
