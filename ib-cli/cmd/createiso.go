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
	"path"
	"path/filepath"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/openshift-kni/lifecycle-agent/api/ibiconfig"
	"github.com/openshift-kni/lifecycle-agent/ib-cli/installationiso"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

var (
	workDir string
)

func addFlags(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&workDir, "dir", "d", "", "The working directory for creating the ISO."+
		"Working directory should contain the configuration file 'image-based-install-iso.yaml'.")

	cmd.MarkFlagRequired("dir")
}

// createCmd represents the create command
var createIsoCmd = &cobra.Command{
	Use:   "create-iso",
	Short: "Create installation ISO from OCI image.",
	Run: func(cmd *cobra.Command, args []string) {
		if err := createIso(); err != nil {
			log.Fatalf("Error executing installationiso command: %v", err)
		}
	},
}

func init() {
	// Add installationiso command
	rootCmd.AddCommand(createIsoCmd)
	addFlags(createIsoCmd)
}

func readIBIConfigFile(configFile string) (*ibiconfig.ImageBasedInstallConfig, error) {
	var config ibiconfig.ImageBasedInstallConfig
	if configFile == "" {
		return nil, fmt.Errorf("configuration file is required")
	}
	if _, err := os.Stat(configFile); err != nil {
		return nil, fmt.Errorf("configuration file %s does not exist", configFile)
	}

	if err := utils.ReadYamlOrJSONFile(configFile, &config); err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", configFile, err)
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("config file validation failed: %w", err)
	}

	config.SetDefaultValues()

	return &config, nil
}

func createIso() error {
	log.Info("Installation ISO creation has started")
	configurationFile := path.Join(workDir, "image-based-install-iso.yaml")
	config, err := readIBIConfigFile(configurationFile)
	if err != nil {
		log.Fatalf("Error reading configuration file: %v", err)
	}

	hostCommandsExecutor := ops.NewRegularExecutor(log, verbose)
	op := ops.NewOps(log, hostCommandsExecutor)

	workDir, err = filepath.Abs(workDir)
	if err != nil {
		err = fmt.Errorf("failed to working directory full path: %w", err)
		log.Errorf(err.Error())
		return err
	}
	isoCreator := installationiso.NewInstallationIso(log, op, workDir)
	if err = isoCreator.Create(config); err != nil {
		err = fmt.Errorf("failed to create installation ISO: %w", err)
		log.Errorf(err.Error())
		return err
	}

	log.Info("Installation ISO created successfully!")
	return nil
}

// Execute executes the root command.
func Execute() error {
	log.SetFormatter(&logrus.TextFormatter{
		DisableColors:   noColor,
		TimestampFormat: "2006-01-02 15:04:05",
		FullTimestamp:   true,
	})
	fmt.Println(`ib-cli assists Image Based Install (IBI).

  Find more information at: https://github.com/openshift-kni/lifecycle-agent/blob/main/ib-cli/README.md
	`)
	if err := rootCmd.Execute(); err != nil {
		return fmt.Errorf("failed to run root cmd for iso: %w", err)
	}
	return nil
}
