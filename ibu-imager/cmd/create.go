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
	"os"
	"path/filepath"

	cp "github.com/otiai10/copy"
	"github.com/spf13/cobra"

	"ibu-imager/internal/ops"
	ostree "ibu-imager/internal/ostree_client"
	seed "ibu-imager/internal/seed_creator"
)

// authFile is the path to the registry credentials used to push the OCI image
var authFile string

// containerRegistry is the registry to push the OCI image
var containerRegistry string

// createCmd represents the create command
var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create OCI image and push it to a container registry.",
	Run: func(cmd *cobra.Command, args []string) {
		create()
	},
}

func init() {

	// Add create command
	rootCmd.AddCommand(createCmd)

	// Add flags related to container registry
	createCmd.Flags().StringVarP(&authFile, "authfile", "a", imageRegistryAuthFile, "The path to the authentication file of the container registry.")
	createCmd.Flags().StringVarP(&containerRegistry, "registry", "r", "", "The container registry used to push the OCI image.")
}

func create() {

	var err error
	log.Printf("OCI image creation has started")

	// Check if containerRegistry was provided by the user
	if containerRegistry == "" {
		fmt.Printf(" *** Please provide a valid container registry to store the created OCI images *** \n")
		log.Info("Skipping OCI image creation.")
		return
	}

	op := ops.NewOps(log, ops.NewExecutor(log, true))
	rpmOstreeClient := ostree.NewClient("ibu-imager", op)

	err = copyConfigurationFiles(op)
	if err != nil {
		log.Fatal("Failed to add configuration files", err)
	}

	seedCreator := seed.NewSeedCreator(log, op, rpmOstreeClient, backupDir, kubeconfigFile,
		containerRegistry, backupTag, authFile)
	err = seedCreator.CreateSeedImage()
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("OCI image created successfully!")
}

// TODO: move those functions to seed creator and add cleanup
func copyConfigurationFiles(ops ops.Ops) error {
	// copy scripts
	err := copyConfigurationScripts()
	if err != nil {
		return err
	}

	return handleServices(ops)
}

func copyConfigurationScripts() error {
	log.Infof("Copying installation_configuration_files/scripts to local/bin")
	return cp.Copy("installation_configuration_files/scripts", "/var/usrlocal/bin", cp.Options{AddPermission: os.FileMode(0777)})
}

func handleServices(ops ops.Ops) error {
	dir := "installation_configuration_files/services"
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}
		log.Infof("Creating service %s", info.Name())
		errC := cp.Copy(filepath.Join(dir, info.Name()), filepath.Join("/etc/systemd/system/", info.Name()))
		if errC != nil {
			return errC
		}
		log.Infof("Enabling service %s", info.Name())
		_, errC = ops.SystemctlAction("enable", info.Name())
		return errC
	})
	return err
}
