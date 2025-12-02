package ipconfig

import (
	"fmt"

	"github.com/sirupsen/logrus"

	intOstree "github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmOstree "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
)

type RollbackHandler struct {
	log    *logrus.Logger
	ops    ops.Ops
	ostree intOstree.IClient
	rpm    rpmOstree.IClient
}

// NewRollbackHandler constructs a RollbackHandler to revert an IP configuration
// by switching the default OSTree deployment back to a specified stateroot.
func NewRollbackHandler(log *logrus.Logger, ops ops.Ops, ostree intOstree.IClient, rpm rpmOstree.IClient) *RollbackHandler {
	return &RollbackHandler{log: log, ops: ops, ostree: ostree, rpm: rpm}
}

// Run performs the rollback by setting the default deployment to the one
// associated with the provided stateroot, if the OSTree feature is available.
func (h *RollbackHandler) Run(stateroot string) error {
	h.log.Infof("IP config rollback started with stateroot: %s", stateroot)

	if h.ostree.IsOstreeAdminSetDefaultFeatureEnabled() {
		idx, err := h.rpm.GetDeploymentIndex(stateroot)
		if err != nil {
			return fmt.Errorf("failed to get deployment index for %s: %w", stateroot, err)
		}
		if err := h.ostree.SetDefaultDeployment(idx); err != nil {
			return fmt.Errorf("failed to set default deployment: %w", err)
		}
	}

	h.log.Info("IP config rollback done successfully")

	return nil
}
