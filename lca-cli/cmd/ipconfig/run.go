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
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	runtimeClient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	intOstree "github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ipconfig"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmOstree "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	lcautils "github.com/openshift-kni/lifecycle-agent/utils"
	ocp_config_v1 "github.com/openshift/api/config/v1"
	machineconfigv1 "github.com/openshift/api/machineconfiguration/v1"
)

var (
	ipConfigScheme = runtime.NewScheme()

	ipv4Address        string
	ipv4MachineNetwork string
	ipv6Address        string
	ipv6MachineNetwork string
	ipv4Gateway        string
	ipv6Gateway        string
	ipv4DNS            string
	ipv6DNS            string
	vlanID             int
	pullSecretRefName  string
	recertImage        string
	dnsIPFamily        string
)

const (
	ipv4AddressFlag        = "ipv4-address"
	ipv4MachineNetworkFlag = "ipv4-machine-network"
	ipv6AddressFlag        = "ipv6-address"
	ipv6MachineNetworkFlag = "ipv6-machine-network"
	ipv4GatewayFlag        = "ipv4-gateway"
	ipv6GatewayFlag        = "ipv6-gateway"
	ipv4DNSFlag            = "ipv4-dns"
	ipv6DNSFlag            = "ipv6-dns"
	vlanIDFlag             = "vlan-id"
	recertImageFlag        = "recert-image"
	pullSecretRefNameFlag  = "pull-secret-ref-name" //nolint:gosec // flag name, not credentials
	dnsIPFamilyFlag        = "dns-ip-family"
	runCmd                 = "run"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(ipConfigScheme))
	utilruntime.Must(machineconfigv1.AddToScheme(ipConfigScheme))
	utilruntime.Must(ocp_config_v1.AddToScheme(ipConfigScheme))

	ipConfigRunCmd.Flags().StringVar(&ipv4Address, ipv4AddressFlag, "", "Target IPv4 address")
	ipConfigRunCmd.Flags().StringVar(&ipv4MachineNetwork, ipv4MachineNetworkFlag, "", "Target IPv4 machine network CIDR")
	ipConfigRunCmd.Flags().StringVar(&ipv6Address, ipv6AddressFlag, "", "Target IPv6 address")
	ipConfigRunCmd.Flags().StringVar(&ipv6MachineNetwork, ipv6MachineNetworkFlag, "", "Target IPv6 machine network CIDR")
	ipConfigRunCmd.Flags().StringVar(&ipv4Gateway, ipv4GatewayFlag, "", "IPv4 default gateway")
	ipConfigRunCmd.Flags().StringVar(&ipv6Gateway, ipv6GatewayFlag, "", "IPv6 default gateway")
	ipConfigRunCmd.Flags().StringVar(&ipv4DNS, ipv4DNSFlag, "", "IPv4 DNS server")
	ipConfigRunCmd.Flags().StringVar(&ipv6DNS, ipv6DNSFlag, "", "IPv6 DNS server")
	ipConfigRunCmd.Flags().IntVar(&vlanID, vlanIDFlag, 0, "Optional VLAN ID to use on the br-ex uplink")
	ipConfigRunCmd.Flags().StringVar(&recertImage, recertImageFlag, "", "The full image name for the recert container tool")
	ipConfigRunCmd.Flags().StringVar(&pullSecretRefName, pullSecretRefNameFlag, "", "The name of the pull secret to use for the recert container tool")
	ipConfigRunCmd.Flags().StringVar(&dnsIPFamily, dnsIPFamilyFlag, "", "IP family for DNS resolution (ipv4|ipv6)")
}

var ipConfigRunCmd = &cobra.Command{
	Use:   runCmd,
	Short: "Execute IP configuration change and reboot to the new configuration",
	Run: func(cmd *cobra.Command, args []string) {
		if err := runIPConfigRun(); err != nil {
			pkgLog.Fatalf("Error executing ip-config run: %v", err)
		}
	},
}

