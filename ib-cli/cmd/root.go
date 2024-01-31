package cmd

import (
	"os"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
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

var (
	rootCmd = &cobra.Command{
		Use:     "ib-cli",
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

func init() {
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Display verbose logs")
	rootCmd.PersistentFlags().BoolVarP(&noColor, "no-color", "c", false, "Control colored output")
}
