package reboot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipcv1 "github.com/openshift-kni/lifecycle-agent/api/ipconfig/v1"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
)

// redirectAutoRollbackConfigs points both auto-rollback config files into a temp dir.
func redirectAutoRollbackConfigs(t *testing.T) (ibuCfg, ipcCfg string) {
	t.Helper()
	if _, err := os.Stat(common.Host); err == nil {
		t.Skip("config paths are prefixed with " + common.Host + " on this host")
	}
	dir := t.TempDir()
	origIBU, origIPC := ibuAutoRollbackConfigFile, ipcAutoRollbackConfigFile
	ibuAutoRollbackConfigFile = filepath.Join(dir, "ibu-autorollback.json")
	ipcAutoRollbackConfigFile = filepath.Join(dir, "ipc-autorollback.json")
	t.Cleanup(func() { ibuAutoRollbackConfigFile, ipcAutoRollbackConfigFile = origIBU, origIPC })
	return ibuAutoRollbackConfigFile, ipcAutoRollbackConfigFile
}

type autoRollbackClient struct {
	name    string
	cfgFile func(ibuCfg, ipcCfg string) string
	newFunc func(log *logr.Logger, ostree ostreeclient.IClient, ctrl *gomock.Controller) RebootIntf
}

var autoRollbackClients = []autoRollbackClient{
	{
		name:    "ibu",
		cfgFile: func(ibuCfg, _ string) string { return ibuCfg },
		newFunc: func(log *logr.Logger, ostree ostreeclient.IClient, ctrl *gomock.Controller) RebootIntf {
			return NewIBURebootClient(log, ops.NewMockExecute(ctrl), rpmostreeclient.NewMockIClient(ctrl), ostree, ops.NewMockOps(ctrl))
		},
	},
	{
		name:    "ipc",
		cfgFile: func(_, ipcCfg string) string { return ipcCfg },
		newFunc: func(log *logr.Logger, ostree ostreeclient.IClient, ctrl *gomock.Controller) RebootIntf {
			return NewIPCRebootClient(log, ops.NewMockExecute(ctrl), rpmostreeclient.NewMockIClient(ctrl), ostree, ops.NewMockOps(ctrl))
		},
	},
}

func TestReadAutoRollbackConfigFile(t *testing.T) {
	for _, rc := range autoRollbackClients {
		t.Run(rc.name, func(t *testing.T) {
			cfgFile := rc.cfgFile(redirectAutoRollbackConfigs(t))
			log := logr.Discard()
			client := rc.newFunc(&log, nil, gomock.NewController(t))

			cfg, err := client.ReadAutoRollbackConfigFile()
			assert.ErrorIs(t, err, os.ErrNotExist)
			assert.False(t, cfg.InitMonitorEnabled, "defaults returned when the file is missing")

			assert.NoError(t, os.WriteFile(cfgFile, []byte("{"), 0o600))
			_, err = client.ReadAutoRollbackConfigFile()
			assert.ErrorContains(t, err, "failed to read and decode")

			assert.NoError(t, os.WriteFile(cfgFile,
				[]byte(`{"monitor_enabled":true,"monitor_timeout":600,"enabled_components":{"postpivot":true}}`), 0o600))
			cfg, err = client.ReadAutoRollbackConfigFile()
			assert.NoError(t, err)
			assert.Equal(t, &AutoRollbackConfig{
				InitMonitorEnabled: true,
				InitMonitorTimeout: 600,
				EnabledComponents:  map[string]bool{PostPivotComponent: true},
			}, cfg)
		})
	}
}

func TestAutoRollbackIfEnabled(t *testing.T) {
	testCases := []struct {
		name            string
		config          string // empty means no config file
		expectRollback  bool
		expectLogSubstr string
	}{
		{name: "missing config is silently ignored"},
		{name: "unreadable config is logged", config: "{", expectLogSubstr: "Unable to read"},
		{
			name:            "component disabled",
			config:          `{"enabled_components":{"postpivot":false}}`,
			expectLogSubstr: "disabled for component: postpivot",
		},
		{
			name:            "component enabled triggers rollback",
			config:          `{"enabled_components":{"postpivot":true}}`,
			expectRollback:  true,
			expectLogSubstr: "automatic rollback not supported",
		},
	}

	for _, rc := range autoRollbackClients {
		for _, tc := range testCases {
			t.Run(rc.name+" "+tc.name, func(t *testing.T) {
				cfgFile := rc.cfgFile(redirectAutoRollbackConfigs(t))
				if tc.config != "" {
					assert.NoError(t, os.WriteFile(cfgFile, []byte(tc.config), 0o600))
				}

				var logs []string
				log := funcr.New(func(prefix, args string) { logs = append(logs, args) }, funcr.Options{})

				ctrl := gomock.NewController(t)
				ostree := ostreeclient.NewMockIClient(ctrl)
				if tc.expectRollback {
					// stop InitiateRollback at its first check
					ostree.EXPECT().IsOstreeAdminSetDefaultFeatureEnabled().Return(false)
				}

				rc.newFunc(&log, ostree, ctrl).AutoRollbackIfEnabled(PostPivotComponent, "post pivot failed")

				joined := strings.Join(logs, "\n")
				if tc.expectLogSubstr == "" {
					assert.Empty(t, logs)
				} else {
					assert.Contains(t, joined, tc.expectLogSubstr)
				}
			})
		}
	}
}

