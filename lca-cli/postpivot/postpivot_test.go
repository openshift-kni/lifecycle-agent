package postpivot

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	clusterconfig_api "github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedclusterinfo"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

func TestSetNodeIPsIfNotProvided_DualStack(t *testing.T) {
	testcases := []struct {
		name            string
		expectedError   bool
		initialNodeIPs  []string
		expectedNodeIPs []string
	}{
		{
			name:            "NodeIPs provided - nothing to do",
			expectedError:   false,
			initialNodeIPs:  []string{"192.168.1.2"},
			expectedNodeIPs: []string{"192.168.1.2"},
		},
		{
			name:            "Dual stack NodeIPs provided - nothing to do",
			expectedError:   false,
			initialNodeIPs:  []string{"192.168.1.2", "2001:db8::2"},
			expectedNodeIPs: []string{"192.168.1.2", "2001:db8::2"},
		},
		{
			name:            "IPv6 only NodeIPs provided - nothing to do",
			expectedError:   false,
			initialNodeIPs:  []string{"2001:db8::10"},
			expectedNodeIPs: []string{"2001:db8::10"},
		},
		{
			name:            "Multiple IPv4 addresses provided - nothing to do",
			expectedError:   false,
			initialNodeIPs:  []string{"192.168.1.10", "10.0.0.10"},
			expectedNodeIPs: []string{"192.168.1.10", "10.0.0.10"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			log := &logrus.Logger{}
			pp := NewPostPivot(nil, log, nil, "", "", "")
			seedReconfig := &clusterconfig_api.SeedReconfiguration{
				NodeIPs: tc.initialNodeIPs,
			}

			err := pp.setNodeIPsIfNotProvided(context.TODO(), seedReconfig)

			if tc.expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expectedNodeIPs, seedReconfig.NodeIPs)
			}
		})
	}
}

func TestParseKubeletNodeIPs(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		expectedIPs []string
		expectedErr bool
	}{
		{
			name:        "single IP with KUBELET_NODE_IP",
			content:     `Environment="KUBELET_NODE_IP=192.168.1.10"`,
			expectedIPs: []string{"192.168.1.10"},
			expectedErr: false,
		},
		{
			name:        "dual stack with KUBELET_NODE_IPS",
			content:     `Environment="KUBELET_NODE_IPS=192.168.1.10,2001:db8::10"`,
			expectedIPs: []string{"192.168.1.10", "2001:db8::10"},
			expectedErr: false,
		},
		{
			name:        "both variables - prefer KUBELET_NODE_IPS",
			content:     `Environment="KUBELET_NODE_IP=192.168.1.5" "KUBELET_NODE_IPS=192.168.1.10,2001:db8::10"`,
			expectedIPs: []string{"192.168.1.10", "2001:db8::10"},
			expectedErr: false,
		},
		{
			name:        "IPv6 only",
			content:     `Environment="KUBELET_NODE_IP=2001:db8::10"`,
			expectedIPs: []string{"2001:db8::10"},
			expectedErr: false,
		},
		{
			name:        "multiple IPv4 addresses",
			content:     `Environment="KUBELET_NODE_IPS=192.168.1.10,10.0.0.10,172.16.1.10"`,
			expectedIPs: []string{"192.168.1.10", "10.0.0.10", "172.16.1.10"},
			expectedErr: false,
		},
		{
			name:        "spaces in IPs list",
			content:     `Environment="KUBELET_NODE_IPS=192.168.1.10, 2001:db8::10 , 172.16.1.10"`,
			expectedIPs: []string{"192.168.1.10", "2001:db8::10", "172.16.1.10"},
			expectedErr: false,
		},
		{
			name: "multiline environment",
			content: `[Unit]
Description=Test
[Service]
Environment="KUBELET_NODE_IP=192.168.1.10"
ExecStart=/bin/test`,
			expectedIPs: []string{"192.168.1.10"},
			expectedErr: false,
		},
		{
			name:        "no IP variables found",
			content:     `Environment="OTHER_VAR=value"`,
			expectedIPs: nil,
			expectedErr: true,
		},
		{
			name:        "empty content",
			content:     "",
			expectedIPs: nil,
			expectedErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ips, err := parseKubeletNodeIPs(tt.content)

			if tt.expectedErr {
				assert.Error(t, err)
				assert.Nil(t, ips)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectedIPs, ips)
			}
		})
	}
}

