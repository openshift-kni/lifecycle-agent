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
	"fmt"
	"github.com/openshift-kni/lifecycle-agent/ibu-imager/clusterinfo"
	"github.com/openshift-kni/lifecycle-agent/utils"
	"k8s.io/apimachinery/pkg/api/errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cro "github.com/RHsyseng/cluster-relocation-operator/api/v1beta1"
	"github.com/go-logr/logr"
	ocpV1 "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const numberOfFilesOnSuccess = 4

var clusterCmData = `
    additionalTrustBundlePolicy: Proxyonly
    apiVersion: v1
    baseDomain: redhat.com
    bootstrapInPlace:
      installationDisk: /dev/disk/by-id/wwn-0x05abcd6da8679a1c
    compute:
    - architecture: amd64
      hyperthreading: Enabled
      name: worker
      platform: {}
      replicas: 0
    controlPlane:
      architecture: amd64
      hyperthreading: Enabled
      name: master
      platform: {}
      replicas: 1
    metadata:
      creationTimestamp: null
      name: test-infra-cluster
    networking:
      clusterNetwork:
      - cidr: 172.30.0.0/16
        hostPrefix: 23
      machineNetwork:
      - cidr: 192.168.127.0/24
      networkType: OVNKubernetes
      serviceNetwork:
      - 10.128.0.0/14
    platform:
      none: {}
    publish: External
    pullSecret: ""
`

var (
	testscheme      = scheme.Scheme
	validMasterNode = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"node-role.kubernetes.io/master": ""}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "192.168.121.10"}}}}
	seedManifestData = clusterinfo.ClusterInfo{Domain: "seed.com", ClusterName: "seed", MasterIP: "192.168.127.10"}
)

