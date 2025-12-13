package reboot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	ipcv1 "github.com/openshift-kni/lifecycle-agent/api/ipconfig/v1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	lcautils "github.com/openshift-kni/lifecycle-agent/utils"
)

var (
	defaultRebootTimeout = 60 * time.Minute
)

const (
	PostPivotComponent                 = "postpivot"
	InstallationConfigurationComponent = "config"
	IPConfigRunComponent               = "ip-config-run"
)

type AutoRollbackConfig struct {
	InitMonitorEnabled bool            `json:"monitor_enabled,omitempty"`
	InitMonitorTimeout int             `json:"monitor_timeout,omitempty"`
	EnabledComponents  map[string]bool `json:"enabled_components,omitempty"`
}

// TODO: Refactor this share most of the code in each of the interface implementations

// RebootIntf is an interface for LCA reboot and rollback commands.
//
//go:generate mockgen -source=reboot.go -package=reboot -destination=mock_reboot.go
type RebootIntf interface {
	ReadAutoRollbackConfigFile() (*AutoRollbackConfig, error)
	DisableInitMonitor() error
	Reboot(rationale string) error
	RebootToNewStateRoot(rationale string) error
	IsOrigStaterootBooted(identifier string) (bool, error)
	InitiateRollback(msg string) error
	AutoRollbackIfEnabled(component, msg string)
}

type IBURebootClient struct {
	log                  *logr.Logger
	hostCommandsExecutor ops.Execute
	rpmOstreeClient      rpmostreeclient.IClient
	ostreeClient         ostreeclient.IClient
	ops                  ops.Ops
}

// NewIBURebootClient creates and returns a RebootIntf interface for reboot and rollback commands
func NewIBURebootClient(log *logr.Logger,
	hostCommandsExecutor ops.Execute,
	rpmOstreeClient rpmostreeclient.IClient,
	ostreeClient ostreeclient.IClient,
	ops ops.Ops) RebootIntf {
	return &IBURebootClient{
		log:                  log,
		hostCommandsExecutor: hostCommandsExecutor,
		rpmOstreeClient:      rpmOstreeClient,
		ostreeClient:         ostreeClient,
		ops:                  ops,
	}
}

func WriteIBUAutoRollbackConfigFile(log logr.Logger, ibu *ibuv1.ImageBasedUpgrade) error {
	stateroot := common.GetStaterootName(ibu.Spec.SeedImageRef.Version)
	staterootPath := common.GetStaterootPath(stateroot)
	cfgfile := common.PathOutsideChroot(filepath.Join(staterootPath, common.IBUAutoRollbackConfigFile))

	cfgdir := filepath.Dir(cfgfile)
	if err := os.MkdirAll(cfgdir, 0o700); err != nil {
		return fmt.Errorf("unable to create config dir: %s: %w", cfgdir, err)
	}

	monitorTimeout := common.IBUAutoRollbackInitMonitorTimeoutDefaultSeconds
	if ibu.Spec.AutoRollbackOnFailure != nil && ibu.Spec.AutoRollbackOnFailure.InitMonitorTimeoutSeconds > 0 {
		monitorTimeout = ibu.Spec.AutoRollbackOnFailure.InitMonitorTimeoutSeconds
	}

	log.Info("Auto-rollback init monitor timeout", "monitorTimeout", monitorTimeout)

	// check autoRollback's InitMonitor config from annotation
	initMonitorEnabled := true
	if val, exists := ibu.GetAnnotations()[common.AutoRollbackOnFailureInitMonitorAnnotation]; exists {
		if val == common.AutoRollbackDisableValue {
			initMonitorEnabled = false
		}
	}
	log.Info("Auto-rollback init monitor config", "initMonitorEnabled", initMonitorEnabled)

	rollbackCfg := AutoRollbackConfig{
		InitMonitorEnabled: initMonitorEnabled,
		InitMonitorTimeout: monitorTimeout,
		EnabledComponents:  make(map[string]bool),
	}

	// check autoRollback's PostReboot config from annotation
	postRebootConfigEnabled := true
	if val, exists := ibu.GetAnnotations()[common.AutoRollbackOnFailurePostRebootConfigAnnotation]; exists {
		if val == common.AutoRollbackDisableValue {
			postRebootConfigEnabled = false
		}
	}
	log.Info("Auto-rollback post reboot config", "postRebootConfigEnabled", postRebootConfigEnabled)

	rollbackCfg.EnabledComponents[InstallationConfigurationComponent] = postRebootConfigEnabled
	rollbackCfg.EnabledComponents[PostPivotComponent] = postRebootConfigEnabled

	if err := lcautils.MarshalToFile(rollbackCfg, cfgfile); err != nil {
		return fmt.Errorf("failed to write rollback config file in %s: %w", cfgfile, err)
	}

	log.Info(fmt.Sprintf("Config saved to %s", cfgdir))
	return nil
}

