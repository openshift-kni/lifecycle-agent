/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ipconfigcmd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	ostreemock "github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ipconfig"
	opsmock "github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
)

// zeroPrePivotGlobals resets the package-level flag variables the pre-pivot
// validators read.
func zeroPrePivotGlobals() {
	newStaterootName = ""
	installInitMonitor = false
	installIpConfigurationService = false
	ipv4Address = ""
	ipv4MachineNetwork = ""
	ipv6Address = ""
	ipv6MachineNetwork = ""
	desiredIPv4Gateway = ""
	desiredIPv6Gateway = ""
	currentGatewayV4 = ""
	currentGatewayV6 = ""
	dnsServers = nil
	vlanID = 0
	pullSecretRefName = ""
	dnsIPFamily = ""
}

func newTestLogger() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return log
}

// setUpPrePivotTest zeroes the flag globals, installs a discard logger, points
// the controller-written flags file at a missing path, and restores everything
// when the test finishes.
func setUpPrePivotTest(t *testing.T) {
	t.Helper()

	zeroPrePivotGlobals()
	pkgLog = newTestLogger()
	stubPrePivotFlagsFile(t, filepath.Join(t.TempDir(), "missing.json"))
	t.Cleanup(zeroPrePivotGlobals)
}

// stubPrePivotFlagsFile points the controller-written flags file at path.
func stubPrePivotFlagsFile(t *testing.T, path string) {
	t.Helper()

	old := prePivotFlagsFile
	prePivotFlagsFile = path
	t.Cleanup(func() { prePivotFlagsFile = old })
}

// stubPrimaryIPFile points the primary-IP read at path.
func stubPrimaryIPFile(t *testing.T, path string) {
	t.Helper()

	old := primaryIPFile
	primaryIPFile = path
	t.Cleanup(func() { primaryIPFile = old })
}

func newNode(name string, ips ...string) *corev1.Node {
	addrs := make([]corev1.NodeAddress, 0, len(ips))
	for _, ip := range ips {
		addrs = append(addrs, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ip})
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Addresses: addrs},
	}
}

func newIPConfigFakeClient(t *testing.T, objs ...runtimeclient.Object) runtimeclient.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(ipPrePivotScheme).WithObjects(objs...).Build()
}

// stubStaterootPrefix pins the deploy-path prefix so stateroot paths are
// predictable, restoring the previous value when the test finishes.
func stubStaterootPrefix(t *testing.T, prefix string) {
	t.Helper()
	old := common.OstreeDeployPathPrefix
	common.OstreeDeployPathPrefix = prefix
	t.Cleanup(func() { common.OstreeDeployPathPrefix = old })
}

