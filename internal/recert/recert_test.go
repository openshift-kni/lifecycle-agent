package recert

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedclusterinfo"
	"github.com/stretchr/testify/assert"
)

func TestFormatRecertProxyFromSeedReconfigProxy(t *testing.T) {
	tests := []struct {
		name        string
		proxy       *seedreconfig.Proxy
		statusProxy *seedreconfig.Proxy
		expected    string
	}{
		{
			name:        "both nil proxies",
			proxy:       nil,
			statusProxy: nil,
			expected:    "",
		},
		{
			name:        "proxy nil, statusProxy non-nil",
			proxy:       nil,
			statusProxy: &seedreconfig.Proxy{HTTPProxy: "http://proxy:8080"},
			expected:    "",
		},
		{
			name:        "proxy non-nil, statusProxy nil",
			proxy:       &seedreconfig.Proxy{HTTPProxy: "http://proxy:8080"},
			statusProxy: nil,
			expected:    "",
		},
		{
			name: "both proxies configured",
			proxy: &seedreconfig.Proxy{
				HTTPProxy:  "http://proxy:8080",
				HTTPSProxy: "https://proxy:8080",
				NoProxy:    "localhost,127.0.0.1",
			},
			statusProxy: &seedreconfig.Proxy{
				HTTPProxy:  "http://status-proxy:8080",
				HTTPSProxy: "https://status-proxy:8080",
				NoProxy:    "status-localhost,127.0.0.1",
			},
			expected: "http://proxy:8080|https://proxy:8080|localhost,127.0.0.1|http://status-proxy:8080|https://status-proxy:8080|status-localhost,127.0.0.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatRecertProxyFromSeedReconfigProxy(tt.proxy, tt.statusProxy)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestSetRecertTrustedCaBundleFromSeedReconfigAdditionaTrustBundle(t *testing.T) {
	tests := []struct {
		name                  string
		additionalTrustBundle seedreconfig.AdditionalTrustBundle
		expectedError         bool
		expectedUserCaBundle  string
		expectedProxyBundle   string
	}{
		{
			name:                  "empty trust bundle",
			additionalTrustBundle: seedreconfig.AdditionalTrustBundle{},
			expectedError:         false,
			expectedUserCaBundle:  "",
			expectedProxyBundle:   "",
		},
		{
			name: "user ca bundle only",
			additionalTrustBundle: seedreconfig.AdditionalTrustBundle{
				UserCaBundle: "-----BEGIN CERTIFICATE-----\nuser-ca-bundle\n-----END CERTIFICATE-----",
			},
			expectedError:        false,
			expectedUserCaBundle: "-----BEGIN CERTIFICATE-----\nuser-ca-bundle\n-----END CERTIFICATE-----",
			expectedProxyBundle:  "",
		},
		{
			name: "error: proxy configmap name without bundle",
			additionalTrustBundle: seedreconfig.AdditionalTrustBundle{
				ProxyConfigmapName: "custom-proxy-ca",
			},
			expectedError: true,
		},
		{
			name: "error: proxy configmap bundle without name",
			additionalTrustBundle: seedreconfig.AdditionalTrustBundle{
				ProxyConfigmapBundle: "-----BEGIN CERTIFICATE-----\ncustom-ca\n-----END CERTIFICATE-----",
			},
			expectedError: true,
		},
		{
			name: "cluster trust bundle name sets name-only proxy bundle",
			additionalTrustBundle: seedreconfig.AdditionalTrustBundle{
				ProxyConfigmapName:   "user-ca-bundle",
				ProxyConfigmapBundle: "-----BEGIN CERTIFICATE-----\nbundle\n-----END CERTIFICATE-----",
			},
			expectedError:        false,
			expectedUserCaBundle: "",
			expectedProxyBundle:  "user-ca-bundle:",
		},
		{
			name: "custom configmap name sets name:bundle proxy bundle",
			additionalTrustBundle: seedreconfig.AdditionalTrustBundle{
				ProxyConfigmapName:   "custom-proxy-ca",
				ProxyConfigmapBundle: "custom-bundle-content",
			},
			expectedError:        false,
			expectedUserCaBundle: "",
			expectedProxyBundle:  "custom-proxy-ca:custom-bundle-content",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &RecertConfig{}
			err := SetRecertTrustedCaBundleFromSeedReconfigAdditionaTrustBundle(config, tt.additionalTrustBundle)

			if tt.expectedError {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, tt.expectedUserCaBundle, config.UserCaBundle)
			assert.Equal(t, tt.expectedProxyBundle, config.ProxyTrustedCaBundle)
		})
	}
}

func TestRecertConfig_DualStackFields(t *testing.T) {
	// Test that the RecertConfig struct properly handles dual-stack fields
	config := RecertConfig{
		IP:                 []string{"192.168.1.10", "2001:db8::10"},
		MachineNetworkCidr: []string{"192.168.1.0/24", "2001:db8::/64"},
		Hostname:           "test-node",
		ClusterRename:      "test-cluster:example.com",
	}

	// Verify the dual-stack specific fields
	assert.Equal(t, []string{"192.168.1.10", "2001:db8::10"}, config.IP)
	assert.Equal(t, []string{"192.168.1.0/24", "2001:db8::/64"}, config.MachineNetworkCidr)
	assert.Equal(t, "test-node", config.Hostname)
	assert.Equal(t, "test-cluster:example.com", config.ClusterRename)
}

func TestAppendCertManagerCryptoRules(t *testing.T) {
	tests := []struct {
		name              string
		setupDir          func(t *testing.T) string
		existingKeyRules  []string
		expectedRuleCount int
		validateRules     func(t *testing.T, rules []string)
	}{
		{
			name: "directory does not exist",
			setupDir: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "nonexistent")
			},
			expectedRuleCount: 0,
		},
		{
			name: "empty directory",
			setupDir: func(t *testing.T) string {
				dir := filepath.Join(t.TempDir(), "certmanager-crypto")
				assert.NoError(t, os.MkdirAll(dir, 0o700))
				return dir
			},
			expectedRuleCount: 0,
		},
		{
			name: "directory with key files",
			setupDir: func(t *testing.T) string {
				dir := filepath.Join(t.TempDir(), "certmanager-crypto")
				assert.NoError(t, os.MkdirAll(dir, 0o700))
				assert.NoError(t, os.WriteFile(filepath.Join(dir, "CN=my-cert.example.com__default_my-cert-tls.key"),
					[]byte("-----BEGIN EC PRIVATE KEY-----\ntest\n-----END EC PRIVATE KEY-----\n"), 0o600))
				assert.NoError(t, os.WriteFile(filepath.Join(dir, "CN=api.example.com__openshift-config_api-cert.key"),
					[]byte("-----BEGIN EC PRIVATE KEY-----\ntest2\n-----END EC PRIVATE KEY-----\n"), 0o600))
				return dir
			},
			expectedRuleCount: 2,
			validateRules: func(t *testing.T, rules []string) {
				// ReadDir returns alphabetical order: api... before my-cert...
				assert.Contains(t, rules[0], "api.example.com ")
				assert.Contains(t, rules[1], "my-cert.example.com ")
			},
		},
		{
			name: "non-key files and files without CN= prefix are ignored",
			setupDir: func(t *testing.T) string {
				dir := filepath.Join(t.TempDir(), "certmanager-crypto")
				assert.NoError(t, os.MkdirAll(dir, 0o700))
				assert.NoError(t, os.WriteFile(filepath.Join(dir, "CN=my-cert.example.com__default_my-cert-tls.key"),
					[]byte("key"), 0o600))
				assert.NoError(t, os.WriteFile(filepath.Join(dir, "default_my-cert-tls.crt"),
					[]byte("cert"), 0o600))
				assert.NoError(t, os.WriteFile(filepath.Join(dir, "readme.txt"),
					[]byte("readme"), 0o600))
				assert.NoError(t, os.WriteFile(filepath.Join(dir, "no-cn-prefix.key"),
					[]byte("key"), 0o600))
				return dir
			},
			expectedRuleCount: 1,
		},
		{
			name: "appends to existing rules",
			setupDir: func(t *testing.T) string {
				dir := filepath.Join(t.TempDir(), "certmanager-crypto")
				assert.NoError(t, os.MkdirAll(dir, 0o700))
				assert.NoError(t, os.WriteFile(filepath.Join(dir, "CN=my-cert.example.com__default_my-cert-tls.key"),
					[]byte("key"), 0o600))
				return dir
			},
			existingKeyRules:  []string{"kube-apiserver-lb-signer /path/loadbalancer-serving-signer.key"},
			expectedRuleCount: 2,
		},
		{
			name: "empty dir path",
			setupDir: func(t *testing.T) string {
				return ""
			},
			expectedRuleCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := tt.setupDir(t)
			config := &RecertConfig{}
			if tt.existingKeyRules != nil {
				config.UseKeyRules = tt.existingKeyRules
			}

			err := appendCertManagerCryptoRules(config, dir)
			assert.NoError(t, err)
			assert.Equal(t, tt.expectedRuleCount, len(config.UseKeyRules))
			if tt.validateRules != nil {
				tt.validateRules(t, config.UseKeyRules)
			}
		})
	}
}

