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
	"os"
	"path/filepath"
	"strings"
	"testing"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"

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

	"github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
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
				{Type: corev1.NodeHostName, Address: "seed"},
			},
		},
	}

	seedManifestData = seedreconfig.SeedReconfiguration{BaseDomain: "seed.com", ClusterName: "seed", NodeIP: "192.168.127.10", Hostname: "seed"}

	csvDeployment = &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: common.CsvDeploymentName, Namespace: common.CsvDeploymentNamespace},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "cluster-version-operator",
			Image: "mirror.redhat.com:5005/openshift-release-dev/ocp-release@sha256:d6a7e20a8929a3ad985373f05472ea64bada8ff46f0beb89e1b6d04919affde3"}}}},
		}}
	kubeconfigRetentionObjects = []client.Object{
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "admin-kubeconfig-client-ca",
				Namespace: "openshift-config",
			},
			Data: map[string]string{"ca-bundle.crt": "test"},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "loadbalancer-serving-signer",
				Namespace: "openshift-kube-apiserver-operator",
			},
			Data: map[string][]byte{"tls.key": []byte("test")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "localhost-serving-signer",
				Namespace: "openshift-kube-apiserver-operator",
			},
			Data: map[string][]byte{"tls.key": []byte("test")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "service-network-serving-signer",
				Namespace: "openshift-kube-apiserver-operator",
			},
			Data: map[string][]byte{"tls.key": []byte("test")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "router-ca",
				Namespace: "openshift-ingress-operator",
			},
			Data: map[string][]byte{"tls.key": []byte("test")},
		},
	}

	infrastructure = &ocpV1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Status: ocpV1.InfrastructureStatus{
			InfrastructureName: "mysno-xsb4m",
		},
	}

	kubeadminSecret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kubeadmin",
			Namespace: "kube-system",
		},
		Data: map[string][]byte{"kubeadmin": []byte(`$2a$10$20Q4iRLy7cWZkjn/D07bF.RZQZonKwstyRGH0qiYbYRkx5Pe4Ztyi`)},
	}
)

