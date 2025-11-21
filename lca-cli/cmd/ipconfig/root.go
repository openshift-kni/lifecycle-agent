package ipconfig

import (
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	pkgLog         *logrus.Logger
	pkgRecertImage string
)

// NewIPConfigCmd builds the ip-config parent command and its subcommands
func NewIPConfigCmd(log *logrus.Logger, recertImage string) *cobra.Command {
	pkgLog = log
	pkgRecertImage = recertImage

	ipConfigCmd := &cobra.Command{
		Use:   "ip-config",
		Short: "IP configuration commands",
	}

	// Subcommands are registered in their init() functions when package is imported,
	// but here we ensure they are attached after globals are set
	ipConfigCmd.AddCommand(ipConfigRunCmd)
	ipConfigCmd.AddCommand(ipConfigPrepareCmd)
	ipConfigCmd.AddCommand(ipConfigRollbackCmd)

	return ipConfigCmd
}