func init() {
	testscheme.AddKnownTypes(ocpV1.GroupVersion, &ocpV1.ClusterVersion{},
		&ocpV1.ImageDigestMirrorSet{}, &ocpV1.ImageDigestMirrorSetList{})
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
		idms           client.Object
		node           client.Object
		expectedErr    bool
		validateFunc   func(t *testing.T, tempDir string, err error, ucc UpgradeClusterConfigGather)
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
			idms: &ocpV1.ImageDigestMirrorSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: "any",
				},
				Spec: ocpV1.ImageDigestMirrorSetSpec{ImageDigestMirrors: []ocpV1.ImageDigestMirrors{{Source: "data"}}},
			},
			node:        validMasterNode,
			expectedErr: false,
			validateFunc: func(t *testing.T, tempDir string, err error, ucc UpgradeClusterConfigGather) {
				clusterConfigPath, err := ucc.configDirs(tempDir)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				manifestsDir := filepath.Join(clusterConfigPath, manifestDir)

				// validate cluster version
				clusterVersion := &ocpV1.ClusterVersion{}
				if err := utils.ReadYamlOrJSONFile(filepath.Join(manifestsDir, clusterIDFileName), clusterVersion); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				assert.Equal(t, clusterVersion.Spec.ClusterID, ocpV1.ClusterID("1"))

				// validate pull secret
				secret := &corev1.Secret{}
				if err := utils.ReadYamlOrJSONFile(filepath.Join(manifestsDir, pullSecretFileName), secret); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				testData := map[string][]byte{"aaa": []byte("bbb")}
				assert.Equal(t, secret.Data, testData)
				assert.Equal(t, secret.Name, pullSecretName)
				assert.Equal(t, secret.Namespace, upgradeConfigurationNamespace)

				// validate pull idms
				idms := &ocpV1.ImageDigestMirrorSetList{}
				if err := utils.ReadYamlOrJSONFile(filepath.Join(manifestsDir, idmsFIlePath), idms); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				assert.Equal(t, idms.Items[0].Name, "any")
				assert.Equal(t, idms.Items[0].Spec.ImageDigestMirrors[0].Source, "data")

				// validate manifest json

				clusterInfo := &clusterinfo.ClusterInfo{}
				if err := utils.ReadYamlOrJSONFile(filepath.Join(clusterConfigPath, clusterInfoFileName), clusterInfo); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				assert.Equal(t, clusterInfo.ClusterName, "test-infra-cluster")
				assert.Equal(t, clusterInfo.Domain, "redhat.com")
				assert.Equal(t, clusterInfo.MasterIP, "192.168.121.10")

				rConfig := &recertConfig{}
				if err := utils.ReadYamlOrJSONFile(filepath.Join(clusterConfigPath, recertConfigFile), rConfig); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				assert.Equal(t, rConfig.ClusterRename, "test-infra-cluster:redhat.com")
				assert.Equal(t, len(rConfig.CNSanReplaceRules), 4)
				assert.Contains(t, rConfig.CNSanReplaceRules, fmt.Sprintf("%s,%s", seedManifestData.MasterIP, clusterInfo.MasterIP))
				assert.Contains(t, rConfig.UseKey, "ingress@test /opt/openshift/certs/ingresskey-ingress@test")

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
			idms:        nil,
			node:        validMasterNode,
			expectedErr: true,
			validateFunc: func(t *testing.T, tempDir string, err error, ucc UpgradeClusterConfigGather) {
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
			idms:           nil,
			node:           validMasterNode,
			clusterVersion: &ocpV1.ClusterVersion{},
			expectedErr:    true,
			validateFunc: func(t *testing.T, tempDir string, err error, ucc UpgradeClusterConfigGather) {
				assert.Equal(t, strings.Contains(err.Error(), "clusterversion"), true)
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
			node:        validMasterNode,
			idms:        nil,
			expectedErr: false,
			validateFunc: func(t *testing.T, tempDir string, err error, ucc UpgradeClusterConfigGather) {
				filesDir, err := ucc.configDirs(tempDir)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				dir, err := os.ReadDir(filepath.Join(filesDir, manifestDir))
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				assert.Equal(t, len(dir), 3)
			},
		},
		{
			name: "master not found, should fail",
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
			node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"node-role.kubernetes.io/worker:": ""}},
				Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "192.168.121.10"}}}},
			idms:        nil,
			expectedErr: true,
			validateFunc: func(t *testing.T, tempDir string, err error, ucc UpgradeClusterConfigGather) {
				assert.Equal(t, strings.Contains(err.Error(), "one master node in sno cluster"), true)
			},
		},
	}

	for _, tc := range testcases {
		tmpDir := t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			installConfig := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: clusterinfo.InstallConfigCM,
				Namespace: clusterinfo.InstallConfigCMNamespace}, Data: map[string]string{"install-config": clusterCmData}}
			objs := []client.Object{tc.secret, tc.clusterVersion, installConfig, tc.node}
			if tc.idms != nil {
				objs = append(objs, tc.idms)
			}
			fakeClient, err := getFakeClientFromObjects(objs...)
			if err != nil {
				t.Errorf("error in creating fake client")
			}

			ucc := UpgradeClusterConfigGather{
				Client: fakeClient,
				Log:    logr.Discard(),
				Scheme: fakeClient.Scheme(),
			}

			if err := os.MkdirAll(filepath.Join(tmpDir, filepath.Dir(clusterConfigDir)), 0o700); err != nil {
				t.Errorf("failed to create opt dir, error: %v", err)
			}
			err = utils.WriteToFile(seedManifestData, filepath.Join(tmpDir, filepath.Dir(clusterConfigDir), seedManifest))
			if err != nil {
				t.Errorf("failed to create seed manifest, error: %v", err)
			}

			if err := os.MkdirAll(filepath.Join(tmpDir, certsDir), 0o700); err != nil {
				t.Errorf("failed to create opt dir, error: %v", err)
			}
			_, err = os.Create(filepath.Join(tmpDir, certsDir, "ingresskey-ingress@test"))
			if err != nil {
				t.Errorf("failed to create ingress file, error: %v", err)
			}

			err = ucc.FetchClusterConfig(context.TODO(), tmpDir)
			if !tc.expectedErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tc.expectedErr && err == nil {
				t.Errorf("expected error but it didn't happened")
			}
			tc.validateFunc(t, tmpDir, err, ucc)
		})
	}
}
