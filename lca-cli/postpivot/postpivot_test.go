package postpivot

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	clusterconfig_api "github.com/openshift-kni/lifecycle-agent/api/seedreconfig"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

func TestSetNodeIPIfNotProvided(t *testing.T) {
	createNodeIpFile := func(t *testing.T, ipFile string, ipToSet string) {
		f, err := os.Create(ipFile)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		defer f.Close()
		if _, err = f.WriteString(ipToSet); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}

	var (
		mockController = gomock.NewController(t)
		mockOps        = ops.NewMockOps(mockController)
	)

	defer func() {
		mockController.Finish()
	}()

	testcases := []struct {
		name             string
		expectedError    bool
		nodeipFileExists bool
		ipProvided       bool
		ipToSet          string
	}{
		{
			name:             "Ip provided nothing to do",
			expectedError:    false,
			nodeipFileExists: true,
			ipProvided:       true,
			ipToSet:          "192.167.1.2",
		},
		{
			name:             "Ip is not provided, bad ip in file",
			expectedError:    true,
			nodeipFileExists: true,
			ipProvided:       false,
			ipToSet:          "bad ip",
		},
		{
			name:             "Ip is not provided, happy flow",
			expectedError:    false,
			nodeipFileExists: true,
			ipProvided:       false,
			ipToSet:          "192.167.1.2",
		},
	}

	for _, tc := range testcases {
		tmpDir := t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			log := &logrus.Logger{}
			pp := NewPostPivot(nil, log, mockOps, "", "", "")
			seedReconfig := &clusterconfig_api.SeedReconfiguration{}
			if tc.ipProvided {
				seedReconfig.NodeIP = tc.ipToSet
			}
			ipFile := path.Join(tmpDir, "primary")
			if tc.nodeipFileExists {
				createNodeIpFile(t, ipFile, tc.ipToSet)
			} else {
				mockOps.EXPECT().SystemctlAction("start", "nodeip-configuration").Return("", nil).Do(func(any) {
					createNodeIpFile(t, ipFile, tc.ipToSet)
				})
			}

			err := pp.setNodeIPIfNotProvided(context.TODO(), seedReconfig, ipFile)
			if !tc.expectedError && err != nil {
				t.Errorf("unexpected error: %v", err)
			} else {
				assert.Equal(t, err != nil, tc.expectedError)
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
	}{
		{
			name: "Happy flow",
			seedReconfiguration: &clusterconfig_api.SeedReconfiguration{
				BaseDomain:  "new.com",
				ClusterName: "new_name",
				NodeIP:      "192.167.127.10",
			},
			expectedError: false,
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
			} else {
				assert.Equal(t, err != nil, tc.expectedError)
			}
			if !tc.expectedError {
				data, errF := os.ReadFile(confFile)
				if errF != nil {
					t.Errorf("unexpected error: %v", err)
				}
				lines := strings.Split(string(data), "\n")
				assert.Equal(t, len(lines), 3)
				assert.Equal(t, lines[0], fmt.Sprintf("SNO_CLUSTER_NAME_OVERRIDE=%s", tc.seedReconfiguration.ClusterName))
				assert.Equal(t, lines[1], fmt.Sprintf("SNO_BASE_DOMAIN_OVERRIDE=%s", tc.seedReconfiguration.BaseDomain))
				assert.Equal(t, lines[2], fmt.Sprintf("SNO_DNSMASQ_IP_OVERRIDE=%s", tc.seedReconfiguration.NodeIP))
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
		name       string
		pullSecret string
	}{
		{
			name:       "Happy flow, pull secret was set",
			pullSecret: "pull-secret",
		},
		{
			name:       "Pull secret was not set",
			pullSecret: "",
		},
	}

	for _, tc := range testcases {
		tmpDir := t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			log := &logrus.Logger{}
			pullSecretFile := path.Join(tmpDir, "config.json")
			pullSecretManifestFile := path.Join(tmpDir, pullSecretFileName)
			clientgoscheme.AddToScheme(scheme)
			pp := NewPostPivot(scheme, log, mockOps, "", tmpDir, "")
			err := pp.createPullSecretFileAndManifest(tc.pullSecret, pullSecretFile, pullSecretManifestFile)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tc.pullSecret != "" {
				secret := &corev1.Secret{}
				err = utils.ReadYamlOrJSONFile(pullSecretManifestFile, secret)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				ps, ok := secret.Data[corev1.DockerConfigJsonKey]
				if !ok {
					t.Errorf("pull secret was not found: %v", err)
				}
				assert.Equal(t, "pull-secret", string(ps))

				ps, err = os.ReadFile(pullSecretFile)
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
		mockOps        = ops.NewMockOps(mockController)
	)
	defer func() {
		mockController.Finish()
	}()

	testcases := []struct {
		name     string
		hostname string
	}{
		{
			name:     "Happy flow",
			hostname: "test",
		},
	}
	for _, tc := range testcases {
		tmpDir := t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			log := &logrus.Logger{}
			pp := NewPostPivot(nil, log, mockOps, "", "", "")
			err := pp.setHostname(tc.hostname, path.Join(tmpDir+"hostname"))
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			data, err := os.ReadFile(path.Join(tmpDir + "hostname"))
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			assert.Equal(t, "test", string(data))
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
				mockOps.EXPECT().ListBlockDevices().Return([]ops.BlockDevice{{Name: deviceName, Label: blockDeviceLabel}}, nil).Times(1)
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
		NodeIP:      "192.167.127.10",
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
			hostnameFile = path.Join(tmpDir, "hostname")
			dnsmasqOverrides = path.Join(tmpDir, "dnsmasqoverrides")

			if tc.restartNMSuccess {
				mockOps.EXPECT().SystemctlAction("restart", nmService).Return("", nil).Times(1)
				if tc.restartDNSMASQSuccess {
					mockOps.EXPECT().SystemctlAction("restart", dnsmasqService).Return("", nil).Times(1)
				} else {
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