// runIPConfigRun coordinates the ip-config "run" flow:
// - writes initial status, loads flags/config, validates inputs
// - infers the primary IP family and builds handler dependencies
// - applies network changes, finalizes status, and reboots
func runIPConfigRun() (retErr error) {
	// This part is not rollback protected because we need the reboot client to be available
	// for auto-rollback. We can consider solutions for it if needed
	opsInterface, hostCommandsExecutor, client, err := buildOpsAndK8sClient(ipConfigScheme)
	if err != nil {
		return err
	}

	rpmClient := rpmOstree.NewClient("lca-cli-ip-config-run", hostCommandsExecutor)
	ostreeClient := intOstree.NewClient(hostCommandsExecutor, false)
	rbClient := reboot.NewIPCRebootClient(&logr.Logger{}, hostCommandsExecutor, rpmClient, ostreeClient, opsInterface)

	defer func() {
		if retErr == nil {
			if err := common.FinalizeIPConfigStatus(
				common.IPConfigRunStatusFile,
				common.IPConfigStatusSucceeded,
				"ip-config run completed successfully",
			); err != nil {
				retErr = fmt.Errorf("failed to mark IP config run as successful: %w", err)
				return
			}

			if err := rbClient.Reboot("ip-config run"); err != nil {
				retErr = fmt.Errorf("failed to reboot: %w", err)
				return
			}
		}

		common.OstreeDeployPathPrefix = sysrootPath
		if err := opsInterface.RemountSysroot(); err != nil {
			retErr = fmt.Errorf("failed to remount sysroot: %w", err)
			return
		}

		if err := common.FinalizeIPConfigStatus(
			common.IPConfigRunStatusFile,
			common.IPConfigStatusFailed,
			fmt.Sprintf("ip-config run failed: %v", retErr),
		); err != nil {
			retErr = fmt.Errorf("failed to finalize current stateroot IP config run status: %w", err)
			return
		}

		rbClient.AutoRollbackIfEnabled(
			reboot.IPConfigRunComponent,
			fmt.Sprintf("automatic rollback: ip-config run failed: %v", retErr),
		)
	}()

	// Start of rollback protected part

	err = common.WriteIPConfigStatus(common.IPConfigRunStatusFile,
		common.IPConfigStatus{
			Phase:     common.IPConfigStatusRunning,
			Message:   "ip-config run started",
			StartedAt: time.Now().UTC().Format(time.RFC3339),
		})
	if err != nil {
		return fmt.Errorf("failed to write initial status: %w", err)
	}

	if data, err := os.ReadFile(common.IPConfigRunFlagsFile); err == nil && len(data) > 0 {
		var cfg common.IPConfigRunConfig
		if jsonErr := json.Unmarshal(data, &cfg); jsonErr == nil {
			ipv4Address = cfg.IPv4Address
			ipv4MachineNetwork = cfg.IPv4MachineNetwork
			ipv6Address = cfg.IPv6Address
			ipv6MachineNetwork = cfg.IPv6MachineNetwork
			ipv4Gateway = cfg.IPv4Gateway
			ipv6Gateway = cfg.IPv6Gateway
			ipv4DNS = cfg.IPv4DNSServer
			ipv6DNS = cfg.IPv6DNSServer
			vlanID = cfg.VLANID
			pullSecretRefName = cfg.PullSecretRefName
			recertImage = cfg.RecertImage
			dnsIPFamily = cfg.DNSIPFamily
		} else {
			pkgLog.Warnf("failed to unmarshal ip-config run config: %v", jsonErr)
		}
	} else {
		pkgLog.Info("using command line flags")
	}

	logRunFlags()

	// test error
	ctx := context.Background()

	if err := validateRunFlags(ctx, client); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	if err := gatherMissingNetworkData(
		ctx,
		client,
		opsInterface,
	); err != nil {
		return err
	}

	effectivePrimary, err := ipconfig.InferPrimaryStack()
	if err != nil {
		return fmt.Errorf("failed to infer primary IP stack: %w", err)
	}

	ipConfigs := ipconfig.BuildIPConfigs(
		ipv4Address, ipv4MachineNetwork, ipv4Gateway, ipv4DNS,
		ipv6Address, ipv6MachineNetwork, ipv6Gateway, ipv6DNS,
		lo.FromPtr(effectivePrimary),
	)

	if recertImage == "" {
		recertImage = common.DefaultRecertImage
	}

	ipConfigHandler := ipconfig.NewIPConfig(
		pkgLog,
		opsInterface,
		hostCommandsExecutor,
		client,
		recertImage,
		ipConfigs,
		pullSecretRefName,
		vlanID,
		dnsIPFamily,
	)

	if err := ipConfigHandler.Run(ctx); err != nil {
		return fmt.Errorf("ip-config run failed: %w", err)
	}

	return nil
}