func TestCreateBaseRecertConfig(t *testing.T) {
	// Test the base configuration creation
	config := createBaseRecertConfig()

	// Verify base configuration
	assert.False(t, config.DryRun)
	assert.Equal(t, "localhost:2379", config.EtcdEndpoint)
	assert.Equal(t, cryptoDirs, config.CryptoDirs)
	assert.Equal(t, cryptoFiles, config.CryptoFiles)
	assert.Equal(t, clusterCustomizationDirs, config.ClusterCustomizationDirs)
	assert.Equal(t, clusterCustomizationFiles, config.ClusterCustomizationFiles)
}

func TestGetIngressKeyPath(t *testing.T) {
	tests := []struct {
		name        string
		files       []string
		expected    string
		expectedErr string
	}{
		{
			name:     "finds ingress key among other files",
			files:    []string{"loadbalancer-serving-signer.key", "ingresskey-ingress-operator.key", "admin-kubeconfig-client-ca.crt"},
			expected: "ingresskey-ingress-operator.key",
		},
		{
			name:     "finds first ingresskey- prefixed file",
			files:    []string{"ingresskey-first.key", "ingresskey-second.key"},
			expected: "ingresskey-first.key",
		},
		{
			name:        "no matching files",
			files:       []string{"loadbalancer-serving-signer.key", "localhost-serving-signer.key"},
			expectedErr: "failed to find ingress key file",
		},
		{
			name:        "empty directory",
			files:       []string{},
			expectedErr: "failed to find ingress key file",
		},
		{
			name:        "near-miss name without dash",
			files:       []string{"ingresskey.key"},
			expectedErr: "failed to find ingress key file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tt.files {
				assert.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte("dummy"), 0o600))
			}

			result, err := getIngressKeyPath(dir)
			if tt.expectedErr != "" {
				assert.ErrorContains(t, err, tt.expectedErr)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestGetIngressKeyPath_NonexistentDir(t *testing.T) {
	_, err := getIngressKeyPath("/nonexistent/path")
	assert.ErrorContains(t, err, "failed to list files")
}

func setupCryptoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := []string{
		"loadbalancer-serving-signer.key",
		"localhost-serving-signer.key",
		"service-network-serving-signer.key",
		"ingresskey-ingress-operator.key",
		"admin-kubeconfig-client-ca.crt",
	}
	for _, f := range files {
		assert.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte("dummy-key"), 0o600))
	}
	return dir
}