func TestValidateDNSServers(t *testing.T) {
	tests := []struct {
		name    string
		servers []string
		wantErr string
	}{
		{name: "nil list"},
		{name: "empty list", servers: []string{}},
		{name: "blank entries are skipped", servers: []string{"", "   "}},
		{name: "valid IPv4", servers: []string{"192.168.1.1"}},
		{name: "valid IPv6", servers: []string{"fd00::1"}},
		{name: "mixed families", servers: []string{"192.168.1.1", "fd00::1"}},
		{name: "surrounding whitespace is trimmed", servers: []string{"  192.168.1.1  "}},
		{name: "hostname is rejected", servers: []string{"dns.example.com"}, wantErr: `invalid IP address: "dns.example.com"`},
		{name: "garbage is rejected", servers: []string{"192.168.1.1", "not-an-ip"}, wantErr: `invalid IP address: "not-an-ip"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDNSServers(tt.servers)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidateIPFamilyConfigArgs(t *testing.T) {
	tests := []struct {
		name    string
		set     func()
		wantErr string
	}{
		{
			name: "IPv4 address and network",
			set: func() {
				ipv4Address = "192.168.1.10"
				ipv4MachineNetwork = "192.168.1.0/24"
			},
		},
		{
			name: "IPv4 with in-network gateway",
			set: func() {
				ipv4Address = "192.168.1.10"
				ipv4MachineNetwork = "192.168.1.0/24"
				desiredIPv4Gateway = "192.168.1.1"
			},
		},
		{
			name: "IPv4 gateway outside the machine network",
			set: func() {
				ipv4Address = "192.168.1.10"
				ipv4MachineNetwork = "192.168.1.0/24"
				desiredIPv4Gateway = "10.0.0.1"
			},
			wantErr: "invalid IPv4 config",
		},
		{
			name: "IPv4 address without a machine network",
			set: func() {
				ipv4Address = "192.168.1.10"
			},
			wantErr: "both address and machine-network must be provided together",
		},
		{
			name: "IPv6 machine network without an address",
			set: func() {
				ipv6MachineNetwork = "fd00::/64"
			},
			wantErr: "both address and machine-network must be provided together",
		},
		{
			name: "only a current IPv4 gateway",
			set: func() {
				currentGatewayV4 = "192.168.1.1"
			},
			wantErr: "both address and machine-network must be provided together",
		},
		{
			name:    "no family at all",
			set:     func() {},
			wantErr: "at least one of IPv4 or IPv6 must be provided",
		},
		{
			name: "invalid IPv4 address",
			set: func() {
				ipv4Address = "192.168.1.300"
				ipv4MachineNetwork = "192.168.1.0/24"
			},
			wantErr: "invalid IPv4 config",
		},
		{
			name: "invalid IPv6 network CIDR",
			set: func() {
				ipv6Address = "fd00::10"
				ipv6MachineNetwork = "fd00::/129"
			},
			wantErr: "invalid IPv6 config",
		},
		{
			name: "IPv6 link-local gateway outside the network is allowed",
			set: func() {
				ipv6Address = "fd00::10"
				ipv6MachineNetwork = "fd00::/64"
				desiredIPv6Gateway = "fe80::1"
			},
		},
		{
			name: "dual stack",
			set: func() {
				ipv4Address = "192.168.1.10"
				ipv4MachineNetwork = "192.168.1.0/24"
				desiredIPv4Gateway = "192.168.1.1"
				ipv6Address = "fd00::10"
				ipv6MachineNetwork = "fd00::/64"
				desiredIPv6Gateway = "fd00::1"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setUpPrePivotTest(t)
			tt.set()

			err := validateIPFamilyConfigArgs()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestInferPrimaryStack(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		absent   bool
		want     string
		wantErr  string
	}{
		{name: "IPv4", contents: "192.168.1.10\n", want: common.IPv4FamilyName},
		{name: "IPv6", contents: "fd00::10\n", want: common.IPv6FamilyName},
		{name: "surrounding whitespace is trimmed", contents: "  192.168.1.10  \n\n", want: common.IPv4FamilyName},
		{name: "empty file", contents: "", wantErr: "primary IP not found"},
		{name: "invalid IP", contents: "not-an-ip", wantErr: "invalid primary IP"},
		{name: "missing file", absent: true, wantErr: "failed to read primary IP"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setUpPrePivotTest(t)

			path := filepath.Join(t.TempDir(), "primary-ip")
			if !tt.absent {
				require.NoError(t, os.WriteFile(path, []byte(tt.contents), 0o600))
			}
			stubPrimaryIPFile(t, path)

			family, err := inferPrimaryStack()
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Nil(t, family)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, family)
			assert.Equal(t, tt.want, *family)
		})
	}
}

func TestBuildIPConfigs(t *testing.T) {
	ipv4 := []string{"192.168.1.10", "192.168.1.0/24", "192.168.1.1", "192.168.1.2"}
	ipv6 := []string{"fd00::10", "fd00::/64", "fd00::1", "fd00::2"}

	// build adapts up to four values per family to buildIPConfigs' positional
	// signature, defaulting any missing value to the empty string.
	build := func(v4, v6 []string, primary string) []*ipconfig.NetworkIPConfig {
		get := func(values []string, i int) string {
			if i < len(values) {
				return values[i]
			}
			return ""
		}
		return buildIPConfigs(
			get(v4, 0), get(v4, 1), get(v4, 2), get(v4, 3),
			get(v6, 0), get(v6, 1), get(v6, 2), get(v6, 3),
			primary,
		)
	}

	v4Config := &ipconfig.NetworkIPConfig{
		IP: ipv4[0], MachineNetwork: ipv4[1], DesiredGateway: ipv4[2], CurrentGateway: ipv4[3],
	}
	v6Config := &ipconfig.NetworkIPConfig{
		IP: ipv6[0], MachineNetwork: ipv6[1], DesiredGateway: ipv6[2], CurrentGateway: ipv6[3],
	}

	tests := []struct {
		name    string
		ipv4    []string
		ipv6    []string
		primary string
		want    []*ipconfig.NetworkIPConfig
	}{
		{
			name:    "IPv4 primary with dual stack",
			ipv4:    ipv4,
			ipv6:    ipv6,
			primary: common.IPv4FamilyName,
			want:    []*ipconfig.NetworkIPConfig{v4Config, v6Config},
		},
		{
			name:    "IPv6 primary with dual stack",
			ipv4:    ipv4,
			ipv6:    ipv6,
			primary: common.IPv6FamilyName,
			want:    []*ipconfig.NetworkIPConfig{v6Config, v4Config},
		},
		{
			name:    "IPv4 primary with IPv6 only",
			ipv6:    ipv6,
			primary: common.IPv4FamilyName,
			want:    []*ipconfig.NetworkIPConfig{v6Config},
		},
		{
			name:    "IPv6 primary with IPv4 only",
			ipv4:    ipv4,
			primary: common.IPv6FamilyName,
			want:    []*ipconfig.NetworkIPConfig{v4Config},
		},
		{
			name:    "incomplete IPv4 family is dropped",
			ipv4:    []string{"192.168.1.10"},
			primary: common.IPv4FamilyName,
			want:    []*ipconfig.NetworkIPConfig{},
		},
		{
			name:    "incomplete IPv6 family is dropped",
			ipv6:    []string{"fd00::10"},
			primary: common.IPv6FamilyName,
			want:    []*ipconfig.NetworkIPConfig{},
		},
		{
			name:    "unknown primary produces no configs",
			ipv4:    ipv4,
			ipv6:    ipv6,
			primary: "",
			want:    []*ipconfig.NetworkIPConfig{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, build(tt.ipv4, tt.ipv6, tt.primary))
		})
	}
}

func TestValidateClusterAPIAndUserIPSpec(t *testing.T) {
	dualStack := newNode("node1", "192.168.1.10", "fd00::10")
	ipv4Only := newNode("node1", "192.168.1.10")
	ipv6Only := newNode("node1", "fd00::10")
	noInternalIPs := newNode("node1")

	tests := []struct {
		name    string
		objects []runtimeclient.Object
		set     func()
		wantErr string
	}{
		{
			name:    "IPs match a dual-stack cluster",
			objects: []runtimeclient.Object{dualStack},
			set: func() {
				ipv4Address = "192.168.1.20"
				ipv4MachineNetwork = "192.168.1.0/24"
				ipv6Address = "fd00::20"
				ipv6MachineNetwork = "fd00::/64"
			},
		},
		{
			name:    "IPv4 requested on an IPv6-only cluster",
			objects: []runtimeclient.Object{ipv6Only},
			set: func() {
				ipv4Address = "192.168.1.20"
				ipv4MachineNetwork = "192.168.1.0/24"
			},
			wantErr: "specified IPv4, but the cluster does not have IPv4",
		},
		{
			name:    "IPv6 requested on an IPv4-only cluster",
			objects: []runtimeclient.Object{ipv4Only},
			set: func() {
				ipv6Address = "fd00::20"
				ipv6MachineNetwork = "fd00::/64"
			},
			wantErr: "specified IPv6, but the cluster does not have IPv6",
		},
		{
			name:    "dns filter family requires dual stack",
			objects: []runtimeclient.Object{ipv4Only},
			set: func() {
				ipv4Address = "192.168.1.20"
				ipv4MachineNetwork = "192.168.1.0/24"
				dnsIPFamily = common.IPv4FamilyName
			},
			wantErr: "dns-ip-family is supported only on dual-stack clusters",
		},
		{
			name:    "invalid DNS server entry",
			objects: []runtimeclient.Object{dualStack},
			set: func() {
				ipv4Address = "192.168.1.20"
				ipv4MachineNetwork = "192.168.1.0/24"
				dnsServers = []string{"not-an-ip"}
			},
			wantErr: `invalid DNS server IP: "not-an-ip"`,
		},
		{
			name:    "IPv4 DNS server on an IPv6-only cluster",
			objects: []runtimeclient.Object{ipv6Only},
			set: func() {
				ipv6Address = "fd00::20"
				ipv6MachineNetwork = "fd00::/64"
				dnsServers = []string{"192.168.1.1"}
			},
			wantErr: "specified IPv4 DNS server",
		},
		{
			name:    "IPv6 DNS server on an IPv4-only cluster",
			objects: []runtimeclient.Object{ipv4Only},
			set: func() {
				ipv4Address = "192.168.1.20"
				ipv4MachineNetwork = "192.168.1.0/24"
				dnsServers = []string{"fd00::1"}
			},
			wantErr: "specified IPv6 DNS server",
		},
		{
			name: "cluster with no nodes is unreachable",
			set: func() {
				ipv4Address = "192.168.1.20"
				ipv4MachineNetwork = "192.168.1.0/24"
			},
			wantErr: "failed to contact cluster API",
		},
		{
			name:    "cluster with two nodes is rejected",
			objects: []runtimeclient.Object{newNode("node1", "192.168.1.10"), newNode("node2", "192.168.1.11")},
			set: func() {
				ipv4Address = "192.168.1.20"
				ipv4MachineNetwork = "192.168.1.0/24"
			},
			wantErr: "failed to contact cluster API",
		},
		{
			name:    "node without internal IPs is rejected",
			objects: []runtimeclient.Object{noInternalIPs},
			set: func() {
				ipv4Address = "192.168.1.20"
				ipv4MachineNetwork = "192.168.1.0/24"
			},
			wantErr: "failed to contact cluster API",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setUpPrePivotTest(t)
			tt.set()
			client := newIPConfigFakeClient(t, tt.objects...)

			err := validateClusterAPIAndUserIPSpec(context.Background(), client)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidatePrePivotFlags(t *testing.T) {
	dualStack := newNode("node1", "192.168.1.10", "fd00::10")

	tests := []struct {
		name    string
		objects []runtimeclient.Object
		set     func()
		wantErr string
	}{
		{
			name:    "new stateroot name is required",
			objects: []runtimeclient.Object{dualStack},
			set:     func() {},
			wantErr: "new-stateroot-name is required",
		},
		{
			name:    "IP family arguments are validated",
			objects: []runtimeclient.Object{dualStack},
			set: func() {
				newStaterootName = "new-root"
				ipv4Address = "192.168.1.20"
			},
			wantErr: "invalid IP config arguments",
		},
		{
			name:    "DNS servers are validated before the cluster",
			objects: []runtimeclient.Object{dualStack},
			set: func() {
				newStaterootName = "new-root"
				ipv4Address = "192.168.1.20"
				ipv4MachineNetwork = "192.168.1.0/24"
				dnsServers = []string{"nope"}
			},
			wantErr: "invalid dns servers",
		},
		{
			name: "cluster validation runs before the VLAN check",
			set: func() {
				newStaterootName = "new-root"
				ipv4Address = "192.168.1.20"
				ipv4MachineNetwork = "192.168.1.0/24"
				vlanID = -1
			},
			wantErr: "failed to validate cluster API and user IP spec",
		},
		{
			name:    "negative VLAN id is rejected",
			objects: []runtimeclient.Object{dualStack},
			set: func() {
				newStaterootName = "new-root"
				ipv4Address = "192.168.1.20"
				ipv4MachineNetwork = "192.168.1.0/24"
				vlanID = -1
			},
			wantErr: "vlan-id must be >= 0",
		},
		{
			name:    "unknown DNS filter family is rejected",
			objects: []runtimeclient.Object{dualStack},
			set: func() {
				newStaterootName = "new-root"
				ipv4Address = "192.168.1.20"
				ipv4MachineNetwork = "192.168.1.0/24"
				dnsIPFamily = "ipv5"
			},
			wantErr: "dns-ip-family must be one of",
		},
		{
			name:    "valid IPv4 with filter family",
			objects: []runtimeclient.Object{dualStack},
			set: func() {
				newStaterootName = "new-root"
				ipv4Address = "192.168.1.20"
				ipv4MachineNetwork = "192.168.1.0/24"
				vlanID = 100
				dnsIPFamily = common.IPv4FamilyName
			},
		},
		{
			name:    "valid IPv6 with filter family",
			objects: []runtimeclient.Object{dualStack},
			set: func() {
				newStaterootName = "new-root"
				ipv6Address = "fd00::20"
				ipv6MachineNetwork = "fd00::/64"
				dnsIPFamily = common.IPv6FamilyName
			},
		},
		{
			name:    "filter family none is allowed on a single-stack cluster",
			objects: []runtimeclient.Object{newNode("node1", "192.168.1.10")},
			set: func() {
				newStaterootName = "new-root"
				ipv4Address = "192.168.1.20"
				ipv4MachineNetwork = "192.168.1.0/24"
				dnsIPFamily = common.DNSFamilyNone
			},
		},
		{
			name:    "valid with DNS filter family none",
			objects: []runtimeclient.Object{dualStack},
			set: func() {
				newStaterootName = "new-root"
				ipv4Address = "192.168.1.20"
				ipv4MachineNetwork = "192.168.1.0/24"
				dnsIPFamily = common.DNSFamilyNone
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setUpPrePivotTest(t)
			tt.set()
			client := newIPConfigFakeClient(t, tt.objects...)

			err := validatePrePivotFlags(context.Background(), client)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// newIPConfigMocks returns gomock-backed Ops and ostree clients.
func newIPConfigMocks(t *testing.T) (*opsmock.MockOps, *ostreemock.MockIClient, *rpmostreeclient.MockIClient) {
	t.Helper()

	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	return opsmock.NewMockOps(ctrl), ostreemock.NewMockIClient(ctrl), rpmostreeclient.NewMockIClient(ctrl)
}

func TestGetCurrentStaterootData(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*ostreemock.MockIClient, *rpmostreeclient.MockIClient)
		want    *ipconfig.StaterootData
		wantErr string
	}{
		{
			name: "resolves name, path, deployment and dir",
			setup: func(ostree *ostreemock.MockIClient, rpm *rpmostreeclient.MockIClient) {
				rpm.EXPECT().GetCurrentStaterootName().Return("old-root", nil)
				ostree.EXPECT().GetDeployment("old-root").Return("old-root-0", nil)
				ostree.EXPECT().GetDeploymentDir("old-root").Return("/sysroot/ostree/deploy/old-root/deploy/old-root-0", nil)
			},
			want: &ipconfig.StaterootData{
				Name:           "old-root",
				Path:           "/sysroot/ostree/deploy/old-root",
				DeploymentName: "old-root-0",
				DeploymentDir:  "/sysroot/ostree/deploy/old-root/deploy/old-root-0",
			},
		},
		{
			name: "current stateroot name error",
			setup: func(_ *ostreemock.MockIClient, rpm *rpmostreeclient.MockIClient) {
				rpm.EXPECT().GetCurrentStaterootName().Return("", assert.AnError)
			},
			wantErr: "failed to get current stateroot name",
		},
		{
			name: "deployment lookup error",
			setup: func(ostree *ostreemock.MockIClient, rpm *rpmostreeclient.MockIClient) {
				rpm.EXPECT().GetCurrentStaterootName().Return("old-root", nil)
				ostree.EXPECT().GetDeployment("old-root").Return("", assert.AnError)
			},
			wantErr: "failed to get deployment for old-root",
		},
		{
			name: "deployment dir lookup error",
			setup: func(ostree *ostreemock.MockIClient, rpm *rpmostreeclient.MockIClient) {
				rpm.EXPECT().GetCurrentStaterootName().Return("old-root", nil)
				ostree.EXPECT().GetDeployment("old-root").Return("old-root-0", nil)
				ostree.EXPECT().GetDeploymentDir("old-root").Return("", assert.AnError)
			},
			wantErr: "failed to get deployment dir for old-root",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubStaterootPrefix(t, "/sysroot")
			_, ostree, rpm := newIPConfigMocks(t)
			tt.setup(ostree, rpm)

			got, err := getCurrentStaterootData(rpm, ostree)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestGetNewStaterootData(t *testing.T) {
	log := newTestLogger()

	tests := []struct {
		name    string
		setup   func(*ostreemock.MockIClient)
		want    *ipconfig.StaterootData
		wantErr string
	}{
		{
			name: "deployment already exists",
			setup: func(ostree *ostreemock.MockIClient) {
				ostree.EXPECT().GetDeployment("new-root").Return("new-root-0", nil)
				ostree.EXPECT().GetDeploymentDir("new-root").Return("/sysroot/ostree/deploy/new-root/deploy/new-root-0", nil)
			},
			want: &ipconfig.StaterootData{
				Name:           "new-root",
				Path:           "/sysroot/ostree/deploy/new-root",
				DeploymentName: "new-root-0",
				DeploymentDir:  "/sysroot/ostree/deploy/new-root/deploy/new-root-0",
			},
		},
		{
			name: "deployment not created yet",
			setup: func(ostree *ostreemock.MockIClient) {
				ostree.EXPECT().GetDeployment("new-root").Return("", nil)
			},
			want: &ipconfig.StaterootData{
				Name: "new-root",
				Path: "/sysroot/ostree/deploy/new-root",
			},
		},
		{
			name: "deployment lookup error",
			setup: func(ostree *ostreemock.MockIClient) {
				ostree.EXPECT().GetDeployment("new-root").Return("", assert.AnError)
			},
			wantErr: "failed to get deployment name for new-root",
		},
		{
			name: "deployment dir lookup error",
			setup: func(ostree *ostreemock.MockIClient) {
				ostree.EXPECT().GetDeployment("new-root").Return("new-root-0", nil)
				ostree.EXPECT().GetDeploymentDir("new-root").Return("", assert.AnError)
			},
			wantErr: "failed to get deployment dir for new-root",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubStaterootPrefix(t, "/sysroot")
			_, ostree, _ := newIPConfigMocks(t)
			tt.setup(ostree)

			got, err := getNewStaterootData("new-root", ostree, log)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestGetOstreeData(t *testing.T) {
	log := newTestLogger()

	tests := []struct {
		name    string
		setup   func(*ostreemock.MockIClient, *rpmostreeclient.MockIClient)
		wantErr string
	}{
		{
			name: "resolves both stateroots",
			setup: func(ostree *ostreemock.MockIClient, rpm *rpmostreeclient.MockIClient) {
				rpm.EXPECT().GetCurrentStaterootName().Return("old-root", nil)
				ostree.EXPECT().GetDeployment("old-root").Return("old-root-0", nil)
				ostree.EXPECT().GetDeploymentDir("old-root").Return("/sysroot/ostree/deploy/old-root/deploy/old-root-0", nil)
				ostree.EXPECT().GetDeployment("new-root").Return("", nil)
			},
		},
		{
			name: "current stateroot error",
			setup: func(_ *ostreemock.MockIClient, rpm *rpmostreeclient.MockIClient) {
				rpm.EXPECT().GetCurrentStaterootName().Return("", assert.AnError)
			},
			wantErr: "failed to get current stateroot data",
		},
		{
			name: "new stateroot error",
			setup: func(ostree *ostreemock.MockIClient, rpm *rpmostreeclient.MockIClient) {
				rpm.EXPECT().GetCurrentStaterootName().Return("old-root", nil)
				ostree.EXPECT().GetDeployment("old-root").Return("old-root-0", nil)
				ostree.EXPECT().GetDeploymentDir("old-root").Return("/sysroot/ostree/deploy/old-root/deploy/old-root-0", nil)
				ostree.EXPECT().GetDeployment("new-root").Return("", assert.AnError)
			},
			wantErr: "failed to get new stateroot data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubStaterootPrefix(t, sysrootPath)
			_, ostree, rpm := newIPConfigMocks(t)
			tt.setup(ostree, rpm)

			got, err := getOstreeData("new-root", rpm, ostree, log)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, "old-root", got.OldStateroot.Name)
			assert.Equal(t, "new-root", got.NewStateroot.Name)
			assert.Equal(t, sysrootPath, common.OstreeDeployPathPrefix)
		})
	}
}

func TestInstallMonitorInitializationServiceInNewStateroot(t *testing.T) {
	log := newTestLogger()

	t.Run("writes and enables the service", func(t *testing.T) {
		opsMock, _, _ := newIPConfigMocks(t)
		root := t.TempDir()
		deploymentDir := filepath.Join(root, "deploy", "new-root-0")
		staterootPath := filepath.Join(root, "stateroot")
		require.NoError(t, os.MkdirAll(filepath.Join(staterootPath, "var/lib/lca/workspace"), 0o755))

		ostreeData := &ipconfig.OstreeData{
			NewStateroot: &ipconfig.StaterootData{Path: staterootPath, DeploymentDir: deploymentDir},
		}

		opsMock.EXPECT().
			SystemctlAction("enable", "--root", deploymentDir, common.IPCInitMonitorService).
			Return("", nil)

		require.NoError(t, installMonitorInitializationServiceInNewStateroot(opsMock, ostreeData, log))

		serviceFile := filepath.Join(deploymentDir, "etc/systemd/system", common.IPCInitMonitorService)
		assert.FileExists(t, serviceFile)
		modeFile := filepath.Join(staterootPath, common.InitMonitorModeFile)
		contents, err := os.ReadFile(modeFile)
		require.NoError(t, err)
		assert.Equal(t, "ipconfig", string(contents))
	})

	t.Run("systemctl failure is returned", func(t *testing.T) {
		opsMock, _, _ := newIPConfigMocks(t)
		root := t.TempDir()
		deploymentDir := filepath.Join(root, "deploy", "new-root-0")
		staterootPath := filepath.Join(root, "stateroot")
		require.NoError(t, os.MkdirAll(filepath.Join(staterootPath, "var/lib/lca/workspace"), 0o755))

		ostreeData := &ipconfig.OstreeData{
			NewStateroot: &ipconfig.StaterootData{Path: staterootPath, DeploymentDir: deploymentDir},
		}

		opsMock.EXPECT().
			SystemctlAction("enable", "--root", deploymentDir, common.IPCInitMonitorService).
			Return("", assert.AnError)

		err := installMonitorInitializationServiceInNewStateroot(opsMock, ostreeData, log)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed enabling service")
	})

	t.Run("unusable deployment dir fails before systemctl", func(t *testing.T) {
		opsMock, _, _ := newIPConfigMocks(t)
		root := t.TempDir()
		// A regular file where the "etc" directory must be makes MkdirAll fail.
		require.NoError(t, os.WriteFile(filepath.Join(root, "etc"), []byte("x"), 0o600))

		ostreeData := &ipconfig.OstreeData{
			NewStateroot: &ipconfig.StaterootData{Path: t.TempDir(), DeploymentDir: root},
		}

		err := installMonitorInitializationServiceInNewStateroot(opsMock, ostreeData, log)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create destination directory")
	})

	t.Run("service file write failure", func(t *testing.T) {
		opsMock, _, _ := newIPConfigMocks(t)
		root := t.TempDir()
		deploymentDir := filepath.Join(root, "deploy", "new-root-0")
		// A directory where the service file must be written makes WriteFile fail.
		require.NoError(t, os.MkdirAll(filepath.Join(deploymentDir, "etc/systemd/system", common.IPCInitMonitorService), 0o750))

		ostreeData := &ipconfig.OstreeData{
			NewStateroot: &ipconfig.StaterootData{Path: t.TempDir(), DeploymentDir: deploymentDir},
		}

		err := installMonitorInitializationServiceInNewStateroot(opsMock, ostreeData, log)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to write init monitor service file")
	})

	t.Run("init monitor mode file write failure", func(t *testing.T) {
		opsMock, _, _ := newIPConfigMocks(t)
		deploymentDir := filepath.Join(t.TempDir(), "deploy", "new-root-0")
		staterootPath := t.TempDir()
		// A directory where the mode file must be written makes WriteFile fail.
		require.NoError(t, os.MkdirAll(filepath.Join(staterootPath, common.InitMonitorModeFile), 0o750))

		ostreeData := &ipconfig.OstreeData{
			NewStateroot: &ipconfig.StaterootData{Path: staterootPath, DeploymentDir: deploymentDir},
		}

		opsMock.EXPECT().
			SystemctlAction("enable", "--root", deploymentDir, common.IPCInitMonitorService).
			Return("", nil)

		err := installMonitorInitializationServiceInNewStateroot(opsMock, ostreeData, log)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to write init monitor mode file")
	})
}

func TestInstallIpConfigurationServiceInNewStateroot(t *testing.T) {
	log := newTestLogger()

	t.Run("writes and enables the service", func(t *testing.T) {
		opsMock, _, _ := newIPConfigMocks(t)
		deploymentDir := filepath.Join(t.TempDir(), "deploy", "new-root-0")

		ostreeData := &ipconfig.OstreeData{
			NewStateroot: &ipconfig.StaterootData{DeploymentDir: deploymentDir},
		}

		opsMock.EXPECT().
			SystemctlAction("enable", "--root", deploymentDir, common.IPConfigurationService).
			Return("", nil)

		require.NoError(t, installIpConfigurationServiceInNewStateroot(ostreeData, opsMock, log))
		assert.FileExists(t, filepath.Join(deploymentDir, "etc/systemd/system", common.IPConfigurationService))
	})

	t.Run("systemctl failure is returned", func(t *testing.T) {
		opsMock, _, _ := newIPConfigMocks(t)
		deploymentDir := filepath.Join(t.TempDir(), "deploy", "new-root-0")

		ostreeData := &ipconfig.OstreeData{
			NewStateroot: &ipconfig.StaterootData{DeploymentDir: deploymentDir},
		}

		opsMock.EXPECT().
			SystemctlAction("enable", "--root", deploymentDir, common.IPConfigurationService).
			Return("", assert.AnError)

		err := installIpConfigurationServiceInNewStateroot(ostreeData, opsMock, log)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed enabling service")
	})

	t.Run("service file write failure", func(t *testing.T) {
		opsMock, _, _ := newIPConfigMocks(t)
		deploymentDir := filepath.Join(t.TempDir(), "deploy", "new-root-0")
		// A directory where the service file must be written makes WriteFile fail.
		require.NoError(t, os.MkdirAll(filepath.Join(deploymentDir, "etc/systemd/system", common.IPConfigurationService), 0o750))

		ostreeData := &ipconfig.OstreeData{
			NewStateroot: &ipconfig.StaterootData{DeploymentDir: deploymentDir},
		}

		err := installIpConfigurationServiceInNewStateroot(ostreeData, opsMock, log)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to write ip configuration service file")
	})
}

func TestCopyLcaCliToNewStateroot(t *testing.T) {
	log := newTestLogger()

	t.Run("copies the binary into the new stateroot", func(t *testing.T) {
		opsMock, _, _ := newIPConfigMocks(t)
		staterootPath := t.TempDir()
		destination := filepath.Join(staterootPath, common.LcaCliBinaryHostPath)

		ostreeData := &ipconfig.OstreeData{
			NewStateroot: &ipconfig.StaterootData{Path: staterootPath},
		}

		opsMock.EXPECT().CopyFile(common.LcaCliBinaryHostPath, destination, os.FileMode(0o777)).Return(nil)

		require.NoError(t, copyLcaCliToNewStateroot(ostreeData, opsMock, log))
	})

	t.Run("copy failure is returned", func(t *testing.T) {
		opsMock, _, _ := newIPConfigMocks(t)
		staterootPath := t.TempDir()
		destination := filepath.Join(staterootPath, common.LcaCliBinaryHostPath)

		ostreeData := &ipconfig.OstreeData{
			NewStateroot: &ipconfig.StaterootData{Path: staterootPath},
		}

		opsMock.EXPECT().CopyFile(common.LcaCliBinaryHostPath, destination, os.FileMode(0o777)).Return(assert.AnError)

		err := copyLcaCliToNewStateroot(ostreeData, opsMock, log)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to copy lca-cli binary")
	})
}

func TestRemoveIPCFileInNewStaterootIfExists(t *testing.T) {
	log := newTestLogger()

	t.Run("missing IPC file is not an error", func(t *testing.T) {
		ostreeData := &ipconfig.OstreeData{
			NewStateroot: &ipconfig.StaterootData{Path: t.TempDir()},
		}
		assert.NoError(t, removeIPCFileInNewStaterootIfExists(ostreeData, log))
	})

	t.Run("existing IPC file is removed", func(t *testing.T) {
		staterootPath := t.TempDir()
		ipcPath := filepath.Join(staterootPath, common.IPCFilePath)
		require.NoError(t, os.MkdirAll(filepath.Dir(ipcPath), 0o755))
		require.NoError(t, os.WriteFile(ipcPath, []byte("{}"), 0o600))

		ostreeData := &ipconfig.OstreeData{
			NewStateroot: &ipconfig.StaterootData{Path: staterootPath},
		}
		require.NoError(t, removeIPCFileInNewStaterootIfExists(ostreeData, log))
		assert.NoFileExists(t, ipcPath)
	})

	t.Run("unusable stateroot path is an error", func(t *testing.T) {
		root := t.TempDir()
		filePath := filepath.Join(root, "not-a-dir")
		require.NoError(t, os.WriteFile(filePath, []byte("x"), 0o600))

		ostreeData := &ipconfig.OstreeData{
			NewStateroot: &ipconfig.StaterootData{Path: filePath},
		}
		err := removeIPCFileInNewStaterootIfExists(ostreeData, log)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to remove IPC file in new stateroot")
	})
}
