/*
Copyright 2024.

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

	"github.com/openshift-kni/lifecycle-agent/lca-cli/initmonitor"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"github.com/spf13/cobra"
)

// initMonitorCmd represents the init-monitor command
var initMonitorCmd = &cobra.Command{
	Use:   "init-monitor",
	Short: "LCA Init Monitor",
	RunE: func(cmd *cobra.Command, args []string) error {
		return initMonitor()
	},
}

var (
	launchMonitor              bool
	monitorSvcUnitComponentTag string
)

func init() {

	// Add init-monitor command
	rootCmd.AddCommand(initMonitorCmd)

	initMonitorCmd.Flags().BoolVar(&launchMonitor, "monitor", false, "Run LCA init monitor")
	initMonitorCmd.Flags().StringVar(&monitorSvcUnitComponentTag, "exec-stop-post", "", "Run ExecStopPost, with specified component tag")
	initMonitorCmd.MarkFlagsMutuallyExclusive("monitor", "exec-stop-post")
}

func initMonitor() error {
	var hostCommandsExecutor ops.Execute
	hostCommandsExecutor = ops.NewRegularExecutor(log, true)

	initMonitorRunner := initmonitor.NewInitMonitor(scheme, log, hostCommandsExecutor, ops.NewOps(log, hostCommandsExecutor), monitorSvcUnitComponentTag)
	if launchMonitor {
		return initMonitorRunner.RunInitMonitor()
	} else if monitorSvcUnitComponentTag != "" {
		return initMonitorRunner.RunExitStopPostCheck()
	}

	return fmt.Errorf("must specify either --monitor or --exec-stop-post")
}
