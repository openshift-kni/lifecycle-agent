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

package common

// Common constants mainly used by packages in lca-cli
const (
	VarFolder       = "/var"
	BackupDir       = "/var/tmp/backup"
	BackupCertsDir  = "/var/tmp/backupCertsDir"
	BackupChecksDir = "/var/tmp/checks"

	// Workload partitioning annotation key and value
	WorkloadManagementAnnotationKey   = "target.workload.openshift.io/management"
	WorkloadManagementAnnotationValue = `{"effect": "PreferredDuringScheduling"}`

	// ImageRegistryAuthFile is the pull secret. Written by the machine-config-operator
	ImageRegistryAuthFile = "/var/lib/kubelet/config.json"
	KubeconfigFile        = "/etc/kubernetes/static-pod-resources/kube-apiserver-certs/secrets/node-kubeconfigs/lb-ext.kubeconfig"

	RecertImageEnvKey      = "RELATED_IMAGE_RECERT_IMAGE"
	DefaultRecertImage     = "quay.io/edge-infrastructure/recert:v0"
	EtcdStaticPodFile      = "/etc/kubernetes/manifests/etcd-pod.yaml"
	EtcdStaticPodContainer = "etcd"
	EtcdDefaultEndpoint    = "localhost:2379"

	OvnNodeCerts = "/var/lib/ovn-ic/etc/ovnkube-node-certs"
	MultusCerts  = "/etc/cni/multus/certs"

	InstallationConfigurationFilesDir = "/usr/local/installation_configuration_files"
	OptOpenshift                      = "/opt/openshift"
	SeedDataDir                       = "/var/seed_data"
	KubeconfigCryptoDir               = "kubeconfig-crypto"
	ClusterConfigDir                  = "cluster-configuration"
	SeedClusterInfoFileName           = "manifest.json"
	SeedReconfigurationFileName       = "manifest.json"
	ManifestsDir                      = "manifests"
	ExtraManifestsDir                 = "extra-manifests"
	EtcdContainerName                 = "recert_etcd"
	LvmConfigDir                      = "lvm-configuration"
	LvmDevicesPath                    = "/etc/lvm/devices/system.devices"
	CABundleFilePath                  = "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem"

	LCAConfigDir                                    = "/var/lib/lca"
	IBUAutoRollbackConfigFile                       = LCAConfigDir + "/autorollback_config.json"
	IBUAutoRollbackInitMonitorTimeoutDefaultSeconds = 1800
	IBUInitMonitorService                           = "lca-init-monitor.service"
	IBUInitMonitorServiceFile                       = "/etc/systemd/system/" + IBUInitMonitorService

	LcaNamespace = "openshift-lifecycle-agent"
	Host         = "/host"

	CsvDeploymentName      = "cluster-version-operator"
	CsvDeploymentNamespace = "openshift-cluster-version"
	// InstallConfigCM cm name
	InstallConfigCM = "cluster-config-v1"
	// InstallConfigCMNamespace cm namespace
	InstallConfigCMNamespace = "kube-system"
	OpenshiftInfraCRName     = "cluster"

	// Env var to configure auto rollback for post-reboot config failure
	IBUPostRebootConfigAutoRollbackOnFailureEnv = "LCA_IBU_AUTO_ROLLBACK_ON_CONFIG_FAILURE"

	// Bump this every time the seed format changes in a backwards incompatible way
	SeedFormatVersion  = 3
	SeedFormatOCILabel = "com.openshift.lifecycle-agent.seed_format_version"

	PullSecretName           = "pull-secret"
	PullSecretEmptyData      = "{\"auths\":{\"registry.connect.redhat.com\":{\"username\":\"empty\",\"password\":\"empty\",\"auth\":\"ZW1wdHk6ZW1wdHk=\",\"email\":\"\"}}}" //nolint:gosec
	OpenshiftConfigNamespace = "openshift-config"

	NMConnectionFolder = "/etc/NetworkManager/system-connections"
	NetworkDir         = "network-configuration"
)

// CertPrefixes is the list of certificate prefixes to be backed up
// before creating the seed image
var CertPrefixes = []string{
	"loadbalancer-serving-signer",
	"localhost-serving-signer",
	"service-network-serving-signer",
}
