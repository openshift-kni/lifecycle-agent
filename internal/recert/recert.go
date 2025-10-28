package recert

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedclusterinfo"
	"github.com/openshift-kni/lifecycle-agent/utils"
	"golang.org/x/crypto/bcrypt"
)

const (
	RecertConfigFile = "recert_config.json"
	SummaryFile      = "/var/tmp/recert-summary.yaml"
)

const (
	EtcMount                    = "/host-etc"
	EtcSSHMount                 = "/ssh"
	EtcKubernetesMount          = "/kubernetes"
	VarLibKubeletMount          = "/kubelet"
	EtcMachineConfigDaemonMount = "/machine-config-daemon"
	EtcPKIMount                 = "/pki"
)

var (
	// we don't want pki to go through recertification, only cluster customization (additional trust bundle appears in /pki)
	cryptoDirs               = []string{VarLibKubeletMount, EtcKubernetesMount, EtcMachineConfigDaemonMount}
	clusterCustomizationDirs = []string{VarLibKubeletMount, EtcKubernetesMount, EtcMachineConfigDaemonMount}

	cryptoFiles               = []string{fmt.Sprintf("%s/mcs-machine-config-content.json", EtcMount)}
	clusterCustomizationFiles = []string{fmt.Sprintf("%s/mcs-machine-config-content.json", EtcMount), fmt.Sprintf("%s/mco/proxy.env", EtcMount), fmt.Sprintf("%s/chrony.conf", EtcMount)}
)

type RecertConfig struct {
	DryRun               bool     `json:"dry_run,omitempty"`
	ExtendExpiration     bool     `json:"extend_expiration,omitempty"`
	ForceExpire          bool     `json:"force_expire,omitempty"`
	EtcdEndpoint         string   `json:"etcd_endpoint,omitempty"`
	ClusterRename        string   `json:"cluster_rename,omitempty"`
	Hostname             string   `json:"hostname,omitempty"`
	IP                   []string `json:"ip,omitempty"`
	Proxy                string   `json:"proxy,omitempty"`
	InstallConfig        string   `json:"install_config,omitempty"`
	UserCaBundle         string   `json:"user_ca_bundle,omitempty"`
	ProxyTrustedCaBundle string   `json:"proxy_trusted_ca_bundle,omitempty"`

	// We intentionally don't omitEmpty this field because an empty string here
	// means "delete the kubeadmin password secret" while a complete omission
	// of the field means "don't touch the secret". We never want the latter,
	// we either want to delete the secret or update it, never leave it as is.
	KubeadminPasswordHash string `json:"kubeadmin_password_hash"`
	// WARNING: You probably don't want use `SummaryFile`! This will leak
	// private keys and tokens!
	SummaryFile               string   `json:"summary_file,omitempty"`
	SummaryFileClean          string   `json:"summary_file_clean,omitempty"`
	CryptoDirs                []string `json:"crypto_dirs,omitempty"`
	CryptoFiles               []string `json:"crypto_files,omitempty"`
	ClusterCustomizationDirs  []string `json:"cluster_customization_dirs,omitempty"`
	ClusterCustomizationFiles []string `json:"cluster_customization_files,omitempty"`
	CNSanReplaceRules         []string `json:"cn_san_replace_rules,omitempty"`
	UseKeyRules               []string `json:"use_key_rules,omitempty"`
	UseCertRules              []string `json:"use_cert_rules,omitempty"`
	PullSecret                string   `json:"pull_secret,omitempty"`
	ChronyConfig              string   `json:"chrony_config,omitempty"`
	RegenerateServerSSHKeys   string   `json:"regenerate_server_ssh_keys,omitempty"`
	MachineNetworkCidr        []string `json:"machine_network_cidr,omitempty"`
	EtcdDefrag                bool     `json:"etcd_defrag,omitempty"`
}

func FormatRecertProxyFromSeedReconfigProxy(proxy, statusProxy *seedreconfig.Proxy) string {
	if proxy == nil || statusProxy == nil {
		// Both must be set, anything else is invalid
		return ""
	}
	return fmt.Sprintf("%s|%s|%s|%s|%s|%s",
		proxy.HTTPProxy, proxy.HTTPSProxy, proxy.NoProxy,
		statusProxy.HTTPProxy, statusProxy.HTTPSProxy, statusProxy.NoProxy,
	)
}

