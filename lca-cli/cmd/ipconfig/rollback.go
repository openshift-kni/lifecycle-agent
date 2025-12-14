/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ipconfigcmd

import (
	"fmt"
	"os"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	intOstree "github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ipconfig"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmOstree "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/spf13/cobra"
)

var (
	rollbackStateroot string
)

const (
	rollbackCmd   = "rollback"
	staterootFlag = "stateroot"
)

func init() {
	// subcommand is added by NewIPConfigCmd after globals are initialized
	ipConfigRollbackCmd.Flags().StringVar(&rollbackStateroot, staterootFlag, "", "Target stateroot to roll back to")
	ipConfigRollbackCmd.MarkFlagRequired(staterootFlag)
}

var ipConfigRollbackCmd = &cobra.Command{
	Use:   rollbackCmd,
	Short: "Execute IP configuration rollback",
	Run: func(cmd *cobra.Command, args []string) {
		if err := runIPConfigRollback(); err != nil {
			pkgLog.Fatalf("Error executing ip-config rollback: %v", err)
		}
	},
}

func runIPConfigRollback() error {
	var hostCommandsExecutor ops.Execute
	if _, err := os.Stat(common.Host); err == nil {
		hostCommandsExecutor = ops.NewChrootExecutor(pkgLog, true, common.Host)
	} else {
		hostCommandsExecutor = ops.NewRegularExecutor(pkgLog, true)
	}
	opsInterface := ops.NewOps(pkgLog, hostCommandsExecutor)
	rpmClient := rpmOstree.NewClient("lca-cli-ip-config-rollback", hostCommandsExecutor)
	ostreeClient := intOstree.NewClient(hostCommandsExecutor, false)

	rb := reboot.NewIPCRebootClient(&logr.Logger{}, hostCommandsExecutor, rpmClient, ostreeClient, opsInterface)

	exec := ipconfig.NewRollbackHandler(pkgLog, opsInterface, ostreeClient, rpmClient)
	if err := exec.Run(rollbackStateroot); err != nil {
		return fmt.Errorf("ip config rollback handler failed: %w", err)
	}

	if err := rb.RebootToNewStateRoot("ip-config rollback"); err != nil {
		return fmt.Errorf("failed to reboot: %w", err)
	}

	return nil
}