func (c *IBURebootClient) ReadAutoRollbackConfigFile() (*AutoRollbackConfig, error) {
	rollbackCfg := &AutoRollbackConfig{
		InitMonitorEnabled: false,
		InitMonitorTimeout: common.IBUAutoRollbackInitMonitorTimeoutDefaultSeconds,
		EnabledComponents:  make(map[string]bool),
	}

	filename := common.PathOutsideChroot(common.IBUAutoRollbackConfigFile)
	if _, err := os.Stat(filename); err != nil {
		return rollbackCfg, fmt.Errorf("unable to find auto-rollback config file (%s): %w", filename, err)
	}

	if err := lcautils.ReadYamlOrJSONFile(filename, rollbackCfg); err != nil {
		return rollbackCfg, fmt.Errorf("failed to read and decode auto-rollback config file: %w", err)
	}

	return rollbackCfg, nil
}

func (c *IBURebootClient) DisableInitMonitor() error {
	// Check whether service-unit is active before stopping. The "stop" command will exit with 0 if already stopped,
	// but would return a failure if the service-unit doesn't exist (for whatever reason).
	if _, err := c.hostCommandsExecutor.Execute("systemctl", "is-active", common.IBUInitMonitorService); err == nil {
		if _, err := c.hostCommandsExecutor.Execute("systemctl", "stop", common.IBUInitMonitorService); err != nil {
			return fmt.Errorf("failed to stop %s: %w", common.IBUInitMonitorService, err)
		}
	}

	// Check whether service-unit is enabled before dsiabling. The "disable" command will exit with 0 if already disabled,
	// but would return a failure if the service-unit doesn't exist (for whatever reason).
	if _, err := c.hostCommandsExecutor.Execute("systemctl", "is-enabled", common.IBUInitMonitorService); err == nil {
		if _, err := c.hostCommandsExecutor.Execute("systemctl", "disable", common.IBUInitMonitorService); err != nil {
			return fmt.Errorf("failed to disable %s: %w", common.IBUInitMonitorService, err)
		}
	}

	if err := os.Remove(common.PathOutsideChroot(common.IBUInitMonitorServiceFile)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete %s: %w", common.IBUInitMonitorServiceFile, err)
	}

	if _, err := c.hostCommandsExecutor.Execute("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload failed after deleting %s: %w", common.IBUInitMonitorServiceFile, err)
	}

	return nil
}

func (c *IBURebootClient) RebootToNewStateRoot(rationale string) error {
	c.log.Info(fmt.Sprintf("rebooting to a new stateroot: %s", rationale))
	return c.Reboot(rationale)
}

func (c *IBURebootClient) Reboot(rationale string) error {
	_, err := c.hostCommandsExecutor.Execute("systemd-run", "--unit", "lifecycle-agent-reboot",
		"--description", fmt.Sprintf("\"lifecycle-agent: %s\"", rationale),
		"systemctl", "--message=\"Image Based Upgrade\"", "reboot")
	if err != nil {
		return fmt.Errorf("failed to reboot with systemd :%w", err)
	}

	c.log.Info(fmt.Sprintf("Wait for %s to be killed via SIGTERM", defaultRebootTimeout.String()))
	time.Sleep(defaultRebootTimeout)

	return fmt.Errorf("failed to reboot. This should never happen! Please check the system")
}

