package recert

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedclusterinfo"
	"github.com/openshift-kni/lifecycle-agent/utils"
	"github.com/stretchr/testify/assert"
)

func TestGetAllNodeIPsFromSeedReconfig(t *testing.T) {
	testcases := []struct {
		name         string
		seedReconfig *seedreconfig.SeedReconfiguration
		expectedIPs  []string
	}{
		{
			name: "Single-stack IPv4 - legacy field only",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				NodeIP: "192.168.1.10",
			},
			expectedIPs: []string{"192.168.1.10"},
		},
		{
			name: "Single-stack IPv6 - legacy field only",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				NodeIP: "2001:db8::10",
			},
			expectedIPs: []string{"2001:db8::10"},
		},
		{
			name: "Dual-stack - new field only",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				NodeIPs: []string{"192.168.1.10", "2001:db8::10"},
			},
			expectedIPs: []string{"192.168.1.10", "2001:db8::10"},
		},
		{
			name: "Dual-stack IPv6 primary",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				NodeIPs: []string{"2001:db8::10", "192.168.1.10"},
			},
			expectedIPs: []string{"2001:db8::10", "192.168.1.10"},
		},
		{
			name: "New field takes precedence",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				NodeIP:  "192.168.1.10",
				NodeIPs: []string{"10.0.0.5", "2001:db8::10"},
			},
			expectedIPs: []string{"10.0.0.5", "2001:db8::10"},
		},
		{
			name:         "Empty configuration",
			seedReconfig: &seedreconfig.SeedReconfiguration{},
			expectedIPs:  []string{},
		},
		{
			name: "Multiple IPv4 addresses",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				NodeIPs: []string{"192.168.1.10", "10.0.0.5", "172.16.1.10"},
			},
			expectedIPs: []string{"192.168.1.10", "10.0.0.5", "172.16.1.10"},
		},
		{
			name: "Multiple IPv6 addresses",
			seedReconfig: &seedreconfig.SeedReconfiguration{
				NodeIPs: []string{"2001:db8::10", "2001:db9::20"},
			},
			expectedIPs: []string{"2001:db8::10", "2001:db9::20"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			result := getAllNodeIPsFromSeedReconfig(tc.seedReconfig)
			assert.Equal(t, tc.expectedIPs, result)
		})
	}
}

func TestGetAllNodeIPsFromSeedClusterInfo(t *testing.T) {
	testcases := []struct {
		name            string
		seedClusterInfo *seedclusterinfo.SeedClusterInfo
		expectedIPs     []string
	}{
		{
			name: "Single-stack IPv4 - legacy field only",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIP: "192.168.1.10",
			},
			expectedIPs: []string{"192.168.1.10"},
		},
		{
			name: "Single-stack IPv6 - legacy field only",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIP: "2001:db8::10",
			},
			expectedIPs: []string{"2001:db8::10"},
		},
		{
			name: "Dual-stack - new field only",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs: []string{"192.168.1.10", "2001:db8::10"},
			},
			expectedIPs: []string{"192.168.1.10", "2001:db8::10"},
		},
		{
			name: "Dual-stack IPv6 primary",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs: []string{"2001:db8::10", "192.168.1.10"},
			},
			expectedIPs: []string{"2001:db8::10", "192.168.1.10"},
		},
		{
			name: "New field takes precedence",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIP:  "192.168.1.10",
				NodeIPs: []string{"10.0.0.5", "2001:db8::10"},
			},
			expectedIPs: []string{"10.0.0.5", "2001:db8::10"},
		},
		{
			name:            "Empty configuration",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{},
			expectedIPs:     []string{},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			result := getAllNodeIPsFromSeedClusterInfo(tc.seedClusterInfo)
			assert.Equal(t, tc.expectedIPs, result)
		})
	}
}

