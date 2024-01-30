package initmonitor

import (
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
}

func NewInitMonitor(scheme *runtime.Scheme, log *logrus.Logger, hostCommandsExecutor ops.Execute, ops ops.Ops, component string) *InitMonitor {
	rpmOstreeClient := rpmostreeclient.NewClient("initmonitor", hostCommandsExecutor)
	ostreeClient := ostreeclient.NewClient(hostCommandsExecutor, false)
	rebootClient := reboot.NewRebootClient(&logr.Logger{}, hostCommandsExecutor, rpmOstreeClient, ostreeClient)
	return &InitMonitor{
		scheme:               scheme,
		log:                  log,
		hostCommandsExecutor: hostCommandsExecutor,
		ops:                  ops,
		component:            component,
		rpmOstreeClient:      rpmOstreeClient,
		ostreeClient:         ostreeClient,
		rebootClient:         rebootClient,
	}
}

func (m *InitMonitor) RunInitMonitor() error {
	rollbackCfg, err := m.rebootClient.ReadIBUAutoRollbackConfigFile()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		m.log.Error(err, "failed to read rollback config file")
		return err
	}

	if !rollbackCfg.InitMonitorEnabled || rollbackCfg.InitMonitorTimeout <= 0 {
		return nil
	}

	time.Sleep(time.Duration(rollbackCfg.InitMonitorTimeout) * time.Second)

	// If we reach this point, the init monitor was not shut down by the Upgrade handler, so trigger rollback

	m.log.Infof("Automatically rolling back due to LCA Init Monitor timeout, after %d seconds", rollbackCfg.InitMonitorTimeout)

	if err := m.rebootClient.InitiateRollback(true); err != nil {
		m.log.Infof("Unable to auto rollback: %s", err)
		return err
	}

	return nil
}

func (m *InitMonitor) checkSvcUnitRollbackNeeded() bool {
	rollbackCfg, err := m.rebootClient.ReadIBUAutoRollbackConfigFile()
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}

		m.log.Error(err, "failed to read rollback config file")
		return false
	}

	// Get the systemd service-unit rc
	rc, ok := os.LookupEnv("SERVICE_RESULT")
	if !ok {
		m.log.Info("SERVICE_RESULT variable is not set")
		return false
	}

	if rc == "0" || !rollbackCfg.EnabledComponents[m.component] {
		// The service-unit succeeded, or auto-rollback is disabled, so do nothing
		return false
	}

	return true
}

func (m *InitMonitor) RunExitStopPostCheck() error {
	if m.checkSvcUnitRollbackNeeded() {
		m.log.Info(fmt.Sprintf("Automatically rolling back due to service-unit failure: component %s", m.component))

		if err := m.rebootClient.InitiateRollback(true); err != nil {
			m.log.Infof("Unable to auto rollback: %s", err)
			return err
		}
	}

	return nil
}
