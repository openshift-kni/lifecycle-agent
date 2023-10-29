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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cro "github.com/RHsyseng/cluster-relocation-operator/api/v1beta1"
	"github.com/go-logr/logr"
	ocpV1 "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const numberOfFilesOnSuccess = 4

var (
	testscheme = scheme.Scheme
)

func init() {
	testscheme.AddKnownTypes(ocpV1.GroupVersion, &ocpV1.ClusterVersion{}, &ocpV1.Ingress{},
		&ocpV1.ImageDigestMirrorSet{})
	testscheme.AddKnownTypes(cro.GroupVersion, &cro.ClusterRelocation{})
}

func getFakeClientFromObjects(objs ...client.Object) (client.WithWatch, error) {
	c := fake.NewClientBuilder().WithScheme(testscheme).WithObjects(objs...).WithStatusSubresource(objs...).Build()
	return c, nil
}

func TestClusterConfig(t *testing.T) {
	testcases := []struct {
		name           string
		secret         client.Object
		clusterVersion client.Object
		ingress        client.Object
		idm            client.Object
		expectedErr    bool
		validateFunc   func(t *testing.T, tempDir string, err error)
	}{
		{
			name: "Validate success flow",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pullSecretName,
					Namespace: configNamespace,
				},
				Data: map[string][]byte{"aaa": []byte("bbb")},
			},
			clusterVersion: &ocpV1.ClusterVersion{
				ObjectMeta: metav1.ObjectMeta{
					Name: "version",
				},
				Spec: ocpV1.ClusterVersionSpec{
					ClusterID: "1",
				},
			},
			ingress: &ocpV1.Ingress{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Spec: ocpV1.IngressSpec{Domain: "test.com"},
			},
			idm: &ocpV1.ImageDigestMirrorSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: imageSetName,
				},
				Spec: ocpV1.ImageDigestMirrorSetSpec{ImageDigestMirrors: []ocpV1.ImageDigestMirrors{{Source: imageSetName}}},
			},
			expectedErr: false,
			validateFunc: func(t *testing.T, tempDir string, err error) {
				filesDir := filepath.Join(tempDir, "namespaces", upgradeConfigurationNamespace, "cluster", clusterConfigDir)
				dir, err := os.ReadDir(filesDir)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				assert.Equal(t, len(dir), numberOfFilesOnSuccess)

				// validate cluster version
				data, err := os.ReadFile(filepath.Join(filesDir, clusterIDFileName))
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				clusterVersion := &ocpV1.ClusterVersion{}
				err = json.Unmarshal(data, clusterVersion)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				assert.Equal(t, clusterVersion.Spec.ClusterID, ocpV1.ClusterID("1"))

				// validate cluster relocation object
				data, err = os.ReadFile(filepath.Join(filesDir, croFileName))
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				clusterRelocation := &cro.ClusterRelocation{}
				err = json.Unmarshal(data, clusterRelocation)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				assert.Equal(t, clusterRelocation.Name, croName)
				assert.Equal(t, clusterRelocation.Spec.Domain, "test.com")
				assert.Equal(t, len(clusterRelocation.Spec.ImageDigestMirrors), 1)
				assert.Equal(t, clusterRelocation.Spec.ImageDigestMirrors[0].Source, imageSetName)
				assert.Equal(t, clusterRelocation.Spec.PullSecretRef.Name, pullSecretName)
				assert.Equal(t, clusterRelocation.Spec.PullSecretRef.Namespace, upgradeConfigurationNamespace)

				// validate pull secret
				data, err = os.ReadFile(filepath.Join(filesDir, pullSecretFileName))
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				secret := &corev1.Secret{}
				err = json.Unmarshal(data, secret)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				testData := map[string][]byte{"aaa": []byte("bbb")}
				assert.Equal(t, secret.Data, testData)
				assert.Equal(t, secret.Name, pullSecretName)
				assert.Equal(t, secret.Namespace, upgradeConfigurationNamespace)
			},
		},
		{
			name:   "no secret found",
			secret: &corev1.Secret{},
			clusterVersion: &ocpV1.ClusterVersion{
				ObjectMeta: metav1.ObjectMeta{
					Name: "version",
				},
				Spec: ocpV1.ClusterVersionSpec{
					ClusterID: "1",
				},
			},
			ingress: &ocpV1.Ingress{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Spec: ocpV1.IngressSpec{Domain: "test.com"},
			},
			idm: &ocpV1.ImageDigestMirrorSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: imageSetName,
				},
				Spec: ocpV1.ImageDigestMirrorSetSpec{ImageDigestMirrors: []ocpV1.ImageDigestMirrors{{Source: imageSetName}}},
			},
			expectedErr: true,
			validateFunc: func(t *testing.T, tempDir string, err error) {
				assert.Equal(t, errors.IsNotFound(err), true)
				assert.Equal(t, strings.Contains(err.Error(), "secret"), true)
			},
		},
		{
			name: " clusterversion error",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pullSecretName,
					Namespace: configNamespace,
				},
			},
			clusterVersion: &ocpV1.ClusterVersion{},
			ingress: &ocpV1.Ingress{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Spec: ocpV1.IngressSpec{Domain: "test.com"},
			},
			idm: &ocpV1.ImageDigestMirrorSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: imageSetName,
				},
				Spec: ocpV1.ImageDigestMirrorSetSpec{ImageDigestMirrors: []ocpV1.ImageDigestMirrors{{Source: imageSetName}}},
			},
			expectedErr: true,
			validateFunc: func(t *testing.T, tempDir string, err error) {
				assert.Equal(t, strings.Contains(err.Error(), "clusterversion"), true)
			},
		},
		{
			name: "ingress error",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pullSecretName,
					Namespace: configNamespace,
				},
			},
			clusterVersion: &ocpV1.ClusterVersion{
				ObjectMeta: metav1.ObjectMeta{
					Name: "version",
				},
				Spec: ocpV1.ClusterVersionSpec{
					ClusterID: "1",
				},
			},
			ingress: &ocpV1.Ingress{},
			idm: &ocpV1.ImageDigestMirrorSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: imageSetName,
				},
				Spec: ocpV1.ImageDigestMirrorSetSpec{ImageDigestMirrors: []ocpV1.ImageDigestMirrors{{Source: imageSetName}}},
			},
			expectedErr: true,
			validateFunc: func(t *testing.T, tempDir string, err error) {
				assert.Equal(t, strings.Contains(err.Error(), "ingress"), true)
			},
		},
		{
			name: "idm not found, should still succeed",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pullSecretName,
					Namespace: configNamespace,
				},
			},
			clusterVersion: &ocpV1.ClusterVersion{
				ObjectMeta: metav1.ObjectMeta{
					Name: "version",
				},
				Spec: ocpV1.ClusterVersionSpec{
					ClusterID: "1",
				},
			},
			ingress: &ocpV1.Ingress{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Spec: ocpV1.IngressSpec{Domain: "test.com"},
			},
			idm:         &ocpV1.ImageDigestMirrorSet{},
			expectedErr: false,
			validateFunc: func(t *testing.T, tempDir string, err error) {
				filesDir := filepath.Join(tempDir, "namespaces", upgradeConfigurationNamespace, "cluster", clusterConfigDir)
				dir, err := os.ReadDir(filesDir)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				assert.Equal(t, len(dir), numberOfFilesOnSuccess)
			},
		},
	}

	for _, tc := range testcases {
		tmpDir := t.TempDir()
		t.Run(tc.name, func(t *testing.T) {

			objs := []client.Object{tc.secret, tc.clusterVersion, tc.ingress, tc.idm}
			fakeClient, err := getFakeClientFromObjects(objs...)
			if err != nil {
				t.Errorf("error in creating fake client")
			}

			ucc := UpgradeClusterConfigGather{
				Client: fakeClient,
				Log:    logr.Discard(),
				Scheme: fakeClient.Scheme(),
			}
			err = ucc.FetchClusterConfig(context.TODO(), tmpDir)
			if !tc.expectedErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tc.expectedErr && err == nil {
				t.Errorf("expected error but it didn't happened")
			}
			tc.validateFunc(t, tmpDir, err)
		})
	}
}