func TestIPComparisonDualStack(t *testing.T) {
	testcases := []struct {
		name             string
		seedClusterInfo  *seedclusterinfo.SeedClusterInfo
		seedReconfig     *seedreconfig.SeedReconfiguration
		expectChanged    bool
		expectedConfigIP string
	}{
		{
			name: "Single-stack IPv4 - no change",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIP: "192.168.1.10",
			},
			seedReconfig: &seedreconfig.SeedReconfiguration{
				NodeIP: "192.168.1.10",
			},
			expectChanged: false,
		},
		{
			name: "Single-stack IPv4 - changed",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIP: "192.168.1.10",
			},
			seedReconfig: &seedreconfig.SeedReconfiguration{
				NodeIP: "192.168.1.20",
			},
			expectChanged:    true,
			expectedConfigIP: "192.168.1.20",
		},
		{
			name: "Single-stack IPv6 - no change",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIP: "2001:db8::10",
			},
			seedReconfig: &seedreconfig.SeedReconfiguration{
				NodeIP: "2001:db8::10",
			},
			expectChanged: false,
		},
		{
			name: "Single-stack IPv6 - changed",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIP: "2001:db8::10",
			},
			seedReconfig: &seedreconfig.SeedReconfiguration{
				NodeIP: "2001:db8::20",
			},
			expectChanged:    true,
			expectedConfigIP: "2001:db8::20",
		},
		{
			name: "Dual-stack - no change",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs: []string{"192.168.1.10", "2001:db8::10"},
			},
			seedReconfig: &seedreconfig.SeedReconfiguration{
				NodeIPs: []string{"192.168.1.10", "2001:db8::10"},
			},
			expectChanged: false,
		},
		{
			name: "Dual-stack - changed IPs",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs: []string{"192.168.1.10", "2001:db8::10"},
			},
			seedReconfig: &seedreconfig.SeedReconfiguration{
				NodeIPs: []string{"192.168.1.20", "2001:db8::20"},
			},
			expectChanged:    true,
			expectedConfigIP: "192.168.1.20,2001:db8::20",
		},
		{
			name: "Dual-stack - order changed",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs: []string{"192.168.1.10", "2001:db8::10"},
			},
			seedReconfig: &seedreconfig.SeedReconfiguration{
				NodeIPs: []string{"2001:db8::10", "192.168.1.10"},
			},
			expectChanged:    true,
			expectedConfigIP: "2001:db8::10,192.168.1.10",
		},
		{
			name: "Single to dual-stack",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIP: "192.168.1.10",
			},
			seedReconfig: &seedreconfig.SeedReconfiguration{
				NodeIPs: []string{"192.168.1.10", "2001:db8::10"},
			},
			expectChanged:    true,
			expectedConfigIP: "192.168.1.10,2001:db8::10",
		},
		{
			name: "Dual to single-stack",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs: []string{"192.168.1.10", "2001:db8::10"},
			},
			seedReconfig: &seedreconfig.SeedReconfiguration{
				NodeIP: "192.168.1.20",
			},
			expectChanged:    true,
			expectedConfigIP: "192.168.1.20",
		},
		{
			name: "IPv6 primary dual-stack",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs: []string{"192.168.1.10", "2001:db8::10"},
			},
			seedReconfig: &seedreconfig.SeedReconfiguration{
				NodeIPs: []string{"2001:db8::20", "192.168.1.20"},
			},
			expectChanged:    true,
			expectedConfigIP: "2001:db8::20,192.168.1.20",
		},
		{
			name: "Multiple IPv4 addresses",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs: []string{"192.168.1.10"},
			},
			seedReconfig: &seedreconfig.SeedReconfiguration{
				NodeIPs: []string{"192.168.1.10", "10.0.0.5", "172.16.1.10"},
			},
			expectChanged:    true,
			expectedConfigIP: "192.168.1.10,10.0.0.5,172.16.1.10",
		},
		{
			name: "Multiple IPv6 addresses",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs: []string{"2001:db8::10"},
			},
			seedReconfig: &seedreconfig.SeedReconfiguration{
				NodeIPs: []string{"2001:db8::10", "2001:db9::20"},
			},
			expectChanged:    true,
			expectedConfigIP: "2001:db8::10,2001:db9::20",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			// Test the IP comparison logic directly
			allSeedNodeIPs := getAllNodeIPsFromSeedClusterInfo(tc.seedClusterInfo)
			allNodeIPs := getAllNodeIPsFromSeedReconfig(tc.seedReconfig)

			// Check if IPs have changed using slices.Equal (same logic as in recert)
			ipsChanged := !slicesEqual(allSeedNodeIPs, allNodeIPs)

			assert.Equal(t, tc.expectChanged, ipsChanged, "IP change detection mismatch")

			if ipsChanged && len(allNodeIPs) > 0 {
				configIP := strings.Join(allNodeIPs, ",")
				assert.Equal(t, tc.expectedConfigIP, configIP, "Config IP format mismatch")
			}
		})
	}
}

