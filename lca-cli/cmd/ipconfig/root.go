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

package ipconfigcmd

import (
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	pkgLog *logrus.Logger
)

// NewIPConfigCmd builds the ip-config parent command and its subcommands
func NewIPConfigCmd(log *logrus.Logger) *cobra.Command {
	pkgLog = log

	ipConfigCmd := &cobra.Command{
		Use:   "ip-config",
		Short: "IP configuration commands",
	}

	// Subcommands are registered in their init() functions when package is imported,
	// but here we ensure they are attached after globals are set
	ipConfigCmd.AddCommand(ipConfigPostPivotCmd)
	ipConfigCmd.AddCommand(ipConfigPrePivotCmd)
	ipConfigCmd.AddCommand(ipConfigRollbackCmd)

	return ipConfigCmd
}