func (c *IBURebootClient) IsOrigStaterootBooted(identifier string) (bool, error) {
	currentStaterootName, err := c.rpmOstreeClient.GetCurrentStaterootName()
	if err != nil {
		return false, fmt.Errorf("failed to get current stateroot name: %w", err)
	}
	c.log.Info(
		"stateroots",
		"current stateroot:", currentStaterootName,
		"desired stateroot",
		common.GetStaterootName(identifier),
	)
	return currentStaterootName != common.GetStaterootName(identifier), nil
}

func (c *IBURebootClient) InitiateRollback(msg string) error {
	if !c.ostreeClient.IsOstreeAdminSetDefaultFeatureEnabled() {
		return fmt.Errorf("automatic rollback not supported in this release")
	}

	c.log.Info("Updating saved IBU CR with status msg for rollback")
	stateroot, err := c.rpmOstreeClient.GetUnbootedStaterootName()
	if err != nil {
		return fmt.Errorf("unable to determine stateroot path for rollback: %w", err)
	}

	if err := c.ops.RemountSysroot(); err != nil {
		return fmt.Errorf("unable to remount sysroot: %w", err)
	}

	filePath := common.PathOutsideChroot(filepath.Join(common.GetStaterootPath(stateroot), utils.IBUFilePath))

	savedIbu := &ibuv1.ImageBasedUpgrade{}
	if err := lcautils.ReadYamlOrJSONFile(filePath, savedIbu); err != nil {
		return fmt.Errorf("unable to read saved IBU CR from %s: %w", filePath, err)
	}

	utils.SetUpgradeStatusFailed(savedIbu, msg)

	if err := lcautils.MarshalToFile(savedIbu, filePath); err != nil {
		return fmt.Errorf("unable to save updated ibu CR to %s: %w", filePath, err)
	}

	c.log.Info("Iniating rollback")

	deploymentIndex, err := c.rpmOstreeClient.GetUnbootedDeploymentIndex()
	if err != nil {
		return fmt.Errorf("unable to get unbooted deployment for automatic rollback: %w", err)
	}

	if err = c.ostreeClient.SetDefaultDeployment(deploymentIndex); err != nil {
		return fmt.Errorf("unable to get set deployment for automatic rollback: %w", err)
	}

	err = c.RebootToNewStateRoot("rollback")
	return fmt.Errorf("unable to get set deployment for automatic rollback: %w", err)
}

func (c *IBURebootClient) AutoRollbackIfEnabled(component, msg string) {
	rollbackCfg, err := c.ReadAutoRollbackConfigFile()
	if err != nil {
		if os.IsNotExist(err) {
			// Config file doesn't exist, so do nothing
			return
		}

		c.log.Info(fmt.Sprintf("Unable to read auto-rollback config: %s", err))
		return
	}

	if !rollbackCfg.EnabledComponents[component] {
		c.log.Info(fmt.Sprintf("Auto-rollback is disabled for component: %s", component))
		return
	}

	c.log.Info(fmt.Sprintf("Auto-rollback is enabled for component: %s", component))
	if err = c.InitiateRollback(msg); err != nil {
		c.log.Info(fmt.Sprintf("Unable to initiate rollback: %s", err))
	}

}

type IPCRebootClient struct {
	log                  *logr.Logger
	hostCommandsExecutor ops.Execute
	rpmOstreeClient      rpmostreeclient.IClient
	ostreeClient         ostreeclient.IClient
	ops                  ops.Ops
}

// NewIPCRebootClient creates a reboot client for IP config auto-rollback handling.
func NewIPCRebootClient(log *logr.Logger,
	hostCommandsExecutor ops.Execute,
	rpmOstreeClient rpmostreeclient.IClient,
	ostreeClient ostreeclient.IClient,
	ops ops.Ops) RebootIntf {
	return &IPCRebootClient{
		log:                  log,
		hostCommandsExecutor: hostCommandsExecutor,
		rpmOstreeClient:      rpmOstreeClient,
		ostreeClient:         ostreeClient,
		ops:                  ops,
	}
}

