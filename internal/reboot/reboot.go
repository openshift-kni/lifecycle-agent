package reboot

import (
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/ibu-imager/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
)

var (
	defaultRebootTimeout = 60 * time.Minute
)

func RebootToNewStateRoot(rationale string, log logr.Logger, e ops.Execute) error {
	log.Info(fmt.Sprintf("rebooting to a new stateroot: %s", rationale))

	_, err := e.Execute("systemd-run", "--unit", "lifecycle-agent-reboot",
		"--description", fmt.Sprintf("\"lifecycle-agent: %s\"", rationale),
		"systemctl --message=\"Image Based Upgrade\" reboot")
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
