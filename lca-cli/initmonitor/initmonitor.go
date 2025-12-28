package initmonitor

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"

	"github.com/go-logr/logr"
)

type InitMonitor struct {
	scheme               *runtime.Scheme
	log                  *logrus.Logger
	hostCommandsExecutor ops.Execute
	ops                  ops.Ops
	component            string
	rpmOstreeClient      *rpmostreeclient.Client
	ostreeClient         ostreeclient.IClient
	rebootClient         reboot.RebootIntf
	mode                 string
}

func NewInitMonitor(scheme *runtime.Scheme, log *logrus.Logger, hostCommandsExecutor ops.Execute, ops ops.Ops, component, mode string) *InitMonitor {
	rpmOstreeClient := rpmostreeclient.NewClient("initmonitor", hostCommandsExecutor)
	ostreeClient := ostreeclient.NewClient(hostCommandsExecutor, false)
	var rebootClient reboot.RebootIntf
	if mode == "ipconfig" {
		rebootClient = reboot.NewIPCRebootClient(&logr.Logger{}, hostCommandsExecutor, rpmOstreeClient, ostreeClient, ops)
	} else {
		rebootClient = reboot.NewIBURebootClient(&logr.Logger{}, hostCommandsExecutor, rpmOstreeClient, ostreeClient, ops)
	}
	return &InitMonitor{
		scheme:               scheme,
		log:                  log,
		hostCommandsExecutor: hostCommandsExecutor,
		ops:                  ops,
		component:            component,
		rpmOstreeClient:      rpmOstreeClient,
		ostreeClient:         ostreeClient,
		rebootClient:         rebootClient,
		mode:                 mode,
	}
}

func (m *InitMonitor) RunInitMonitor() error {
	rollbackCfg, err := m.rebootClient.ReadAutoRollbackConfigFile()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("failed to read rollback config file: %w", err)
	}

	if !rollbackCfg.InitMonitorEnabled {
		m.log.Info("LCA Init Monitor automatic rollback is disabled in IBU configuration")
		return nil
	}

	if rollbackCfg.InitMonitorTimeout <= 0 {
		// This shouldn't happen, as value of <= 0 is treated as "use default" when config file is written
		m.log.Infof("LCA Init Monitor timeout value must be greater than 0: current value %d", rollbackCfg.InitMonitorTimeout)
		return nil
	}

	timeout := time.Duration(rollbackCfg.InitMonitorTimeout) * time.Second

	// Adjust log message based on mode
	contextText := "upgrade"
	if m.mode == "ipconfig" {
		contextText = "ip-config"
	}
	m.log.Infof("Launching LCA Init Monitor timeout. Automatic rollback will occur in %s if %s is not completed successfully within that time", timeout, contextText)

	time.Sleep(timeout)

	// If we reach this point, the init monitor was not shut down by the handler, so trigger rollback

	msg := fmt.Sprintf("Rollback due to LCA Init Monitor timeout, after %s", timeout)
	m.log.Info(msg)

	if err := m.rebootClient.InitiateRollback(msg); err != nil {
		return fmt.Errorf("unable to auto rollback: %w", err)
	}

	return nil
}

func (m *InitMonitor) checkSvcUnitRollbackNeeded() bool {
	rollbackCfg, err := m.rebootClient.ReadAutoRollbackConfigFile()
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}

		m.log.Error(err, "failed to read rollback config file")
		return false
	}

	if !rollbackCfg.EnabledComponents[m.component] {
		// Auto-rollback is disabled, so do nothing
		m.log.Info(fmt.Sprintf("LCA Init Monitor Auto-rollback is disabled for component %s", m.component))
		return false
	}

	// Get the systemd service-unit serviceResult
	serviceResult, ok := os.LookupEnv("SERVICE_RESULT")
	if !ok {
		m.log.Info("SERVICE_RESULT variable is not set")
		return false
	}

	m.log.Info(fmt.Sprintf("LCA Init Monitor: component %s exited with SERVICE_RESULT=%s", m.component, serviceResult))

	if serviceResult == "success" {
		// The service-unit succeeded, or auto-rollback is disabled, so do nothing
		return false
	}

	return true
}

func (m *InitMonitor) RunExitStopPostCheck() error {
	if m.checkSvcUnitRollbackNeeded() {
		msg := fmt.Sprintf("Rollback due to service-unit failure: component %s", m.component)
		m.log.Info(msg)

		if err := m.rebootClient.InitiateRollback(msg); err != nil {
			return fmt.Errorf("unable to auto rollback: %w", err)
		}
	}

	return nil
}