func SetRecertTrustedCaBundleFromSeedReconfigAdditionaTrustBundle(recertConfig *RecertConfig, additionalTrustBundle seedreconfig.AdditionalTrustBundle) error {
	if additionalTrustBundle.UserCaBundle != "" {
		recertConfig.UserCaBundle = additionalTrustBundle.UserCaBundle
	}

	if (additionalTrustBundle.ProxyConfigmapName != "" && additionalTrustBundle.ProxyConfigmapBundle == "") ||
		(additionalTrustBundle.ProxyConfigmapName == "" && additionalTrustBundle.ProxyConfigmapBundle != "") {
		return fmt.Errorf("both or neither of proxy configmap bundle and proxy configmap name must be set")
	}

	switch additionalTrustBundle.ProxyConfigmapName {
	case common.ClusterAdditionalTrustBundleName:
		recertConfig.ProxyTrustedCaBundle = fmt.Sprintf("%s:", common.ClusterAdditionalTrustBundleName)
	case "":
		recertConfig.ProxyTrustedCaBundle = ""
	default:
		recertConfig.ProxyTrustedCaBundle = fmt.Sprintf("%s:%s", additionalTrustBundle.ProxyConfigmapName, additionalTrustBundle.ProxyConfigmapBundle)
	}

	return nil
}

// CreateRecertConfigFile function to create recert config file
// those params will be provided to an installation script after reboot
// that will run recert command with them
func CreateRecertConfigFile(seedReconfig *seedreconfig.SeedReconfiguration, seedClusterInfo *seedclusterinfo.SeedClusterInfo, cryptoDir, recertConfigFolder string) error {
	config := createBaseRecertConfig()

	config.ClusterRename = fmt.Sprintf("%s:%s", seedReconfig.ClusterName, seedReconfig.BaseDomain)
	if seedReconfig.InfraID != "" {
		config.ClusterRename = fmt.Sprintf("%s:%s", config.ClusterRename, seedReconfig.InfraID)
	}

	if seedReconfig.Hostname != seedClusterInfo.SNOHostname {
		config.Hostname = seedReconfig.Hostname
	}

	ipsChanged := !slices.Equal(seedClusterInfo.NodeIPs, seedReconfig.NodeIPs)
	if ipsChanged && len(seedReconfig.NodeIPs) > 0 {
		config.IP = seedReconfig.NodeIPs
	}

	config.Proxy = FormatRecertProxyFromSeedReconfigProxy(seedReconfig.Proxy, seedReconfig.StatusProxy)

	if err := SetRecertTrustedCaBundleFromSeedReconfigAdditionaTrustBundle(&config, seedReconfig.AdditionalTrustBundle); err != nil {
		return fmt.Errorf("failed to set recert trusted ca bundle from seed reconfig additional trust bundle: %w", err)
	}

	config.InstallConfig = seedReconfig.InstallConfig

	config.SummaryFileClean = SummaryFile
	seedFullDomain := fmt.Sprintf("%s.%s", seedClusterInfo.ClusterName, seedClusterInfo.BaseDomain)
	clusterFullDomain := fmt.Sprintf("%s.%s", seedReconfig.ClusterName, seedReconfig.BaseDomain)
	config.ExtendExpiration = true
	config.KubeadminPasswordHash = seedReconfig.KubeadminPasswordHash
	config.PullSecret = seedReconfig.PullSecret
	if seedReconfig.ChronyConfig != "" {
		config.ChronyConfig = seedReconfig.ChronyConfig
	}

	if len(seedReconfig.ServerSSHKeys) == 0 {
		// IBI case, simply ask recert to regenerate fresh SSH server keys. In
		// IBU ServerSSHKeys is non-empty and LCA will later write those keys
		// (from the original cluster) to the SSH directory on its own without
		// recert's help.
		config.RegenerateServerSSHKeys = EtcSSHMount
	}

	if _, err := os.Stat(cryptoDir); err == nil {
		ingressKeyFile, err := getIngressKeyPath(cryptoDir)
		if err != nil {
			return err
		}
		config.UseKeyRules = []string{
			fmt.Sprintf("kube-apiserver-lb-signer %s/loadbalancer-serving-signer.key", cryptoDir),
			fmt.Sprintf("kube-apiserver-localhost-signer %s/localhost-serving-signer.key", cryptoDir),
			fmt.Sprintf("kube-apiserver-service-network-signer %s/service-network-serving-signer.key", cryptoDir),
			fmt.Sprintf("%s %s/%s", seedClusterInfo.IngressCertificateCN, cryptoDir, ingressKeyFile),
		}
		config.UseCertRules = []string{filepath.Join(cryptoDir, "admin-kubeconfig-client-ca.crt")}
	}

	config.CNSanReplaceRules = []string{
		fmt.Sprintf("system:node:%s,system:node:%s", seedClusterInfo.SNOHostname, seedReconfig.Hostname),
		fmt.Sprintf("%s,%s", seedClusterInfo.SNOHostname, seedReconfig.Hostname),
		fmt.Sprintf("api.%s,api.%s", seedFullDomain, clusterFullDomain),
		fmt.Sprintf("api-int.%s,api-int.%s", seedFullDomain, clusterFullDomain),
		fmt.Sprintf("*.apps.%s,*.apps.%s", seedFullDomain, clusterFullDomain),
	}

	if len(seedClusterInfo.NodeIPs) == len(seedReconfig.NodeIPs) {
		for i := range seedClusterInfo.NodeIPs {
			config.CNSanReplaceRules = append(
				config.CNSanReplaceRules, fmt.Sprintf("%s,%s", seedClusterInfo.NodeIPs[i], seedReconfig.NodeIPs[i]),
			)
		}
	}

	if seedReconfig.KubeconfigCryptoRetention.IngresssCrypto.IngressCertificateCN != "" {
		config.CNSanReplaceRules = append(config.CNSanReplaceRules,
			fmt.Sprintf("%s,%s", seedClusterInfo.IngressCertificateCN, seedReconfig.KubeconfigCryptoRetention.IngresssCrypto.IngressCertificateCN))
	}

	if !slices.Equal(seedReconfig.MachineNetworks, seedClusterInfo.MachineNetworks) {
		config.MachineNetworkCidr = seedReconfig.MachineNetworks
	}

	p := filepath.Join(recertConfigFolder, RecertConfigFile)
	if err := utils.MarshalToFile(config, p); err != nil {
		return fmt.Errorf("failed to marshal recert config file to %s: %w", p, err)
	}

	return nil
}

