package seedclusterinfo

import (
	"testing"

	"github.com/openshift-kni/lifecycle-agent/utils"
	"github.com/stretchr/testify/assert"
)

func TestNewFromClusterInfo(t *testing.T) {
	testcases := []struct {
		name                    string
		clusterInfo             *utils.ClusterInfo
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
				NodeIPs:         []string{"192.168.1.10"},
				MachineNetworks: []string{"192.168.1.0/24"},
				ClusterNetworks: []string{"10.244.0.0/16"},
				ServiceNetworks: []string{"10.96.0.0/16"},
				Hostname:        "test-node",
			},
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
				NodeIPs:         []string{"2001:db8::10"},
				MachineNetworks: []string{"2001:db8::/64"},
				ClusterNetworks: []string{"2001:db8:1::/64"},
				ServiceNetworks: []string{"2001:db8:2::/64"},
				Hostname:        "test-node",
			},
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
				NodeIPs:         []string{"192.168.1.10", "2001:db8::10"},
				MachineNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
				ClusterNetworks: []string{"10.244.0.0/16", "2001:db8:1::/64"},
				ServiceNetworks: []string{"10.96.0.0/16", "2001:db8:2::/64"},
				Hostname:        "test-node",
			},
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

func TestSeedClusterInfoNetworkFieldCompatibility(t *testing.T) {
	testcases := []struct {
		name        string
		nodeIPs     []string
		description string
	}{
		{
			name:        "Legacy single IPv4",
			nodeIPs:     []string{"192.168.1.10"},
			description: "Single IPv4 node IP",
		},
		{
			name:        "Legacy single IPv6",
			nodeIPs:     []string{"2001:db8::10"},
			description: "Single IPv6 node IP",
		},
		{
			name:        "Dual-stack field",
			nodeIPs:     []string{"192.168.1.10", "2001:db8::10"},
			description: "Dual-stack IPv4 and IPv6",
		},
		{
			name:        "IPv6 primary dual-stack",
			nodeIPs:     []string{"2001:db8::10", "192.168.1.10"},
			description: "Dual-stack with IPv6 primary",
		},
		{
			name:        "Empty NodeIPs",
			nodeIPs:     []string{},
			description: "Empty NodeIPs field",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			seedClusterInfo := &SeedClusterInfo{
				NodeIPs: tc.nodeIPs,
			}

			// Verify the structure can hold the fields
			assert.Equal(t, tc.nodeIPs, seedClusterInfo.NodeIPs, "NodeIPs field should be preserved")

			// Test field validity
			if len(tc.nodeIPs) > 0 {
				assert.NotEmpty(t, seedClusterInfo.NodeIPs, "NodeIPs field should not be empty when set")
			}
		})
	}
}
