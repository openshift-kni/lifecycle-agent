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

package clusterconfig

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/stretchr/testify/assert"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	lv1 = &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "local.storage.openshift.io/v1",
			"kind":       "LocalVolume",
			"metadata": map[string]interface{}{
				"name":      "lv1",
				"namespace": "openshift-local-storage",
			},
			"spec": map[string]interface{}{
				"storageClassName": "local-sc1",
			},
		},
	}

	lv2 = &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "local.storage.openshift.io/v1",
			"kind":       "LocalVolume",
			"metadata": map[string]interface{}{
				"name":      "lv2",
				"namespace": "openshift-local-storage",
			},
			"spec": map[string]interface{}{
				"storageClassName": "local-sc2",
			},
		},
	}

	lvCRD = &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "localvolumes.local.storage.openshift.io",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:     "LocalVolume",
				ListKind: "LocalVolumeList",
			},
		},
	}
)

func init() {
	testscheme.AddKnownTypes(apiextensionsv1.SchemeGroupVersion, &apiextensionsv1.CustomResourceDefinition{})
}

func TestFetchLvmConfig(t *testing.T) {
	testcases := []struct {
		name           string
		objs           []client.Object
		lvmFilesToCopy []string
		validateFunc   func(t *testing.T, lvmConfigDir, manifestsDir string)
	}{
		{
			name:           "success path",
			objs:           []client.Object{lvCRD, lv1, lv2},
			lvmFilesToCopy: []string{common.LvmDevicesPath},
			validateFunc: func(t *testing.T, lvmConfigDir, manifestsDir string) {
				// validate lvm devices file
				lvmDevicesPath := filepath.Join(lvmConfigDir, filepath.Base(common.LvmDevicesPath))
				_, err := os.Stat(lvmDevicesPath)
				assert.NoError(t, err)

				// validate lvs
				manifests, err := os.ReadDir(manifestsDir)
				if err != nil {
					t.Errorf("Unexpected err: %v", err)
				}
				assert.Equal(t, 2, len(manifests))
			},
		},
		{
			name:           "lvm devices file does not exist",
			objs:           []client.Object{lvCRD, lv1},
			lvmFilesToCopy: []string{},
			validateFunc: func(t *testing.T, lvmConfigDir, manifestsDir string) {
				manifests, err := os.ReadDir(manifestsDir)
				if err != nil {
					t.Errorf("Unexpected err: %v", err)
				}
				assert.Equal(t, 1, len(manifests))
			},
		},
		{
			name:           "no localvolumes found",
			objs:           []client.Object{lvCRD},
			lvmFilesToCopy: []string{},
			validateFunc: func(t *testing.T, lvmConfigDir, manifestsDir string) {
				manifests, err := os.ReadDir(manifestsDir)
				if err != nil {
					t.Errorf("Unexpected err: %v", err)
				}
				assert.Equal(t, 0, len(manifests))
			},
		},
		{
			name:           "localvolume CRD doesn't exist",
			objs:           []client.Object{},
			lvmFilesToCopy: []string{},
			validateFunc: func(t *testing.T, lvmConfigDir, manifestsDir string) {
				manifests, err := os.ReadDir(manifestsDir)
				if err != nil {
					t.Errorf("Unexpected err: %v", err)
				}
				assert.Equal(t, 0, len(manifests))
			},
		},
	}

	for _, tc := range testcases {
		tmpDir := t.TempDir()
		lvmConfigDir := filepath.Join(tmpDir, common.OptOpenshift, common.LvmConfigDir)
		manifestsDir := filepath.Join(tmpDir, common.OptOpenshift, common.ClusterConfigDir, manifestDir)

		t.Run(tc.name, func(t *testing.T) {
			// Create the original files
			hostPath = filepath.Join(tmpDir, "host")
			for _, file := range tc.lvmFilesToCopy {
				lvmFilePath := filepath.Join(hostPath, file)
				if err := os.MkdirAll(filepath.Dir(lvmFilePath), 0o700); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				file, err := os.Create(lvmFilePath)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				file.Close()
			}

			if err := os.MkdirAll(manifestsDir, 0o700); err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			c := fake.NewClientBuilder().WithScheme(testscheme).WithObjects(tc.objs...).Build()
			ucc := &UpgradeClusterConfigGather{
				Client: c,
				Log:    logr.Discard(),
				Scheme: c.Scheme(),
			}

			err := ucc.FetchLvmConfig(context.Background(), tmpDir)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			tc.validateFunc(t, lvmConfigDir, manifestsDir)
		})
	}
}
