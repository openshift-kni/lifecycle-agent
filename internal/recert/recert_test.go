package recert

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
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

func TestValidateMachineNetworksAndIPs_SingleStackIPv4_OK(t *testing.T) {
	seed := &seedclusterinfo.SeedClusterInfo{NodeIPs: []string{"192.168.1.10"}}
	reconfig := &seedreconfig.SeedReconfiguration{
		NodeIPs:         []string{"192.168.1.20"},
		MachineNetworks: []string{"192.168.1.0/24"},
	}
	err := validateMachineNetworksAndIPs(reconfig, seed)
	assert.NoError(t, err)
}

func TestValidateMachineNetworksAndIPs_SingleStackIPv6_OK(t *testing.T) {
	seed := &seedclusterinfo.SeedClusterInfo{NodeIPs: []string{"2001:db8::10"}}
	reconfig := &seedreconfig.SeedReconfiguration{
		NodeIPs:         []string{"2001:db8::20"},
		MachineNetworks: []string{"2001:db8::/64"},
	}
	err := validateMachineNetworksAndIPs(reconfig, seed)
	assert.NoError(t, err)
}

func TestValidateMachineNetworksAndIPs_DualStack_OK(t *testing.T) {
	seed := &seedclusterinfo.SeedClusterInfo{NodeIPs: []string{"192.168.1.10", "2001:db8::10"}}
	reconfig := &seedreconfig.SeedReconfiguration{
		NodeIPs:         []string{"192.168.1.20", "2001:db8::20"},
		MachineNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
	}
	err := validateMachineNetworksAndIPs(reconfig, seed)
	assert.NoError(t, err)
}

func TestValidateMachineNetworksAndIPs_CountMismatch_NewIPsVsNewNets(t *testing.T) {
	seed := &seedclusterinfo.SeedClusterInfo{NodeIPs: []string{"192.168.1.10"}}
	reconfig := &seedreconfig.SeedReconfiguration{
		NodeIPs:         []string{"192.168.1.20", "2001:db8::20"},
		MachineNetworks: []string{"192.168.1.0/24"},
	}
	err := validateMachineNetworksAndIPs(reconfig, seed)
	assert.Error(t, err)
}

func TestValidateMachineNetworksAndIPs_OrderMismatch_OldIPsVsNewNets(t *testing.T) {
	seed := &seedclusterinfo.SeedClusterInfo{NodeIPs: []string{"2001:db8::10", "192.168.1.10"}} // v6, v4
	reconfig := &seedreconfig.SeedReconfiguration{
		NodeIPs:         []string{"2001:db8::20", "192.168.1.20"},
		MachineNetworks: []string{"192.168.1.0/24", "2001:db8::/64"}, // v4, v6
	}
	err := validateMachineNetworksAndIPs(reconfig, seed)
	assert.Error(t, err)
}

func TestValidateMachineNetworksAndIPs_CountMismatch_OldIPsVsNewIPs(t *testing.T) {
	seed := &seedclusterinfo.SeedClusterInfo{NodeIPs: []string{"192.168.1.10", "2001:db8::10"}}
	reconfig := &seedreconfig.SeedReconfiguration{
		NodeIPs:         []string{"192.168.1.20"},
		MachineNetworks: []string{"192.168.1.0/24"},
	}
	err := validateMachineNetworksAndIPs(reconfig, seed)
	assert.Error(t, err)
}

func TestValidateMachineNetworksAndIPs_OrderMismatch_OldIPsVsNewIPs(t *testing.T) {
	seed := &seedclusterinfo.SeedClusterInfo{NodeIPs: []string{"192.168.1.10", "2001:db8::10"}} // v4, v6
	reconfig := &seedreconfig.SeedReconfiguration{
		NodeIPs:         []string{"2001:db8::20", "192.168.1.20"}, // v6, v4
		MachineNetworks: []string{"2001:db8::/64", "192.168.1.0/24"},
	}
	err := validateMachineNetworksAndIPs(reconfig, seed)
	assert.Error(t, err)
}

func TestValidateMachineNetworksAndIPs_InvalidIP(t *testing.T) {
	seed := &seedclusterinfo.SeedClusterInfo{NodeIPs: []string{"192.168.1.10"}}
	reconfig := &seedreconfig.SeedReconfiguration{
		NodeIPs:         []string{"not-an-ip"},
		MachineNetworks: []string{"192.168.1.0/24"},
	}
	err := validateMachineNetworksAndIPs(reconfig, seed)
	assert.Error(t, err)
}

func TestValidateMachineNetworksAndIPs_InvalidCIDR(t *testing.T) {
	seed := &seedclusterinfo.SeedClusterInfo{NodeIPs: []string{"192.168.1.10"}}
	reconfig := &seedreconfig.SeedReconfiguration{
		NodeIPs:         []string{"192.168.1.20"},
		MachineNetworks: []string{"192.168.1.0/33"}, // invalid prefix
	}
	err := validateMachineNetworksAndIPs(reconfig, seed)
	assert.Error(t, err)
}

func TestCreateRecertConfigFile_EndToEnd(t *testing.T) {
	cryptoDir := t.TempDir()
	recertFolder := t.TempDir()

	// Provide an ingress key file so getIngressKeyPath() can find it
	ingressFile := "ingresskey-ingress-operator.key"
	err := os.WriteFile(filepath.Join(cryptoDir, ingressFile), []byte("dummy"), 0o600)
	assert.NoError(t, err)

	seed := &seedclusterinfo.SeedClusterInfo{
		BaseDomain:           "old.example.com",
		ClusterName:          "old-cluster",
		SNOHostname:          "old-host",
		NodeIPs:              []string{"192.168.1.10", "2001:db8::10"},
		IngressCertificateCN: "ingress-operator@1700000000",
		MachineNetworks:      []string{"10.0.0.0/24", "fd00::/64"},
	}

	reconfig := &seedreconfig.SeedReconfiguration{
		BaseDomain:      "new.example.com",
		ClusterName:     "new-cluster",
		Hostname:        "new-host",
		NodeIPs:         []string{"192.168.1.20", "2001:db8::20"},
		MachineNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
	}

	// Should succeed and write recert_config.json
	err = CreateRecertConfigFile(reconfig, seed, cryptoDir, recertFolder)
	assert.NoError(t, err)

	bytes, err := os.ReadFile(filepath.Join(recertFolder, RecertConfigFile))
	assert.NoError(t, err)

	var cfg RecertConfig
	err = json.Unmarshal(bytes, &cfg)
	assert.NoError(t, err)

	// Key assertions
	assert.Equal(t, []string{"192.168.1.20", "2001:db8::20"}, cfg.IP)
	assert.Equal(t, "new-cluster:new.example.com", cfg.ClusterRename)
	assert.Equal(t, "new-host", cfg.Hostname)
	assert.Equal(t, []string{filepath.Join(cryptoDir, "admin-kubeconfig-client-ca.crt")}, cfg.UseCertRules)
	if assert.Len(t, cfg.UseKeyRules, 4) {
		assert.Equal(t, seed.IngressCertificateCN+" "+filepath.Join(cryptoDir, ingressFile), cfg.UseKeyRules[3])
	}
	// When machine networks differ, we copy the seed's machine networks to the config
	assert.Equal(t, []string{"10.0.0.0/24", "fd00::/64"}, cfg.MachineNetworkCidr)
	assert.Equal(t, EtcSSHMount, cfg.RegenerateServerSSHKeys)
	assert.Equal(t, SummaryFile, cfg.SummaryFileClean)
}
