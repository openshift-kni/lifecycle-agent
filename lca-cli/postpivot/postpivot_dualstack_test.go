package postpivot

import (
	"os"
	"path/filepath"
	"testing"

	clusterconfig_api "github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

func TestGetMachineNetworksFromSeedReconfig(t *testing.T) {
	testcases := []struct {
		name             string
		seedReconfig     *clusterconfig_api.SeedReconfiguration
		expectedNetworks []string
	}{
		{
			name: "Single-stack IPv4 - legacy field only",
			seedReconfig: &clusterconfig_api.SeedReconfiguration{
				MachineNetwork: "192.168.1.0/24",
			},
			expectedNetworks: []string{"192.168.1.0/24"},
		},
		{
			name: "Single-stack IPv6 - legacy field only",
			seedReconfig: &clusterconfig_api.SeedReconfiguration{
				MachineNetwork: "2001:db8::/64",
			},
			expectedNetworks: []string{"2001:db8::/64"},
		},
		{
			name: "Dual-stack - new field only",
			seedReconfig: &clusterconfig_api.SeedReconfiguration{
				MachineNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
			},
			expectedNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
		},
		{
			name: "Dual-stack IPv6 primary",
			seedReconfig: &clusterconfig_api.SeedReconfiguration{
				MachineNetworks: []string{"2001:db8::/64", "192.168.1.0/24"},
			},
			expectedNetworks: []string{"2001:db8::/64", "192.168.1.0/24"},
		},
		{
			name: "Multiple IPv4 networks",
			seedReconfig: &clusterconfig_api.SeedReconfiguration{
				MachineNetworks: []string{"192.168.1.0/24", "10.0.0.0/8", "172.16.0.0/12"},
			},
			expectedNetworks: []string{"192.168.1.0/24", "10.0.0.0/8", "172.16.0.0/12"},
		},
		{
			name: "Multiple IPv6 networks",
			seedReconfig: &clusterconfig_api.SeedReconfiguration{
				MachineNetworks: []string{"2001:db8::/64", "2001:db9::/64"},
			},
			expectedNetworks: []string{"2001:db8::/64", "2001:db9::/64"},
		},
		{
			name: "New field takes precedence over legacy",
			seedReconfig: &clusterconfig_api.SeedReconfiguration{
				MachineNetwork:  "192.168.1.0/24",
				MachineNetworks: []string{"10.0.0.0/8", "2001:db8::/64"},
			},
			expectedNetworks: []string{"10.0.0.0/8", "2001:db8::/64"},
		},
		{
			name:             "Empty configuration",
			seedReconfig:     &clusterconfig_api.SeedReconfiguration{},
			expectedNetworks: []string{},
		},
		{
			name: "Empty new field falls back to legacy",
			seedReconfig: &clusterconfig_api.SeedReconfiguration{
				MachineNetwork:  "192.168.1.0/24",
				MachineNetworks: []string{},
			},
			expectedNetworks: []string{"192.168.1.0/24"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			result := getMachineNetworksFromSeedReconfig(tc.seedReconfig)
			assert.Equal(t, tc.expectedNetworks, result)
		})
	}
}

func TestSetNodeIpHintDualStack(t *testing.T) {
	testcases := []struct {
		name            string
		machineNetworks []string
		expectedContent string
		expectedError   bool
	}{
		{
			name:            "Single-stack IPv4",
			machineNetworks: []string{"192.168.1.0/24"},
			expectedContent: "KUBELET_NODEIP_HINT=192.168.1.0",
		},
		{
			name:            "Single-stack IPv6",
			machineNetworks: []string{"2001:db8::/64"},
			expectedContent: "KUBELET_NODEIP_HINT=2001:db8::",
		},
		{
			name:            "Dual-stack IPv4 primary",
			machineNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
			expectedContent: "KUBELET_NODEIP_HINT=192.168.1.0 2001:db8::",
		},
		{
			name:            "Dual-stack IPv6 primary",
			machineNetworks: []string{"2001:db8::/64", "192.168.1.0/24"},
			expectedContent: "KUBELET_NODEIP_HINT=2001:db8:: 192.168.1.0",
		},
		{
			name:            "Multiple IPv4 networks",
			machineNetworks: []string{"192.168.1.0/24", "10.0.0.0/8", "172.16.0.0/12"},
			expectedContent: "KUBELET_NODEIP_HINT=192.168.1.0 10.0.0.0 172.16.0.0",
		},
		{
			name:            "Complex dual-stack",
			machineNetworks: []string{"192.168.1.0/24", "2001:db8::/64", "10.0.0.0/8", "2001:db9::/64"},
			expectedContent: "KUBELET_NODEIP_HINT=192.168.1.0 2001:db8:: 10.0.0.0 2001:db9::",
		},
		{
			name:            "Invalid CIDR",
			machineNetworks: []string{"invalid-cidr"},
			expectedError:   true,
		},
		{
			name:            "Empty networks",
			machineNetworks: []string{},
			expectedError:   false, // Function should not error, just skip
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			nodeIPHintFile = tmpDir + "/hint"

			// Create a PostPivot instance with a proper logger
			log := &logrus.Logger{}
			pp := &PostPivot{log: log}
			err := pp.setNodeIpHint(tc.machineNetworks)

			if tc.expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				if len(tc.machineNetworks) > 0 {
					content, readErr := os.ReadFile(nodeIPHintFile)
					assert.NoError(t, readErr)
					assert.Equal(t, tc.expectedContent, string(content))
				}
			}
		})
	}
}

