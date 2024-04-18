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
	"path/filepath"

	"github.com/openshift-kni/lifecycle-agent/ib-cli/installationiso"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

var (
	seedImage           string
	seedVersion         string
	pullSecretFile      string
	authFile            string
	sshPublicKeyFile    string
	lcaImage            string
	rhcosLiveIso        string
	installationDisk    string
	extraPartitionStart string
	workDir             string
	precacheBestEffort  bool
	precacheDisabled    bool
	shutdown            bool
)

func addFlags(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&seedImage, "seed-image", "s", "", "Seed image.")
	cmd.Flags().StringVarP(&seedVersion, "seed-version", "", "", "Seed version.")
	cmd.Flags().StringVarP(&authFile, "auth-file", "a", "", "The path to the authentication file of the container registry of seed image.")
	cmd.Flags().StringVarP(&pullSecretFile, "pullsecret-file", "p", "", "The path to the pull secret file for precache process.")
	cmd.Flags().StringVarP(&sshPublicKeyFile, "ssh-public-key-file", "k", "", "The path to ssh public key to be added to the installed host.")
	cmd.Flags().StringVarP(&lcaImage, "lca-image", "l", "quay.io/openshift-kni/lifecycle-agent-operator:4.16.0", "The lifecycle-agent image to use for generating the ISO.")
	cmd.Flags().StringVarP(&rhcosLiveIso, "rhcos-live-iso", "r", "https://mirror.openshift.com/pub/openshift-v4/amd64/dependencies/rhcos/latest/rhcos-live.x86_64.iso", "The URL to the rhcos-live-iso for generating the ISO.")
	cmd.Flags().StringVarP(&installationDisk, "installation-disk", "i", "", "The disk that will be used for the installation.")
	cmd.Flags().StringVarP(&extraPartitionStart, "extra-partition-start", "e", "", "Start of extra partition used for /var/lib/containers. Partition will expand until the end of the disk. Uses sgdisk notation")
	cmd.Flags().StringVarP(&workDir, "dir", "d", "", "The working directory for creating the ISO.")
	cmd.Flags().BoolVarP(&precacheBestEffort, "precache-best-effort", "", false, "Set image precache to best effort mode")
	cmd.Flags().BoolVarP(&precacheDisabled, "precache-disabled", "", false, "Disable precaching, no image precaching will run")
	cmd.Flags().BoolVarP(&shutdown, "shutdown", "", false, "Shutdown of the host after the preparation process is done.")

	cmd.MarkFlagRequired("installation-disk")
	cmd.MarkFlagRequired("extra-partition-start")
	cmd.MarkFlagRequired("dir")
	cmd.MarkFlagRequired("seed-image")
	cmd.MarkFlagRequired("seed-version")
	cmd.MarkFlagRequired("auth-file")
	cmd.MarkFlagRequired("pullsecret-file")
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

func createIso() error {

	var err error
	log.Info("Installation ISO creation has started")

	hostCommandsExecutor := ops.NewRegularExecutor(log, verbose)
	op := ops.NewOps(log, hostCommandsExecutor)

	workDir, err = filepath.Abs(workDir)
	if err != nil {
		err = fmt.Errorf("failed to working directory full path: %w", err)
		log.Errorf(err.Error())
		return err
	}
	isoCreator := installationiso.NewInstallationIso(log, op, workDir)
	if err = isoCreator.Create(seedImage, seedVersion, authFile, pullSecretFile, sshPublicKeyFile, lcaImage, rhcosLiveIso,
		installationDisk, extraPartitionStart, precacheBestEffort, precacheDisabled, shutdown); err != nil {
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