func TestSetDnsMasqConfiguration(t *testing.T) {
	var (
		mockController = gomock.NewController(t)
		mockOps        = ops.NewMockOps(mockController)
	)

	defer func() {
		mockController.Finish()
	}()

	testcases := []struct {
		name                string
		expectedError       bool
		seedReconfiguration *clusterconfig_api.SeedReconfiguration
		expectedIP          string
	}{
		{
			name: "Happy flow with single IPv4",
			seedReconfiguration: &clusterconfig_api.SeedReconfiguration{
				BaseDomain:  "new.com",
				ClusterName: "new_name",
				NodeIPs:     []string{"192.167.127.10"},
			},
			expectedError: false,
			expectedIP:    "192.167.127.10",
		},
		{
			name: "IPv6 only",
			seedReconfiguration: &clusterconfig_api.SeedReconfiguration{
				BaseDomain:  "new.com",
				ClusterName: "new_name",
				NodeIPs:     []string{"2001:db8::10"},
			},
			expectedError: false,
			expectedIP:    "2001:db8::10",
		},
		{
			name: "Dual stack primary IPv4",
			seedReconfiguration: &clusterconfig_api.SeedReconfiguration{
				BaseDomain:  "new.com",
				ClusterName: "new_name",
				NodeIPs:     []string{"192.167.127.10", "2001:db8::10"},
			},
			expectedError: false,
			expectedIP:    "192.167.127.10",
		},
		{
			name: "Dual stack primary IPv6",
			seedReconfiguration: &clusterconfig_api.SeedReconfiguration{
				BaseDomain:  "new.com",
				ClusterName: "new_name",
				NodeIPs:     []string{"2001:db8::10", "192.167.127.10"},
			},
			expectedError: false,
			expectedIP:    "2001:db8::10",
		},
		{
			name: "Empty NodeIPs",
			seedReconfiguration: &clusterconfig_api.SeedReconfiguration{
				BaseDomain:  "new.com",
				ClusterName: "new_name",
				NodeIPs:     []string{},
			},
			expectedError: true,
		},
		{
			name: "Legacy NodeIP field (backward compatibility)",
			seedReconfiguration: &clusterconfig_api.SeedReconfiguration{
				BaseDomain:  "new.com",
				ClusterName: "new_name",
				NodeIP:      "192.167.127.10",
				NodeIPs:     []string{}, // Empty, should fall back to NodeIP
			},
			expectedError: true, // Current implementation expects NodeIPs to be populated
		},
	}

	for _, tc := range testcases {
		tmpDir := t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			log := &logrus.Logger{}
			pp := NewPostPivot(nil, log, mockOps, "", "", "")
			confFile := path.Join(tmpDir, "override")
			err := pp.setDnsMasqConfiguration(tc.seedReconfiguration, confFile)
			if !tc.expectedError && err != nil {
				t.Errorf("unexpected error: %v", err)
			} else if tc.expectedError && err == nil {
				t.Errorf("expected error but got none")
			}

			if !tc.expectedError && err == nil {
				// Verify the configuration file content
				content, readErr := os.ReadFile(confFile)
				if readErr != nil {
					t.Errorf("failed to read config file: %v", readErr)
				}
				configStr := string(content)
				assert.Contains(t, configStr, fmt.Sprintf("SNO_DNSMASQ_IP_OVERRIDE=%s", tc.expectedIP))
				assert.Contains(t, configStr, fmt.Sprintf("SNO_CLUSTER_NAME_OVERRIDE=%s", tc.seedReconfiguration.ClusterName))
				assert.Contains(t, configStr, fmt.Sprintf("SNO_BASE_DOMAIN_OVERRIDE=%s", tc.seedReconfiguration.BaseDomain))
			}
		})
	}
}

func TestGetMachineNetworksFromSeedReconfig(t *testing.T) {
	tests := []struct {
		name             string
		seedReconfig     *clusterconfig_api.SeedReconfiguration
		expectedNetworks []string
	}{
		{
			name: "new field populated",
			seedReconfig: &clusterconfig_api.SeedReconfiguration{
				MachineNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
			},
			expectedNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
		},
		{
			name: "legacy field populated",
			seedReconfig: &clusterconfig_api.SeedReconfiguration{
				MachineNetwork: "192.168.1.0/24",
			},
			expectedNetworks: []string{"192.168.1.0/24"},
		},
		{
			name: "both fields populated - new takes precedence",
			seedReconfig: &clusterconfig_api.SeedReconfiguration{
				MachineNetwork:  "10.0.0.0/8",
				MachineNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
			},
			expectedNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
		},
		{
			name: "both fields empty",
			seedReconfig: &clusterconfig_api.SeedReconfiguration{
				MachineNetwork:  "",
				MachineNetworks: []string{},
			},
			expectedNetworks: []string{},
		},
		{
			name: "legacy field empty, new field populated",
			seedReconfig: &clusterconfig_api.SeedReconfiguration{
				MachineNetwork:  "",
				MachineNetworks: []string{"192.168.1.0/24"},
			},
			expectedNetworks: []string{"192.168.1.0/24"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getMachineNetworksFromSeedReconfig(tt.seedReconfig)
			assert.Equal(t, tt.expectedNetworks, result)
		})
	}
}

func TestSeedClusterInfoNodeIPs(t *testing.T) {
	tests := []struct {
		name            string
		seedClusterInfo *seedclusterinfo.SeedClusterInfo
		expectedNodeIPs []string
	}{
		{
			name: "new field populated",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs: []string{"192.168.1.10", "2001:db8::10"},
			},
			expectedNodeIPs: []string{"192.168.1.10", "2001:db8::10"},
		},
		{
			name: "legacy field populated",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIP: "192.168.1.10",
			},
			expectedNodeIPs: []string{"192.168.1.10"},
		},
		{
			name: "both fields populated - new takes precedence",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIP:  "10.0.0.10",
				NodeIPs: []string{"192.168.1.10", "2001:db8::10"},
			},
			expectedNodeIPs: []string{"192.168.1.10", "2001:db8::10"},
		},
		{
			name: "both fields empty",
			seedClusterInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIP:  "",
				NodeIPs: []string{},
			},
			expectedNodeIPs: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := seedClusterInfoNodeIPs(tt.seedClusterInfo)
			assert.Equal(t, tt.expectedNodeIPs, result)
		})
	}
}

