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
	"strings"
	"testing"

	cro "github.com/RHsyseng/cluster-relocation-operator/api/v1beta1"
	"github.com/go-logr/logr"
	ocpV1 "github.com/openshift/api/config/v1"
	mcv1 "github.com/openshift/api/machineconfiguration/v1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/clusterinfo"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

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
	testscheme = scheme.Scheme

	validMasterNode = &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"node-role.kubernetes.io/master": ""},
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.121.10"},
			},
		},
	}

	seedManifestData = clusterinfo.ClusterInfo{Domain: "seed.com", ClusterName: "seed", MasterIP: "192.168.127.10"}

	machineConfigs = []*mcv1.MachineConfig{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "99-master-ssh",
				Labels: map[string]string{"machineconfiguration.openshift.io/role": "master"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "99-worker-ssh",
				Labels: map[string]string{"machineconfiguration.openshift.io/role": "worker"},
			},
		},
	}
)

func init() {
	testscheme.AddKnownTypes(
		ocpV1.GroupVersion,
		&ocpV1.ClusterVersion{},
		&ocpV1.ImageDigestMirrorSet{},
		&ocpV1.ImageDigestMirrorSetList{},
		&ocpV1.Proxy{},
		&mcv1.MachineConfig{},
		&ocpV1.ImageContentPolicy{},
		&ocpV1.ImageContentPolicyList{})
	testscheme.AddKnownTypes(cro.GroupVersion, &cro.ClusterRelocation{})
}

func getFakeClientFromObjects(objs ...client.Object) (client.WithWatch, error) {
	var objectsToAdd []client.Object
	for _, obj := range objs {
		if obj != nil {
			objectsToAdd = append(objectsToAdd, obj)
		}
	}
	c := fake.NewClientBuilder().WithScheme(testscheme).WithObjects(objectsToAdd...).WithStatusSubresource(objectsToAdd...).Build()
	return c, nil
}