// completeMissingIPFamilyFromClusterAndHost fills in missing IP family configuration
// (address, machine network, gateway, DNS) by inspecting the current SNO host and
// cluster state. It is used when the user only specifies one IP family on a
// dual-stack cluster so that recert and nmstate still see a complete configuration.
func gatherMissingNetworkData(
	ctx context.Context,
	client runtimeClient.Client,
	hostOps ops.Ops,
) error {
	ipv4Provided := ipv4Address != "" && ipv4MachineNetwork != "" && ipv4Gateway != "" && ipv4DNS != ""
	ipv6Provided := ipv6Address != "" && ipv6MachineNetwork != "" && ipv6Gateway != "" && ipv6DNS != ""

	if ipv4Provided && ipv6Provided {
		return nil
	}

	// dual-stack clusters only

	nmOutput, err := hostOps.RunInHostNamespace("nmstatectl", "show", "--json", "-q")
	if err != nil {
		return fmt.Errorf("failed to run nmstatectl show --json: %w", err)
	}

	nmState, err := lcautils.ParseNmstate(nmOutput)
	if err != nil {
		return fmt.Errorf("failed to parse nmstate output: %w", err)
	}

	dnsV4, dnsV6 := lcautils.ExtractDNS(nmState)
	if dnsV4 == "" || dnsV6 == "" {
		pkgLog.Infof("No DNS servers found in nmstate output")
	}

	gw4, gw6 := lcautils.FindDefaultGateways(
		nmState,
		ipconfig.BridgeExternalName,
		ipconfig.DefaultRouteV4,
		ipconfig.DefaultRouteV6,
	)
	if gw4 == "" || gw6 == "" {
		pkgLog.Infof("No default gateways found in nmstate output")
	}

	ips, err := lcautils.GetNodeInternalIPs(ctx, client)
	if err != nil {
		return fmt.Errorf("failed to get cluster info: %w", err)
	}

	var nodeIPv4, nodeIPv6 string
	for _, ip := range ips {
		if strings.Contains(ip, ":") {
			if nodeIPv6 == "" {
				nodeIPv6 = ip
			}
		} else {
			if nodeIPv4 == "" {
				nodeIPv4 = ip
			}
		}
	}

	clusterHasIPv4, clusterHasIPv6 := common.DetectClusterIPFamilies(ips)
	machineNetworks, err := lcautils.GetMachineNetworks(ctx, client)
	if err != nil {
		return fmt.Errorf("failed to get machine networks: %w", err)
	}

	// Auto-complete IPv4 if user omitted it but cluster is IPv4-capable.
	if !ipv4Provided && clusterHasIPv4 {
		cidr := lcautils.FindMatchingCIDR(nodeIPv4, machineNetworks)
		if cidr == "" {
			return fmt.Errorf("failed to find machine network CIDR for node IPv4 %s", nodeIPv4)
		}

		pkgLog.Infof("Auto-completing IPv4 configuration from cluster/host state: ip=%s, cidr=%s", nodeIPv4, cidr)
		ipv4Address = nodeIPv4
		ipv4MachineNetwork = cidr
		ipv4Gateway = gw4
		ipv4DNS = dnsV4
	}

	// Auto-complete IPv6 if user omitted it but cluster is IPv6-capable.
	if !ipv6Provided && clusterHasIPv6 {
		cidr := lcautils.FindMatchingCIDR(nodeIPv6, machineNetworks)
		if cidr == "" {
			return fmt.Errorf("failed to find machine network CIDR for node IPv6 %s", nodeIPv6)
		}

		pkgLog.Infof("Auto-completing IPv6 configuration from cluster/host state: ip=%s, cidr=%s", nodeIPv6, cidr)
		ipv6Address = nodeIPv6
		ipv6MachineNetwork = cidr
		ipv6Gateway = gw6
		ipv6DNS = dnsV6
	}

	return nil
}

// validateClusterAPIAndUserIPSpec ensures that:
//  1. the cluster API is reachable, by attempting to read the node internal IPs
//  2. the user-provided IP family configuration (IPv4/IPv6/both) is compatible
//     with the cluster's configured IP families (single-stack or dual-stack).
func validateClusterAPIAndUserIPSpec(
	ctx context.Context,
	client runtimeClient.Client,
) error {
	ips, err := lcautils.GetNodeInternalIPs(ctx, client)
	if err != nil {
		return fmt.Errorf("failed to contact cluster API: %w", err)
	}

	clusterHasIPv4, clusterHasIPv6 := common.DetectClusterIPFamilies(ips)

	ipv4Provided := ipv4Address != "" || ipv4MachineNetwork != "" || ipv4Gateway != "" || ipv4DNS != ""
	ipv6Provided := ipv6Address != "" || ipv6MachineNetwork != "" || ipv6Gateway != "" || ipv6DNS != ""

	if ipv4Provided && !clusterHasIPv4 {
		return fmt.Errorf("IPv4 is specified, but the cluster does not have IPv4")
	}

	if ipv6Provided && !clusterHasIPv6 {
		return fmt.Errorf("IPv6 is specified, but the cluster does not have IPv6")
	}

	return nil
}

