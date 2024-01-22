package reboot

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
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

type IBUAutoRollbackConfig struct {
	InitMonitorEnabled bool              `json:"monitor_enabled,omitempty"`
	InitMonitorTimeout int               `json:"monitor_timeout,omitempty"`
	EnabledComponents  map[string]bool   `json:"enabled_components,omitempty"`
	LcaTestVars        map[string]string `json:"lca_test_vars,omitempty"`
}

func writeInstallationConfigurationEnvFile(ostreeClient ostreeclient.IClient, stateroot, content string) error {
	deploymentDir, err := ostreeClient.GetDeploymentDir(stateroot)
	if err != nil {
		return fmt.Errorf("unable to determine deployment dir: %w", err)
	}

	envFile := common.PathOutsideChroot(filepath.Join(deploymentDir, common.InstallationConfigurationEnvFile))
	f, err := os.OpenFile(envFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("unable to open env file %s: %w", envFile, err)
	}
	defer f.Close()

	if _, err := f.WriteString(content); err != nil {
		return fmt.Errorf("unable to write to env file %s: %w", envFile, err)
	}

	return nil
}

func WriteIBUAutoRollbackConfigFile(ibu *lcav1alpha1.ImageBasedUpgrade, ostreeClient ostreeclient.IClient) error {
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

	rollbackCfg := IBUAutoRollbackConfig{
		InitMonitorEnabled: !ibu.Spec.AutoRollbackOnFailure.DisabledInitMonitor,
		InitMonitorTimeout: monitorTimeout,
		EnabledComponents:  make(map[string]bool),
		LcaTestVars:        make(map[string]string),
	}

	rollbackCfg.EnabledComponents["config"] = !ibu.Spec.AutoRollbackOnFailure.DisabledForPostRebootConfig
	rollbackCfg.EnabledComponents["postpivot"] = !ibu.Spec.AutoRollbackOnFailure.DisabledForPostRebootConfig

	// Check environ for LCA_TEST_* vars and add them
	envFileContent := ""
	re := regexp.MustCompile(`^LCA_TEST_`)
	for _, envVar := range os.Environ() {
		if re.MatchString(envVar) {
			pair := strings.SplitN(envVar, "=", 2)
			rollbackCfg.LcaTestVars[pair[0]] = pair[1]
			envFileContent += envVar + "\n"
		}
	}

	if envFileContent != "" {
		if err := writeInstallationConfigurationEnvFile(ostreeClient, stateroot, envFileContent); err != nil {
			return err
		}
	}

	return lcautils.MarshalToFile(rollbackCfg, cfgfile)
}

func ReadIBUAutoRollbackConfigFile() (*IBUAutoRollbackConfig, error) {
	rollbackCfg := &IBUAutoRollbackConfig{
		EnabledComponents: make(map[string]bool),
		LcaTestVars:       make(map[string]string),
	}

	filename := common.IBUAutoRollbackConfigFile
	if _, err := os.Stat(filename); err != nil {
		filename = common.PathOutsideChroot(common.IBUAutoRollbackConfigFile)
		if _, err := os.Stat(filename); err != nil {
			return nil, err
		}
	}

	if err := lcautils.ReadYamlOrJSONFile(filename, rollbackCfg); err != nil {
		return nil, fmt.Errorf("failed to read and decode auto-rollback config file: %w", err)
	}

	return rollbackCfg, nil
}

func CheckIBUAutoRollbackInjectedFailure(component string) bool {
	if rollbackCfg, err := ReadIBUAutoRollbackConfigFile(); err == nil {
		tag := "LCA_TEST_inject_failure_" + component
		return (rollbackCfg.LcaTestVars[tag] == "yes")
	}

	return false
}

func DisableInitMonitor(log logr.Logger, e ops.Execute) error {
	if _, err := e.Execute("systemctl", "is-active", common.IBUInitMonitorService); err == nil {
		if _, err := e.Execute("systemctl", "stop", common.IBUInitMonitorService); err != nil {
			return fmt.Errorf("failed to stop %s: %w", common.IBUInitMonitorService, err)
		}
	}

	if _, err := e.Execute("systemctl", "is-enabled", common.IBUInitMonitorService); err == nil {
		if _, err := e.Execute("systemctl", "disable", common.IBUInitMonitorService); err != nil {
			return fmt.Errorf("failed to disable %s: %w", common.IBUInitMonitorService, err)
		}
	}

	if err := os.Remove(common.PathOutsideChroot(common.IBUInitMonitorServiceFile)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete %s: %w", common.IBUInitMonitorServiceFile, err)
	}

	if _, err := e.Execute("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload failed after deleting %s: %w", common.IBUInitMonitorServiceFile, err)
	}

	return nil
}

func RebootToNewStateRoot(rationale string, log logr.Logger, e ops.Execute) error {
	log.Info(fmt.Sprintf("rebooting to a new stateroot: %s", rationale))

	_, err := e.Execute("systemd-run", "--unit", "lifecycle-agent-reboot",
		"--description", fmt.Sprintf("\"lifecycle-agent: %s\"", rationale),
		"systemctl", "--message=\"Image Based Upgrade\"", "reboot")
	if err != nil {
		return err
	}

	log.Info(fmt.Sprintf("Wait for %s to be killed via SIGTERM", defaultRebootTimeout.String()))
	time.Sleep(defaultRebootTimeout)

	return fmt.Errorf("failed to reboot. This should never happen! Please check the system")
}

func IsOrigStaterootBooted(ibu *v1alpha1.ImageBasedUpgrade, r rpmostreeclient.IClient, log logr.Logger) (bool, error) {
	currentStaterootName, err := r.GetCurrentStaterootName()
	if err != nil {
		return false, err
	}
	log.Info("stateroots", "current stateroot:", currentStaterootName, "desired stateroot", common.GetDesiredStaterootName(ibu))
	return currentStaterootName != common.GetDesiredStaterootName(ibu), nil
}

func InitiateRollback(auto bool, log logr.Logger, e ops.Execute, rpmOstreeClient rpmostreeclient.IClient, ostreeClient ostreeclient.IClient) error {
	if !ostreeClient.IsOstreeAdminSetDefaultFeatureEnabled() {
		return fmt.Errorf("automatic rollback not supported in this release")
	}

	log.Info("Iniating rollback")

	deploymentIndex, err := rpmOstreeClient.GetUnbootedDeploymentIndex()
	if err != nil {
		return fmt.Errorf("unable to get unbooted deployment for automatic rollback: %w", err)
	}

	if err = ostreeClient.SetDefaultDeployment(deploymentIndex); err != nil {
		return fmt.Errorf("unable to get set deployment for automatic rollback: %w", err)
	}

	if err = RebootToNewStateRoot("rollback", log, e); err != nil {
		return fmt.Errorf("unable to get set deployment for automatic rollback: %w", err)
	}

	// Should never get here
	return nil
}