func TestSeedReconfigurationNodeIPs(t *testing.T) {
	tests := []struct {
		name                string
		seedReconfiguration *clusterconfig_api.SeedReconfiguration
		expectedNodeIPs     []string
	}{
		{
			name: "new field populated",
			seedReconfiguration: &clusterconfig_api.SeedReconfiguration{
				NodeIPs: []string{"192.168.1.10", "2001:db8::10"},
			},
			expectedNodeIPs: []string{"192.168.1.10", "2001:db8::10"},
		},
		{
			name: "legacy field populated",
			seedReconfiguration: &clusterconfig_api.SeedReconfiguration{
				NodeIP: "192.168.1.10",
			},
			expectedNodeIPs: []string{"192.168.1.10"},
		},
		{
			name: "both fields populated - new takes precedence",
			seedReconfiguration: &clusterconfig_api.SeedReconfiguration{
				NodeIP:  "10.0.0.10",
				NodeIPs: []string{"192.168.1.10", "2001:db8::10"},
			},
			expectedNodeIPs: []string{"192.168.1.10", "2001:db8::10"},
		},
		{
			name: "both fields empty",
			seedReconfiguration: &clusterconfig_api.SeedReconfiguration{
				NodeIP:  "",
				NodeIPs: []string{},
			},
			expectedNodeIPs: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := seedReconfigurationNodeIPs(tt.seedReconfiguration)
			assert.Equal(t, tt.expectedNodeIPs, result)
		})
	}
}

func TestValidateIPAndMachineNetworkConsistency(t *testing.T) {
	tests := []struct {
		name          string
		seedInfo      *seedclusterinfo.SeedClusterInfo
		reconfig      *clusterconfig_api.SeedReconfiguration
		expectError   bool
		errorContains string
	}{
		{
			name: "single-stack ipv4 success without seed machine networks",
			seedInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs:         []string{"192.168.1.10"},
				MachineNetworks: []string{},
			},
			reconfig: &clusterconfig_api.SeedReconfiguration{
				NodeIPs:         []string{"10.0.0.2"},
				MachineNetworks: []string{"10.0.0.0/24"},
			},
			expectError:   false,
			errorContains: "",
		},
		{
			name: "dual-stack success with seed machine networks",
			seedInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs:         []string{"192.168.1.10", "2001:db8::10"},
				MachineNetworks: []string{"192.168.1.0/24", "2001:db8::/64"},
			},
			reconfig: &clusterconfig_api.SeedReconfiguration{
				NodeIPs:         []string{"10.0.0.2", "2001:db8::2"},
				MachineNetworks: []string{"10.0.0.0/24", "2001:db8::/64"},
			},
			expectError:   false,
			errorContains: "",
		},
		{
			name: "error when seed node IPs empty",
			seedInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs: []string{},
			},
			reconfig: &clusterconfig_api.SeedReconfiguration{
				NodeIPs:         []string{"10.0.0.2"},
				MachineNetworks: []string{"10.0.0.0/24"},
			},
			expectError:   true,
			errorContains: "node IPs must be provided",
		},
		{
			name: "error when reconfig node IPs empty",
			seedInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs: []string{"192.168.1.10"},
			},
			reconfig: &clusterconfig_api.SeedReconfiguration{
				NodeIPs:         []string{},
				MachineNetworks: []string{"10.0.0.0/24"},
			},
			expectError:   true,
			errorContains: "node IPs must be provided",
		},
		{
			name: "error node IP count mismatch",
			seedInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs: []string{"192.168.1.10", "2001:db8::10"},
			},
			reconfig: &clusterconfig_api.SeedReconfiguration{
				NodeIPs:         []string{"10.0.0.2"},
				MachineNetworks: []string{"10.0.0.0/24"},
			},
			expectError:   true,
			errorContains: "node IPs count mismatch",
		},
		{
			name: "error node IP family mismatch at index",
			seedInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs: []string{"192.168.1.10", "2001:db8::10"},
			},
			reconfig: &clusterconfig_api.SeedReconfiguration{
				NodeIPs:         []string{"2001:db8::2", "10.0.0.2"},
				MachineNetworks: []string{"2001:db8::/64", "10.0.0.0/24"},
			},
			expectError:   true,
			errorContains: "node IP family mismatch",
		},
		{
			name: "error when machine networks count differs from node IPs",
			seedInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs: []string{"192.168.1.10", "2001:db8::10"},
			},
			reconfig: &clusterconfig_api.SeedReconfiguration{
				NodeIPs:         []string{"10.0.0.2", "2001:db8::2"},
				MachineNetworks: []string{"10.0.0.0/24"},
			},
			expectError:   true,
			errorContains: "machineNetworks count",
		},
		{
			name: "error when machine networks family mismatches node IP family",
			seedInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs: []string{"192.168.1.10", "2001:db8::10"},
			},
			reconfig: &clusterconfig_api.SeedReconfiguration{
				NodeIPs:         []string{"10.0.0.2", "2001:db8::2"},
				MachineNetworks: []string{"2001:db8::/64", "10.0.0.0/24"},
			},
			expectError:   true,
			errorContains: "machineNetwork family at index",
		},
		{
			name: "error when seed machine networks count mismatches reconfig",
			seedInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs:         []string{"192.168.1.10", "2001:db8::10"},
				MachineNetworks: []string{"192.168.1.0/24"},
			},
			reconfig: &clusterconfig_api.SeedReconfiguration{
				NodeIPs:         []string{"10.0.0.2", "2001:db8::2"},
				MachineNetworks: []string{"10.0.0.0/24", "2001:db8::/64"},
			},
			expectError:   true,
			errorContains: "seed machineNetworks count",
		},
		{
			name: "error when seed machine networks family mismatches reconfig",
			seedInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs:         []string{"192.168.1.10", "2001:db8::10"},
				MachineNetworks: []string{"2001:db8::/64", "192.168.1.0/24"},
			},
			reconfig: &clusterconfig_api.SeedReconfiguration{
				NodeIPs:         []string{"10.0.0.2", "2001:db8::2"},
				MachineNetworks: []string{"10.0.0.0/24", "2001:db8::/64"},
			},
			expectError:   true,
			errorContains: "machineNetwork family mismatch",
		},
		{
			name: "error invalid seed IP",
			seedInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs: []string{"not-an-ip"},
			},
			reconfig: &clusterconfig_api.SeedReconfiguration{
				NodeIPs:         []string{"10.0.0.2"},
				MachineNetworks: []string{"10.0.0.0/24"},
			},
			expectError:   true,
			errorContains: "invalid seed node IP",
		},
		{
			name: "error invalid reconfiguration IP",
			seedInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs: []string{"10.0.0.1"},
			},
			reconfig: &clusterconfig_api.SeedReconfiguration{
				NodeIPs:         []string{"bad-ip"},
				MachineNetworks: []string{"10.0.0.0/24"},
			},
			expectError:   true,
			errorContains: "invalid reconfiguration node IP",
		},
		{
			name: "error invalid machine network cidr",
			seedInfo: &seedclusterinfo.SeedClusterInfo{
				NodeIPs: []string{"10.0.0.1"},
			},
			reconfig: &clusterconfig_api.SeedReconfiguration{
				NodeIPs:         []string{"10.0.0.2"},
				MachineNetworks: []string{"badcidr"},
			},
			expectError:   true,
			errorContains: "invalid reconfiguration machineNetwork",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateIPAndMachineNetworkConsistency(tt.seedInfo, tt.reconfig)
			if tt.expectError {
				assert.Error(t, err)
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestApplyNMStateConfiguration(t *testing.T) {
	var (
		mockController = gomock.NewController(t)
		mockOps        = ops.NewMockOps(mockController)
	)

	defer func() {
		mockController.Finish()
	}()

	testcases := []struct {
		name                string
		expectedError       bool
		seedReconfiguration *clusterconfig_api.SeedReconfiguration
	}{
		{
			name: "Happy flow",
			seedReconfiguration: &clusterconfig_api.SeedReconfiguration{
				RawNMStateConfig: `
interfaces:
  - name: wan0
    type: ethernet
    state: up
    identifier: mac-address
    mac-address: 1e:bd:23:e9:fb:94
    ipv4:
      enabled: true
      dhcp: true
    ipv6:
      enabled: true
      dhcp: true
      autoconf: true`,
			},
			expectedError: false,
		},
		{
			name: "Failure in nmstatectl apply",
			seedReconfiguration: &clusterconfig_api.SeedReconfiguration{
				RawNMStateConfig: `
interfaces:
  - name: wan0
    type: ethernet
    state: up
    identifier: mac-address
    mac-address: 1e:bd:23:e9:fb:94
    ipv4:
      enabled: true
      dhcp: true
    ipv6:
      enabled: true
      dhcp: true
      autoconf: true`,
			},
			expectedError: true,
		},
	}

	for _, tc := range testcases {
		tmpDir := t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			log := &logrus.Logger{}
			nmstateFile := path.Join(tmpDir, "nmstate.yaml")
			pp := NewPostPivot(nil, log, mockOps, "", tmpDir, "")
			if tc.expectedError {
				mockOps.EXPECT().RunInHostNamespace("nmstatectl", "apply", nmstateFile).Return("", fmt.Errorf("Dummy"))
			} else {
				mockOps.EXPECT().RunInHostNamespace("nmstatectl", "apply", nmstateFile).Return("", nil)
			}

			err := pp.applyNMStateConfiguration(tc.seedReconfiguration)
			if !tc.expectedError {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				var data []interface{}
				errF := utils.ReadYamlOrJSONFile(nmstateFile, data)
				if errF != nil {
					t.Errorf("unexpected error: %v", err)
				}
			} else {
				assert.Equal(t, err != nil, tc.expectedError)
			}
		})
	}
}

