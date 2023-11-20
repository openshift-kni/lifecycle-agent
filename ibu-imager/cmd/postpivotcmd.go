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

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
	"github.com/openshift-kni/lifecycle-agent/ibu-imager/postpivot"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/spf13/cobra"
)

// createCmd represents the create command
var postPivotCmd = &cobra.Command{
	Use:   "post-pivot",
	Short: "post pivot configuration",
	Run: func(cmd *cobra.Command, args []string) {
		postPivot()
	},
}

var inContainer bool

func init() {

	// Add create command
	rootCmd.AddCommand(postPivotCmd)

	// TODO: maybe better to get as os env?
	postPivotCmd.Flags().StringVarP(&recertContainerImage, "recert-image", "e", common.DefaultRecertImage, "The full image name for the recert container tool.")
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

	postPivotRunner := postpivot.NewPostPivot(scheme, log, ops.NewOps(log, hostCommandsExecutor), recertContainerImage,
		common.ImageRegistryAuthFile, common.OptOpenshift, common.KubeconfigFile)
	if err := postPivotRunner.PostPivotConfiguration(context.TODO()); err != nil {
		log.Fatal(err)
	}

	log.Info("Post pivot operation finished successfully!")
}
