package recert

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedclusterinfo"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

const (
	RecertConfigFile = "recert_config.json"
	SummaryFile      = "/var/tmp/recert-summary.yaml"
)

var staticDirs = []string{"/kubelet", "/kubernetes", "/machine-config-daemon"}

type RecertConfig struct {
	DryRun            bool     `json:"dry_run,omitempty"`
	ExtendExpiration  bool     `json:"extend_expiration,omitempty"`
	ForceExpire       bool     `json:"force_expire,omitempty"`
	EtcdEndpoint      string   `json:"etcd_endpoint,omitempty"`
	ClusterRename     string   `json:"cluster_rename,omitempty"`
	Hostname          string   `json:"hostname,omitempty"`
	SummaryFile       string   `json:"summary_file,omitempty"`
	SummaryFileClean  string   `json:"summary_file_clean,omitempty"`
	StaticDirs        []string `json:"static_dirs,omitempty"`
	StaticFiles       []string `json:"static_files,omitempty"`
	CNSanReplaceRules []string `json:"cn_san_replace_rules,omitempty"`
	UseKeyRules       []string `json:"use_key_rules,omitempty"`
	UseCertRules      []string `json:"use_cert_rules,omitempty"`
}

// CreateRecertConfigFile function to create recert config file
// those params will be provided to an installation script after reboot
// that will run recert command with them
func CreateRecertConfigFile(clusterInfo *seedreconfig.SeedReconfiguration, seedClusterInfo *seedclusterinfo.SeedClusterInfo, cryptoDir, recertConfigFolder string) error {
	config := createBasicEmptyRecertConfig()

	config.ClusterRename = fmt.Sprintf("%s:%s", clusterInfo.ClusterName, clusterInfo.BaseDomain)
	if clusterInfo.InfraID != "" {
		config.ClusterRename = fmt.Sprintf("%s:%s", config.ClusterRename, clusterInfo.InfraID)
	}

	if clusterInfo.Hostname != seedClusterInfo.SNOHostname {
		config.Hostname = clusterInfo.Hostname
	}

	config.SummaryFile = SummaryFile
	seedFullDomain := fmt.Sprintf("%s.%s", seedClusterInfo.ClusterName, seedClusterInfo.BaseDomain)
	clusterFullDomain := fmt.Sprintf("%s.%s", clusterInfo.ClusterName, clusterInfo.BaseDomain)
	config.ExtendExpiration = true
	config.CNSanReplaceRules = []string{
		fmt.Sprintf("system:node:%s,system:node:%s", seedClusterInfo.SNOHostname, clusterInfo.Hostname),
		fmt.Sprintf("%s,%s", seedClusterInfo.SNOHostname, clusterInfo.Hostname),
		fmt.Sprintf("%s,%s", seedClusterInfo.NodeIP, clusterInfo.NodeIP),
		fmt.Sprintf("api.%s,api.%s", seedFullDomain, clusterFullDomain),
		fmt.Sprintf("api-int.%s,api-int.%s", seedFullDomain, clusterFullDomain),
		fmt.Sprintf("*.apps.%s,*.apps.%s", seedFullDomain, clusterFullDomain),
	}

	if _, err := os.Stat(cryptoDir); err == nil {
		ingressFile, ingressCN, err := getIngressCNAndFile(cryptoDir)
		if err != nil {
			return err
		}
		config.UseKeyRules = []string{
			fmt.Sprintf("kube-apiserver-lb-signer %s/loadbalancer-serving-signer.key", cryptoDir),
			fmt.Sprintf("kube-apiserver-localhost-signer %s/localhost-serving-signer.key", cryptoDir),
			fmt.Sprintf("kube-apiserver-service-network-signer %s/service-network-serving-signer.key", cryptoDir),
			fmt.Sprintf("%s %s/%s", ingressCN, cryptoDir, ingressFile),
		}
		config.UseCertRules = []string{filepath.Join(cryptoDir, "admin-kubeconfig-client-ca.crt")}
	}

	return utils.MarshalToFile(config, filepath.Join(recertConfigFolder, RecertConfigFile))
}

func CreateRecertConfigFileForSeedCreation(path string) error {
	config := createBasicEmptyRecertConfig()
	config.SummaryFileClean = "/kubernetes/recert-seed-summary.yaml"
	config.ForceExpire = true
	return utils.MarshalToFile(config, path)
}

func CreateRecertConfigFileForSeedRestoration(path string) error {
	config := createBasicEmptyRecertConfig()
	config.SummaryFileClean = "/kubernetes/recert-seed-summary.yaml"
	config.ExtendExpiration = true
	config.UseKeyRules = []string{
		fmt.Sprintf("kube-apiserver-lb-signer %s/loadbalancer-serving-signer.key", common.BackupCertsDir),
		fmt.Sprintf("kube-apiserver-localhost-signer %s/localhost-serving-signer.key", common.BackupCertsDir),
		fmt.Sprintf("kube-apiserver-service-network-signer %s/service-network-serving-signer.key", common.BackupCertsDir),
		fmt.Sprintf("ingresskey-ingress-operator %s/ingresskey-ingress-operator.key", common.BackupCertsDir),
	}
	config.UseCertRules = []string{filepath.Join(common.BackupCertsDir, "admin-kubeconfig-client-ca.crt")}

	return utils.MarshalToFile(config, path)
}

func createBasicEmptyRecertConfig() RecertConfig {
	return RecertConfig{
		DryRun:       false,
		EtcdEndpoint: common.EtcdDefaultEndpoint,
		StaticDirs:   staticDirs,
		StaticFiles:  []string{"/host-etc/mcs-machine-config-content.json"},
	}
}

func getIngressCNAndFile(certsFolder string) (string, string, error) {
	certsFiles, err := os.ReadDir(certsFolder)
	if err != nil {
		return "", "", fmt.Errorf("failed to list files in %s while searching for ingress cn, "+
			"err: %w", certsFolder, err)
	}

	for _, path := range certsFiles {
		if strings.HasPrefix(path.Name(), "ingresskey-") {
			return path.Name(), strings.Replace(path.Name(), "ingresskey-", "", 1), nil
		}
	}
	return "", "", fmt.Errorf("failed to find ingress key file")
}