func TestClusterConfig(t *testing.T) {
	testcases := []struct {
		name           string
		secret         client.Object
		caBundleCM     client.Object
		clusterVersion client.Object
		idms           client.Object
		icsp           client.Object
		node           client.Object
		proxy          client.Object
		machineConfigs []*mcv1.MachineConfig
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
			node: validMasterNode,
			proxy: &ocpV1.Proxy{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Spec: ocpV1.ProxySpec{
					HTTPProxy: "some-http-proxy",
				},
			},
			icsp:           nil,
			caBundleCM:     nil,
			machineConfigs: machineConfigs,
			expectedErr:    false,
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
				assert.Equal(t, ocpV1.ClusterID("1"), clusterVersion.Spec.ClusterID)

				// validate proxy
				proxy := &ocpV1.Proxy{}
				if err := utils.ReadYamlOrJSONFile(filepath.Join(manifestsDir, proxyFileName), proxy); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				assert.Equal(t, proxyName, proxy.Name)
				assert.Equal(t, "some-http-proxy", proxy.Spec.HTTPProxy)

				// validate pull secret
				secret := &corev1.Secret{}
				if err := utils.ReadYamlOrJSONFile(filepath.Join(manifestsDir, pullSecretFileName), secret); err != nil {
					t.Errorf("unexpected error: %v", err)
				}

				testData := map[string][]byte{"aaa": []byte("bbb")}
				assert.Equal(t, testData, secret.Data)
				assert.Equal(t, pullSecretName, secret.Name)
				assert.Equal(t, configNamespace, secret.Namespace)

				// validate pull idms
				idms := &ocpV1.ImageDigestMirrorSetList{}
				if err := utils.ReadYamlOrJSONFile(filepath.Join(manifestsDir, idmsFileName), idms); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if assert.Equal(t, 1, len(idms.Items)) {
					assert.Equal(t, "any", idms.Items[0].Name)
					assert.Equal(t, "data", idms.Items[0].Spec.ImageDigestMirrors[0].Source)
				}

				// validate manifest json

				clusterInfo := &clusterinfo.ClusterInfo{}
				if err := utils.ReadYamlOrJSONFile(filepath.Join(clusterConfigPath, common.ClusterInfoFileName), clusterInfo); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				assert.Equal(t, "test-infra-cluster", clusterInfo.ClusterName)
				assert.Equal(t, "redhat.com", clusterInfo.Domain)
				assert.Equal(t, "192.168.121.10", clusterInfo.MasterIP)
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
			idms:       nil,
			icsp:       nil,
			caBundleCM: nil,
			node:       validMasterNode,
			proxy: &ocpV1.Proxy{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
			},
			machineConfigs: machineConfigs,
			expectedErr:    true,
			validateFunc: func(t *testing.T, tempDir string, err error, ucc UpgradeClusterConfigGather) {
				assert.Equal(t, true, errors.IsNotFound(err))
				assert.Equal(t, true, strings.Contains(err.Error(), "secret"))
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
			idms:       nil,
			icsp:       nil,
			caBundleCM: nil,
			node:       validMasterNode,
			proxy: &ocpV1.Proxy{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
			},
			machineConfigs: machineConfigs,
			clusterVersion: &ocpV1.ClusterVersion{},
			expectedErr:    true,
			validateFunc: func(t *testing.T, tempDir string, err error, ucc UpgradeClusterConfigGather) {
				assert.Equal(t, true, strings.Contains(err.Error(), "clusterversion"))
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
			idms:       nil,
			icsp:       nil,
			caBundleCM: nil,
			node:       validMasterNode,
			proxy: &ocpV1.Proxy{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
			},
			machineConfigs: machineConfigs,
			expectedErr:    false,
			validateFunc: func(t *testing.T, tempDir string, err error, ucc UpgradeClusterConfigGather) {
				filesDir, err := ucc.configDirs(tempDir)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				dir, err := os.ReadDir(filepath.Join(filesDir, manifestDir))
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				assert.Equal(t, 6, len(dir))
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
			idms:       nil,
			icsp:       nil,
			caBundleCM: nil,
			node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"node-role.kubernetes.io/worker:": ""}},
				Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "192.168.121.10"}}}},
			proxy: &ocpV1.Proxy{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
			},
			machineConfigs: machineConfigs,
			expectedErr:    true,
			validateFunc: func(t *testing.T, tempDir string, err error, ucc UpgradeClusterConfigGather) {
				assert.Equal(t, true, strings.Contains(err.Error(), "one master node in sno cluster"))
			},
		},
		{
			name: "Validate mirror values",
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
			node: validMasterNode,
			proxy: &ocpV1.Proxy{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
			},
			icsp: &ocpV1.ImageContentPolicy{Spec: ocpV1.ImageContentPolicySpec{
				RepositoryDigestMirrors: []ocpV1.RepositoryDigestMirrors{{Source: "icspData"}}}},
			caBundleCM: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: caBundleCMName,
				Namespace: configNamespace}, Data: map[string]string{"test": "data"}},
			machineConfigs: machineConfigs,
			expectedErr:    false,
			validateFunc: func(t *testing.T, tempDir string, err error, ucc UpgradeClusterConfigGather) {
				clusterConfigPath, err := ucc.configDirs(tempDir)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				manifestsDir := filepath.Join(clusterConfigPath, manifestDir)

				// validate pull idms
				idms := &ocpV1.ImageDigestMirrorSetList{}
				if err := utils.ReadYamlOrJSONFile(filepath.Join(manifestsDir, idmsFileName), idms); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if assert.Equal(t, 1, len(idms.Items)) {
					assert.Equal(t, "any", idms.Items[0].Name)
					assert.Equal(t, "data", idms.Items[0].Spec.ImageDigestMirrors[0].Source)
				}

				// validate icsp
				icsps := &ocpV1.ImageContentPolicyList{}
				if err := utils.ReadYamlOrJSONFile(filepath.Join(manifestsDir, icspsFileName), icsps); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if assert.Equal(t, 1, len(icsps.Items)) {
					assert.Equal(t, "icspData", icsps.Items[0].Spec.RepositoryDigestMirrors[0].Source)
				}

				// validate caBundle
				caBundle := &corev1.ConfigMap{}
				if err := utils.ReadYamlOrJSONFile(filepath.Join(manifestsDir, caBundleFileName), caBundle); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				assert.Equal(t, caBundleCMName, caBundle.Name)
				assert.Equal(t, caBundle.Data, map[string]string{"test": "data"})
			},
		},
	}

	for _, tc := range testcases {
		tmpDir := t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			installConfig := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterinfo.InstallConfigCM,
					Namespace: clusterinfo.InstallConfigCMNamespace,
				},
				Data: map[string]string{"install-config": clusterCmData},
			}
			objs := []client.Object{tc.secret, tc.clusterVersion, installConfig, tc.node,
				tc.idms, tc.proxy, tc.icsp, tc.caBundleCM}
			for _, mc := range tc.machineConfigs {
				objs = append(objs, mc)
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

			if err := os.MkdirAll(filepath.Join(tmpDir, common.OptOpenshift), 0o700); err != nil {
				t.Errorf("failed to create opt dir, error: %v", err)
			}
			err = utils.MarshalToFile(seedManifestData, filepath.Join(tmpDir, common.OptOpenshift, common.SeedManifest))
			if err != nil {
				t.Errorf("failed to create seed manifest, error: %v", err)
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