func TestParseKubeletNodeIPsDualStack(t *testing.T) {
	testcases := []struct {
		name            string
		content         string
		expectedNodeIP  string
		expectedNodeIPs []string
		expectedError   bool
	}{
		{
			name: "Single-stack IPv4",
			content: `[Service]
Environment="KUBELET_NODE_IP=192.168.1.10"`,
			expectedNodeIP:  "192.168.1.10",
			expectedNodeIPs: []string{"192.168.1.10"},
		},
		{
			name: "Single-stack IPv6",
			content: `[Service]
Environment="KUBELET_NODE_IP=2001:db8::10"`,
			expectedNodeIP:  "2001:db8::10",
			expectedNodeIPs: []string{"2001:db8::10"},
		},
		{
			name: "Dual-stack IPv4 primary",
			content: `[Service]
Environment="KUBELET_NODE_IP=192.168.127.172" "KUBELET_NODE_IPS=192.168.127.172,2001:db8::3c"`,
			expectedNodeIP:  "192.168.127.172",
			expectedNodeIPs: []string{"192.168.127.172", "2001:db8::3c"},
		},
		{
			name: "Dual-stack IPv6 primary",
			content: `[Service]
Environment="KUBELET_NODE_IP=2001:db8::3c" "KUBELET_NODE_IPS=2001:db8::3c,192.168.127.172"`,
			expectedNodeIP:  "2001:db8::3c",
			expectedNodeIPs: []string{"2001:db8::3c", "192.168.127.172"},
		},
		{
			name: "Only KUBELET_NODE_IPS dual-stack",
			content: `[Service]
Environment="KUBELET_NODE_IPS=192.168.127.172,2001:db8::3c"`,
			expectedNodeIP:  "192.168.127.172", // First IP from list
			expectedNodeIPs: []string{"192.168.127.172", "2001:db8::3c"},
		},
		{
			name: "Only KUBELET_NODE_IPS IPv6 primary",
			content: `[Service]
Environment="KUBELET_NODE_IPS=2001:db8::3c,192.168.127.172"`,
			expectedNodeIP:  "2001:db8::3c", // First IP from list
			expectedNodeIPs: []string{"2001:db8::3c", "192.168.127.172"},
		},
		{
			name: "Multiple IPs with spaces",
			content: `[Service]
Environment="KUBELET_NODE_IPS=192.168.1.10, 2001:db8::10, 10.0.0.5"`,
			expectedNodeIP:  "192.168.1.10",
			expectedNodeIPs: []string{"192.168.1.10", "2001:db8::10", "10.0.0.5"},
		},
		{
			name: "Multiple lines format dual-stack",
			content: `[Service]
Environment="KUBELET_NODE_IP=192.168.1.10"
Environment="KUBELET_NODE_IPS=192.168.1.10,2001:db8::10"`,
			expectedNodeIP:  "192.168.1.10",
			expectedNodeIPs: []string{"192.168.1.10", "2001:db8::10"},
		},
		{
			name: "Only IPv6 networks",
			content: `[Service]
Environment="KUBELET_NODE_IPS=2001:db8::10,2001:db9::20"`,
			expectedNodeIP:  "2001:db8::10",
			expectedNodeIPs: []string{"2001:db8::10", "2001:db9::20"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			nodeIP, nodeIPs, err := parseKubeletNodeIPs(tc.content)

			if tc.expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expectedNodeIP, nodeIP)
				assert.Equal(t, tc.expectedNodeIPs, nodeIPs)
			}
		})
	}
}