func TestCreatePullSecretFile(t *testing.T) {
	var (
		mockController = gomock.NewController(t)
		mockOps        = ops.NewMockOps(mockController)
		scheme         = runtime.NewScheme()
	)

	defer func() {
		mockController.Finish()
	}()

	testcases := []struct {
		name          string
		pullSecret    string
		expectedError bool
	}{
		{
			name:          "Happy flow, pull secret was set",
			pullSecret:    "pull-secret",
			expectedError: false,
		},
		{
			name:          "Pull secret was not set, should fail",
			pullSecret:    "",
			expectedError: true,
		},
	}

	for _, tc := range testcases {
		tmpDir := t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			log := &logrus.Logger{}
			pullSecretFile := path.Join(tmpDir, "config.json")
			clientgoscheme.AddToScheme(scheme)
			pp := NewPostPivot(scheme, log, mockOps, "", tmpDir, "")
			err := pp.createPullSecretFile(tc.pullSecret, pullSecretFile)
			assert.Equal(t, err != nil, tc.expectedError)
			if tc.pullSecret != "" {
				ps, err := os.ReadFile(pullSecretFile)
				if err != nil {
					t.Errorf("unexpected error while reading pull secret file: %v", err)
				}
				assert.Equal(t, "pull-secret", string(ps))

			} else if _, err := os.Stat(pullSecretFile); err == nil {
				t.Errorf("expected no pull secret file to be created")
			}
		})
	}
}

