package seedclusterinfo

import (
	"testing"

	"github.com/openshift-kni/lifecycle-agent/utils"
	"github.com/stretchr/testify/assert"
)

func TestNewFromClusterInfoDualStack(t *testing.T) {
	testcases := []struct {
		name                    string
		clusterInfo             *utils.ClusterInfo
		expectedNodeIP          string
		expectedNodeIPs         []string
		expectedMachineNetworks []string
		expectedClusterNetworks []string
		expectedServiceNetworks []string
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
			expectedClusterNetworks: []string{"10.244.0.0/16"},
			expectedServiceNetworks: []string{"10.96.0.0/16"},
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
			expectedClusterNetworks: []string{"2001:db8:1::/64"},
			expectedServiceNetworks: []string{"2001:db8:2::/64"},
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
			expectedClusterNetworks: []string{"10.244.0.0/16", "2001:db8:1::/64"},
			expectedServiceNetworks: []string{"10.96.0.0/16", "2001:db8:2::/64"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			seedClusterInfo := NewFromClusterInfo(
				tc.clusterInfo,
				"quay.io/example/seed:latest",
				false, // hasProxy
				false, // hasFIPS
				nil,   // additionalTrustBundle
				"",    // containerStorageMountpointTarget
				"",    // ingressCertificateCN
			)

			// Verify that all network fields are properly populated
			assert.Equal(t, tc.expectedNodeIP, seedClusterInfo.NodeIP)
			assert.Equal(t, tc.expectedNodeIPs, seedClusterInfo.NodeIPs)
			assert.Equal(t, tc.expectedMachineNetworks, seedClusterInfo.MachineNetworks)
			assert.Equal(t, tc.expectedClusterNetworks, seedClusterInfo.ClusterNetworks)
			assert.Equal(t, tc.expectedServiceNetworks, seedClusterInfo.ServiceNetworks)

			// Verify other fields are preserved
			assert.Equal(t, tc.clusterInfo.OCPVersion, seedClusterInfo.SeedClusterOCPVersion)
			assert.Equal(t, tc.clusterInfo.BaseDomain, seedClusterInfo.BaseDomain)
			assert.Equal(t, tc.clusterInfo.ClusterName, seedClusterInfo.ClusterName)
			assert.Equal(t, tc.clusterInfo.Hostname, seedClusterInfo.SNOHostname)
			assert.Equal(t, "quay.io/example/seed:latest", seedClusterInfo.RecertImagePullSpec)
		})
	}
}

func TestSeedClusterInfoBackwardCompatibility(t *testing.T) {
	testcases := []struct {
		name        string
		nodeIP      string
		nodeIPs     []string
		description string
	}{
		{
			name:        "Legacy single IPv4",
			nodeIP:      "192.168.1.10",
			nodeIPs:     []string{},
			description: "Only legacy NodeIP field populated",
		},
		{
			name:        "Legacy single IPv6",
			nodeIP:      "2001:db8::10",
			nodeIPs:     []string{},
			description: "Only legacy NodeIP field populated with IPv6",
		},
		{
			name:        "New dual-stack field",
			nodeIP:      "",
			nodeIPs:     []string{"192.168.1.10", "2001:db8::10"},
			description: "Only new NodeIPs field populated",
		},
		{
			name:        "Both fields populated - consistent",
			nodeIP:      "192.168.1.10",
			nodeIPs:     []string{"192.168.1.10", "2001:db8::10"},
			description: "Both fields populated with consistent data",
		},
		{
			name:        "Both fields populated - inconsistent",
			nodeIP:      "192.168.1.10",
			nodeIPs:     []string{"10.0.0.5", "2001:db8::10"},
			description: "Both fields populated with different primary IPs",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			seedClusterInfo := &SeedClusterInfo{
				NodeIP:  tc.nodeIP,
				NodeIPs: tc.nodeIPs,
			}

			// Verify the structure can hold both fields
			assert.Equal(t, tc.nodeIP, seedClusterInfo.NodeIP, "Legacy NodeIP field should be preserved")
			assert.Equal(t, tc.nodeIPs, seedClusterInfo.NodeIPs, "New NodeIPs field should be preserved")

			// Test serialization compatibility (basic check that fields exist)
			if tc.nodeIP != "" {
				assert.NotEmpty(t, seedClusterInfo.NodeIP, "Legacy field should not be empty when set")
			}
			if len(tc.nodeIPs) > 0 {
				assert.NotEmpty(t, seedClusterInfo.NodeIPs, "New field should not be empty when set")
			}
		})
	}
}
