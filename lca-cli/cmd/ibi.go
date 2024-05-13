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
	"os"

	preinstallUtils "github.com/rh-ecosystem-edge/preinstall-utils/pkg"
	"github.com/spf13/cobra"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	ibipreparation "github.com/openshift-kni/lifecycle-agent/lca-cli/ibi-preparation"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	ostree "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

// ibi represents the ibi preparation command
var ibi = &cobra.Command{
	Use:   "ibi",
	Short: "prepare ibi",
	Run: func(cmd *cobra.Command, args []string) {
		runIBI()
	},
}

var (
	configurationFile string
)

func init() {

	// Add create command
	rootCmd.AddCommand(ibi)
	ibi.Flags().StringVarP(&configurationFile, "configuration-file", "f", "", "The path to the configuration file.")

	rootCmd.MarkFlagRequired("configuration-file")
}

func runIBI() {

	config, err := utils.ReadIBIConfigFile(configurationFile)
	if err != nil {
		log.Fatalf("Error reading configuration file: %v", err)
	}

	log.Info("IBI preparation process has started")
	var hostCommandsExecutor ops.Execute
	// if we run in container we will get /host as a host path and should use chroot executor
	if _, err := os.Stat(common.Host); err == nil {
		hostCommandsExecutor = ops.NewChrootExecutor(log, true, common.Host)
	} else {
		hostCommandsExecutor = ops.NewRegularExecutor(log, true)
	}

	cleanupDevice := preinstallUtils.NewCleanupDevice(log, preinstallUtils.NewDiskOps(log, hostCommandsExecutor))
	rpmOstreeClient := ostree.NewClient("lca-cli", hostCommandsExecutor)
	ostreeClient := ostreeclient.NewClient(hostCommandsExecutor, true)

	ibiRunner := ibipreparation.NewIBIPrepare(log, ops.NewOps(log, hostCommandsExecutor),
		rpmOstreeClient, ostreeClient, cleanupDevice, config)
	if err := ibiRunner.Run(); err != nil {
		log.Fatal(err)
	}

	log.Info("IBI preparation process finished successfully!")
}
