package recert

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/clusterinfo"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

const RecertConfigFile = "recert_config.json"

var staticDirs = []string{"/kubelet", "/kubernetes", "/machine-config-daemon"}

type RecertConfig struct {
	DryRun            bool     `json:"dry_run,omitempty"`
	ExtendExpiration  bool     `json:"extend_expiration,omitempty"`
	ForceExpire       bool     `json:"force_expire,omitempty"`
	EtcdEndpoint      string   `json:"etcd_endpoint,omitempty"`
	ClusterRename     string   `json:"cluster_rename,omitempty"`
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
func CreateRecertConfigFile(clusterInfo, seedClusterInfo *clusterinfo.ClusterInfo, certsDir, recertConfigFolder string) error {
	config := createBasicEmptyRecertConfig()
	config.ClusterRename = fmt.Sprintf("%s:%s", clusterInfo.ClusterName, clusterInfo.Domain)
	config.SummaryFile = "/var/tmp/recert-summary.yaml"
	seedFullDomain := fmt.Sprintf("%s.%s", seedClusterInfo.ClusterName, seedClusterInfo.Domain)
	clusterFullDomain := fmt.Sprintf("%s.%s", clusterInfo.ClusterName, clusterInfo.Domain)
	config.ExtendExpiration = true
	config.CNSanReplaceRules = []string{
		fmt.Sprintf("%s,%s", seedClusterInfo.MasterIP, clusterInfo.MasterIP),
		fmt.Sprintf("api.%s,api.%s", seedFullDomain, clusterFullDomain),
		fmt.Sprintf("api-int.%s,api-int.%s", seedFullDomain, clusterFullDomain),
		fmt.Sprintf("*.apps.%s,*.apps.%s", seedFullDomain, clusterFullDomain),
	}

	if _, err := os.Stat(certsDir); err != nil {
		return fmt.Errorf("failed to find certs directory %s, err: %w", certsDir, err)
	}

	ingressFile, ingressCN, err := getIngressCNAndFile(certsDir)
	if err != nil {
		return err
	}
	config.UseKeyRules = []string{
		fmt.Sprintf("kube-apiserver-lb-signer %s/loadbalancer-serving-signer.key", certsDir),
		fmt.Sprintf("kube-apiserver-localhost-signer %s/localhost-serving-signer.key", certsDir),
		fmt.Sprintf("kube-apiserver-service-network-signer %s/service-network-serving-signer.key", certsDir),
		fmt.Sprintf("%s %s/%s", ingressCN, certsDir, ingressFile),
	}
	config.UseCertRules = []string{filepath.Join(certsDir, "admin-kubeconfig-client-ca.crt")}

	return utils.WriteToFile(config, filepath.Join(recertConfigFolder, RecertConfigFile))
}

func CreateRecertConfigFileForSeedCreation(path string) error {
	config := createBasicEmptyRecertConfig()
	config.SummaryFileClean = "/kubernetes/recert-seed-summary.yaml"
	config.ForceExpire = true
	return utils.WriteToFile(config, path)
}

func createBasicEmptyRecertConfig() RecertConfig {
	return RecertConfig{
		DryRun:       false,
		EtcdEndpoint: "localhost:2379",
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
