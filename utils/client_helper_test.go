package utils

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

func newFakeClient(t *testing.T, objs ...crclient.Object) crclient.Reader {
	t.Helper()
	sch := scheme.Scheme
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
}

func TestGetInstallConfig_Success(t *testing.T) {
	ctx := context.Background()

	cfg := `networking:
  machineNetwork:
  - cidr: 192.0.2.0/24`
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-config-v1",
			Namespace: "kube-system",
		},
		Data: map[string]string{
			"install-config": cfg,
		},
	}

	cl := newFakeClient(t, cm)

	got, err := GetInstallConfig(ctx, cl)
	assert.NoError(t, err)
	assert.Equal(t, cfg, got)
}

func TestGetInstallConfig_NilClient(t *testing.T) {
	ctx := context.Background()
	got, err := GetInstallConfig(ctx, nil)
	assert.Error(t, err)
	assert.Empty(t, got)
}

func TestGetLocalNodeName_SingleNode(t *testing.T) {
	ctx := context.Background()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "master-0",
		},
	}

	cl := newFakeClient(t, node)

	name, err := GetLocalNodeName(ctx, cl)
	assert.NoError(t, err)
	assert.Equal(t, "master-0", name)
}

func TestGetLocalNodeName_MultipleNodes(t *testing.T) {
	ctx := context.Background()

	node1 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
	node2 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-2"}}

	cl := newFakeClient(t, node1, node2)

	name, err := GetLocalNodeName(ctx, cl)
	assert.Error(t, err)
	assert.Empty(t, name)
}

func TestGetNodeInternalIPs_Wrapper(t *testing.T) {
	ctx := context.Background()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "master-0",
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.0.2.10"},
				{Type: corev1.NodeInternalIP, Address: "2001:db8::10"},
			},
		},
	}

	cl := newFakeClient(t, node)

	ips, err := GetNodeInternalIPs(ctx, cl)
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"192.0.2.10", "2001:db8::10"}, ips)
}

func TestGetNodeInternalIPs_NoInternal(t *testing.T) {
	ctx := context.Background()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "master-0",
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeHostName, Address: "master-0"},
			},
		},
	}

	cl := newFakeClient(t, node)

	ips, err := GetNodeInternalIPs(ctx, cl)
	assert.Error(t, err)
	assert.Nil(t, ips)
}

func TestGetMachineNetworks_Success(t *testing.T) {
	ctx := context.Background()

	installConfigYAML := `networking:
  machineNetwork:
  - cidr: 192.0.2.0/24
  - cidr: 2001:db8::/64`

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-config-v1",
			Namespace: "kube-system",
		},
		Data: map[string]string{
			"install-config": installConfigYAML,
		},
	}

	cl := newFakeClient(t, cm)

	nets, err := GetMachineNetworks(ctx, cl)
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"192.0.2.0/24", "2001:db8::/64"}, nets)
}

func TestGetMachineNetworks_InvalidYAML(t *testing.T) {
	ctx := context.Background()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-config-v1",
			Namespace: "kube-system",
		},
		Data: map[string]string{
			"install-config": ":\n  - not: valid",
		},
	}

	cl := newFakeClient(t, cm)

	nets, err := GetMachineNetworks(ctx, cl)
	assert.Error(t, err)
	assert.Nil(t, nets)
}