func readRecertConfig(t *testing.T, configFolder string) RecertConfig {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(configFolder, RecertConfigFile))
	assert.NoError(t, err)
	var config RecertConfig
	assert.NoError(t, json.Unmarshal(data, &config))
	return config
}

func TestCreateRecertConfigFile(t *testing.T) {
	tests := []struct {
		name            string
		seedReconfig    *seedreconfig.SeedReconfiguration
		seedClusterInfo *seedclusterinfo.SeedClusterInfo
		hasCryptoDir    bool
		validate        func(t *testing.T, config RecertConfig)
	}{
		{
			name: "hostname changed",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName: "target-cluster",
				BaseDomain:  "target.example.com",
				Hostname:    "target-node",
				NodeIPs:     []string{"10.0.0.1"},
				PullSecret:  "pull-secret",
			},
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName:          "seed-cluster",
				BaseDomain:           "seed.example.com",
				SNOHostname:          "seed-node",
				NodeIPs:              []string{"10.0.0.1"},
				IngressCertificateCN: "ingress-operator@1234",
			},
			hasCryptoDir: false,
			validate: func(t *testing.T, config RecertConfig) {
				assert.Equal(t, "target-node", config.Hostname)
				assert.Equal(t, "target-cluster:target.example.com", config.ClusterRename)
				assert.Contains(t, config.CNSanReplaceRules, "system:node:seed-node,system:node:target-node")
				assert.Contains(t, config.CNSanReplaceRules, "seed-node,target-node")
				assert.Contains(t, config.CNSanReplaceRules, "api.seed-cluster.seed.example.com,api.target-cluster.target.example.com")
				assert.Contains(t, config.CNSanReplaceRules, "api-int.seed-cluster.seed.example.com,api-int.target-cluster.target.example.com")
				assert.Contains(t, config.CNSanReplaceRules, "*.apps.seed-cluster.seed.example.com,*.apps.target-cluster.target.example.com")
			},
		},
		{
			name: "hostname unchanged is omitted",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName: "cluster",
				BaseDomain:  "example.com",
				Hostname:    "same-host",
				NodeIPs:     []string{"10.0.0.1"},
			},
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName: "seed",
				BaseDomain:  "seed.example.com",
				SNOHostname: "same-host",
				NodeIPs:     []string{"10.0.0.1"},
			},
			hasCryptoDir: false,
			validate: func(t *testing.T, config RecertConfig) {
				assert.Empty(t, config.Hostname)
			},
		},
		{
			name: "IPs changed single stack",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName: "cluster",
				BaseDomain:  "example.com",
				Hostname:    "node",
				NodeIPs:     []string{"10.0.0.2"},
			},
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName: "seed",
				BaseDomain:  "seed.example.com",
				SNOHostname: "seed-node",
				NodeIPs:     []string{"10.0.0.1"},
			},
			hasCryptoDir: false,
			validate: func(t *testing.T, config RecertConfig) {
				assert.Equal(t, []string{"10.0.0.2"}, config.IP)
				assert.Contains(t, config.CNSanReplaceRules, "10.0.0.1,10.0.0.2")
			},
		},
		{
			name: "IPs changed dual stack",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName: "cluster",
				BaseDomain:  "example.com",
				Hostname:    "node",
				NodeIPs:     []string{"10.0.0.2", "2001:db8::2"},
			},
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName: "seed",
				BaseDomain:  "seed.example.com",
				SNOHostname: "seed-node",
				NodeIPs:     []string{"10.0.0.1", "2001:db8::1"},
			},
			hasCryptoDir: false,
			validate: func(t *testing.T, config RecertConfig) {
				assert.Equal(t, []string{"10.0.0.2", "2001:db8::2"}, config.IP)
				assert.Contains(t, config.CNSanReplaceRules, "10.0.0.1,10.0.0.2")
				assert.Contains(t, config.CNSanReplaceRules, "2001:db8::1,2001:db8::2")
			},
		},
		{
			name: "IPs unchanged is omitted",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName: "cluster",
				BaseDomain:  "example.com",
				Hostname:    "node",
				NodeIPs:     []string{"10.0.0.1"},
			},
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName: "seed",
				BaseDomain:  "seed.example.com",
				SNOHostname: "seed-node",
				NodeIPs:     []string{"10.0.0.1"},
			},
			hasCryptoDir: false,
			validate: func(t *testing.T, config RecertConfig) {
				assert.Nil(t, config.IP)
			},
		},
		{
			name: "IP count mismatch skips IP replacement rules",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName: "cluster",
				BaseDomain:  "example.com",
				Hostname:    "node",
				NodeIPs:     []string{"10.0.0.2", "2001:db8::2"},
			},
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName: "seed",
				BaseDomain:  "seed.example.com",
				SNOHostname: "seed-node",
				NodeIPs:     []string{"10.0.0.1"},
			},
			hasCryptoDir: false,
			validate: func(t *testing.T, config RecertConfig) {
				assert.Equal(t, []string{"10.0.0.2", "2001:db8::2"}, config.IP)
				for _, rule := range config.CNSanReplaceRules {
					assert.NotContains(t, rule, "10.0.0.1,")
				}
			},
		},
		{
			name: "InfraID present adds three-segment ClusterRename",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName: "cluster",
				BaseDomain:  "example.com",
				InfraID:     "cluster-abc12",
				Hostname:    "node",
				NodeIPs:     []string{"10.0.0.1"},
			},
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName: "seed",
				BaseDomain:  "seed.example.com",
				SNOHostname: "seed-node",
				NodeIPs:     []string{"10.0.0.1"},
			},
			hasCryptoDir: false,
			validate: func(t *testing.T, config RecertConfig) {
				assert.Equal(t, "cluster:example.com:cluster-abc12", config.ClusterRename)
			},
		},
		{
			name: "InfraID absent gives two-segment ClusterRename",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName: "cluster",
				BaseDomain:  "example.com",
				Hostname:    "node",
				NodeIPs:     []string{"10.0.0.1"},
			},
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName: "seed",
				BaseDomain:  "seed.example.com",
				SNOHostname: "seed-node",
				NodeIPs:     []string{"10.0.0.1"},
			},
			hasCryptoDir: false,
			validate: func(t *testing.T, config RecertConfig) {
				assert.Equal(t, "cluster:example.com", config.ClusterRename)
			},
		},
		{
			name: "crypto dir present populates UseKeyRules and UseCertRules",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName: "cluster",
				BaseDomain:  "example.com",
				Hostname:    "node",
				NodeIPs:     []string{"10.0.0.1"},
			},
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName:          "seed",
				BaseDomain:           "seed.example.com",
				SNOHostname:          "seed-node",
				NodeIPs:              []string{"10.0.0.1"},
				IngressCertificateCN: "ingress-operator@1234",
			},
			hasCryptoDir: true,
			validate: func(t *testing.T, config RecertConfig) {
				assert.Len(t, config.UseKeyRules, 4)
				assert.Contains(t, config.UseKeyRules[0], "kube-apiserver-lb-signer")
				assert.Contains(t, config.UseKeyRules[1], "kube-apiserver-localhost-signer")
				assert.Contains(t, config.UseKeyRules[2], "kube-apiserver-service-network-signer")
				assert.Contains(t, config.UseKeyRules[3], "ingress-operator@1234")
				assert.Contains(t, config.UseKeyRules[3], "ingresskey-ingress-operator.key")
				assert.Len(t, config.UseCertRules, 1)
				assert.Contains(t, config.UseCertRules[0], "admin-kubeconfig-client-ca.crt")
			},
		},
		{
			name: "crypto dir absent leaves UseKeyRules and UseCertRules nil",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName: "cluster",
				BaseDomain:  "example.com",
				Hostname:    "node",
				NodeIPs:     []string{"10.0.0.1"},
			},
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName: "seed",
				BaseDomain:  "seed.example.com",
				SNOHostname: "seed-node",
				NodeIPs:     []string{"10.0.0.1"},
			},
			hasCryptoDir: false,
			validate: func(t *testing.T, config RecertConfig) {
				assert.Nil(t, config.UseKeyRules)
				assert.Nil(t, config.UseCertRules)
			},
		},
		{
			name: "ingress CN retention appends CN replace rule",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName: "cluster",
				BaseDomain:  "example.com",
				Hostname:    "node",
				NodeIPs:     []string{"10.0.0.1"},
				KubeconfigCryptoRetention: seedreconfig.KubeConfigCryptoRetention{
					IngresssCrypto: seedreconfig.IngresssCrypto{
						IngressCertificateCN: "ingress-operator@9999",
					},
				},
			},
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName:          "seed",
				BaseDomain:           "seed.example.com",
				SNOHostname:          "seed-node",
				NodeIPs:              []string{"10.0.0.1"},
				IngressCertificateCN: "ingress-operator@1234",
			},
			hasCryptoDir: false,
			validate: func(t *testing.T, config RecertConfig) {
				assert.Contains(t, config.CNSanReplaceRules, "ingress-operator@1234,ingress-operator@9999")
			},
		},
		{
			name: "machine networks changed",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName:     "cluster",
				BaseDomain:      "example.com",
				Hostname:        "node",
				NodeIPs:         []string{"10.0.0.1"},
				MachineNetworks: []string{"10.0.0.0/24"},
			},
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName:     "seed",
				BaseDomain:      "seed.example.com",
				SNOHostname:     "seed-node",
				NodeIPs:         []string{"10.0.0.1"},
				MachineNetworks: []string{"192.168.1.0/24"},
			},
			hasCryptoDir: false,
			validate: func(t *testing.T, config RecertConfig) {
				assert.Equal(t, []string{"10.0.0.0/24"}, config.MachineNetworkCidr)
			},
		},
		{
			name: "machine networks unchanged is nil",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName:     "cluster",
				BaseDomain:      "example.com",
				Hostname:        "node",
				NodeIPs:         []string{"10.0.0.1"},
				MachineNetworks: []string{"10.0.0.0/24"},
			},
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName:     "seed",
				BaseDomain:      "seed.example.com",
				SNOHostname:     "seed-node",
				NodeIPs:         []string{"10.0.0.1"},
				MachineNetworks: []string{"10.0.0.0/24"},
			},
			hasCryptoDir: false,
			validate: func(t *testing.T, config RecertConfig) {
				assert.Nil(t, config.MachineNetworkCidr)
			},
		},
		{
			name: "IBI case with empty ServerSSHKeys sets RegenerateServerSSHKeys",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName:   "cluster",
				BaseDomain:    "example.com",
				Hostname:      "node",
				NodeIPs:       []string{"10.0.0.1"},
				ServerSSHKeys: nil,
			},
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName: "seed",
				BaseDomain:  "seed.example.com",
				SNOHostname: "seed-node",
				NodeIPs:     []string{"10.0.0.1"},
			},
			hasCryptoDir: false,
			validate: func(t *testing.T, config RecertConfig) {
				assert.Equal(t, EtcSSHMount, config.RegenerateServerSSHKeys)
			},
		},
		{
			name: "IBU case with non-empty ServerSSHKeys leaves RegenerateServerSSHKeys empty",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName: "cluster",
				BaseDomain:  "example.com",
				Hostname:    "node",
				NodeIPs:     []string{"10.0.0.1"},
				ServerSSHKeys: []seedreconfig.ServerSSHKey{
					{FileName: "ssh_host_rsa_key", FileContent: "key-content"},
				},
			},
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName: "seed",
				BaseDomain:  "seed.example.com",
				SNOHostname: "seed-node",
				NodeIPs:     []string{"10.0.0.1"},
			},
			hasCryptoDir: false,
			validate: func(t *testing.T, config RecertConfig) {
				assert.Empty(t, config.RegenerateServerSSHKeys)
			},
		},
		{
			name: "common fields are set correctly",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName:           "cluster",
				BaseDomain:            "example.com",
				Hostname:              "node",
				NodeIPs:               []string{"10.0.0.1"},
				KubeadminPasswordHash: "$2a$10$hash",
				PullSecret:            `{"auths":{}}`,
				ChronyConfig:          "server ntp.example.com iburst",
			},
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName: "seed",
				BaseDomain:  "seed.example.com",
				SNOHostname: "seed-node",
				NodeIPs:     []string{"10.0.0.1"},
			},
			hasCryptoDir: false,
			validate: func(t *testing.T, config RecertConfig) {
				assert.True(t, config.ExtendExpiration)
				assert.Equal(t, "$2a$10$hash", config.KubeadminPasswordHash)
				assert.Equal(t, `{"auths":{}}`, config.PullSecret)
				assert.Equal(t, "server ntp.example.com iburst", config.ChronyConfig)
				assert.Equal(t, SummaryFile, config.SummaryFileClean)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cryptoDir := "/nonexistent/crypto"
			if tt.hasCryptoDir {
				cryptoDir = setupCryptoDir(t)
			}
			configFolder := t.TempDir()

			err := CreateRecertConfigFile(tt.seedReconfig, tt.seedClusterInfo, cryptoDir, configFolder, "")
			assert.NoError(t, err)

			config := readRecertConfig(t, configFolder)
			tt.validate(t, config)
		})
	}
}