func init() {
	testscheme.AddKnownTypes(
		ocpV1.GroupVersion,
		&ocpV1.ClusterVersion{},
		&ocpV1.ImageDigestMirrorSet{},
		&ocpV1.ImageDigestMirrorSetList{},
		&ocpV1.Proxy{},
		&ocpV1.Infrastructure{},
		&mcv1.MachineConfig{},
		&ocpV1.ImageContentPolicy{},
		&ocpV1.ImageContentPolicyList{},
		&ocpV1.Network{})
	testscheme.AddKnownTypes(operatorv1alpha1.GroupVersion,
		&operatorv1alpha1.ImageContentSourcePolicyList{},
		&operatorv1alpha1.ImageContentSourcePolicy{})
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
	defaultPullSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pullSecretName,
			Namespace: common.OpenshiftConfigNamespace,
		},
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte("pull-secret")},
	}

	noPullSecret := &corev1.Secret{}

	defaultClusterVersion := &ocpV1.ClusterVersion{
		ObjectMeta: metav1.ObjectMeta{
			Name: "version",
		},
		Spec: ocpV1.ClusterVersionSpec{
			ClusterID: "1",
		},
	}

	emptyClusterVersion := &ocpV1.ClusterVersion{}

	defaultIDMS := &ocpV1.ImageDigestMirrorSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "any",
		},
		Spec: ocpV1.ImageDigestMirrorSetSpec{ImageDigestMirrors: []ocpV1.ImageDigestMirrors{{Source: "data"}}},
	}

	defaultProxy := &ocpV1.Proxy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Spec: ocpV1.ProxySpec{
			HTTPProxy: "some-http-proxy",
		},
		Status: ocpV1.ProxyStatus{
			HTTPProxy: "some-http-proxy-status",
		},
	}

	installConfig := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      common.InstallConfigCM,
			Namespace: common.InstallConfigCMNamespace,
		},
		Data: map[string]string{"install-config": clusterCmData},
	}

	network := &ocpV1.Network{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Status: ocpV1.NetworkStatus{
			ClusterNetwork: []ocpV1.ClusterNetworkEntry{{CIDR: "1.1.1.1/24"}},
			ServiceNetwork: []string{"2.2.2.2/24"},
		},
	}

	testcases := []struct {
		testCaseName    string
		pullSecret      client.Object
		caBundleCM      client.Object
		clusterVersion  client.Object
		idms            client.Object
		icsps           []client.Object
		node            client.Object
		proxy           client.Object
		deleteKubeadmin bool
		expectedErr     bool
		validateFunc    func(t *testing.T, tempDir string, err error, ucc UpgradeClusterConfigGather)
	}{
		{
			testCaseName:   "Validate success flow",
			pullSecret:     defaultPullSecret,
			clusterVersion: defaultClusterVersion,
			idms:           defaultIDMS,
			node:           validMasterNode,
			proxy:          defaultProxy,
			icsps:          nil,
			caBundleCM:     nil,
			expectedErr:    false,
			validateFunc: func(t *testing.T, tempDir string, err error, ucc UpgradeClusterConfigGather) {
				clusterConfigPath, err := ucc.configDir(tempDir)
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

				seedReconfig, err := getSeedReconfigFromUcc(ucc, tempDir)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}

				assert.Equal(t, "$2a$10$20Q4iRLy7cWZkjn/D07bF.RZQZonKwstyRGH0qiYbYRkx5Pe4Ztyi", seedReconfig.KubeadminPasswordHash)
				assert.Equal(t, "mysno-xsb4m", seedReconfig.InfraID)
				assert.Equal(t, "pull-secret", seedReconfig.PullSecret)
				assert.Equal(t, "ssh-key", seedReconfig.SSHKey)
				assert.Equal(t, "test-infra-cluster", seedReconfig.ClusterName)
				assert.Equal(t, "redhat.com", seedReconfig.BaseDomain)
				assert.Equal(t, "192.168.121.10", seedReconfig.NodeIP)
				assert.Equal(t, "mirror.redhat.com:5005", seedReconfig.ReleaseRegistry)
				assert.Equal(t, "some-http-proxy", seedReconfig.Proxy.HTTPProxy)
				assert.Equal(t, "some-http-proxy-status", seedReconfig.StatusProxy.HTTPProxy)
			},
		},
		{
			testCaseName:   "no pull secret found",
			pullSecret:     noPullSecret,
			clusterVersion: defaultClusterVersion,
			idms:           nil,
			icsps:          nil,
			caBundleCM:     nil,
			node:           validMasterNode,
			proxy:          defaultProxy,
			expectedErr:    true,
			validateFunc: func(t *testing.T, tempDir string, err error, ucc UpgradeClusterConfigGather) {
				assert.Equal(t, true, errors.IsNotFound(err))
				assert.Equal(t, true, strings.Contains(err.Error(), "secret"))
			},
		},
		{
			testCaseName:    "no kubeadmin secret found",
			pullSecret:      defaultPullSecret,
			clusterVersion:  defaultClusterVersion,
			idms:            nil,
			icsps:           nil,
			caBundleCM:      nil,
			node:            validMasterNode,
			proxy:           defaultProxy,
			expectedErr:     false,
			deleteKubeadmin: true,
			validateFunc: func(t *testing.T, tempDir string, err error, ucc UpgradeClusterConfigGather) {
				seedReconfig, err := getSeedReconfigFromUcc(ucc, tempDir)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				assert.Equal(t, "", seedReconfig.KubeadminPasswordHash)
			},
		},
		{
			testCaseName:   " clusterversion error",
			pullSecret:     defaultPullSecret,
			idms:           nil,
			icsps:          nil,
			caBundleCM:     nil,
			node:           validMasterNode,
			proxy:          defaultProxy,
			clusterVersion: emptyClusterVersion,
			expectedErr:    true,
			validateFunc: func(t *testing.T, tempDir string, err error, ucc UpgradeClusterConfigGather) {
				assert.Equal(t, true, strings.Contains(err.Error(), "clusterversion"))
			},
		},
		{
			testCaseName:   "idm not found, should still succeed",
			pullSecret:     defaultPullSecret,
			clusterVersion: defaultClusterVersion,
			idms:           nil,
			icsps:          nil,
			caBundleCM:     nil,
			node:           validMasterNode,
			proxy:          defaultProxy,
			expectedErr:    false,
			validateFunc: func(t *testing.T, tempDir string, err error, ucc UpgradeClusterConfigGather) {
				filesDir, err := ucc.configDir(tempDir)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				dir, err := os.ReadDir(filepath.Join(filesDir, manifestDir))
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				assert.Equal(t, 0, len(dir))
			},
		},
		{
			testCaseName:   "master not found, should fail",
			pullSecret:     defaultPullSecret,
			clusterVersion: defaultClusterVersion,
			idms:           nil,
			icsps:          nil,
			caBundleCM:     nil,
			node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"node-role.kubernetes.io/worker:": ""}},
				Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "192.168.121.10"}}}},
			proxy:       defaultProxy,
			expectedErr: true,
			validateFunc: func(t *testing.T, tempDir string, err error, ucc UpgradeClusterConfigGather) {
				assert.Equal(t, true, strings.Contains(err.Error(), "one master node in sno cluster"))
			},
		},
		{
			testCaseName:   "Validate mirror values",
			pullSecret:     defaultPullSecret,
			clusterVersion: defaultClusterVersion,
			idms:           defaultIDMS,
			node:           validMasterNode,
			proxy:          defaultProxy,
			icsps: []client.Object{&operatorv1alpha1.ImageContentSourcePolicy{ObjectMeta: metav1.ObjectMeta{
				Name: "1",
			}, Spec: operatorv1alpha1.ImageContentSourcePolicySpec{
				RepositoryDigestMirrors: []operatorv1alpha1.RepositoryDigestMirrors{
					{Source: "icspData"}}}},
				&operatorv1alpha1.ImageContentSourcePolicy{ObjectMeta: metav1.ObjectMeta{
					Name: "2",
				}, Spec: operatorv1alpha1.ImageContentSourcePolicySpec{
					RepositoryDigestMirrors: []operatorv1alpha1.RepositoryDigestMirrors{{Source: "icspData2"}}}}},
			caBundleCM: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: caBundleCMName,
				Namespace: common.OpenshiftConfigNamespace}, Data: map[string]string{"test": "data"}},
			expectedErr: false,
			validateFunc: func(t *testing.T, tempDir string, err error, ucc UpgradeClusterConfigGather) {
				clusterConfigPath, err := ucc.configDir(tempDir)
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
				icsps := &operatorv1alpha1.ImageContentSourcePolicyList{}
				if err := utils.ReadYamlOrJSONFile(filepath.Join(manifestsDir, icspsFileName), icsps); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if assert.Equal(t, 2, len(icsps.Items)) {
					resultSourcesAsString := ""
					for _, icsp := range icsps.Items {
						resultSourcesAsString = resultSourcesAsString + "" + icsp.Spec.RepositoryDigestMirrors[0].Source
					}

					assert.Contains(t, resultSourcesAsString, "icspData")
					assert.Contains(t, resultSourcesAsString, "icspData2")
				}

				// validate caBundle
				caBundle := &corev1.ConfigMap{}
				if err := utils.ReadYamlOrJSONFile(filepath.Join(manifestsDir, caBundleFileName), caBundle); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				assert.Equal(t, caBundleCMName, caBundle.Name)
				assert.Equal(t, caBundle.Data, map[string]string{"test": "data"})

				_, err = os.Stat(filepath.Join(clusterConfigPath, filepath.Base(common.CABundleFilePath)))
				assert.Nil(t, err)
			},
		},
	}

	for _, testCase := range testcases {
		clusterConfigDir := t.TempDir()
		t.Run(testCase.testCaseName, func(t *testing.T) {
			hostPath = clusterConfigDir

			if testCase.caBundleCM != nil {
				dir := filepath.Join(clusterConfigDir, filepath.Dir(common.CABundleFilePath))
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				newPath := filepath.Join(dir, filepath.Base(common.CABundleFilePath))
				f, err := os.Create(newPath)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				_ = f.Close()
			}

			sshKeyDir := filepath.Join(clusterConfigDir, filepath.Dir(sshKeyFile))
			if err := os.MkdirAll(sshKeyDir, 0o700); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if err := os.WriteFile(filepath.Join(clusterConfigDir, sshKeyFile), []byte("ssh-key"), 0o600); err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			k8sResources := []client.Object{
				testCase.pullSecret, testCase.clusterVersion, installConfig, testCase.node,
				testCase.idms, testCase.proxy, testCase.caBundleCM, csvDeployment, infrastructure,
			}

			k8sResources = append(k8sResources, network)

			if !testCase.deleteKubeadmin {
				k8sResources = append(k8sResources, kubeadminSecret)
			}

			for _, kcro := range kubeconfigRetentionObjects {
				k8sResources = append(k8sResources, kcro)
			}

			if testCase.icsps != nil {
				for _, icsp := range testCase.icsps {
					k8sResources = append(k8sResources, icsp)
				}
			}

			fakeK8sClient, err := getFakeClientFromObjects(k8sResources...)
			if err != nil {
				t.Errorf("error in creating fake client")
			}

			if err := os.MkdirAll(filepath.Join(clusterConfigDir, common.OptOpenshift), 0o700); err != nil {
				t.Errorf("failed to create opt dir, error: %v", err)
			}
			if err := os.MkdirAll(filepath.Join(clusterConfigDir, common.SeedDataDir), 0o700); err != nil {
				t.Errorf("failed to create %s dir, error: %v", common.SeedDataDir, err)
			}
			err = utils.MarshalToFile(seedManifestData, filepath.Join(clusterConfigDir, common.SeedDataDir, common.SeedClusterInfoFileName))
			if err != nil {
				t.Errorf("failed to create seed manifest, error: %v", err)
			}

			ucc := UpgradeClusterConfigGather{
				Client: fakeK8sClient,
				Log:    logr.Discard(),
				Scheme: fakeK8sClient.Scheme(),
			}
			err = ucc.FetchClusterConfig(context.TODO(), clusterConfigDir)
			if !testCase.expectedErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if testCase.expectedErr && err == nil {
				t.Errorf("expected error but it didn't happened")
			}
			testCase.validateFunc(t, clusterConfigDir, err, ucc)
		})
	}
}