func TestSetHostname(t *testing.T) {
	var (
		mockController = gomock.NewController(t)
	)
	defer func() {
		mockController.Finish()
	}()

	testcases := []struct {
		name          string
		hostname      string
		osHostname    string
		expectedError bool
	}{
		{
			name:          "Happy flow - hostname set",
			hostname:      "test",
			expectedError: false,
			osHostname:    "",
		},
		{
			name:          "Happy flow - hostname empty but GetHostname returns a valid hostname",
			hostname:      "",
			expectedError: false,
			osHostname:    "goodOne",
		},
		{
			name:          "Hostname is localhost, should fail",
			hostname:      "localhost",
			expectedError: true,
			osHostname:    "",
		},
		{
			name:          "Hostname is empty, os hostname is localhost should fail",
			hostname:      "localhost",
			expectedError: true,
			osHostname:    "",
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			log := &logrus.Logger{}
			mockOps := ops.NewMockOps(mockController)
			pp := NewPostPivot(nil, log, mockOps, "", "", "")
			if tc.hostname != "" && tc.hostname != localhost {
				mockOps.EXPECT().RunInHostNamespace("hostnamectl", "set-hostname", tc.hostname).Return("", nil).Times(1)
			} else if tc.hostname == "" {
				mockOps.EXPECT().RunInHostNamespace("hostnamectl", "set-hostname", tc.hostname).Return("", nil).Times(0)
				mockOps.EXPECT().GetHostname().Return(tc.osHostname, nil).Times(1)
			}

			hostname, err := pp.setHostname(tc.hostname)
			assert.Equal(t, tc.expectedError, err != nil, err)
			if tc.hostname == "" {
				assert.Equal(t, tc.osHostname, hostname)
			} else if !tc.expectedError {
				assert.Equal(t, tc.hostname, hostname)
			}
		})
	}
}

func TestWaitForConfiguration(t *testing.T) {
	var deviceName = "testDevice"
	testcases := []struct {
		name                      string
		configurationFolderExists bool
		expectedError             bool
		listBlockDevicesSucceeds  bool
		mountSucceeds             bool
	}{
		{
			name:                      "Configuration folder exists",
			configurationFolderExists: true,
			expectedError:             false,
		},
		{
			name:                      "Block device with label was added",
			configurationFolderExists: false,
			listBlockDevicesSucceeds:  true,
			mountSucceeds:             true,
			expectedError:             false,
		},
		{
			name:                      "Failed to list block devices",
			configurationFolderExists: false,
			listBlockDevicesSucceeds:  false,
			expectedError:             true,
		},
		{
			name:                      "Failed to mount block device and it will exit wait function",
			configurationFolderExists: false,
			listBlockDevicesSucceeds:  true,
			mountSucceeds:             false,
			expectedError:             true,
		},
	}

	for _, tc := range testcases {
		ctrl := gomock.NewController(t)
		mockOps := ops.NewMockOps(ctrl)
		log := &logrus.Logger{}
		pp := NewPostPivot(nil, log, mockOps, "", "", "")
		ctx, cancel := context.WithCancel(context.TODO())
		tmpDir := t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			configFolder := path.Join(tmpDir, "config")
			if tc.configurationFolderExists {
				if err := os.MkdirAll(configFolder, 0o700); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
			if tc.listBlockDevicesSucceeds {
				mockOps.EXPECT().ListBlockDevices().Return([]ops.BlockDevice{{Name: deviceName,
					Label: clusterconfig_api.BlockDeviceLabel}}, nil).Times(1)
				if tc.mountSucceeds {
					mockOps.EXPECT().Mount(deviceName, gomock.Any()).Return(nil).Times(1)
					mockOps.EXPECT().Umount(deviceName).Return(nil).Times(1)
				} else {
					mockOps.EXPECT().Mount(deviceName, gomock.Any()).Return(fmt.Errorf("dummy")).Times(1)
				}
			} else if !tc.configurationFolderExists {
				mockOps.EXPECT().ListBlockDevices().Return(nil, fmt.Errorf("dummy")).Do(cancel).Times(1)
			}

			err := pp.waitForConfiguration(ctx, configFolder, configFolder)
			assert.Equal(t, tc.expectedError, err != nil)
		})
	}
}

func TestNetworkConfiguration(t *testing.T) {
	seedReconfiguration := &clusterconfig_api.SeedReconfiguration{
		BaseDomain:  "new.com",
		ClusterName: "new_name",
		NodeIPs:     []string{"192.167.127.10"},
		Hostname:    "test",
	}

	testcases := []struct {
		name                  string
		expectedError         bool
		restartNMSuccess      bool
		restartDNSMASQSuccess bool
	}{
		{
			name:                  "Happy flow",
			restartNMSuccess:      true,
			restartDNSMASQSuccess: true,
			expectedError:         false,
		},
		{
			name:                  "Restart nm failed",
			restartNMSuccess:      false,
			restartDNSMASQSuccess: true,
			expectedError:         true,
		},
		{
			name:                  "Restart dnsmasq failed",
			restartNMSuccess:      true,
			restartDNSMASQSuccess: false,
			expectedError:         true,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			ctrl := gomock.NewController(t)
			mockOps := ops.NewMockOps(ctrl)
			log := &logrus.Logger{}
			pp := NewPostPivot(nil, log, mockOps, "", tmpDir, "")
			nmConnectionFolder = path.Join(tmpDir, "nmfiles")
			dnsmasqOverrides = path.Join(tmpDir, "dnsmasqoverrides")

			if tc.restartNMSuccess {
				mockOps.EXPECT().SystemctlAction("restart", nmService).Return("", nil).Times(1)
				if tc.restartDNSMASQSuccess {
					mockOps.EXPECT().RunInHostNamespace("hostnamectl", "set-hostname", "test").Return("", nil).Times(1)
					mockOps.EXPECT().SystemctlAction("restart", dnsmasqService).Return("", nil).Times(1)
				} else {
					mockOps.EXPECT().RunInHostNamespace("hostnamectl", "set-hostname", "test").Return("", nil).Times(1)
					mockOps.EXPECT().SystemctlAction("restart", dnsmasqService).Return("", fmt.Errorf("dummy")).Times(1)
				}
			} else {
				mockOps.EXPECT().SystemctlAction("restart", nmService).Return("", fmt.Errorf("dummy")).Times(1)
			}

			err := pp.networkConfiguration(context.TODO(), seedReconfiguration)
			assert.Equal(t, tc.expectedError, err != nil, err)
		})
	}
}

