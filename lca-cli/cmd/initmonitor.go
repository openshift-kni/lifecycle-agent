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
	"os"
	"strings"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
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
	monitorMode                string
)

func init() {

	// Add init-monitor command
	rootCmd.AddCommand(initMonitorCmd)

	initMonitorCmd.Flags().BoolVar(&launchMonitor, "monitor", false, "Run LCA init monitor")
	initMonitorCmd.Flags().StringVar(&monitorSvcUnitComponentTag, "exec-stop-post", "", "Run ExecStopPost, with specified component tag")
	initMonitorCmd.Flags().StringVar(&monitorMode, "mode", "ibu", "Init monitor mode: 'ibu' or 'ipc'")
	initMonitorCmd.MarkFlagsMutuallyExclusive("monitor", "exec-stop-post")
}

func initMonitor() error {
	var hostCommandsExecutor ops.Execute
	hostCommandsExecutor = ops.NewRegularExecutor(log, true)

	if data, err := os.ReadFile(common.PathOutsideChroot(common.InitMonitorModeFile)); err == nil {
		if val := strings.TrimSpace(string(data)); val != "" {
			monitorMode = val
			log.Infof("init-monitor mode set from file: %s", monitorMode)
		}
	}

	initMonitorRunner := initmonitor.NewInitMonitor(
		scheme,
		log,
		hostCommandsExecutor,
		ops.NewOps(log, hostCommandsExecutor),
		monitorSvcUnitComponentTag,
		monitorMode,
	)
	if launchMonitor {
		if err := initMonitorRunner.RunInitMonitor(); err != nil {
			return fmt.Errorf("failed to run init monitor: %w", err)
		}
		return nil
	} else if monitorSvcUnitComponentTag != "" {
		if err := initMonitorRunner.RunExitStopPostCheck(); err != nil {
			return fmt.Errorf("failed to run exit stop post check: %w", err)
		}
		return nil
	}

	return fmt.Errorf("must specify either --monitor or --exec-stop-post")
}