func TestCreateRecertConfigFile_TrustBundleError(t *testing.T) {
	seedReconfig := &seedreconfig.SeedReconfiguration{
		ClusterName: "cluster",
		BaseDomain:  "example.com",
		Hostname:    "node",
		NodeIPs:     []string{"10.0.0.1"},
		AdditionalTrustBundle: seedreconfig.AdditionalTrustBundle{
			ProxyConfigmapName: "has-name-but-no-bundle",
		},
	}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{
		ClusterName: "seed",
		BaseDomain:  "seed.example.com",
		SNOHostname: "seed-node",
		NodeIPs:     []string{"10.0.0.1"},
	}

	err := CreateRecertConfigFile(seedReconfig, seedClusterInfo, "/nonexistent", t.TempDir(), "")
	assert.ErrorContains(t, err, "failed to set recert trusted ca bundle")
}

func TestCreateRecertConfigFile_MissingIngressKeyInCryptoDir(t *testing.T) {
	cryptoDir := t.TempDir()
	assert.NoError(t, os.WriteFile(filepath.Join(cryptoDir, "loadbalancer-serving-signer.key"), []byte("key"), 0o600))

	seedReconfig := &seedreconfig.SeedReconfiguration{
		ClusterName: "cluster",
		BaseDomain:  "example.com",
		Hostname:    "node",
		NodeIPs:     []string{"10.0.0.1"},
	}
	seedClusterInfo := &seedclusterinfo.SeedClusterInfo{
		ClusterName:          "seed",
		BaseDomain:           "seed.example.com",
		SNOHostname:          "seed-node",
		NodeIPs:              []string{"10.0.0.1"},
		IngressCertificateCN: "ingress-operator@1234",
	}

	err := CreateRecertConfigFile(seedReconfig, seedClusterInfo, cryptoDir, t.TempDir(), "")
	assert.ErrorContains(t, err, "failed to find ingress key file")
}