func fakeICSP(name string) *operatorv1alpha1.ImageContentSourcePolicy {
	return &operatorv1alpha1.ImageContentSourcePolicy{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ImageContentSourcePolicy",
			APIVersion: operatorv1alpha1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: operatorv1alpha1.ImageContentSourcePolicySpec{
			RepositoryDigestMirrors: []operatorv1alpha1.RepositoryDigestMirrors{
				{Source: "icspData"},
			},
		},
	}
}

func fakeManifests(tmpDir string) error {
	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: corev1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "user-ca-bundle",
			Namespace: "openshift-config",
		},
		Data: map[string]string{
			"test-bundle": "data",
		},
	}
	if err := utils.MarshalToFile(cm, filepath.Join(tmpDir, "user-ca-bundle.json")); err != nil {
		return fmt.Errorf("Unexpected error: %v", err)
	}

	icspList := &operatorv1alpha1.ImageContentSourcePolicyList{}
	icsp1 := fakeICSP("icsp1")
	icsp1.SetLabels(map[string]string{"test-label": "true"})
	icsp2 := fakeICSP("icsp2")
	icsp2.SetLabels(map[string]string{"test-label": "true"})
	icspList.Items = append(icspList.Items, *icsp1, *icsp2)
	if err := utils.MarshalToFile(icspList, filepath.Join(tmpDir, "icsps.json")); err != nil {
		return fmt.Errorf("Unexpected error: %v", err)
	}
	return nil
}

func fakeManifestsSingleFile(tmpDir string) error {
	resources := `---
apiVersion: v1
kind: Namespace
metadata:
  name: ibi-post-config

---
apiVersion: v1
kind: ConfigMap
metadata:
  name: ibi-config
  namespace: ibi-post-config
data:
  config-key1: "ibi"

---
apiVersion: v1
kind: ConfigMap
metadata:
  name: ibi-config-2
  namespace: ibi-post-config
data:
  config-key2: "ibi"
`
	filePath := filepath.Join(tmpDir, "extramManifests.yaml")
	err := os.WriteFile(filePath, []byte(resources), 0o600)
	if err != nil {
		return fmt.Errorf("failed to write file to %s: %w", filePath, err)
	}
	return nil
}

func TestApplyManifestsWithMultipleResources(t *testing.T) {
	var (
		mockController = gomock.NewController(t)
		mockOps        = ops.NewMockOps(mockController)
	)

	defer func() {
		mockController.Finish()
	}()

	testscheme := clientgoscheme.Scheme
	testscheme.AddKnownTypes(operatorv1alpha1.GroupVersion,
		&operatorv1alpha1.ImageContentSourcePolicyList{})

	// create static rest mapper
	namespaceGvk := corev1.SchemeGroupVersion.WithKind("Namespace")
	cmGvk := corev1.SchemeGroupVersion.WithKind("ConfigMap")
	restMapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		operatorv1alpha1.GroupVersion,
		corev1.SchemeGroupVersion,
	})
	restMapper.Add(namespaceGvk, meta.RESTScopeRoot)
	restMapper.Add(cmGvk, meta.RESTScopeNamespace)

	// create objs mappings
	namespaceGvkMapping, _ := restMapper.RESTMapping(namespaceGvk.GroupKind(), corev1.SchemeGroupVersion.Version)
	cmMapping, _ := restMapper.RESTMapping(cmGvk.GroupKind(), corev1.SchemeGroupVersion.Version)

	tmpDir := t.TempDir()
	log := &logrus.Logger{}
	pp := NewPostPivot(nil, log, mockOps, "", tmpDir, "")

	// fake manifests in a single file
	if err := fakeManifestsSingleFile(tmpDir); err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// fake dynamic client
	dynamicClient := dynamicfake.NewSimpleDynamicClient(testscheme)

	// apply manifests
	err := pp.applyManifests(context.Background(), tmpDir, dynamicClient, restMapper)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// validate applied resources
	opts := metav1.ListOptions{}
	namespaceResource := dynamicClient.Resource(namespaceGvkMapping.Resource)
	namespaces, err := namespaceResource.List(context.Background(), opts)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	assert.Equal(t, 1, len(namespaces.Items))
	assert.Equal(t, "ibi-post-config", namespaces.Items[0].GetName())

	cmResource := dynamicClient.Resource(cmMapping.Resource)
	configMaps, err := cmResource.List(context.Background(), opts)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	assert.Equal(t, 2, len(configMaps.Items))
	assert.Equal(t, "ibi-post-config", configMaps.Items[0].GetNamespace())
	assert.Equal(t, "ibi-post-config", configMaps.Items[1].GetNamespace())
}

