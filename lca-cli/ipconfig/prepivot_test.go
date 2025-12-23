package ipconfig

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	ostreemock "github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	rebootmock "github.com/openshift-kni/lifecycle-agent/internal/reboot"
	"github.com/openshift-kni/lifecycle-agent/internal/recert"
	opsmock "github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
)

func newTestHandler(t *testing.T) (*PrePivotHandler, *opsmock.MockOps, *ostreemock.MockIClient, *rpmostreeclient.MockIClient) {
	t.Helper()

	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	mockOps := opsmock.NewMockOps(ctrl)
	mockOstree := ostreemock.NewMockIClient(ctrl)
	mockRPM := rpmostreeclient.NewMockIClient(ctrl)

	handler := &PrePivotHandler{
		log:                  logrus.New(),
		ops:                  mockOps,
		ostree:               mockOstree,
		rpm:                  mockRPM,
		reboot:               rebootmock.NewMockRebootIntf(ctrl),
		ostreeData:           &OstreeData{OldStateroot: &StaterootData{}, NewStateroot: &StaterootData{}},
		hostWorkspaceDir:     t.TempDir(),
		mcdCurrentConfigPath: filepath.Join(t.TempDir(), "mcd", "currentconfig"),
	}

	return handler, mockOps, mockOstree, mockRPM
}

func TestSelectIPOfSameFamily(t *testing.T) {
	ip, err := selectIPOfSameFamily("2001::1", []string{"10.0.0.1", "2001::2"})
	if !assert.NoError(t, err) {
		return
	}
	assert.Equal(t, "2001::2", ip)
}

func TestSelectIPOfSameFamilyNoMatch(t *testing.T) {
	_, err := selectIPOfSameFamily("2001::1", []string{"10.0.0.1"})
	if !assert.Error(t, err) {
		return
	}
	assert.Contains(t, err.Error(), "NodeInternalIP")
}

func TestIPFamilyOfString(t *testing.T) {
	assert.Equal(t, common.IPv4FamilyName, ipFamilyOfString("10.0.0.1"))
	assert.Equal(t, common.IPv6FamilyName, ipFamilyOfString("2001::1"))
}

func TestGetDNSOverrideIP(t *testing.T) {
	t.Run("prefers_requested_family", func(t *testing.T) {
		handler, _, _, _ := newTestHandler(t)
		handler.dnsFilterOutFamily = common.IPv6FamilyName
		handler.ipConfigs = []*NetworkIPConfig{
			{IP: "10.0.0.1"},
			{IP: "2001::1"},
		}

		ip, err := handler.getDNSOverrideIP()
		if !assert.NoError(t, err) {
			return
		}
		// Filtering out IPv6 means we keep IPv4, so dnsmasq override should use an IPv4 address.
		assert.Equal(t, "10.0.0.1", ip)
	})

	t.Run("falls_back_to_first_when_family_missing", func(t *testing.T) {
		handler, _, _, _ := newTestHandler(t)
		handler.dnsFilterOutFamily = common.IPv6FamilyName
		handler.ipConfigs = []*NetworkIPConfig{
			{IP: "10.0.0.1"},
		}

		ip, err := handler.getDNSOverrideIP()
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, "10.0.0.1", ip)
	})

	t.Run("errors_when_no_ips", func(t *testing.T) {
		handler, _, _, _ := newTestHandler(t)
		handler.dnsFilterOutFamily = common.IPv4FamilyName

		_, err := handler.getDNSOverrideIP()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "no IP available")
	})
}

func TestDetectBrExNetworkInterface(t *testing.T) {
	t.Run("returns_physical_interface", func(t *testing.T) {
		handler, ops, _, _ := newTestHandler(t)
		ops.EXPECT().RunInHostNamespace("ovs-vsctl", "list-ports", BridgeExternalName).
			Return("ens3\npatch-br-ex-test", nil)

		iface, err := handler.detectBrExNetworkInterface()
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, "ens3", iface)
	})

	t.Run("errors_when_only_patch_ports", func(t *testing.T) {
		handler, ops, _, _ := newTestHandler(t)
		ops.EXPECT().RunInHostNamespace("ovs-vsctl", "list-ports", BridgeExternalName).
			Return("patch-br-ex-test", nil)

		_, err := handler.detectBrExNetworkInterface()
		assert.Error(t, err)
	})

	t.Run("errors_when_command_fails", func(t *testing.T) {
		handler, ops, _, _ := newTestHandler(t)
		ops.EXPECT().RunInHostNamespace("ovs-vsctl", "list-ports", BridgeExternalName).
			Return("", errors.New("boom"))

		_, err := handler.detectBrExNetworkInterface()
		assert.Error(t, err)
	})
}