func TestCreateRecertConfigFileForIPConfig(t *testing.T) {
	tests := []struct {
		name               string
		oldIPs             []string
		newIPs             []string
		newMachineNetworks []string
		installConfig      string
		hasCryptoDir       bool
		ingressCN          string
		expectedErr        string
		validate           func(t *testing.T, config *RecertConfig)
	}{
		{
			name:               "single stack IP change",
			oldIPs:             []string{"10.0.0.1"},
			newIPs:             []string{"10.0.0.2"},
			newMachineNetworks: []string{"10.0.0.0/24"},
			installConfig:      "install-config-yaml",
			ingressCN:          "ingress-operator@1234",
			hasCryptoDir:       false,
			validate: func(t *testing.T, config *RecertConfig) {
				assert.Equal(t, []string{"10.0.0.2"}, config.IP)
				assert.Contains(t, config.CNSanReplaceRules, "10.0.0.1,10.0.0.2")
				assert.Equal(t, []string{"10.0.0.0/24"}, config.MachineNetworkCidr)
				assert.False(t, config.ExtendExpiration)
				assert.True(t, config.IPChangeOnly)
			},
		},
		{
			name:               "dual stack IP change",
			oldIPs:             []string{"10.0.0.1", "2001:db8::1"},
			newIPs:             []string{"10.0.0.2", "2001:db8::2"},
			newMachineNetworks: []string{"10.0.0.0/24", "2001:db8::/64"},
			ingressCN:          "ingress-operator@1234",
			hasCryptoDir:       false,
			validate: func(t *testing.T, config *RecertConfig) {
				assert.Equal(t, []string{"10.0.0.2", "2001:db8::2"}, config.IP)
				assert.Contains(t, config.CNSanReplaceRules, "10.0.0.1,10.0.0.2")
				assert.Contains(t, config.CNSanReplaceRules, "2001:db8::1,2001:db8::2")
			},
		},
		{
			name:        "mismatched IP counts returns error",
			oldIPs:      []string{"10.0.0.1"},
			newIPs:      []string{"10.0.0.2", "2001:db8::2"},
			ingressCN:   "ingress-operator@1234",
			expectedErr: "oldIPs and newIPs must have the same length",
		},
		{
			name:         "with crypto dir populates UseKeyRules",
			oldIPs:       []string{"10.0.0.1"},
			newIPs:       []string{"10.0.0.2"},
			ingressCN:    "ingress-operator@1234",
			hasCryptoDir: true,
			validate: func(t *testing.T, config *RecertConfig) {
				assert.Len(t, config.UseKeyRules, 4)
				assert.Contains(t, config.UseKeyRules[3], "ingress-operator@1234")
				assert.Len(t, config.UseCertRules, 1)
			},
		},
		{
			name:         "without crypto dir leaves key rules nil",
			oldIPs:       []string{"10.0.0.1"},
			newIPs:       []string{"10.0.0.2"},
			ingressCN:    "ingress-operator@1234",
			hasCryptoDir: false,
			validate: func(t *testing.T, config *RecertConfig) {
				assert.Nil(t, config.UseKeyRules)
				assert.Nil(t, config.UseCertRules)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cryptoDir := "/nonexistent/crypto"
			if tt.hasCryptoDir {
				cryptoDir = setupCryptoDir(t)
			}

			config, err := CreateRecertConfigFileForIPConfig(
				tt.oldIPs, tt.newIPs, tt.newMachineNetworks,
				tt.installConfig, cryptoDir, tt.ingressCN,
			)
			if tt.expectedErr != "" {
				assert.ErrorContains(t, err, tt.expectedErr)
				return
			}
			assert.NoError(t, err)
			tt.validate(t, config)
		})
	}
}

