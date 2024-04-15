package reboot

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	lcautils "github.com/openshift-kni/lifecycle-agent/utils"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
)

var (
	defaultRebootTimeout = 60 * time.Minute
)

const (
	PostPivotComponent                 = "postpivot"
	InstallationConfigurationComponent = "config"
)

type IBUAutoRollbackConfig struct {
	InitMonitorEnabled bool            `json:"monitor_enabled,omitempty"`
	InitMonitorTimeout int             `json:"monitor_timeout,omitempty"`
	EnabledComponents  map[string]bool `json:"enabled_components,omitempty"`
}

// RebootIntf is an interface for LCA reboot and rollback commands.
//
//go:generate mockgen -source=reboot.go -package=reboot -destination=mock_reboot.go
type RebootIntf interface {
	WriteIBUAutoRollbackConfigFile(ibu *lcav1alpha1.ImageBasedUpgrade) error
	ReadIBUAutoRollbackConfigFile() (*IBUAutoRollbackConfig, error)
	DisableInitMonitor() error
	RebootToNewStateRoot(rationale string) error
	IsOrigStaterootBooted(ibu *v1alpha1.ImageBasedUpgrade) (bool, error)
	InitiateRollback(msg string) error
	AutoRollbackIfEnabled(component, msg string)
}

type RebootClient struct {
	log                  *logr.Logger
	hostCommandsExecutor ops.Execute
	rpmOstreeClient      rpmostreeclient.IClient
	ostreeClient         ostreeclient.IClient
	ops                  ops.Ops
}

// NewRebootClient creates and returns a RebootIntf interface for reboot and rollback commands
func NewRebootClient(log *logr.Logger,
	hostCommandsExecutor ops.Execute,
	rpmOstreeClient rpmostreeclient.IClient,
	ostreeClient ostreeclient.IClient,
	ops ops.Ops) RebootIntf {
	return &RebootClient{
		log:                  log,
		hostCommandsExecutor: hostCommandsExecutor,
		rpmOstreeClient:      rpmOstreeClient,
		ostreeClient:         ostreeClient,
		ops:                  ops,
	}
}

func (c *RebootClient) WriteIBUAutoRollbackConfigFile(ibu *lcav1alpha1.ImageBasedUpgrade) error {
	stateroot := common.GetStaterootName(ibu.Spec.SeedImageRef.Version)
	staterootPath := common.GetStaterootPath(stateroot)
	cfgfile := common.PathOutsideChroot(filepath.Join(staterootPath, common.IBUAutoRollbackConfigFile))

	cfgdir := filepath.Dir(cfgfile)
	if err := os.MkdirAll(cfgdir, 0o700); err != nil {
		return fmt.Errorf("unable to create config dir: %s: %w", cfgdir, err)
	}

	monitorTimeout := ibu.Spec.AutoRollbackOnFailure.InitMonitorTimeoutSeconds
	if monitorTimeout <= 0 {
		monitorTimeout = common.IBUAutoRollbackInitMonitorTimeoutDefaultSeconds
	}
	c.log.Info("Auto-rollback init monitor timeout", "monitorTimeout", monitorTimeout)

	// check autoRollback's InitMonitor config from annotation
	initMonitorEnabled := true
	if val, exists := ibu.GetAnnotations()[common.AutoRollbackOnFailureInitMonitorAnnotation]; exists {
		if val == common.AutoRollbackDisableValue {
			initMonitorEnabled = false
		}
	}
	c.log.Info("Auto-rollback init monitor config", "initMonitorEnabled", initMonitorEnabled)

	rollbackCfg := IBUAutoRollbackConfig{
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
	c.log.Info("Auto-rollback post reboot config", "postRebootConfigEnabled", postRebootConfigEnabled)

	rollbackCfg.EnabledComponents[InstallationConfigurationComponent] = postRebootConfigEnabled
	rollbackCfg.EnabledComponents[PostPivotComponent] = postRebootConfigEnabled

	if err := lcautils.MarshalToFile(rollbackCfg, cfgfile); err != nil {
		return fmt.Errorf("failed to write rollback config file in %s: %w", cfgfile, err)
	}

	return nil
}

func (c *RebootClient) ReadIBUAutoRollbackConfigFile() (*IBUAutoRollbackConfig, error) {
	rollbackCfg := &IBUAutoRollbackConfig{
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

func (c *RebootClient) DisableInitMonitor() error {
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

func (c *RebootClient) RebootToNewStateRoot(rationale string) error {
	c.log.Info(fmt.Sprintf("rebooting to a new stateroot: %s", rationale))

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

func (c *RebootClient) IsOrigStaterootBooted(ibu *v1alpha1.ImageBasedUpgrade) (bool, error) {
	currentStaterootName, err := c.rpmOstreeClient.GetCurrentStaterootName()
	if err != nil {
		return false, fmt.Errorf("failed to get current stateroot name: %w", err)
	}
	c.log.Info("stateroots", "current stateroot:", currentStaterootName, "desired stateroot", common.GetDesiredStaterootName(ibu))
	return currentStaterootName != common.GetDesiredStaterootName(ibu), nil
}

func (c *RebootClient) InitiateRollback(msg string) error {
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

	savedIbu := &lcav1alpha1.ImageBasedUpgrade{}
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

	if err = c.RebootToNewStateRoot("rollback"); err != nil {
		return fmt.Errorf("unable to get set deployment for automatic rollback: %w", err)
	}

	// Should never get here
	return nil
}

func (c *RebootClient) AutoRollbackIfEnabled(component, msg string) {
	rollbackCfg, err := c.ReadIBUAutoRollbackConfigFile()
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

	return
}