func TestPrepareNetworkConfiguration(t *testing.T) {
	t.Run("builds_nmstate_config", func(t *testing.T) {
		handler, ops, _, _ := newTestHandler(t)
		handler.vlanID = 100
		handler.ipConfigs = []*NetworkIPConfig{
			{
				IP:             "10.1.1.10",
				MachineNetwork: "10.1.1.0/24",
				DesiredGateway: "10.1.1.1",
				CurrentGateway: "10.1.1.254",
				DNSServer:      "1.1.1.1",
			},
			{
				IP:             "2001::10",
				MachineNetwork: "2001::/64",
				DesiredGateway: "2001::1",
				CurrentGateway: "2001::254",
				DNSServer:      "2001::2",
			},
		}
		ops.EXPECT().RunInHostNamespace("ovs-vsctl", "list-ports", BridgeExternalName).
			Return("ens3\npatch-br-ex", nil)

		nmstate, err := handler.prepareNetworkConfiguration()
		if !assert.NoError(t, err) {
			return
		}
		if !assert.NotNil(t, nmstate) {
			return
		}
		assert.Contains(t, *nmstate, "ens3")
		assert.Contains(t, *nmstate, "10.1.1.10")
		assert.Contains(t, *nmstate, "2001::10")
		assert.Contains(t, *nmstate, "state: absent")
		assert.Contains(t, *nmstate, "10.1.1.254")
		assert.Contains(t, *nmstate, "2001::254")
	})

	t.Run("returns_errors_for_bad_inputs", func(t *testing.T) {
		tests := map[string]struct {
			configs []*NetworkIPConfig
			expect  string
		}{
			"no_configs": {
				configs: nil,
				expect:  "no IP configurations",
			},
			"nil_entry": {
				configs: []*NetworkIPConfig{nil},
				expect:  "nil IP configuration",
			},
			"missing_fields": {
				configs: []*NetworkIPConfig{{IP: "", MachineNetwork: ""}},
				expect:  "must include both IP and machine network",
			},
			"invalid_cidr": {
				configs: []*NetworkIPConfig{{IP: "10.0.0.5", MachineNetwork: "bad-cidr"}},
				expect:  "invalid machine network",
			},
		}

		for name, tc := range tests {
			t.Run(name, func(t *testing.T) {
				handler, ops, _, _ := newTestHandler(t)
				handler.ipConfigs = tc.configs
				ops.EXPECT().RunInHostNamespace("ovs-vsctl", "list-ports", BridgeExternalName).AnyTimes().
					Return("", errors.New("not needed"))

				_, err := handler.prepareNetworkConfiguration()
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tc.expect)
			})
		}
	})

	t.Run("propagates_interface_detection_error", func(t *testing.T) {
		handler, ops, _, _ := newTestHandler(t)
		handler.ipConfigs = []*NetworkIPConfig{{IP: "10.0.0.1", MachineNetwork: "10.0.0.0/24"}}
		ops.EXPECT().RunInHostNamespace("ovs-vsctl", "list-ports", BridgeExternalName).
			Return("", errors.New("fail"))

		_, err := handler.prepareNetworkConfiguration()
		assert.Error(t, err)
	})

	t.Run("does_not_remove_default_gateway_when_current_equals_desired", func(t *testing.T) {
		handler, ops, _, _ := newTestHandler(t)
		handler.vlanID = 0
		handler.ipConfigs = []*NetworkIPConfig{
			{
				IP:             "10.1.1.10",
				MachineNetwork: "10.1.1.0/24",
				DesiredGateway: "10.1.1.1",
				CurrentGateway: "10.1.1.1",
				DNSServer:      "1.1.1.1",
			},
		}
		ops.EXPECT().RunInHostNamespace("ovs-vsctl", "list-ports", BridgeExternalName).
			Return("ens3\npatch-br-ex", nil)

		nmstate, err := handler.prepareNetworkConfiguration()
		if !assert.NoError(t, err) {
			return
		}
		assert.NotNil(t, nmstate)
		assert.NotContains(t, *nmstate, "state: absent")
	})
}

