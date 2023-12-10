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

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
)

// Create logger
var log = &logrus.Logger{
	Out:   os.Stdout,
	Level: logrus.InfoLevel,
}

// verbose is the optional command that will display INFO logs
var verbose bool

// noColor is the optional flag for controlling ANSI sequence output
var noColor bool

// version is an optional command that will display the current release version
var releaseVersion string

func addCommonFlags(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&authFile, "authfile", "a", common.ImageRegistryAuthFile, "The path to the authentication file of the container registry.")
	cmd.Flags().StringVarP(&containerRegistry, "image", "i", "", "The full image name with the container registry to push the OCI image.")
	cmd.Flags().StringVarP(&recertContainerImage, "recert-image", "e", common.DefaultRecertImage, "The full image name for the recert container tool.")
	cmd.Flags().BoolVarP(&recertSkipValidation, "skip-recert-validation", "", false, "Skips the validations performed by the recert tool.")
	cmd.Flags().BoolVarP(&skipCleanup, "skip-cleanup", "", false, "Skips cleanup.")

	// Mark flags as required
	cmd.MarkFlagRequired("image")
}

func init() {
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Display verbose logs")
	rootCmd.PersistentFlags().BoolVarP(&noColor, "no-color", "c", false, "Control colored output")
}

var (
	rootCmd = &cobra.Command{
		Use:     "ibu-imager",
		Version: releaseVersion,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			if verbose {
				log.SetLevel(logrus.DebugLevel)
			} else {
				log.SetLevel(logrus.InfoLevel)
			}
		},
	}
)

// Execute executes the root command.
func Execute() error {
	log.SetFormatter(&logrus.TextFormatter{
		DisableColors:   noColor,
		TimestampFormat: "2006-01-02 15:04:05",
		FullTimestamp:   true,
	})
	fmt.Println(`
 ___ ____  _   _            ___                                 
|_ _| __ )| | | |          |_ _|_ __ ___   __ _  __ _  ___ _ __ 
 | ||  _ \| | | |   _____   | ||  _   _ \ / _  |/ _  |/ _ \ '__|
 | || |_) | |_| |  |_____|  | || | | | | | (_| | (_| |  __/ |
|___|____/ \___/           |___|_| |_| |_|\__,_|\__, |\___|_|
                                                |___/

 A tool to assist in building OCI seed images for Image Based Upgrades (IBU)
	`)
	return rootCmd.Execute()
}
