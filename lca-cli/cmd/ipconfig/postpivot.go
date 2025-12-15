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
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	intOstree "github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ipconfig"
	rpmOstree "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	ocp_config_v1 "github.com/openshift/api/config/v1"
	machineconfigv1 "github.com/openshift/api/machineconfiguration/v1"
)

var (
	ipConfigScheme = runtime.NewScheme()

	recertImage string
)

const (
	recertImageFlag = "recert-image"
	postPivotCmd    = "post-pivot"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(ipConfigScheme))
	utilruntime.Must(machineconfigv1.AddToScheme(ipConfigScheme))
	utilruntime.Must(ocp_config_v1.AddToScheme(ipConfigScheme))

	ipConfigPostPivotCmd.Flags().StringVar(&recertImage, recertImageFlag, common.DefaultRecertImage, "The full image name for the recert container tool")
	ipConfigPostPivotCmd.Flags().StringVar(&dnsIPFamily, dnsIPFamilyFlag, "", "IP family for DNS resolution (ipv4|ipv6)")

}

var ipConfigPostPivotCmd = &cobra.Command{
	Use:   postPivotCmd,
	Short: "Execute IP configuration post-pivot",
	Run: func(cmd *cobra.Command, args []string) {
		if err := runIPConfigPostPivot(); err != nil {
			pkgLog.Fatalf("Error executing ip-config post-pivot: %v", err)
		}
	},
}

// runIPConfigPostPivot coordinates the ip-config "post-pivot" flow:
// - writes initial status, loads flags/config, validates inputs
// - infers the primary IP family and builds handler dependencies
// - applies network changes, finalizes status
func runIPConfigPostPivot() (retErr error) {
	opsInterface, hostCommandsExecutor, client, err := buildOpsAndK8sClient(ipConfigScheme)
	if err != nil {
		return err
	}

	rpmClient := rpmOstree.NewClient("lca-cli-ip-config-post-pivot", hostCommandsExecutor)
	ostreeClient := intOstree.NewClient(hostCommandsExecutor, false)
	rbClient := reboot.NewIPCRebootClient(&logr.Logger{}, hostCommandsExecutor, rpmClient, ostreeClient, opsInterface)

	defer func() {
		if retErr == nil {
			return
		}

		rbClient.AutoRollbackIfEnabled(
			reboot.IPConfigRunComponent,
			fmt.Sprintf("automatic rollback: ip-config post-pivot failed: %v", retErr),
		)
	}()

	// Prefer file-based config if present (written by the controller), otherwise use CLI flags.
	if data, err := os.ReadFile(common.IPConfigPostPivotFlagsFile); err == nil && len(data) > 0 {
		var cfg common.IPConfigPostPivotConfig
		if jsonErr := json.Unmarshal(data, &cfg); jsonErr == nil {
			if cfg.RecertImage != "" {
				recertImage = cfg.RecertImage
			}
			if cfg.DNSIPFamily != "" {
				dnsIPFamily = cfg.DNSIPFamily
			}
		} else {
			pkgLog.Warnf("failed to unmarshal ip-config post-pivot config: %v", jsonErr)
		}
	} else {
		pkgLog.Info("using command line flags")
	}

	logPostPivotFlags()

	if err := validatePostPivotFlags(); err != nil {
		return fmt.Errorf("post-pivot flags validation failed: %w", err)
	}

	ctx := context.Background()

	postPivotHandler := ipconfig.NewIPConfigPostPivotHandler(
		pkgLog,
		opsInterface,
		client,
		recertImage,
		dnsIPFamily,
		ipConfigScheme,
		common.KubeconfigFile,
		common.LCAWorkspaceDir,
	)

	if err := postPivotHandler.Run(ctx); err != nil {
		return fmt.Errorf("ip config post-pivot handler failed: %w", err)
	}

	return nil
}

func logPostPivotFlags() {
	pkgLog.Info("IP config post-pivot flags:")
	pkgLog.Infof("  %s=%q", recertImageFlag, recertImage)
	pkgLog.Infof("  %s=%q", dnsIPFamilyFlag, dnsIPFamily)
}

// validatePostPivotFlags validates post-pivot cmd flags.
func validatePostPivotFlags() error {
	if dnsIPFamily != "" {
		switch dnsIPFamily {
		case common.IPv4FamilyName:
		case common.IPv6FamilyName:
		default:
			return fmt.Errorf("dns-ip-family must be one of: %s|%s", common.IPv4FamilyName, common.IPv6FamilyName)
		}
	}

	return nil
}
