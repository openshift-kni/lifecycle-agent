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

package cmd

import (
	"context"
	"fmt"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/postpivot"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
)

var postPivotCmd = &cobra.Command{
	Use:   "post-pivot",
	Short: "Apply post-pivot configuration after an image-based upgrade",
	Long: `Apply post-pivot configuration after the node has rebooted into the new
OSTree stateroot during an Image Based Upgrade. Reconfigures cluster
certificates, network settings, and operator state to match the target
cluster identity.

Triggers an automatic rollback if post-pivot configuration fails and
auto-rollback is enabled.`,
	Run: func(cmd *cobra.Command, args []string) {
		postPivot()
	},
}

var inContainer bool

func init() {

	// Add create command
	rootCmd.AddCommand(postPivotCmd)

	postPivotCmd.Flags().BoolVarP(&inContainer, "in-container", "", false, "Use this flag if this command is being ran inside a container")
}

func postPivot() {
	log.Info("Post pivot operation has started")
	var hostCommandsExecutor ops.Execute
	if inContainer {
		hostCommandsExecutor = ops.NewNsenterExecutor(log, true)
	} else {
		hostCommandsExecutor = ops.NewRegularExecutor(log, true)
	}
	opsClient := ops.NewOps(log, hostCommandsExecutor)
	rpmOstreeClient := rpmostreeclient.NewClient("initmonitor", hostCommandsExecutor)
	ostreeClient := ostreeclient.NewClient(hostCommandsExecutor, false)
	rebootClient := reboot.NewIBURebootClient(&logr.Logger{}, hostCommandsExecutor, rpmOstreeClient, ostreeClient, opsClient)

	postPivotRunner := postpivot.NewPostPivot(scheme, log, opsClient,
		common.ImageRegistryAuthFile, common.OptOpenshift, common.KubeconfigFile)
	if err := postPivotRunner.PostPivotConfiguration(context.TODO()); err != nil {
		log.Error(err)
		rebootClient.AutoRollbackIfEnabled(reboot.PostPivotComponent, fmt.Sprintf("Rollback due to postpivot failure: %s", err))
		log.Fatal("Post pivot operation failed")
	}

	log.Info("Post pivot operation finished successfully!")
}
