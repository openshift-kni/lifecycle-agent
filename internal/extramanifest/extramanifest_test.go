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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

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
