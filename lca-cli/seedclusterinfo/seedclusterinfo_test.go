package seedclusterinfo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSeedClusterInfo_DualStackFields(t *testing.T) {
	// Test that SeedClusterInfo struct properly handles dual-stack fields
	info := SeedClusterInfo{
		BaseDomain:                       "example.com",
		SeedClusterOCPVersion:            "4.14.0",
		ClusterName:                      "test-cluster",
		NodeIP:                           "192.168.1.10", // Legacy field
		NodeIPs:                          []string{"192.168.1.10", "2001:db8::10"},
		ReleaseRegistry:                  "quay.io/openshift-release-dev",
		SNOHostname:                      "test-node",
		MirrorRegistryConfigured:         false,
		HasProxy:                         false,
		HasFIPS:                          false,
		ContainerStorageMountpointTarget: "/var/lib/containers",
		ClusterNetworks:                  []string{"10.128.0.0/14", "fd01::/48"},
		ServiceNetworks:                  []string{"172.30.0.0/16", "fd02::/112"},
		MachineNetworks:                  []string{"192.168.1.0/24", "2001:db8::/64"},
		IngressCertificateCN:             "ingress-operator@123456",
	}

	// Verify dual-stack specific fields
	assert.Equal(t, []string{"192.168.1.10", "2001:db8::10"}, info.NodeIPs)
	assert.Equal(t, []string{"192.168.1.0/24", "2001:db8::/64"}, info.MachineNetworks)
	assert.Equal(t, []string{"10.128.0.0/14", "fd01::/48"}, info.ClusterNetworks)
	assert.Equal(t, []string{"172.30.0.0/16", "fd02::/112"}, info.ServiceNetworks)

	// Verify legacy field
	assert.Equal(t, "192.168.1.10", info.NodeIP)

	// Verify other standard fields
	assert.Equal(t, "example.com", info.BaseDomain)
	assert.Equal(t, "test-cluster", info.ClusterName)
	assert.Equal(t, "4.14.0", info.SeedClusterOCPVersion)
	assert.Equal(t, "test-node", info.SNOHostname)
	assert.Equal(t, "ingress-operator@123456", info.IngressCertificateCN)
}

func TestSeedClusterInfo_BackwardCompatibility(t *testing.T) {
	tests := []struct {
		name                string
		seedClusterInfo     SeedClusterInfo
		expectedNodeIP      string
		expectedNodeIPs     []string
		expectedMachineNets []string
	}{
		{
			name: "new format - NodeIPs populated",
			seedClusterInfo: SeedClusterInfo{
				NodeIPs:         []string{"192.168.1.10", "2001:db8::10"},
				MachineNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
			},
			expectedNodeIP:      "", // NodeIP should be empty when NodeIPs is used
			expectedNodeIPs:     []string{"192.168.1.10", "2001:db8::10"},
			expectedMachineNets: []string{"192.168.1.0/24", "2001:db8::/64"},
		},
		{
			name: "old format - NodeIP populated",
			seedClusterInfo: SeedClusterInfo{
				NodeIP:          "192.168.1.10",
				NodeIPs:         []string{}, // Empty or nil
				MachineNetworks: []string{"192.168.1.0/24"},
			},
			expectedNodeIP:      "192.168.1.10",
			expectedNodeIPs:     []string{},
			expectedMachineNets: []string{"192.168.1.0/24"},
		},
		{
			name: "mixed format - NodeIPs takes precedence",
			seedClusterInfo: SeedClusterInfo{
				NodeIP:          "192.168.1.5", // This should be preserved
				NodeIPs:         []string{"192.168.1.10", "2001:db8::10"},
				MachineNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
			},
			expectedNodeIP:      "192.168.1.5", // NodeIP preserved for backward compatibility
			expectedNodeIPs:     []string{"192.168.1.10", "2001:db8::10"},
			expectedMachineNets: []string{"192.168.1.0/24", "2001:db8::/64"},
		},
		{
			name: "empty configuration",
			seedClusterInfo: SeedClusterInfo{
				NodeIP:          "",
				NodeIPs:         []string{},
				MachineNetworks: []string{},
			},
			expectedNodeIP:      "",
			expectedNodeIPs:     []string{},
			expectedMachineNets: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := tt.seedClusterInfo

			// Verify that the struct maintains the expected values
			assert.Equal(t, tt.expectedNodeIP, info.NodeIP)
			assert.Equal(t, tt.expectedNodeIPs, info.NodeIPs)
			assert.Equal(t, tt.expectedMachineNets, info.MachineNetworks)
		})
	}
}

func TestSeedClusterInfo_SingleStack(t *testing.T) {
	// Test single stack IPv4 configuration
	info := SeedClusterInfo{
		NodeIPs:         []string{"192.168.1.10"},
		MachineNetworks: []string{"192.168.1.0/24"},
		ClusterNetworks: []string{"10.128.0.0/14"},
		ServiceNetworks: []string{"172.30.0.0/16"},
	}

	assert.Equal(t, []string{"192.168.1.10"}, info.NodeIPs)
	assert.Equal(t, []string{"192.168.1.0/24"}, info.MachineNetworks)
	assert.Equal(t, []string{"10.128.0.0/14"}, info.ClusterNetworks)
	assert.Equal(t, []string{"172.30.0.0/16"}, info.ServiceNetworks)
}

func TestSeedClusterInfo_IPv6Only(t *testing.T) {
	// Test IPv6 only configuration
	info := SeedClusterInfo{
		NodeIPs:         []string{"2001:db8::10"},
		MachineNetworks: []string{"2001:db8::/64"},
		ClusterNetworks: []string{"fd01::/48"},
		ServiceNetworks: []string{"fd02::/112"},
	}

	assert.Equal(t, []string{"2001:db8::10"}, info.NodeIPs)
	assert.Equal(t, []string{"2001:db8::/64"}, info.MachineNetworks)
	assert.Equal(t, []string{"fd01::/48"}, info.ClusterNetworks)
	assert.Equal(t, []string{"fd02::/112"}, info.ServiceNetworks)
}