func TestApplyManifests(t *testing.T) {
	var (
		mockController = gomock.NewController(t)
		mockOps        = ops.NewMockOps(mockController)
	)

	defer func() {
		mockController.Finish()
	}()

	testscheme := clientgoscheme.Scheme
	testscheme.AddKnownTypes(operatorv1alpha1.GroupVersion,
		&operatorv1alpha1.ImageContentSourcePolicyList{})

	// create static rest mapper
	icspGvk := operatorv1alpha1.GroupVersion.WithKind("ImageContentSourcePolicy")
	cmGvk := corev1.SchemeGroupVersion.WithKind("ConfigMap")
	restMapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		operatorv1alpha1.GroupVersion,
		corev1.SchemeGroupVersion,
	})
	restMapper.Add(icspGvk, meta.RESTScopeRoot)
	restMapper.Add(cmGvk, meta.RESTScopeNamespace)

	// create objs mappings
	icspMapping, _ := restMapper.RESTMapping(icspGvk.GroupKind(), operatorv1alpha1.GroupVersion.Version)
	cmMapping, _ := restMapper.RESTMapping(cmGvk.GroupKind(), corev1.SchemeGroupVersion.Version)

	testcases := []struct {
		name         string
		existingObjs []runtime.Object
	}{
		{
			name:         "objs do not exist",
			existingObjs: []runtime.Object{},
		},
		{
			name:         "objs exist",
			existingObjs: []runtime.Object{fakeICSP("icsp1"), fakeICSP("icsp2")},
		},
	}

	for _, tc := range testcases {
		tmpDir := t.TempDir()
		log := &logrus.Logger{}
		t.Run(tc.name, func(t *testing.T) {
			pp := NewPostPivot(nil, log, mockOps, "", tmpDir, "")

			// fake manifest files
			if err := fakeManifests(tmpDir); err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			// fake dynamic client
			dynamicClient := dynamicfake.NewSimpleDynamicClient(testscheme, tc.existingObjs...)

			// check the number of resource prior to apply
			opts := metav1.ListOptions{}
			icspResource := dynamicClient.Resource(icspMapping.Resource)
			currentIcsps, err := icspResource.List(context.Background(), opts)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			cmResource := dynamicClient.Resource(cmMapping.Resource)
			currentCms, err := cmResource.List(context.Background(), opts)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
			assert.Equal(t, len(tc.existingObjs), len(currentIcsps.Items)+len(currentCms.Items))

			// apply manifests
			err = pp.applyManifests(context.Background(), tmpDir, dynamicClient, restMapper)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			// validate applied resources
			currentIcsps, err = icspResource.List(context.Background(), opts)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
			assert.Equal(t, 2, len((currentIcsps.Items)))
			for _, icsp := range currentIcsps.Items {
				assert.Equal(t, map[string]string{"test-label": "true"}, icsp.GetLabels())
			}

			currentCms, err = cmResource.List(context.Background(), opts)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
			assert.Equal(t, 1, len(currentCms.Items))
			assert.Equal(t, "user-ca-bundle", currentCms.Items[0].GetName())
			assert.Equal(t, "openshift-config", currentCms.Items[0].GetNamespace())
		})
	}
}

func TestSetNodeIPHint(t *testing.T) {
	testcases := []struct {
		name          string
		nodeCidrs     []string
		expectedError bool
		expectedHint  string
	}{
		{
			name:          "Wrong cidr provided, expected error",
			nodeCidrs:     []string{"192.167.1.2"},
			expectedError: true,
		},
		{
			name:          "Happy flow single stack",
			nodeCidrs:     []string{"192.167.1.0/24"},
			expectedError: false,
			expectedHint:  "KUBELET_NODEIP_HINT=192.167.1.0",
		},
		{
			name:          "Happy flow dual stack",
			nodeCidrs:     []string{"192.167.1.0/24", "2001:db8::/64"},
			expectedError: false,
			expectedHint:  "KUBELET_NODEIP_HINT=192.167.1.0 2001:db8::",
		},
		{
			name:          "IPv6 only",
			nodeCidrs:     []string{"2001:db8::/64"},
			expectedError: false,
			expectedHint:  "KUBELET_NODEIP_HINT=2001:db8::",
		},
		{
			name:          "No cidrs provided",
			nodeCidrs:     []string{},
			expectedError: false,
		},
		{
			name:          "Multiple IPv4 cidrs",
			nodeCidrs:     []string{"192.168.1.0/24", "10.0.0.0/8"},
			expectedError: false,
			expectedHint:  "KUBELET_NODEIP_HINT=192.168.1.0 10.0.0.0",
		},
	}

	for _, tc := range testcases {
		tmpDir := t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			log := &logrus.Logger{}
			pp := NewPostPivot(nil, log, nil, "", tmpDir, "")
			nodeIPHintFile = path.Join(tmpDir, "hint")
			err := pp.setNodeIpHint(tc.nodeCidrs)
			assert.Equal(t, tc.expectedError, err != nil, err)
			if !tc.expectedError {
				if len(tc.nodeCidrs) > 0 {
					hint, err := os.ReadFile(nodeIPHintFile)
					if err != nil {
						t.Errorf("unexpected error: %v", err)
					}
					assert.Equal(t, tc.expectedHint, string(hint))
				} else if _, err := os.Stat(nodeIPHintFile); err == nil {
					t.Errorf("expected no node ip hint file to be created")
				}
			}
		})
	}
}