func TestSetDnsMasqConfigurationDualStack(t *testing.T) {
	testcases := []struct {
		name           string
		nodeIP         string
		nodeIPs        []string
		expectedOutput string
		description    string
	}{
		{
			name:           "Single-stack IPv4 - legacy field",
			nodeIP:         "192.168.1.10",
			nodeIPs:        []string{},
			expectedOutput: "SNO_DNSMASQ_IP_OVERRIDE=192.168.1.10",
			description:    "Should use legacy NodeIP field when NodeIPs is empty",
		},
		{
			name:           "Single-stack IPv4 - new field",
			nodeIP:         "",
			nodeIPs:        []string{"192.168.1.10"},
			expectedOutput: "SNO_DNSMASQ_IP_OVERRIDE=192.168.1.10",
			description:    "Should use single IP from NodeIPs field",
		},
		{
			name:           "Single-stack IPv6",
			nodeIP:         "",
			nodeIPs:        []string{"2001:db8::10"},
			expectedOutput: "SNO_DNSMASQ_IP_OVERRIDE=2001:db8::10",
			description:    "Should handle single IPv6 address",
		},
		{
			name:           "Dual-stack IPv4 primary",
			nodeIP:         "",
			nodeIPs:        []string{"192.168.1.10", "2001:db8::10"},
			expectedOutput: "SNO_DNSMASQ_IP_OVERRIDE=192.168.1.10,2001:db8::10",
			description:    "Should join IPv4 and IPv6 addresses with comma",
		},
		{
			name:           "Dual-stack IPv6 primary",
			nodeIP:         "",
			nodeIPs:        []string{"2001:db8::10", "192.168.1.10"},
			expectedOutput: "SNO_DNSMASQ_IP_OVERRIDE=2001:db8::10,192.168.1.10",
			description:    "Should preserve order with IPv6 first",
		},
		{
			name:           "NodeIPs takes precedence",
			nodeIP:         "192.168.1.10",
			nodeIPs:        []string{"10.0.0.5", "2001:db8::10"},
			expectedOutput: "SNO_DNSMASQ_IP_OVERRIDE=10.0.0.5,2001:db8::10",
			description:    "NodeIPs should take precedence over NodeIP",
		},
		{
			name:           "Multiple IPv4 addresses",
			nodeIP:         "",
			nodeIPs:        []string{"192.168.1.10", "10.0.0.5", "172.16.1.10"},
			expectedOutput: "SNO_DNSMASQ_IP_OVERRIDE=192.168.1.10,10.0.0.5,172.16.1.10",
			description:    "Should handle multiple IPv4 addresses",
		},
		{
			name:           "Multiple IPv6 addresses",
			nodeIP:         "",
			nodeIPs:        []string{"2001:db8::10", "2001:db9::20"},
			expectedOutput: "SNO_DNSMASQ_IP_OVERRIDE=2001:db8::10,2001:db9::20",
			description:    "Should handle multiple IPv6 addresses",
		},
		{
			name:           "Complex dual-stack",
			nodeIP:         "",
			nodeIPs:        []string{"192.168.1.10", "2001:db8::10", "10.0.0.5", "2001:db9::20"},
			expectedOutput: "SNO_DNSMASQ_IP_OVERRIDE=192.168.1.10,2001:db8::10,10.0.0.5,2001:db9::20",
			description:    "Should handle complex multi-IP scenario",
		},
		{
			name:           "Empty configuration",
			nodeIP:         "",
			nodeIPs:        []string{},
			expectedOutput: "SNO_DNSMASQ_IP_OVERRIDE=",
			description:    "Should handle empty configuration gracefully",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			// Create a temporary file for the test
			tmpFile := filepath.Join(t.TempDir(), "dnsmasq_overrides")

			// Create a PostPivot instance with a mock logger
			postPivot := &PostPivot{
				log: logrus.New(),
			}

			// Create test seed reconfiguration
			seedReconfig := &clusterconfig_api.SeedReconfiguration{
				ClusterName: "test-cluster",
				BaseDomain:  "example.com",
				NodeIP:      tc.nodeIP,
				NodeIPs:     tc.nodeIPs,
			}

			// Call the function
			err := postPivot.setDnsMasqConfiguration(seedReconfig, tmpFile)
			assert.NoError(t, err, tc.description)

			// Read the generated file
			content, err := os.ReadFile(tmpFile)
			assert.NoError(t, err, "Should be able to read generated file")

			// Verify the content contains expected IP override
			contentStr := string(content)
			assert.Contains(t, contentStr, tc.expectedOutput, tc.description)

			// Verify it also contains cluster name and base domain
			assert.Contains(t, contentStr, "SNO_CLUSTER_NAME_OVERRIDE=test-cluster")
			assert.Contains(t, contentStr, "SNO_BASE_DOMAIN_OVERRIDE=example.com")
		})
	}
}