// validateIPFamilyConfigArgs validates IPv4/IPv6 arguments for consistency and correctness.
// It enforces that each family is either fully specified or omitted, and that
// addresses and gateways belong to their respective machine networks.
func validateIPFamilyConfigArgs() error {
	ipv4All := ipv4Address != "" && ipv4MachineNetwork != "" && ipv4Gateway != "" && ipv4DNS != ""
	ipv4None := ipv4Address == "" && ipv4MachineNetwork == "" && ipv4Gateway == "" && ipv4DNS == ""
	ipv6All := ipv6Address != "" && ipv6MachineNetwork != "" && ipv6Gateway != "" && ipv6DNS != ""
	ipv6None := ipv6Address == "" && ipv6MachineNetwork == "" && ipv6Gateway == "" && ipv6DNS == ""

	if (!ipv4All && !ipv4None) || (!ipv6All && !ipv6None) {
		return fmt.Errorf("both address, machine-network, gateway and DNS must be provided together for each IP family")
	}

	if ipv4None && ipv6None {
		return fmt.Errorf("at least one of IPv4 or IPv6 must be provided")
	}

	if ipv4All {
		if err := lcautils.ValidateIPFamilyConfig(
			common.IPv4FamilyName,
			ipv4Address,
			ipv4MachineNetwork,
			ipv4Gateway,
			ipv4DNS,
		); err != nil {
			return fmt.Errorf("invalid IPv4 config: %w", err)
		}
	}

	if ipv6All {
		if err := lcautils.ValidateIPFamilyConfig(
			common.IPv6FamilyName,
			ipv6Address,
			ipv6MachineNetwork,
			ipv6Gateway,
			ipv6DNS,
		); err != nil {
			return fmt.Errorf("invalid IPv6 config: %w", err)
		}
	}

	return nil
}

// logRunFlags logs the effective flags used by the ip-config run command.
func logRunFlags() {
	pkgLog.Infof("ip-config run effective flags:")

	if ipv4Address != "" {
		pkgLog.Infof("  ipv4-address=%s", ipv4Address)
	}
	if ipv4MachineNetwork != "" {
		pkgLog.Infof("  ipv4-machine-network=%s", ipv4MachineNetwork)
	}
	if ipv4Gateway != "" {
		pkgLog.Infof("  ipv4-gateway=%s", ipv4Gateway)
	}
	if ipv4DNS != "" {
		pkgLog.Infof("  ipv4-dns=%s", ipv4DNS)
	}
	if ipv6Address != "" {
		pkgLog.Infof("  ipv6-address=%s", ipv6Address)
	}
	if ipv6MachineNetwork != "" {
		pkgLog.Infof("  ipv6-machine-network=%s", ipv6MachineNetwork)
	}
	if ipv6Gateway != "" {
		pkgLog.Infof("  ipv6-gateway=%s", ipv6Gateway)
	}
	if ipv6DNS != "" {
		pkgLog.Infof("  ipv6-dns=%s", ipv6DNS)
	}
	if vlanID != 0 {
		pkgLog.Infof("  vlan-id=%d", vlanID)
	}
	if pullSecretRefName != "" {
		pkgLog.Infof("  pull-secret-ref-name=%s", pullSecretRefName)
	}
	if recertImage != "" {
		pkgLog.Infof("  recert-image=%s", recertImage)
	}
	if dnsIPFamily != "" {
		pkgLog.Infof("  dns-ip-family=%s", dnsIPFamily)
	}
}

// validateRunFlags validates all run cmd flags including IP configs, VLAN and DNS family.
func validateRunFlags(ctx context.Context, client runtimeClient.Client) error {
	if err := validateIPFamilyConfigArgs(); err != nil {
		return fmt.Errorf("invalid IP config arguments: %w", err)
	}

	if err := validateClusterAPIAndUserIPSpec(ctx, client); err != nil {
		return fmt.Errorf("failed to validate cluster API and user IP spec: %w", err)
	}

	if vlanID < 0 {
		return fmt.Errorf("vlan-id must be >= 0")
	}

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
