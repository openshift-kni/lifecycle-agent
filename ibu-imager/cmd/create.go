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
	"fmt"

	v1 "github.com/openshift/api/config/v1"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	runtimeClient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
	ostree "github.com/openshift-kni/lifecycle-agent/ibu-imager/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/ibu-imager/seedcreator"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
)

var (
	scheme = runtime.NewScheme()

	// authFile is the path to the registry credentials used to push the OCI image
	authFile string

	// containerRegistry is the registry to push the OCI image
	containerRegistry string

	// recertContainerImage is the container image for the recert tool
	recertContainerImage string
	recertSkipValidation bool
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

// createCmd represents the create command
var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create OCI image and push it to a container registry.",
	Run: func(cmd *cobra.Command, args []string) {
		if err := create(); err != nil {
			log.Fatalf("Error executing create command: %v", err)
		}
	},
}

func init() {

	// Add create command
	rootCmd.AddCommand(createCmd)

	// Add flags to imager command
	createCmd.Flags().StringVarP(&authFile, "authfile", "a", common.ImageRegistryAuthFile, "The path to the authentication file of the container registry.")
	createCmd.Flags().StringVarP(&containerRegistry, "image", "i", "", "The full image name with the container registry to push the OCI image.")
	createCmd.Flags().StringVarP(&recertContainerImage, "recert-image", "e", common.DefaultRecertImage, "The full image name for the recert container tool.")
	createCmd.Flags().BoolVarP(&recertSkipValidation, "skip-recert-validation", "", false, "Skips the validations performed by the recert tool.")

	// Mark flags as required
	createCmd.MarkFlagRequired("image")
}

func create() error {

	var err error
	log.Info("OCI image creation has started")

	hostCommandsExecutor := ops.NewNsenterExecutor(log, true)
	op := ops.NewOps(log, hostCommandsExecutor)
	rpmOstreeClient := ostree.NewClient("ibu-imager", hostCommandsExecutor)

	config, err := clientcmd.BuildConfigFromFlags("", common.KubeconfigFile)
	if err != nil {
		return fmt.Errorf("failed to create k8s config: %w", err)
	}

	client, err := runtimeClient.New(config, runtimeClient.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("failed to create runtime client: %w", err)
	}

	seedCreator := seedcreator.NewSeedCreator(client, log, op, rpmOstreeClient, common.BackupDir, common.KubeconfigFile,
		containerRegistry, authFile, recertContainerImage, recertSkipValidation)
	if err = seedCreator.CreateSeedImage(); err != nil {
		return fmt.Errorf("failed to create seed image: %w", err)
	}

	// TODO: add cleanup

	log.Info("OCI image created successfully!")
	return nil
}