func TestSetDefaultDeploymentIfEnabled(t *testing.T) {
	t.Run("feature_enabled", func(t *testing.T) {
		handler, _, ostree, rpm := newTestHandler(t)
		handler.ostree = ostree
		handler.rpm = rpm

		ostree.EXPECT().IsOstreeAdminSetDefaultFeatureEnabled().Return(true)
		rpm.EXPECT().GetDeploymentIndex("new").Return(1, nil)
		ostree.EXPECT().SetDefaultDeployment(1).Return(nil)

		assert.NoError(t, handler.setDefaultDeploymentIfEnabled("new"))
	})

	t.Run("feature_disabled", func(t *testing.T) {
		handler, _, ostree, rpm := newTestHandler(t)
		handler.ostree = ostree
		handler.rpm = rpm

		ostree.EXPECT().IsOstreeAdminSetDefaultFeatureEnabled().Return(false)

		err := handler.setDefaultDeploymentIfEnabled("new")
		assert.Error(t, err)
	})
}

func TestCopyVar(t *testing.T) {
	handler, ops, _, _ := newTestHandler(t)

	oldPath := "/old"
	newPath := "/new"
	expectedCmd := fmt.Sprintf("cp -ar --preserve=context '%s/' '%s/'", filepath.Join(oldPath, "var"), newPath)

	ops.EXPECT().RunInHostNamespace("bash", "-c", expectedCmd).Return("", nil)

	assert.NoError(t, handler.copyVar(oldPath, newPath))

	ops.EXPECT().RunInHostNamespace("bash", "-c", expectedCmd).Return("", errors.New("fail"))
	err := handler.copyVar(oldPath, newPath)
	assert.Error(t, err)
}

func TestCopyEtc(t *testing.T) {
	handler, ops, _, _ := newTestHandler(t)

	oldDir := "/old/deploy"
	newDir := "/new/deploy"
	expectedCmd := fmt.Sprintf("cp -ar --preserve=context '%s/' '%s/'", filepath.Join(oldDir, "etc"), newDir)

	ops.EXPECT().RunInHostNamespace("bash", "-c", expectedCmd).Return("", nil)
	assert.NoError(t, handler.copyEtc(oldDir, newDir))

	ops.EXPECT().RunInHostNamespace("bash", "-c", expectedCmd).Return("", errors.New("fail"))
	assert.Error(t, handler.copyEtc(oldDir, newDir))
}

func TestCopyDeploymentOrigin(t *testing.T) {
	handler, ops, _, _ := newTestHandler(t)

	oldPath := "/old"
	newPath := "/new"
	oldName := "olddep"
	newName := "newdep"
	expectedCmd := fmt.Sprintf(
		"cp -a --preserve=context '%s' '%s'",
		filepath.Join(oldPath, "deploy", fmt.Sprintf("%s.origin", oldName)),
		filepath.Join(newPath, "deploy", fmt.Sprintf("%s.origin", newName)),
	)

	ops.EXPECT().RunInHostNamespace("bash", "-c", expectedCmd).Return("", nil)
	assert.NoError(t, handler.copyDeploymentOrigin(oldPath, newPath, oldName, newName))

	ops.EXPECT().RunInHostNamespace("bash", "-c", expectedCmd).Return("", errors.New("fail"))
	assert.Error(t, handler.copyDeploymentOrigin(oldPath, newPath, oldName, newName))
}

func TestEnsureSysrootWritable(t *testing.T) {
	handler, ops, _, _ := newTestHandler(t)
	ops.EXPECT().RemountSysroot().Return(nil)
	assert.NoError(t, handler.ensureSysrootWritable())

	ops.EXPECT().RemountSysroot().Return(errors.New("ro"))
	assert.Error(t, handler.ensureSysrootWritable())
}

func TestGetBootedCommit(t *testing.T) {
	handler, _, _, rpm := newTestHandler(t)
	handler.rpm = rpm

	rpm.EXPECT().QueryStatus().Return(&rpmostreeclient.Status{
		Deployments: []rpmostreeclient.Deployment{{Checksum: "abc", Booted: true}},
	}, nil)
	commit, err := handler.getBootedCommit()
	if !assert.NoError(t, err) {
		return
	}
	if !assert.NotNil(t, commit) {
		return
	}
	assert.Equal(t, "abc", *commit)

	rpm.EXPECT().QueryStatus().Return(&rpmostreeclient.Status{
		Deployments: []rpmostreeclient.Deployment{{Checksum: "abc", Booted: false}},
	}, nil)
	_, err = handler.getBootedCommit()
	assert.Error(t, err)
}