func TestProxyAndProxyStatus(t *testing.T) {
	testcases := []struct {
		name           string
		proxy          *clusterconfig_api.Proxy
		seedHasProxy   bool
		setMachineCIDR bool
		expectedError  bool
	}{
		{
			name: "Happy flow, no statusProxy",
			proxy: &clusterconfig_api.Proxy{
				HTTPProxy:  "http://proxy.com:8080",
				HTTPSProxy: "https://proxy.com:8080",
				NoProxy:    "user_data"},
			seedHasProxy:   true,
			setMachineCIDR: true,
			expectedError:  false,
		},
		{
			name: "seed doesn't have proxy, cluster has proxy",
			proxy: &clusterconfig_api.Proxy{
				HTTPProxy:  "http://proxy.com:8080",
				HTTPSProxy: "https://proxy.com:8080",
				NoProxy:    "user_data"},
			seedHasProxy:   false,
			setMachineCIDR: true,
			expectedError:  true,
		},
		{
			name:           "seed have proxy, cluster doesn't have proxy",
			proxy:          nil,
			seedHasProxy:   true,
			setMachineCIDR: true,
			expectedError:  true,
		},
		{
			name: "Machine cidr not set",
			proxy: &clusterconfig_api.Proxy{
				HTTPProxy:  "http://proxy.com:8080",
				HTTPSProxy: "https://proxy.com:8080",
				NoProxy:    "user_data"},
			seedHasProxy:   true,
			setMachineCIDR: false,
			expectedError:  true,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			log := &logrus.Logger{}
			pp := NewPostPivot(nil, log, nil, "", "", "")
			seedReconfiguration := &clusterconfig_api.SeedReconfiguration{
				BaseDomain:  "new.com",
				ClusterName: "new_name",
			}

			seedInfo := &seedclusterinfo.SeedClusterInfo{
				ClusterNetworks: []string{"1.1.1.1/24", "2.2.2.2/24"},
				ServiceNetworks: []string{"3.3.3.3/24", "4.4.4.4/24"},
				HasProxy:        tc.seedHasProxy,
			}
			if tc.setMachineCIDR {
				seedReconfiguration.MachineNetwork = "8.8.8.8/24"
			}

			if tc.proxy != nil {
				seedReconfiguration.Proxy = &clusterconfig_api.Proxy{}
				*seedReconfiguration.Proxy = *tc.proxy
			}

			err := pp.setProxyAndProxyStatus(seedReconfiguration, seedInfo)
			assert.Equal(t, tc.expectedError, err != nil, err)
			if !tc.expectedError {
				assert.Equal(t, tc.proxy.HTTPProxy, seedReconfiguration.Proxy.HTTPProxy)
				assert.Equal(t, tc.proxy.HTTPSProxy, seedReconfiguration.Proxy.HTTPSProxy)
				assert.Contains(t, seedReconfiguration.Proxy.NoProxy, fmt.Sprintf("api-int.%s.%s", seedReconfiguration.ClusterName,
					seedReconfiguration.BaseDomain))
				for _, clusterNetwork := range seedInfo.ClusterNetworks {
					assert.Contains(t, seedReconfiguration.Proxy.NoProxy, clusterNetwork)
				}

				for _, serviceNetwork := range seedInfo.ServiceNetworks {
					assert.Contains(t, seedReconfiguration.Proxy.NoProxy, serviceNetwork)

				}
				assert.Contains(t, seedReconfiguration.Proxy.NoProxy, seedReconfiguration.MachineNetwork)
				assert.Contains(t, seedReconfiguration.Proxy.NoProxy, tc.proxy.NoProxy)

				assert.Equal(t, seedReconfiguration.Proxy, seedReconfiguration.StatusProxy)
			}

		})
	}
}

// test nodeLabelsProvided
func TestNodeLabelsProvided(t *testing.T) {
	testcases := []struct {
		name           string
		nodeLabels     map[string]string
		labelsToFilter map[string]string
		node           *corev1.Node
		expectedError  bool
	}{
		{
			name: "Happy flow",
			nodeLabels: map[string]string{
				"test": "test",
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Labels: map[string]string{
						"node-role.kubernetes.io/master": "",
					},
				},
			},
			expectedError: false,
		},
		{
			name: "Happy flow filter default values test",
			nodeLabels: map[string]string{
				"test":                                  "test",
				"beta.kubernetes.io/arch":               "test",
				"beta.kubernetes.io/os":                 "test",
				"kubernetes.io/arch":                    "test",
				"kubernetes.io/hostname":                "test",
				"kubernetes.io/os":                      "test",
				"node-role.kubernetes.io/control-plane": "test",
				"node.openshift.io/os_id":               "test",
				"node-role.kubernetes.io/worker":        "test",
			},
			labelsToFilter: map[string]string{
				"beta.kubernetes.io/arch":               "test",
				"beta.kubernetes.io/os":                 "test",
				"kubernetes.io/arch":                    "test",
				"kubernetes.io/hostname":                "test",
				"kubernetes.io/os":                      "test",
				"node-role.kubernetes.io/control-plane": "test",
				"node.openshift.io/os_id":               "test",
				"node-role.kubernetes.io/worker":        "test",
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Labels: map[string]string{
						"node-role.kubernetes.io/master": "",
					},
				},
			},
			expectedError: false,
		},
		{
			name:          "No labels provided",
			nodeLabels:    map[string]string{},
			expectedError: false,
		},
		{
			name: "Only default labels provided",
			nodeLabels: map[string]string{
				"beta.kubernetes.io/arch":               "test",
				"beta.kubernetes.io/os":                 "test",
				"kubernetes.io/arch":                    "test",
				"kubernetes.io/hostname":                "test",
				"kubernetes.io/os":                      "test",
				"node-role.kubernetes.io/control-plane": "test",
				"node.openshift.io/os_id":               "test",
				"node-role.kubernetes.io/worker":        "test",
			},
			expectedError: false,
		},
		{
			name:          "No node found",
			nodeLabels:    map[string]string{"test": "test"},
			expectedError: true,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			log := &logrus.Logger{}
			pp := NewPostPivot(nil, log, nil, "", "", "")
			localScheme := runtime.NewScheme()
			_ = corev1.AddToScheme(localScheme)
			client := fake.NewClientBuilder().WithScheme(localScheme).Build()
			ctx := context.TODO()
			// Create the node using the dynamic client
			if tc.node != nil {
				err := client.Create(ctx, tc.node)
				assert.Nil(t, err)
			}

			err := pp.setNodeLabels(ctx, client, tc.nodeLabels, 2*time.Second)
			assert.Equal(t, tc.expectedError, err != nil, err)
			if !tc.expectedError && tc.node != nil {
				node := &corev1.Node{}
				err = client.Get(ctx, types.NamespacedName{Name: tc.node.Name}, node)
				assert.Nil(t, err)
				for k, v := range tc.nodeLabels {
					val, ok := node.Labels[k]
					if _, okFilter := tc.labelsToFilter[k]; !okFilter {
						assert.Equal(t, ok, true)
						assert.Equal(t, val, v)
					} else {
						assert.Equal(t, ok, false)
					}
				}
			}

		})
	}
}