// WriteIPCAutoRollbackConfigFile writes the IP-config auto-rollback configuration
func WriteIPCAutoRollbackConfigFile(log logr.Logger, ipc *ipcv1.IPConfig, ops ops.Ops) error {
	cfgfile := common.PathOutsideChroot(common.IPCAutoRollbackConfigFile)

	monitorTimeout := common.IPCAutoRollbackInitMonitorTimeoutDefaultSeconds
	if ipc != nil && ipc.Spec.AutoRollbackOnFailure != nil && ipc.Spec.AutoRollbackOnFailure.InitMonitorTimeoutSeconds > 0 {
		monitorTimeout = ipc.Spec.AutoRollbackOnFailure.InitMonitorTimeoutSeconds
	}

	initMonitorEnabled := true
	if ipc != nil {
		if val, exists := ipc.GetAnnotations()[common.AutoRollbackOnFailureInitMonitorAnnotation]; exists {
			if val == common.AutoRollbackDisableValue {
				initMonitorEnabled = false
			}
		}
	}

	enabledComponents := map[string]bool{
		IPConfigRunComponent: true,
	}
	if ipc != nil {
		if val, exists := ipc.GetAnnotations()[common.AutoRollbackOnFailureIPConfigRunAnnotation]; exists {
			if val == common.AutoRollbackDisableValue {
				enabledComponents[IPConfigRunComponent] = false
			}
		}
	}

	log.Info("IPConfig Auto-rollback init monitor timeout", "monitorTimeout", monitorTimeout)
	log.Info("IPConfig Auto-rollback init monitor enabled", "initMonitorEnabled", initMonitorEnabled)
	log.Info("IPConfig Auto-rollback ipconfig run enabled", "enabledComponents", enabledComponents)

	rollbackCfg := AutoRollbackConfig{
		InitMonitorEnabled: initMonitorEnabled,
		InitMonitorTimeout: monitorTimeout,
		EnabledComponents:  enabledComponents,
	}

	data, err := json.Marshal(rollbackCfg)
	if err != nil {
		return fmt.Errorf("failed to marshal rollback config: %w", err)
	}

	if err := ops.WriteFile(cfgfile, data, 0o600); err != nil {
		return fmt.Errorf("failed to write ip-config auto-rollback config file in %s: %w", cfgfile, err)
	}

	log.Info(fmt.Sprintf("IPConfig auto-rollback config saved to %s", cfgfile))

	return nil
}

// ReadAutoRollbackConfigFile for IPC client reads from the IPConfig-specific config path but into the common struct.
func (c *IPCRebootClient) ReadAutoRollbackConfigFile() (*AutoRollbackConfig, error) { //nolint:ireturn
	rollbackCfg := &AutoRollbackConfig{
		InitMonitorEnabled: false,
		InitMonitorTimeout: common.IPCAutoRollbackInitMonitorTimeoutDefaultSeconds,
		EnabledComponents:  make(map[string]bool),
	}

	filename := common.PathOutsideChroot(common.IPCAutoRollbackConfigFile)
	if _, err := os.Stat(filename); err != nil {
		return rollbackCfg, fmt.Errorf("unable to find ip-config auto-rollback config file (%s): %w", filename, err)
	}

	if err := lcautils.ReadYamlOrJSONFile(filename, rollbackCfg); err != nil {
		return rollbackCfg, fmt.Errorf("failed to read and decode ip-config auto-rollback config file: %w", err)
	}

	return rollbackCfg, nil
}

// DisableInitMonitor stops the transient init-monitor unit for IP config if running.
func (c *IPCRebootClient) DisableInitMonitor() error {
	if _, err := c.hostCommandsExecutor.Execute("systemctl", "is-active", common.IPCInitMonitorService); err == nil {
		if _, err := c.hostCommandsExecutor.Execute("systemctl", "stop", common.IPCInitMonitorService); err != nil {
			return fmt.Errorf("failed to stop %s: %w", common.IPCInitMonitorService, err)
		}
	}
	return nil
}

