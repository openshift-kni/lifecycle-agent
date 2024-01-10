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
	"github.com/spf13/cobra"

	ibipreparation "github.com/openshift-kni/lifecycle-agent/ibu-imager/ibi-preparation"
	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
	ostree "github.com/openshift-kni/lifecycle-agent/ibu-imager/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
)

// ibi represents the ibi preparation command
var ibi = &cobra.Command{
	Use:   "ibi",
	Short: "prepare ibi",
	Run: func(cmd *cobra.Command, args []string) {
		runIBI()
	},
}

var seedImage string
var seedVersion string
var pullSecretFile string

func init() {

	// Add create command
	rootCmd.AddCommand(ibi)

	ibi.Flags().StringVarP(&seedImage, "seed-image", "s", "", "Seed image.")
	ibi.Flags().StringVarP(&seedVersion, "seed-version", "", "", "Seed version.")
	ibi.Flags().StringVarP(&authFile, "authfile", "a", "", "The path to the authentication file of the container registry of seed image.")
	ibi.Flags().StringVarP(&pullSecretFile, "pullSecretFile", "p", "", "The path to the pull secret file for precache process.")

}

func runIBI() {
	log.Info("IBI preparation process has started")
	hostCommandsExecutor := ops.NewChrootExecutor(log, true, common.Host)
	rpmOstreeClient := ostree.NewClient("ibu-imager", hostCommandsExecutor)
	ostreeClient := ostreeclient.NewClient(hostCommandsExecutor, true)

	ibiRunner := ibipreparation.NewIBIPrepare(log, ops.NewOps(log, hostCommandsExecutor), rpmOstreeClient, ostreeClient, seedImage, authFile, pullSecretFile, seedVersion)
	if err := ibiRunner.Run(); err != nil {
		log.Fatal(err)
	}

	log.Info("IBI preparation process finished successfully!")
}