func TestCreateRecertConfigFileForIPConfig_MissingIngressKey(t *testing.T) {
	cryptoDir := t.TempDir()
	assert.NoError(t, os.WriteFile(filepath.Join(cryptoDir, "some-other-file.key"), []byte("key"), 0o600))

	_, err := CreateRecertConfigFileForIPConfig(
		[]string{"10.0.0.1"}, []string{"10.0.0.2"}, nil,
		"", cryptoDir, "ingress-operator@1234",
	)
	assert.ErrorContains(t, err, "failed to find ingress key file")
}

func TestCreateRecertConfigFileForSeedCreation(t *testing.T) {
	t.Run("with password generates bcrypt hash", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), RecertConfigFile)
		err := CreateRecertConfigFileForSeedCreation(configPath, true)
		assert.NoError(t, err)

		data, err := os.ReadFile(configPath)
		assert.NoError(t, err)
		var config RecertConfig
		assert.NoError(t, json.Unmarshal(data, &config))

		assert.True(t, config.ForceExpire)
		assert.True(t, config.EtcdDefrag)
		assert.NotEmpty(t, config.KubeadminPasswordHash)
		assert.Contains(t, config.KubeadminPasswordHash, "$2a$")
		assert.Equal(t, "/kubernetes/recert-seed-creation-summary.yaml", config.SummaryFileClean)
	})

	t.Run("without password sets empty hash", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), RecertConfigFile)
		err := CreateRecertConfigFileForSeedCreation(configPath, false)
		assert.NoError(t, err)

		data, err := os.ReadFile(configPath)
		assert.NoError(t, err)
		var config RecertConfig
		assert.NoError(t, json.Unmarshal(data, &config))

		assert.True(t, config.ForceExpire)
		assert.Empty(t, config.KubeadminPasswordHash)
	})
}

func TestCreateRecertConfigFileForSeedRestoration(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), RecertConfigFile)
	originalHash := "$2a$10$original-password-hash"

	err := CreateRecertConfigFileForSeedRestoration(configPath, originalHash)
	assert.NoError(t, err)

	data, err := os.ReadFile(configPath)
	assert.NoError(t, err)
	var config RecertConfig
	assert.NoError(t, json.Unmarshal(data, &config))

	assert.True(t, config.ExtendExpiration)
	assert.False(t, config.ForceExpire)
	assert.Equal(t, originalHash, config.KubeadminPasswordHash)
	assert.Equal(t, "/kubernetes/recert-seed-restoration-summary.yaml", config.SummaryFileClean)

	assert.Len(t, config.UseKeyRules, 4)
	for _, rule := range config.UseKeyRules {
		assert.Contains(t, rule, common.BackupCertsDir)
	}
	assert.Contains(t, config.UseKeyRules[3], "ingresskey-ingress-operator")

	assert.Len(t, config.UseCertRules, 1)
	assert.Contains(t, config.UseCertRules[0], "admin-kubeconfig-client-ca.crt")
}
