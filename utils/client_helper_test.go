package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

func TestGetNodeInternalIPs_DualStack(t *testing.T) {
	tests := []struct {
		name        string
		node        corev1.Node
		expectedIPs []string
		expectedErr bool
	}{
		{
			name: "single IPv4 address",
			node: corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
						{Type: corev1.NodeHostName, Address: "test-node"},
						{Type: corev1.NodeExternalIP, Address: "203.0.113.10"},
					},
				},
			},
			expectedIPs: []string{"192.168.1.10"},
			expectedErr: false,
		},
		{
			name: "dual stack IPv4 and IPv6",
			node: corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
						{Type: corev1.NodeInternalIP, Address: "2001:db8::10"},
						{Type: corev1.NodeHostName, Address: "test-node"},
						{Type: corev1.NodeExternalIP, Address: "203.0.113.10"},
					},
				},
			},
			expectedIPs: []string{"192.168.1.10", "2001:db8::10"},
			expectedErr: false,
		},
		{
			name: "single IPv6 address",
			node: corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "2001:db8::10"},
						{Type: corev1.NodeHostName, Address: "test-node"},
					},
				},
			},
			expectedIPs: []string{"2001:db8::10"},
			expectedErr: false,
		},
		{
			name: "multiple IPv4 addresses",
			node: corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
						{Type: corev1.NodeInternalIP, Address: "10.0.0.10"},
						{Type: corev1.NodeHostName, Address: "test-node"},
					},
				},
			},
			expectedIPs: []string{"192.168.1.10", "10.0.0.10"},
			expectedErr: false,
		},
		{
			name: "multiple IPv6 addresses",
			node: corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "2001:db8::10"},
						{Type: corev1.NodeInternalIP, Address: "fd00::10"},
						{Type: corev1.NodeHostName, Address: "test-node"},
					},
				},
			},
			expectedIPs: []string{"2001:db8::10", "fd00::10"},
			expectedErr: false,
		},
		{
			name: "no internal IP addresses",
			node: corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeHostName, Address: "test-node"},
						{Type: corev1.NodeExternalIP, Address: "203.0.113.10"},
					},
				},
			},
			expectedIPs: nil,
			expectedErr: true,
		},
		{
			name: "empty node addresses",
			node: corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{},
				},
			},
			expectedIPs: nil,
			expectedErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ips, err := getNodeInternalIPs(tt.node)

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

func TestClusterInfo_DualStackFields(t *testing.T) {
	// Test that ClusterInfo struct properly handles dual-stack fields
	clusterInfo := ClusterInfo{
		BaseDomain:           "example.com",
		ClusterName:          "test-cluster",
		ClusterID:            "test-cluster-id",
		OCPVersion:           "4.14.0",
		NodeIPs:              []string{"192.168.1.10", "2001:db8::10"},
		ReleaseRegistry:      "quay.io/openshift-release-dev",
		Hostname:             "test-node",
		ClusterNetworks:      []string{"10.128.0.0/14", "fd01::/48"},
		ServiceNetworks:      []string{"172.30.0.0/16", "fd02::/112"},
		MachineNetworks:      []string{"192.168.1.0/24", "2001:db8::/64"},
		NodeLabels:           map[string]string{"test": "label"},
		IngressCertificateCN: "ingress-operator@123456",
	}

	// Verify dual-stack specific fields
	assert.Equal(t, []string{"192.168.1.10", "2001:db8::10"}, clusterInfo.NodeIPs)
	assert.Equal(t, []string{"192.168.1.0/24", "2001:db8::/64"}, clusterInfo.MachineNetworks)
	assert.Equal(t, []string{"10.128.0.0/14", "fd01::/48"}, clusterInfo.ClusterNetworks)
	assert.Equal(t, []string{"172.30.0.0/16", "fd02::/112"}, clusterInfo.ServiceNetworks)

	// Verify other standard fields
	assert.Equal(t, "example.com", clusterInfo.BaseDomain)
	assert.Equal(t, "test-cluster", clusterInfo.ClusterName)
	assert.Equal(t, "4.14.0", clusterInfo.OCPVersion)
	assert.Equal(t, "test-node", clusterInfo.Hostname)
	assert.Equal(t, "ingress-operator@123456", clusterInfo.IngressCertificateCN)
}
