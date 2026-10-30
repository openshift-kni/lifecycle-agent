/*
Copyright 2026.

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
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/postpivot"

	"github.com/spf13/cobra"
)

// restoreNMConnectionsCmd copies the provided nmconnection files before NetworkManager starts
var restoreNMConnectionsCmd = &cobra.Command{
	Use:   "restore-nmconnections",
	Short: "Restore provided NetworkManager connection profiles before NetworkManager starts",
	Run: func(cmd *cobra.Command, args []string) {
		restoreNMConnections()
	},
}

func init() {
	rootCmd.AddCommand(restoreNMConnectionsCmd)
}

func restoreNMConnections() {
	opsClient := ops.NewOps(log, ops.NewRegularExecutor(log, true))
	postPivotRunner := postpivot.NewPostPivot(scheme, log, opsClient,
		common.ImageRegistryAuthFile, common.OptOpenshift, common.KubeconfigFile)
	// No rollback here: post-pivot copies the files again and handles failures
	if err := postPivotRunner.RestoreNMConnectionFiles(); err != nil {
		log.Fatal(err)
	}
	log.Info("NetworkManager connection profiles restored")
}