func TestDeployNewStateroot(t *testing.T) {
	handler, _, ostree, rpm := newTestHandler(t)
	handler.ostree = ostree
	handler.rpm = rpm

	data := &OstreeData{NewStateroot: &StaterootData{Name: "new"}}

	ostree.EXPECT().OSInit("new").Return(nil)
	ostree.EXPECT().Deploy("new", "commit", []string{"karg"}, rpm, false).Return(nil)
	ostree.EXPECT().GetDeployment("new").Return("deploy1", nil)
	ostree.EXPECT().GetDeploymentDir("new").Return("/deployment/dir", nil)

	assert.NoError(t, handler.deployNewStateroot(data, "commit", []string{"karg"}))
	assert.Equal(t, "deploy1", data.NewStateroot.DeploymentName)
	assert.Equal(t, "/deployment/dir", data.NewStateroot.DeploymentDir)
}

func TestPrepareNewStaterootDeploysWhenMissing(t *testing.T) {
	handler, ops, ostree, rpm := newTestHandler(t)
	handler.ostree = ostree
	handler.rpm = rpm
	handler.ostreeData = &OstreeData{
		OldStateroot: &StaterootData{
			Path:           "/old",
			DeploymentDir:  "/old/deploy",
			DeploymentName: "olddep",
		},
		NewStateroot: &StaterootData{
			Name: "new",
			Path: "/new",
		},
	}

	ops.EXPECT().RemountSysroot().Return(nil)
	rpm.EXPECT().QueryStatus().Return(&rpmostreeclient.Status{
		Deployments: []rpmostreeclient.Deployment{{Checksum: "abc", Booted: true}},
	}, nil)
	ostree.EXPECT().OSInit("new").Return(nil)
	ostree.EXPECT().Deploy("new", "abc", []string{"karg"}, rpm, false).Return(nil)
	ostree.EXPECT().GetDeployment("new").Return("newdep", nil)
	ostree.EXPECT().GetDeploymentDir("new").Return("/new/deploy", nil)
	ops.EXPECT().RunInHostNamespace(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes().Return("", nil)
	ostree.EXPECT().IsOstreeAdminSetDefaultFeatureEnabled().Return(true)
	rpm.EXPECT().GetDeploymentIndex("new").Return(1, nil)
	ostree.EXPECT().SetDefaultDeployment(1).Return(nil)

	err := handler.prepareNewStateroot(handler.ostreeData, []string{"karg"})
	assert.NoError(t, err)
	assert.Equal(t, "newdep", handler.ostreeData.NewStateroot.DeploymentName)
}

func TestPrepareNewStaterootSkipsDeployWhenAlreadyPresent(t *testing.T) {
	handler, ops, ostree, rpm := newTestHandler(t)
	handler.ostree = ostree
	handler.rpm = rpm
	handler.ostreeData = &OstreeData{
		OldStateroot: &StaterootData{
			Path:           "/old",
			DeploymentDir:  "/old/deploy",
			DeploymentName: "olddep",
		},
		NewStateroot: &StaterootData{
			Name:           "new",
			Path:           "/new",
			DeploymentDir:  "/new/deploy",
			DeploymentName: "existing",
		},
	}

	ops.EXPECT().RemountSysroot().Return(nil)
	ostree.EXPECT().Deploy(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	ops.EXPECT().RunInHostNamespace(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes().Return("", nil)
	ostree.EXPECT().IsOstreeAdminSetDefaultFeatureEnabled().Return(true)
	rpm.EXPECT().GetDeploymentIndex("new").Return(1, nil)
	ostree.EXPECT().SetDefaultDeployment(1).Return(nil)

	assert.NoError(t, handler.prepareNewStateroot(handler.ostreeData, nil))
}

func TestUpdateDNSMasqOverrideIPInNewStateroot(t *testing.T) {
	t.Run("replaces_existing_override", func(t *testing.T) {
		handler, ops, _, _ := newTestHandler(t)
		handler.ipConfigs = []*NetworkIPConfig{{IP: "2.2.2.2"}}
		handler.ostreeData = &OstreeData{
			NewStateroot: &StaterootData{DeploymentDir: "/new/deploy"},
		}

		ops.EXPECT().ReadFile(common.DnsmasqOverrides).Return([]byte("FOO=bar\nSNO_DNSMASQ_IP_OVERRIDE=1.1.1.1\n"), nil)
		expectedPath := filepath.Join(handler.ostreeData.NewStateroot.DeploymentDir, common.DnsmasqOverrides)
		ops.EXPECT().WriteFile(expectedPath, []byte("FOO=bar\nSNO_DNSMASQ_IP_OVERRIDE=2.2.2.2\n"), os.FileMode(common.FileMode0600)).
			Return(nil)

		assert.NoError(t, handler.updateDNSMasqOverrideIPInNewStateroot())
	})

	t.Run("appends_when_file_missing", func(t *testing.T) {
		handler, ops, _, _ := newTestHandler(t)
		handler.ipConfigs = []*NetworkIPConfig{{IP: "3.3.3.3"}}
		handler.ostreeData = &OstreeData{
			NewStateroot: &StaterootData{DeploymentDir: "/new/deploy"},
		}

		ops.EXPECT().ReadFile(common.DnsmasqOverrides).Return(nil, errors.New("not-exist"))
		ops.EXPECT().IsNotExist(gomock.Any()).Return(true)
		expectedPath := filepath.Join(handler.ostreeData.NewStateroot.DeploymentDir, common.DnsmasqOverrides)
		ops.EXPECT().WriteFile(expectedPath, []byte("SNO_DNSMASQ_IP_OVERRIDE=3.3.3.3\n"), os.FileMode(common.FileMode0600)).
			Return(nil)

		assert.NoError(t, handler.updateDNSMasqOverrideIPInNewStateroot())
	})

	t.Run("returns_error_on_read_failure", func(t *testing.T) {
		handler, ops, _, _ := newTestHandler(t)
		handler.ipConfigs = []*NetworkIPConfig{{IP: "4.4.4.4"}}
		handler.ostreeData = &OstreeData{
			NewStateroot: &StaterootData{DeploymentDir: "/new/deploy"},
		}

		ops.EXPECT().ReadFile(common.DnsmasqOverrides).Return(nil, errors.New("boom"))
		ops.EXPECT().IsNotExist(gomock.Any()).Return(false)

		assert.Error(t, handler.updateDNSMasqOverrideIPInNewStateroot())
	})
}

func TestUpdateDNSMasqOverrideIPChoosesFamily(t *testing.T) {
	handler, _, _, _ := newTestHandler(t)
	handler.dnsFilterOutFamily = common.IPv6FamilyName
	handler.ipConfigs = []*NetworkIPConfig{
		{IP: "10.0.0.1"},
		{IP: "2001::10"},
	}

	ip, err := handler.getDNSOverrideIP()
	if !assert.NoError(t, err) {
		return
	}
	assert.Equal(t, "10.0.0.1", ip)
}

func TestPrepareNewStaterootErrorsPropagate(t *testing.T) {
	handler, ops, _, _ := newTestHandler(t)
	handler.ostreeData = &OstreeData{
		OldStateroot: &StaterootData{Path: "/old", DeploymentDir: "/old/deploy", DeploymentName: "old"},
		NewStateroot: &StaterootData{Name: "new", Path: "/new"},
	}

	ops.EXPECT().RemountSysroot().Return(errors.New("ro"))
	err := handler.prepareNewStateroot(handler.ostreeData, nil)
	assert.Error(t, err)
}

func TestUpdateDNSMasqOverrideIPPreservesOtherLines(t *testing.T) {
	handler, ops, _, _ := newTestHandler(t)
	handler.ipConfigs = []*NetworkIPConfig{{IP: "5.5.5.5"}}
	handler.ostreeData = &OstreeData{
		NewStateroot: &StaterootData{DeploymentDir: "/new/deploy"},
	}

	content := strings.Join([]string{
		"FOO=1",
		"SNO_DNSMASQ_IP_OVERRIDE=1.1.1.1",
		"BAR=2",
	}, "\n") + "\n"

	ops.EXPECT().ReadFile(common.DnsmasqOverrides).Return([]byte(content), nil)
	expectedPath := filepath.Join(handler.ostreeData.NewStateroot.DeploymentDir, common.DnsmasqOverrides)
	expectedContent := strings.Join([]string{
		"FOO=1",
		"SNO_DNSMASQ_IP_OVERRIDE=5.5.5.5",
		"BAR=2",
	}, "\n") + "\n"

	ops.EXPECT().WriteFile(expectedPath, []byte(expectedContent), os.FileMode(common.FileMode0600)).Return(nil)

	assert.NoError(t, handler.updateDNSMasqOverrideIPInNewStateroot())
}

func TestUpdateDNSMasqFilterInNewStateroot(t *testing.T) {
	t.Run("ipv4 writes filter file", func(t *testing.T) {
		handler, ops, _, _ := newTestHandler(t)
		handler.dnsFilterOutFamily = common.IPv4FamilyName
		handler.ostreeData = &OstreeData{
			NewStateroot: &StaterootData{DeploymentDir: "/new/deploy"},
		}

		expectedPath := filepath.Join(handler.ostreeData.NewStateroot.DeploymentDir, "etc/dnsmasq.d/single-node-filter.conf")
		ops.EXPECT().MkdirAll(filepath.Dir(expectedPath), os.FileMode(0o755)).Return(nil)
		ops.EXPECT().WriteFile(
			expectedPath,
			[]byte(common.DnsmasqFilterManagedByIPConfigHeader+common.DnsmasqFilterOutIPv4+"\n"),
			os.FileMode(common.FileMode0644),
		).Return(nil)

		assert.NoError(t, handler.updateDNSMasqFilterInNewStateroot())
	})

	t.Run("none removes filter file", func(t *testing.T) {
		handler, ops, _, _ := newTestHandler(t)
		handler.dnsFilterOutFamily = common.DNSFamilyNone
		handler.ostreeData = &OstreeData{
			NewStateroot: &StaterootData{DeploymentDir: "/new/deploy"},
		}

		expectedPath := filepath.Join(handler.ostreeData.NewStateroot.DeploymentDir, "etc/dnsmasq.d/single-node-filter.conf")
		ops.EXPECT().RemoveFile(expectedPath).Return(os.ErrNotExist)
		ops.EXPECT().IsNotExist(os.ErrNotExist).Return(true)

		assert.NoError(t, handler.updateDNSMasqFilterInNewStateroot())
	})
}

func TestRunStopsAndReenablesOnFailure(t *testing.T) {
	// This test exercises the real Run() flow using only the existing gomock
	// interfaces. We intentionally fail at the DNSMasq override update step to
	// avoid touching the host's stale-file removal paths.
	handler, ops, ostree, rpm := newTestHandler(t)
	ops.EXPECT().ReadFile(handler.mcdCurrentConfigPath).Return([]byte(minimalMachineConfigYAML([]string{"foo=bar"})), nil)

	handler.client = newFakeClient(t)
	handler.ipConfigs = []*NetworkIPConfig{
		{IP: "10.1.1.10", MachineNetwork: "10.1.1.0/24", DesiredGateway: "10.1.1.1", DNSServer: "1.1.1.1"},
	}
	handler.ostreeData = &OstreeData{
		OldStateroot: &StaterootData{
			Path:           "/old",
			DeploymentDir:  "/old/deploy",
			DeploymentName: "olddep",
		},
		NewStateroot: &StaterootData{
			Name:           "new",
			Path:           "/new",
			DeploymentDir:  "/new/deploy",
			DeploymentName: "newdep", // already deployed, so Run() won't call ostree admin deploy
		},
	}
	handler.ostree = ostree
	handler.rpm = rpm

	// Pull secret from disk (mocked)
	ops.EXPECT().ReadFile(common.ImageRegistryAuthFile).Return([]byte("ps"), nil)

	// Recert crypto collection (createCryptoDir uses ops.MkdirAll; BackupKubeconfigCrypto uses the fake client)
	ops.EXPECT().MkdirAll(filepath.Join(handler.hostWorkspaceDir, common.KubeconfigCryptoDir), os.FileMode(0o755)).Return(nil)

	// Network config
	ops.EXPECT().RunInHostNamespace("ovs-vsctl", "list-ports", BridgeExternalName).Return("ens3\npatch-br-ex", nil)

	// Stop/enable services
	ops.EXPECT().StopClusterServices().Return(nil)
	ops.EXPECT().EnableClusterServices().Return(nil)

	// New stateroot prep (no deploy)
	ops.EXPECT().RemountSysroot().Return(nil)
	ops.EXPECT().RunInHostNamespace("bash", "-c", gomock.Any()).AnyTimes().Return("", nil)
	ostree.EXPECT().IsOstreeAdminSetDefaultFeatureEnabled().Return(true)
	rpm.EXPECT().GetDeploymentIndex("new").Return(1, nil)
	ostree.EXPECT().SetDefaultDeployment(1).Return(nil)

	// Writes to new stateroot workspace
	ops.EXPECT().WriteFile(
		filepath.Join(handler.ostreeData.NewStateroot.Path, common.IPConfigPullSecretFile),
		[]byte("ps"),
		os.FileMode(0o600),
	).Return(nil)
	ops.EXPECT().WriteFile(
		filepath.Join(handler.ostreeData.NewStateroot.Path, common.LCAWorkspaceDir, recert.RecertConfigFile),
		gomock.Any(),
		os.FileMode(common.FileMode0600),
	).Return(nil)
	ops.EXPECT().WriteFile(
		filepath.Join(handler.ostreeData.NewStateroot.Path, common.LCAWorkspaceDir, common.NmstateConfigFileName),
		gomock.Any(),
		os.FileMode(common.FileMode0600),
	).Return(nil)

	// Fail at dnsmasq override update
	ops.EXPECT().ReadFile(common.DnsmasqOverrides).Return(nil, errors.New("not-exist"))
	ops.EXPECT().IsNotExist(gomock.Any()).Return(true)
	ops.EXPECT().WriteFile(
		filepath.Join(handler.ostreeData.NewStateroot.DeploymentDir, common.DnsmasqOverrides),
		gomock.Any(),
		os.FileMode(common.FileMode0600),
	).Return(errors.New("boom"))

	err := handler.Run(context.Background())
	assert.Error(t, err)
}

func TestRunFailsBeforeStoppingServices(t *testing.T) {
	handler, ops, _, _ := newTestHandler(t)
	handler.ostreeData = &OstreeData{
		OldStateroot: &StaterootData{},
		NewStateroot: &StaterootData{},
	}
	ops.EXPECT().ReadFile(handler.mcdCurrentConfigPath).Return(nil, os.ErrNotExist)

	ops.EXPECT().StopClusterServices().Times(0)
	ops.EXPECT().EnableClusterServices().Times(0)

	err := handler.Run(context.Background())
	assert.Error(t, err)
}

func TestRunDoesNotPersistOrDeleteACMHubKubeconfigSecretWhenPresent(t *testing.T) {
	handler, ops, ostree, rpm := newTestHandler(t)

	ops.EXPECT().ReadFile(handler.mcdCurrentConfigPath).Return([]byte(minimalMachineConfigYAML([]string{"foo=bar"})), nil)

	handler.client = newFakeClient(t)
	// Add one ACM hub kubeconfig secret
	err := handler.client.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "open-cluster-management-agent",
			Name:      "hub-kubeconfig-secret",
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"kubeconfig": []byte("apiVersion: v1\nclusters: []\n"),
		},
	})
	if !assert.NoError(t, err) {
		return
	}

	handler.ipConfigs = []*NetworkIPConfig{
		{IP: "10.1.1.10", MachineNetwork: "10.1.1.0/24", DesiredGateway: "10.1.1.1", DNSServer: "1.1.1.1"},
	}
	handler.ostreeData = &OstreeData{
		OldStateroot: &StaterootData{
			Path:           "/old",
			DeploymentDir:  "/old/deploy",
			DeploymentName: "olddep",
		},
		NewStateroot: &StaterootData{
			Name:           "new",
			Path:           "/new",
			DeploymentDir:  "/new/deploy",
			DeploymentName: "newdep", // already deployed, so Run() won't call ostree admin deploy
		},
	}
	handler.ostree = ostree
	handler.rpm = rpm

	ops.EXPECT().ReadFile(common.ImageRegistryAuthFile).Return([]byte("ps"), nil)
	ops.EXPECT().MkdirAll(filepath.Join(handler.hostWorkspaceDir, common.KubeconfigCryptoDir), os.FileMode(0o755)).Return(nil)
	ops.EXPECT().RunInHostNamespace("ovs-vsctl", "list-ports", BridgeExternalName).Return("ens3\npatch-br-ex", nil)

	ops.EXPECT().StopClusterServices().Return(nil)
	ops.EXPECT().EnableClusterServices().Return(nil)

	ops.EXPECT().RemountSysroot().Return(nil)
	ops.EXPECT().RunInHostNamespace("bash", "-c", gomock.Any()).AnyTimes().Return("", nil)
	ostree.EXPECT().IsOstreeAdminSetDefaultFeatureEnabled().Return(true)
	rpm.EXPECT().GetDeploymentIndex("new").Return(1, nil)
	ostree.EXPECT().SetDefaultDeployment(1).Return(nil)

	ops.EXPECT().WriteFile(
		filepath.Join(handler.ostreeData.NewStateroot.Path, common.IPConfigPullSecretFile),
		[]byte("ps"),
		os.FileMode(0o600),
	).Return(nil)
	ops.EXPECT().WriteFile(
		filepath.Join(handler.ostreeData.NewStateroot.Path, common.LCAWorkspaceDir, recert.RecertConfigFile),
		gomock.Any(),
		os.FileMode(common.FileMode0600),
	).Return(nil)
	ops.EXPECT().WriteFile(
		filepath.Join(handler.ostreeData.NewStateroot.Path, common.LCAWorkspaceDir, common.NmstateConfigFileName),
		gomock.Any(),
		os.FileMode(common.FileMode0600),
	).Return(nil)

	// Stop after we have written the secret by failing at dnsmasq override update
	ops.EXPECT().ReadFile(common.DnsmasqOverrides).Return(nil, errors.New("not-exist"))
	ops.EXPECT().IsNotExist(gomock.Any()).Return(true)
	ops.EXPECT().WriteFile(
		filepath.Join(handler.ostreeData.NewStateroot.DeploymentDir, common.DnsmasqOverrides),
		gomock.Any(),
		os.FileMode(common.FileMode0600),
	).Return(errors.New("boom"))

	runErr := handler.Run(context.Background())
	assert.Error(t, runErr)

	// The secret should still exist (we no longer persist/delete ACM hub kubeconfig secrets).
	s := &corev1.Secret{}
	getErr := handler.client.Get(
		context.Background(),
		types.NamespacedName{Namespace: "open-cluster-management-agent", Name: "hub-kubeconfig-secret"},
		s,
	)
	assert.NoError(t, getErr)
}