func getSeedReconfigFromUcc(ucc UpgradeClusterConfigGather, tempDir string) (*seedreconfig.SeedReconfiguration, error) {
	clusterConfigPath, err := ucc.configDir(tempDir)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster config path, error: %w", err)
	}
	seedReconfig := &seedreconfig.SeedReconfiguration{}
	if err := utils.ReadYamlOrJSONFile(filepath.Join(clusterConfigPath, common.SeedClusterInfoFileName), seedReconfig); err != nil {
		return nil, fmt.Errorf("failed to read seed manifest, error: %w", err)
	}
	return seedReconfig, nil
}

func TestNetworkConfig(t *testing.T) {
	testcases := []struct {
		name          string
		filesToCreate []string
		expectedErr   bool
		validateFunc  func(t *testing.T, tmpDir string, err error, files []string, unc UpgradeClusterConfigGather)
	}{
		{
			name: "Validate success flow",
			filesToCreate: []string{"/etc/hostname", "/etc/NetworkManager/system-connections/test1.txt",
				"/etc/NetworkManager/system-connections/scripts/test1.txt"},
			expectedErr: false,
			validateFunc: func(t *testing.T, tmpDir string, err error, files []string, unc UpgradeClusterConfigGather) {
				dir := filepath.Join(tmpDir, filepath.Join(common.OptOpenshift, common.NetworkDir))
				counter := 0
				for _, file := range files {
					err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
						if err == nil && info.Name() == filepath.Base(file) {
							counter++
						}
						return nil
					})
					if err != nil {
						t.Errorf("unexpected error: %v", err)
					}
				}
				assert.Equal(t, len(files), counter)
			},
		},
	}

	for _, tc := range testcases {
		tmpDir := t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			listOfNetworkFilesPaths = []string{}
			hostPath = tmpDir
			// create list of files to copy
			for _, path := range tc.filesToCreate {
				dir := filepath.Join(tmpDir, filepath.Dir(path))
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				newPath := filepath.Join(dir, filepath.Base(path))
				f, err := os.Create(newPath)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}

				_ = f.Close()
				listOfNetworkFilesPaths = append(listOfNetworkFilesPaths, path)
			}

			unc := UpgradeClusterConfigGather{
				Log: logr.Discard(),
			}
			err := unc.fetchNetworkConfig(tmpDir)
			if !tc.expectedErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tc.expectedErr && err == nil {
				t.Errorf("expected error but it didn't happened")
			}
			tc.validateFunc(t, tmpDir, err, tc.filesToCreate, unc)
		})
	}
}