func TestWriteIPCAutoRollbackConfigFile(t *testing.T) {
	_, ipcCfg := redirectAutoRollbackConfigs(t)
	ctrl := gomock.NewController(t)
	log := logr.Discard()

	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().WriteFile(ipcCfg, gomock.Any(), os.FileMode(0o600)).
		DoAndReturn(func(name string, data []byte, perm os.FileMode) error { return os.WriteFile(name, data, perm) })
	assert.NoError(t, WriteIPCAutoRollbackConfigFile(log, nil, mockOps))

	// round trip: the IPC client reads back what was written to the redirected path
	cfg, err := NewIPCRebootClient(&log, nil, nil, nil, nil).ReadAutoRollbackConfigFile()
	assert.NoError(t, err)
	assert.Equal(t, &AutoRollbackConfig{
		InitMonitorEnabled: true,
		InitMonitorTimeout: common.IPCAutoRollbackInitMonitorTimeoutDefaultSeconds,
		EnabledComponents:  map[string]bool{IPConfigRunComponent: true},
	}, cfg)

	mockOps.EXPECT().WriteFile(ipcCfg, gomock.Any(), gomock.Any()).Return(os.ErrPermission)
	assert.ErrorIs(t, WriteIPCAutoRollbackConfigFile(log, nil, mockOps), os.ErrPermission)
}

func TestWriteIPCAutoRollbackConfigFileWithIPC(t *testing.T) {
	testCases := []struct {
		name              string
		ipc               *ipcv1.IPConfig
		expectTimeout     int
		expectInitMonitor bool
		expectIPConfigRun bool
	}{
		{
			name:              "defaults when nothing is set",
			ipc:               &ipcv1.IPConfig{},
			expectTimeout:     common.IPCAutoRollbackInitMonitorTimeoutDefaultSeconds,
			expectInitMonitor: true,
			expectIPConfigRun: true,
		},
		{
			name: "custom timeout with both annotations disabled",
			ipc: &ipcv1.IPConfig{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
					common.AutoRollbackOnFailureInitMonitorAnnotation: common.AutoRollbackDisableValue,
					common.AutoRollbackOnFailureIPConfigRunAnnotation: common.AutoRollbackDisableValue,
				}},
				Spec: ipcv1.IPConfigSpec{
					AutoRollbackOnFailure: &ipcv1.AutoRollbackOnFailure{InitMonitorTimeoutSeconds: 42},
				},
			},
			expectTimeout:     42,
			expectInitMonitor: false,
			expectIPConfigRun: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, ipcCfg := redirectAutoRollbackConfigs(t)
			log := logr.Discard()

			ctrl := gomock.NewController(t)
			mockOps := ops.NewMockOps(ctrl)
			mockOps.EXPECT().WriteFile(ipcCfg, gomock.Any(), os.FileMode(0o600)).
				DoAndReturn(func(name string, data []byte, perm os.FileMode) error { return os.WriteFile(name, data, perm) })
			assert.NoError(t, WriteIPCAutoRollbackConfigFile(log, tc.ipc, mockOps))

			cfg, err := NewIPCRebootClient(&log, nil, nil, nil, nil).ReadAutoRollbackConfigFile()
			assert.NoError(t, err)
			assert.Equal(t, tc.expectTimeout, cfg.InitMonitorTimeout)
			assert.Equal(t, tc.expectInitMonitor, cfg.InitMonitorEnabled)
			assert.Equal(t, map[string]bool{IPConfigRunComponent: tc.expectIPConfigRun}, cfg.EnabledComponents)
		})
	}
}
