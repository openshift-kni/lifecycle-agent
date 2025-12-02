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
func runIPConfigRun() error {
	err := common.WriteIPConfigStatus(common.IPConfigRunStatusFile,
		common.IPConfigRunStatus{
			Phase:     common.IPConfigPhaseRunning,
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

	opsInterface, hostCommandsExecutor, client, err := buildOpsAndClient(ipConfigScheme)
	if err != nil {
		return err
	}

	ctx := context.Background()

	if err := validateRunFlags(ctx, client); err != nil {
		return err
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

	rpmClient := rpmOstree.NewClient("lca-cli-ip-config-run", hostCommandsExecutor)
	ostreeClient := intOstree.NewClient(hostCommandsExecutor, false)
	rbClient := reboot.NewIPCRebootClient(&logr.Logger{}, hostCommandsExecutor, rpmClient, ostreeClient, opsInterface)

	if err := ipConfigHandler.Run(ctx); err != nil {
		internalErr := common.FinalizeIPConfigStatus(
			common.IPConfigRunStatusFile,
			common.IPConfigPhaseFailed,
			fmt.Sprintf("ip-config run failed: %v", err),
		)
		if internalErr != nil {
			return fmt.Errorf("failed to finalize IP config run status: %w", internalErr)
		}

		rbClient.AutoRollbackIfEnabled(
			reboot.IPConfigRunComponent, fmt.Sprintf("ip-config run failed: %v", err),
		)

		return fmt.Errorf("failed to run IP config: %w", err)
	}

	if err := common.FinalizeIPConfigStatus(
		common.IPConfigRunStatusFile,
		common.IPConfigPhaseSucceeded,
		"ip-config run completed successfully",
	); err != nil {
		return fmt.Errorf("failed to mark IP config run as successful: %w", err)
	}

	if err := rbClient.Reboot("ip-config run"); err != nil {
		return fmt.Errorf("failed to reboot: %w", err)
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
	if dnsV4 == "" && dnsV6 == "" {
		pkgLog.Infof("No DNS servers found in nmstate output")
	}

	gw4, gw6 := lcautils.FindDefaultGateways(
		nmState,
		ipconfig.BridgeExternalName,
		ipconfig.DefaultRouteV4,
		ipconfig.DefaultRouteV6,
	)
	if gw4 == "" && gw6 == "" {
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

	clusterHasIPv4, clusterHasIPv6 := detectClusterIPFamilies(ips)
	machineNetworks, err := lcautils.GetMachineNetworks(ctx, client)
	if err != nil {
		return fmt.Errorf("failed to get machine networks: %w", err)
	}

	if ipv4Provided && !clusterHasIPv4 {
		if err := completeConfig(
			nodeIPv4,
			machineNetworks,
			gw4,
			dnsV4,
			common.IPv4FamilyName,
		); err != nil {
			return err
		}
	}

	if ipv6Provided && !clusterHasIPv6 {
		if err := completeConfig(
			nodeIPv6,
			machineNetworks,
			gw6,
			dnsV6,
			common.IPv6FamilyName,
		); err != nil {
			return err
		}
	}

	return nil
}

// completeIPv4ConfigIfNeeded auto-completes IPv4 config from cluster/host state
// when the user omitted it but the cluster is IPv4-capable.
func completeConfig(
	nodeIP string,
	machineNetwork []string,
	gw string,
	dns string,
	ipFamily string,
) error {
	cidr := lcautils.FindMatchingCIDR(nodeIP, machineNetwork)
	if cidr == "" {
		return fmt.Errorf("failed to find machine network CIDR for node %s %s", ipFamily, nodeIP)
	}

	pkgLog.Infof(
		"Complete %s configuration from cluster/host state with the following values: ip=%s, cidr=%s, gateway=%s, dns=%s",
		ipFamily,
		nodeIP, cidr, gw, dns,
	)

	ipv4Address = nodeIP
	ipv4MachineNetwork = cidr
	ipv4Gateway = gw
	ipv4DNS = dns

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

	clusterHasIPv4, clusterHasIPv6 := detectClusterIPFamilies(ips)

	ipv4Provided := ipv4Address != "" && ipv4MachineNetwork != "" && ipv4Gateway != "" && ipv4DNS != ""
	ipv6Provided := ipv6Address != "" && ipv6MachineNetwork != "" && ipv6Gateway != "" && ipv6DNS != ""

	switch {
	case ipv4Provided && ipv6Provided:
		// Both families requested: cluster must be dual-stack.
		if !(clusterHasIPv4 && clusterHasIPv6) {
			return fmt.Errorf("both IPv4 and IPv6 flags provided but cluster is not configured as dual-stack")
		}
	case ipv4Provided && !ipv6Provided:
		// Only IPv4 requested: cluster must support IPv4 (single-stack IPv4 or dual-stack).
		if !clusterHasIPv4 {
			return fmt.Errorf("only IPv4 flags provided but cluster is not configured for IPv4")
		}
	case !ipv4Provided && ipv6Provided:
		// Only IPv6 requested: cluster must support IPv6 (single-stack IPv6 or dual-stack).
		if !clusterHasIPv6 {
			return fmt.Errorf("only IPv6 flags provided but cluster is not configured for IPv6")
		}
	default:
	}

	return nil
}

// detectClusterIPFamilies inspects cluster info to determine whether the
// cluster is configured with IPv4, IPv6, or both (dual-stack).
func detectClusterIPFamilies(ips []string) (bool, bool) {
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

	clusterHasIPv4 := nodeIPv4 != ""
	clusterHasIPv6 := nodeIPv6 != ""

	return clusterHasIPv4, clusterHasIPv6
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

	if err := validateVLANID(vlanID); err != nil {
		return fmt.Errorf("invalid vlan-id: %w", err)
	}

	if err := validateDNSIPFamily(dnsIPFamily); err != nil {
		return fmt.Errorf("invalid dns-ip-family: %w", err)
	}

	return nil
}

func validateVLANID(vlanID int) error {
	if vlanID < 0 {
		return fmt.Errorf("vlan-id must be >= 0")
	}
	return nil
}

func validateDNSIPFamily(dnsIPFamily string) error {
	if dnsIPFamily != common.IPv4FamilyName && dnsIPFamily != common.IPv6FamilyName {
		return fmt.Errorf("dns-ip-family must be one of: %s|%s", common.IPv4FamilyName, common.IPv6FamilyName)
	}
	return nil
}