// InitiateRollback for IP config simply sets default to the unbooted deployment and reboots.
func (c *IPCRebootClient) InitiateRollback(msg string) error {
	if !c.ostreeClient.IsOstreeAdminSetDefaultFeatureEnabled() {
		return fmt.Errorf("automatic rollback not supported in this release")
	}

	c.log.Info("Initiating IPConfig rollback", "reason", msg)

	deploymentIndex, err := c.rpmOstreeClient.GetUnbootedDeploymentIndex()
	if err != nil {
		return fmt.Errorf("unable to get unbooted deployment for automatic IPConfig rollback: %w", err)
	}

	if err = c.ostreeClient.SetDefaultDeployment(deploymentIndex); err != nil {
		return fmt.Errorf("unable to set default deployment for automatic IPConfig rollback: %w", err)
	}

	if err := c.ops.RemountSysroot(); err != nil {
		return fmt.Errorf("failed to remount sysroot: %w", err)
	}

	oldStateroot, err := c.rpmOstreeClient.GetUnbootedStaterootName()
	if err != nil {
		return fmt.Errorf("failed to get unbooted stateroot name: %w", err)
	}
	oldStaterootPath := common.GetStaterootPath(oldStateroot)
	savedIPCPath := common.PathOutsideChroot(filepath.Join(oldStaterootPath, utils.IPCFilePath))
	savedIPC := &ipcv1.IPConfig{}
	if err := lcautils.ReadYamlOrJSONFile(savedIPCPath, savedIPC); err != nil {
		return fmt.Errorf("unable to read saved IPC CR from %s: %w", savedIPCPath, err)
	}

	utils.SetIPConfigStatusFailed(savedIPC, msg)

	if err := lcautils.MarshalToFile(savedIPC, savedIPCPath); err != nil {
		return fmt.Errorf("unable to save updated IPC CR to %s: %w", savedIPCPath, err)
	}

	return c.RebootToNewStateRoot("ip-config rollback")
}

// AutoRollbackIfEnabled reads the IPC config and triggers rollback if enabled for the component.
func (c *IPCRebootClient) AutoRollbackIfEnabled(component, msg string) {
	rollbackCfg, err := c.ReadAutoRollbackConfigFile() // reads IPC path
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		c.log.Info(fmt.Sprintf("Unable to read IPConfig auto-rollback config: %s", err))
		return
	}

	if !rollbackCfg.EnabledComponents[component] {
		c.log.Info(fmt.Sprintf("IPConfig auto-rollback is disabled for component: %s", component))
		return
	}

	c.log.Info(fmt.Sprintf("IPConfig auto-rollback is enabled for component: %s", component))
	if err = c.InitiateRollback(msg); err != nil {
		c.log.Info(fmt.Sprintf("Unable to initiate IPConfig rollback: %s", err))
	}
}

func (c *IPCRebootClient) IsOrigStaterootBooted(identifier string) (bool, error) {
	currentStaterootName, err := c.rpmOstreeClient.GetCurrentStaterootName()
	if err != nil {
		return false, fmt.Errorf("failed to get current stateroot name: %w", err)
	}
	c.log.Info(
		"stateroots",
		"current stateroot:", currentStaterootName,
		"desired stateroot:", common.GetStaterootName(identifier),
	)

	return currentStaterootName != common.GetStaterootName(identifier), nil
}

func (c *IPCRebootClient) RebootToNewStateRoot(rationale string) error {
	c.log.Info(fmt.Sprintf("rebooting to a new stateroot: %s", rationale))
	return c.Reboot(rationale)
}

func (c *IPCRebootClient) Reboot(rationale string) error {
	_, err := c.hostCommandsExecutor.Execute("systemd-run", "--unit", "lifecycle-agent-reboot",
		"--description", fmt.Sprintf("\"lifecycle-agent: %s\"", rationale),
		"systemctl", "--message=\"IP Config\"", "reboot")
	if err != nil {
		return fmt.Errorf("failed to reboot with systemd :%w", err)
	}

	c.log.Info(fmt.Sprintf("Wait for %s to be killed via SIGTERM", defaultRebootTimeout.String()))
	time.Sleep(defaultRebootTimeout)

	return fmt.Errorf("failed to reboot. This should never happen! Please check the system")
}
