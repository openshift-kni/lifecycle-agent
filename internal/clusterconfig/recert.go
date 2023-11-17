package clusterconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/clusterinfo"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

const recertConfigFile = "recert_config.json"

// PodmanRecertArgs is the list of arguments to be used by podman when
// calling the recert tool
var PodmanRecertArgs = []string{
	"run", "--detach", "--rm", "--network=host", "--privileged", "--replace", "--authfile",
}

// ForceExpireAdditionalFlags are the flags to be passed to recert
// to force expiring of seed cluster certificates
var ForceExpireAdditionalFlags = []string{
	"--summary-file-clean", "/kubernetes/recert-seed-summary.yaml",
	"--force-expire",
}

type recertConfig struct {
	DryRun            bool     `json:"dry_run,omitempty"`
	ExtendExpiration  bool     `json:"extend_expiration,omitempty"`
	EtcdEndpoint      string   `json:"etcd_endpoint,omitempty"`
	ClusterRename     string   `json:"cluster_rename,omitempty"`
	SummaryFile       string   `json:"summary_file,omitempty"`
	StaticDirs        []string `json:"static_dirs,omitempty"`
	StaticFiles       []string `json:"static_files,omitempty"`
	CNSanReplaceRules []string `json:"cn_san_replace_rules,omitempty"`
	UseKey            []string `json:"use_key,omitempty"`
	UseCert           string   `json:"use_cert,omitempty"`
}

// CreateRecertConfigFile function to create recert config file
// those params will be provided to an installation script after reboot
// that will run recert command with them
func CreateRecertConfigFile(clusterData *clusterinfo.ClusterInfo,
	clusterConfigPath string, ostreeDir string) error {
	// read seed manifest json
	seedManifestFile := filepath.Join(filepath.Dir(clusterConfigPath), seedManifest)
	seedClusterInfo := &clusterinfo.ClusterInfo{}
	err := utils.ReadYamlOrJSONFile(seedManifestFile, seedClusterInfo)
	if err != nil {
		return fmt.Errorf("failed to parse %s data, error: %w", seedManifestFile, err)
	}

	config := recertConfig{
		DryRun:           false,
		EtcdEndpoint:     "localhost:2379",
		ExtendExpiration: true,
		StaticDirs:       []string{"/kubelet", "/kubernetes", "/machine-config-daemon"},
		StaticFiles:      []string{"/host-etc/mcs-machine-config-content.json"},
		ClusterRename:    fmt.Sprintf("%s:%s", clusterData.ClusterName, clusterData.Domain),
		SummaryFile:      "/var/tmp/recert-summary.yaml",
	}

	seedFullDomain := fmt.Sprintf("%s.%s", seedClusterInfo.ClusterName, seedClusterInfo.Domain)
	clusterFullDomain := fmt.Sprintf("%s.%s", clusterData.ClusterName, clusterData.Domain)

	config.CNSanReplaceRules = []string{
		fmt.Sprintf("%s,%s", seedClusterInfo.MasterIP, clusterData.MasterIP),
		fmt.Sprintf("api.%s,api.%s", seedFullDomain, clusterFullDomain),
		fmt.Sprintf("api-int.%s,api-int.%s", seedFullDomain, clusterFullDomain),
		fmt.Sprintf("*.apps.%s,*.apps.%s", seedFullDomain, clusterFullDomain),
	}

	hostPathCertDir := filepath.Join(ostreeDir, certsDir)
	if _, err := os.Stat(hostPathCertDir); err != nil {
		return fmt.Errorf("failed to find certs directory %s, err: %w", hostPathCertDir, err)
	}

	ingressFile, ingressCN, err := getIngressCNAndFile(filepath.Join(ostreeDir, certsDir))
	if err != nil {
		return err
	}
	config.UseKey = []string{
		fmt.Sprintf("kube-apiserver-lb-signer %s/loadbalancer-serving-signer.key", certsDir),
		fmt.Sprintf("kube-apiserver-localhost-signer %s/localhost-serving-signer.key", certsDir),
		fmt.Sprintf("kube-apiserver-service-network-signer %s/service-network-serving-signer.key", certsDir),
		fmt.Sprintf("%s %s/%s", ingressCN, certsDir, ingressFile),
	}
	config.UseCert = filepath.Join(certsDir, "admin-kubeconfig-client-ca.crt")

	return utils.WriteToFile(config, filepath.Join(clusterConfigPath, recertConfigFile))
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