// Helper function to simulate slices.Equal for testing
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCreateRecertConfigFileDualStack(t *testing.T) {
	tmpDir := t.TempDir()

	testcases := []struct {
		name                       string
		seedClusterInfo            *seedclusterinfo.SeedClusterInfo
		seedReconfig               *seedreconfig.SeedReconfiguration
		expectedIPsInConfig        []string
		expectedMachineNetworkCIDR []string
		expectIPsSet               bool
		expectMachineNetworkSet    bool
	}{
		{
			name: "Single-stack IPv4 - no change",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName:     "test-cluster",
				BaseDomain:      "example.com",
				NodeIP:          "192.168.1.10",
				SNOHostname:     "test-node",
				MachineNetworks: []string{"192.168.1.0/24"},
			},
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName:     "test-cluster",
				BaseDomain:      "example.com",
				NodeIP:          "192.168.1.10",
				Hostname:        "test-node",
				MachineNetworks: []string{"192.168.1.0/24"},
			},
			expectIPsSet:            false, // IPs haven't changed
			expectMachineNetworkSet: false, // Networks haven't changed
		},
		{
			name: "Single-stack IPv4 - IP changed",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName:     "test-cluster",
				BaseDomain:      "example.com",
				NodeIP:          "192.168.1.10",
				SNOHostname:     "test-node",
				MachineNetworks: []string{"192.168.1.0/24"},
			},
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName:     "test-cluster",
				BaseDomain:      "example.com",
				NodeIP:          "192.168.1.20",
				Hostname:        "test-node",
				MachineNetworks: []string{"192.168.1.0/24"},
			},
			expectedIPsInConfig:     []string{"192.168.1.20"},
			expectIPsSet:            true,
			expectMachineNetworkSet: false,
		},
		{
			name: "Dual-stack - IPs changed",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName:     "test-cluster",
				BaseDomain:      "example.com",
				NodeIPs:         []string{"192.168.1.10", "2001:db8::10"},
				SNOHostname:     "test-node",
				MachineNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
			},
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName:     "test-cluster",
				BaseDomain:      "example.com",
				NodeIPs:         []string{"192.168.1.20", "2001:db8::20"},
				Hostname:        "test-node",
				MachineNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
			},
			expectedIPsInConfig:     []string{"192.168.1.20", "2001:db8::20"},
			expectIPsSet:            true,
			expectMachineNetworkSet: false,
		},
		{
			name: "IPv6 primary dual-stack",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName:     "test-cluster",
				BaseDomain:      "example.com",
				NodeIPs:         []string{"192.168.1.10", "2001:db8::10"},
				SNOHostname:     "test-node",
				MachineNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
			},
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName:     "test-cluster",
				BaseDomain:      "example.com",
				NodeIPs:         []string{"2001:db8::20", "192.168.1.20"}, // IPv6 first
				Hostname:        "test-node",
				MachineNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
			},
			expectedIPsInConfig:     []string{"2001:db8::20", "192.168.1.20"},
			expectIPsSet:            true,
			expectMachineNetworkSet: false,
		},
		{
			name: "Machine networks changed",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName:     "test-cluster",
				BaseDomain:      "example.com",
				NodeIP:          "192.168.1.10",
				SNOHostname:     "test-node",
				MachineNetworks: []string{"192.168.1.0/24"},
			},
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName:     "test-cluster",
				BaseDomain:      "example.com",
				NodeIP:          "192.168.1.10",
				Hostname:        "test-node",
				MachineNetworks: []string{"10.0.0.0/16"}, // Different network
			},
			expectedMachineNetworkCIDR: []string{"192.168.1.0/24"}, // Should use seed cluster info networks
			expectIPsSet:               false,
			expectMachineNetworkSet:    true,
		},
		{
			name: "Both IPs and machine networks changed",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				ClusterName:     "test-cluster",
				BaseDomain:      "example.com",
				NodeIPs:         []string{"192.168.1.10", "2001:db8::10"},
				SNOHostname:     "test-node",
				MachineNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
			},
			seedReconfig: &seedreconfig.SeedReconfiguration{
				ClusterName:     "test-cluster",
				BaseDomain:      "example.com",
				NodeIPs:         []string{"10.0.0.5", "2001:db9::20"},
				Hostname:        "test-node",
				MachineNetworks: []string{"10.0.0.0/16", "2001:db9::/64"},
			},
			expectedIPsInConfig:        []string{"10.0.0.5", "2001:db9::20"},
			expectedMachineNetworkCIDR: []string{"192.168.1.0/24", "2001:db8::/64"},
			expectIPsSet:               true,
			expectMachineNetworkSet:    true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			recertConfigFolder := tmpDir
			cryptoDir := "" // No crypto dir for these tests

			err := CreateRecertConfigFile(tc.seedReconfig, tc.seedClusterInfo, cryptoDir, recertConfigFolder)
			assert.NoError(t, err, "CreateRecertConfigFile should not return an error")

			// Read and verify the generated config
			configPath := filepath.Join(recertConfigFolder, RecertConfigFile)
			var config RecertConfig
			err = utils.ReadYamlOrJSONFile(configPath, &config)
			assert.NoError(t, err, "Should be able to read the generated config file")

			// Verify IP configuration
			if tc.expectIPsSet {
				assert.Equal(t, tc.expectedIPsInConfig, config.IP, "IP field should be set as slice")
				assert.NotEmpty(t, config.IP, "IP field should not be empty when IPs changed")
			} else {
				assert.Empty(t, config.IP, "IP field should be empty when IPs haven't changed")
			}

			// Verify machine network configuration
			if tc.expectMachineNetworkSet {
				assert.Equal(t, tc.expectedMachineNetworkCIDR, config.MachineNetworkCidr, "MachineNetworkCidr should be set correctly")
				assert.NotEmpty(t, config.MachineNetworkCidr, "MachineNetworkCidr should not be empty when networks changed")
			} else {
				assert.Empty(t, config.MachineNetworkCidr, "MachineNetworkCidr should be empty when networks haven't changed")
			}

			// Verify basic config structure
			assert.Equal(t, fmt.Sprintf("%s:%s", tc.seedReconfig.ClusterName, tc.seedReconfig.BaseDomain), config.ClusterRename)
			assert.Equal(t, common.EtcdDefaultEndpoint, config.EtcdEndpoint)
			assert.True(t, config.ExtendExpiration)
		})
	}
}