func newFakeClient(t *testing.T) runtimeclient.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add corev1 to scheme: %v", err)
	}
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add apiextensionsv1 to scheme: %v", err)
	}

	ingressCrt := mustSelfSignedCertPEM(t, "ingress-cn")

	objects := []runtimeclient.Object{
		// install-config (used by GetInstallConfig)
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-config-v1", Namespace: "kube-system"},
			Data:       map[string]string{"install-config": "apiVersion: v1\nbaseDomain: example.com\n"},
		},
		// node internal IP (used by GetNodeInternalIPs)
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node1"},
			Status: corev1.NodeStatus{
				Addresses: []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
				},
			},
		},
		// kubeconfig crypto inputs (used by BackupKubeconfigCrypto)
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "admin-kubeconfig-client-ca", Namespace: "openshift-config"},
			Data:       map[string]string{"ca-bundle.crt": "dummy-ca"},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "loadbalancer-serving-signer", Namespace: "openshift-kube-apiserver-operator"},
			Data:       map[string][]byte{"tls.key": []byte("dummy-key")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "localhost-serving-signer", Namespace: "openshift-kube-apiserver-operator"},
			Data:       map[string][]byte{"tls.key": []byte("dummy-key")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "service-network-serving-signer", Namespace: "openshift-kube-apiserver-operator"},
			Data:       map[string][]byte{"tls.key": []byte("dummy-key")},
		},
		// ingress router-ca (used by GetIngressCertificateCN and BackupKubeconfigCrypto)
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "router-ca", Namespace: "openshift-ingress-operator"},
			Data: map[string][]byte{
				"tls.crt": []byte(ingressCrt),
				"tls.key": []byte("dummy-ingress-key"),
			},
		},
	}

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func mustSelfSignedCertPEM(t *testing.T, cn string) string {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatalf("failed to generate serial: %v", err)
	}

	tmpl := x509.Certificate{
		SerialNumber: serial,
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		Subject:      pkix.Name{CommonName: cn},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes}))
}

func minimalMachineConfigYAML(kernelArgs []string) string {
	var b strings.Builder
	b.WriteString("apiVersion: machineconfiguration.openshift.io/v1\n")
	b.WriteString("kind: MachineConfig\n")
	b.WriteString("metadata:\n  name: test\n")
	b.WriteString("spec:\n")
	if len(kernelArgs) == 0 {
		b.WriteString("  kernelArguments: []\n")
		return b.String()
	}
	b.WriteString("  kernelArguments:\n")
	for _, a := range kernelArgs {
		b.WriteString(fmt.Sprintf("  - %q\n", a))
	}
	return b.String()
}
