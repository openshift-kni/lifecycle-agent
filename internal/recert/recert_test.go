package recert

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
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
