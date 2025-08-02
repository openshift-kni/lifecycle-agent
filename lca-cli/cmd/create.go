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

	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	v1 "github.com/openshift/api/config/v1"
	mcv1 "github.com/openshift/api/machineconfiguration/v1"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	runtimeClient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	ostree "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedcreator"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedrestoration"
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

	skipCleanup bool
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1.AddToScheme(scheme))
	utilruntime.Must(mcv1.AddToScheme(scheme))
	utilruntime.Must(operatorv1alpha1.AddToScheme(scheme))
	utilruntime.Must(operatorsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(ibuv1.AddToScheme(scheme))
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

	// Add flags to create command
	addCommonFlags(createCmd)
}

func create() error {

	var err error
	log.Info("OCI image creation has started")

	hostCommandsExecutor := ops.NewNsenterExecutor(log, true)
	op := ops.NewOps(log, hostCommandsExecutor)
	rpmOstreeClient := ostree.NewClient("lca-cli", hostCommandsExecutor)

	if !skipCleanup {
		defer func() {
			if err = seedrestoration.NewSeedRestoration(log, op, common.BackupDir, containerRegistry,
				authFile, recertContainerImage, recertSkipValidation).RestoreSeedCluster(); err != nil {
				log.Fatalf("Failed to restore seed cluster: %v", err)
			}
			log.Info("Seed cluster restored successfully!")
		}()
	}

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
		err = fmt.Errorf("failed to create seed image: %w", err)
		log.Error(err)
		return err
	}

	log.Info("OCI image created successfully!")
	return nil
}