func CreateRecertConfigFileForSeedCreation(path string, withPassword bool) error {
	config := createBaseRecertConfig()
	config.SummaryFileClean = "/kubernetes/recert-seed-creation-summary.yaml"
	config.ForceExpire = true
	config.EtcdDefrag = true

	config.KubeadminPasswordHash = ""
	if withPassword {
		bytes, err := generateDisposablePasswordHash()
		if err != nil {
			return fmt.Errorf("failed to generate password hash: %w", err)
		}
		config.KubeadminPasswordHash = string(bytes)
	}

	if err := utils.MarshalToFile(config, path); err != nil {
		return fmt.Errorf("failed create recert config file for seed creation in %s: %w", path, err)
	}

	return nil
}

// generateDisposablePasswordHash generates a random password hash from a ridiculously
// long length password that is never meant to be known or used by anyone, but
// only to be used as a placeholder in the seed. It will be replaced or deleted
// during seed reconfiguration.
func generateDisposablePasswordHash() ([]byte, error) {
	bcryptLargestSupportedLength := 72
	token := make([]byte, bcryptLargestSupportedLength)
	_, err := rand.Read(token)
	if err != nil {
		return nil, fmt.Errorf("failed to generate random bytes for password: %w", err)
	}
	bytes, err := bcrypt.GenerateFromPassword(token, bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("failed to generate password hash: %w", err)
	}
	return bytes, nil
}

func CreateRecertConfigFileForSeedRestoration(path, originalPasswordHash string) error {
	config := createBaseRecertConfig()
	config.SummaryFileClean = "/kubernetes/recert-seed-restoration-summary.yaml"
	config.ExtendExpiration = true
	config.UseKeyRules = []string{
		fmt.Sprintf("kube-apiserver-lb-signer %s/loadbalancer-serving-signer.key", common.BackupCertsDir),
		fmt.Sprintf("kube-apiserver-localhost-signer %s/localhost-serving-signer.key", common.BackupCertsDir),
		fmt.Sprintf("kube-apiserver-service-network-signer %s/service-network-serving-signer.key", common.BackupCertsDir),
		fmt.Sprintf("ingresskey-ingress-operator %s/ingresskey-ingress-operator.key", common.BackupCertsDir),
	}
	config.UseCertRules = []string{filepath.Join(common.BackupCertsDir, "admin-kubeconfig-client-ca.crt")}
	config.KubeadminPasswordHash = originalPasswordHash

	if err := utils.MarshalToFile(config, path); err != nil {
		return fmt.Errorf("failed to marshal recert config file for seed restoration: %w", err)
	}
	return nil
}

func createBaseRecertConfig() RecertConfig {
	return RecertConfig{
		DryRun:                    false,
		EtcdEndpoint:              common.EtcdDefaultEndpoint,
		CryptoDirs:                cryptoDirs,
		CryptoFiles:               cryptoFiles,
		ClusterCustomizationDirs:  clusterCustomizationDirs,
		ClusterCustomizationFiles: clusterCustomizationFiles,
	}
}

func getIngressKeyPath(certsFolder string) (string, error) {
	certsFiles, err := os.ReadDir(certsFolder)
	if err != nil {
		return "", fmt.Errorf("failed to list files in %s while searching for ingress cn, "+
			"err: %w", certsFolder, err)
	}

	for _, path := range certsFiles {
		if strings.HasPrefix(path.Name(), "ingresskey-") {
			return path.Name(), nil
		}
	}
	return "", fmt.Errorf("failed to find ingress key file")
}
