package clusterconfig

import (
	"strings"
	"testing"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedclusterinfo"
	"github.com/openshift-kni/lifecycle-agent/utils"
	"github.com/stretchr/testify/assert"
)

func TestSeedReconfigurationDifferentStack(t *testing.T) {
	testcases := []struct {
		name                    string
		clusterInfo             *utils.ClusterInfo
		expectedNodeIP          string
		expectedNodeIPs         []string
		expectedMachineNetworks []string
	}{
		{
			name: "Single-stack IPv4",
			clusterInfo: &utils.ClusterInfo{
				OCPVersion:      "4.15.0",
				BaseDomain:      "example.com",
				ClusterName:     "test-cluster",
				NodeIP:          "192.168.1.10",
				NodeIPs:         []string{"192.168.1.10"},
				MachineNetwork:  "192.168.1.0/24",
				MachineNetworks: []string{"192.168.1.0/24"},
				ClusterNetworks: []string{"10.244.0.0/16"},
				ServiceNetworks: []string{"10.96.0.0/16"},
				Hostname:        "test-node",
			},
			expectedNodeIP:          "192.168.1.10",
			expectedNodeIPs:         []string{"192.168.1.10"},
			expectedMachineNetworks: []string{"192.168.1.0/24"},
		},
		{
			name: "Single-stack IPv6",
			clusterInfo: &utils.ClusterInfo{
				OCPVersion:      "4.15.0",
				BaseDomain:      "example.com",
				ClusterName:     "test-cluster",
				NodeIP:          "2001:db8::10",
				NodeIPs:         []string{"2001:db8::10"},
				MachineNetwork:  "2001:db8::/64",
				MachineNetworks: []string{"2001:db8::/64"},
				ClusterNetworks: []string{"2001:db8:1::/64"},
				ServiceNetworks: []string{"2001:db8:2::/64"},
				Hostname:        "test-node",
			},
			expectedNodeIP:          "2001:db8::10",
			expectedNodeIPs:         []string{"2001:db8::10"},
			expectedMachineNetworks: []string{"2001:db8::/64"},
		},
		{
			name: "Dual-stack IPv4 primary",
			clusterInfo: &utils.ClusterInfo{
				OCPVersion:      "4.15.0",
				BaseDomain:      "example.com",
				ClusterName:     "test-cluster",
				NodeIP:          "192.168.1.10",
				NodeIPs:         []string{"192.168.1.10", "2001:db8::10"},
				MachineNetwork:  "192.168.1.0/24",
				MachineNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
				ClusterNetworks: []string{"10.244.0.0/16", "2001:db8:1::/64"},
				ServiceNetworks: []string{"10.96.0.0/16", "2001:db8:2::/64"},
				Hostname:        "test-node",
			},
			expectedNodeIP:          "192.168.1.10",
			expectedNodeIPs:         []string{"192.168.1.10", "2001:db8::10"},
			expectedMachineNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
		},
		{
			name: "Dual-stack IPv6 primary",
			clusterInfo: &utils.ClusterInfo{
				OCPVersion:      "4.15.0",
				BaseDomain:      "example.com",
				ClusterName:     "test-cluster",
				NodeIP:          "2001:db8::10",
				NodeIPs:         []string{"2001:db8::10", "192.168.1.10"},
				MachineNetwork:  "2001:db8::/64",
				MachineNetworks: []string{"2001:db8::/64", "192.168.1.0/24"},
				ClusterNetworks: []string{"2001:db8:1::/64", "10.244.0.0/16"},
				ServiceNetworks: []string{"2001:db8:2::/64", "10.96.0.0/16"},
				Hostname:        "test-node",
			},
			expectedNodeIP:          "2001:db8::10",
			expectedNodeIPs:         []string{"2001:db8::10", "192.168.1.10"},
			expectedMachineNetworks: []string{"2001:db8::/64", "192.168.1.0/24"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			// Create SeedClusterInfo from ClusterInfo
			seedClusterInfo := seedclusterinfo.NewFromClusterInfo(
				tc.clusterInfo,
				"quay.io/example/seed:latest",
				false, // hasProxy
				false, // hasFIPS
				nil,   // additionalTrustBundle
				"",    // containerStorageMountpointTarget
				"",    // ingressCertificateCN
			)

			// The SeedReconfiguration would be created from this SeedClusterInfo
			// For testing purposes, we verify that the SeedClusterInfo contains the expected data
			assert.Equal(t, tc.expectedNodeIP, seedClusterInfo.NodeIP)
			assert.Equal(t, tc.expectedNodeIPs, seedClusterInfo.NodeIPs)
			assert.Equal(t, tc.expectedMachineNetworks, seedClusterInfo.MachineNetworks)

			// Verify that backward compatibility is maintained
			if len(tc.expectedNodeIPs) > 0 {
				assert.Equal(t, tc.expectedNodeIPs[0], seedClusterInfo.NodeIP, "First NodeIP should match legacy NodeIP field")
			}
			if len(tc.expectedMachineNetworks) > 0 {
				// Note: SeedClusterInfo doesn't have a single MachineNetwork field, only MachineNetworks slice
				assert.Contains(t, seedClusterInfo.MachineNetworks, tc.expectedMachineNetworks[0], "First machine network should be in the list")
			}
		})
	}
}

func TestIPStackTypeDetermination(t *testing.T) {
	testcases := []struct {
		name        string
		nodeIPs     []string
		description string
		stackType   string
	}{
		{
			name:        "Single-stack IPv4",
			nodeIPs:     []string{"192.168.1.10"},
			description: "Only IPv4 address",
			stackType:   "IPv4",
		},
		{
			name:        "Single-stack IPv6",
			nodeIPs:     []string{"2001:db8::10"},
			description: "Only IPv6 address",
			stackType:   "IPv6",
		},
		{
			name:        "Dual-stack IPv4 primary",
			nodeIPs:     []string{"192.168.1.10", "2001:db8::10"},
			description: "IPv4 and IPv6, IPv4 first",
			stackType:   "dual-stack",
		},
		{
			name:        "Dual-stack IPv6 primary",
			nodeIPs:     []string{"2001:db8::10", "192.168.1.10"},
			description: "IPv4 and IPv6, IPv6 first",
			stackType:   "dual-stack",
		},
		{
			name:        "Multiple IPv4 addresses",
			nodeIPs:     []string{"192.168.1.10", "10.0.0.5", "172.16.1.10"},
			description: "Multiple IPv4 addresses",
			stackType:   "IPv4",
		},
		{
			name:        "Multiple IPv6 addresses",
			nodeIPs:     []string{"2001:db8::10", "2001:db9::20"},
			description: "Multiple IPv6 addresses",
			stackType:   "IPv6",
		},
		{
			name:        "Complex dual-stack",
			nodeIPs:     []string{"192.168.1.10", "2001:db8::10", "10.0.0.5", "2001:db9::20"},
			description: "Multiple IPv4 and IPv6 addresses",
			stackType:   "dual-stack",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			// Helper function to determine IP stack type
			stackType := determineIPStackType(tc.nodeIPs)
			assert.Equal(t, tc.stackType, stackType, tc.description)
		})
	}
}

// Helper function to determine IP stack type for testing
func determineIPStackType(ips []string) string {
	hasIPv4 := false
	hasIPv6 := false

	for _, ip := range ips {
		if strings.Contains(ip, ":") {
			hasIPv6 = true
		} else {
			hasIPv4 = true
		}
	}

	if hasIPv4 && hasIPv6 {
		return "dual-stack"
	} else if hasIPv6 {
		return "IPv6"
	} else {
		return "IPv4"
	}
}
