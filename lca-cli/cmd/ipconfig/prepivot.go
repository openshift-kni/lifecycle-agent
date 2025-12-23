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
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	runtimeClient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	intOstree "github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	lcacli "github.com/openshift-kni/lifecycle-agent/lca-cli"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ipconfig"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmOstree "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/utils"
	"github.com/spf13/cobra"
)

var (
	ipPrePivotScheme = runtime.NewScheme()

	newStaterootName              string
	installInitMonitor            bool
	installIpConfigurationService bool
	ipv4Address                   string
	ipv4MachineNetwork            string
	ipv6Address                   string
	ipv6MachineNetwork            string
	desiredIPv4Gateway            string
	desiredIPv6Gateway            string
	currentGatewayV4              string
	currentGatewayV6              string
	ipv4DNS                       string
	ipv6DNS                       string
	vlanID                        int
	pullSecretRefName             string
)

const (
	newStaterootNameFlag              = "new-stateroot-name"
	installInitMonitorFlag            = "install-init-monitor"
	installIpConfigurationServiceFlag = "install-ip-configuration-service"
	ipv4AddressFlag                   = "ipv4-address"
	ipv4MachineNetworkFlag            = "ipv4-machine-network"
	ipv6AddressFlag                   = "ipv6-address"
	ipv6MachineNetworkFlag            = "ipv6-machine-network"
	ipv4GatewayFlag                   = "ipv4-gateway"
	ipv6GatewayFlag                   = "ipv6-gateway"
	currentIPv4GatewayFlag            = "current-ipv4-gateway"
	currentIPv6GatewayFlag            = "current-ipv6-gateway"
	ipv4DNSFlag                       = "ipv4-dns"
	ipv6DNSFlag                       = "ipv6-dns"
	vlanIDFlag                        = "vlan-id"
	pullSecretRefNameFlag             = "pull-secret-ref-name" //nolint:gosec // flag name, not credentials
	prePivotCmd                       = "pre-pivot"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(ipPrePivotScheme))

	ipConfigPrePivotCmd.Flags().StringVar(&newStaterootName, newStaterootNameFlag, "", "New stateroot name")
	ipConfigPrePivotCmd.Flags().BoolVar(&installInitMonitor, installInitMonitorFlag, false, "Install init monitor service in the new stateroot")
	ipConfigPrePivotCmd.Flags().BoolVar(&installIpConfigurationService, installIpConfigurationServiceFlag, false, "Install ip configuration service in the new stateroot")
	ipConfigPrePivotCmd.Flags().StringVar(&ipv4Address, ipv4AddressFlag, "", "Target IPv4 address")
	ipConfigPrePivotCmd.Flags().StringVar(&ipv4MachineNetwork, ipv4MachineNetworkFlag, "", "Target IPv4 machine network CIDR")
	ipConfigPrePivotCmd.Flags().StringVar(&ipv6Address, ipv6AddressFlag, "", "Target IPv6 address")
	ipConfigPrePivotCmd.Flags().StringVar(&ipv6MachineNetwork, ipv6MachineNetworkFlag, "", "Target IPv6 machine network CIDR")
	ipConfigPrePivotCmd.Flags().StringVar(&desiredIPv4Gateway, ipv4GatewayFlag, "", "Desired IPv4 default gateway")
	ipConfigPrePivotCmd.Flags().StringVar(&desiredIPv6Gateway, ipv6GatewayFlag, "", "Desired IPv6 default gateway")
	ipConfigPrePivotCmd.Flags().StringVar(&currentGatewayV4, currentIPv4GatewayFlag, "", "Current IPv4 default gateway (used to remove old default route when it differs from desired)")
	ipConfigPrePivotCmd.Flags().StringVar(&currentGatewayV6, currentIPv6GatewayFlag, "", "Current IPv6 default gateway (used to remove old default route when it differs from desired)")
	ipConfigPrePivotCmd.Flags().StringVar(&ipv4DNS, ipv4DNSFlag, "", "IPv4 DNS server")
	ipConfigPrePivotCmd.Flags().StringVar(&ipv6DNS, ipv6DNSFlag, "", "IPv6 DNS server")
	ipConfigPrePivotCmd.Flags().IntVar(&vlanID, vlanIDFlag, 0, "Optional VLAN ID to use on the br-ex uplink")
	ipConfigPrePivotCmd.Flags().StringVar(&pullSecretRefName, pullSecretRefNameFlag, "", "The name of the pull secret to use for the recert container tool")
	ipConfigPrePivotCmd.Flags().StringVar(&dnsIPFamily, dnsIPFamilyFlag, "", "IP family to filter out from DNS responses (ipv4|ipv6|none)")

}

var ipConfigPrePivotCmd = &cobra.Command{
	Use:   prePivotCmd,
	Short: "Execute IP configuration pre-pivot",
	Run: func(cmd *cobra.Command, args []string) {
		if err := runIPConfigPrePivot(); err != nil {
			pkgLog.Fatalf("Error executing ip-config pre-pivot: %v", err)
		}
	},
}

// runIPConfigPrePivot orchestrates the ip-config "pre-pivot" flow:
// - sets up executors and logs flags
// - gathers OSTree state for old/new stateroots
// - writes progress status, runs pre-pivot actions, optionally installs an init monitor
// - finalizes status and reboots into the new stateroot
func runIPConfigPrePivot() (retErr error) {
	opsInterface, hostCommandsExecutor, client, err := buildOpsAndK8sClient(ipPrePivotScheme)
	if err != nil {
		return err
	}

	rpmClient := rpmOstree.NewClient("lca-cli-ip-config-pre-pivot", hostCommandsExecutor)
	ostreeClient := intOstree.NewClient(hostCommandsExecutor, false)
	rbClient := reboot.NewIPCRebootClient(
		&logr.Logger{},
		hostCommandsExecutor,
		rpmClient,
		ostreeClient,
		opsInterface,
	)

	defer func() {
		if retErr == nil {
			if err := rbClient.RebootToNewStateRoot("ip-config pre-pivot"); err != nil {
				retErr = fmt.Errorf("failed to reboot to new stateroot: %w", err)
				return
			}
			return
		}

		retErr = fmt.Errorf("ip config pre-pivot failed: %w", retErr)
	}()

	if data, err := os.ReadFile(common.IPConfigPrePivotFlagsFile); err == nil && len(data) > 0 {
		var cfg common.IPConfigPrePivotConfig
		if jsonErr := json.Unmarshal(data, &cfg); jsonErr == nil {
			newStaterootName = cfg.NewStaterootName
			ipv4Address = cfg.IPv4Address
			ipv4MachineNetwork = cfg.IPv4MachineNetwork
			ipv6Address = cfg.IPv6Address
			ipv6MachineNetwork = cfg.IPv6MachineNetwork
			desiredIPv4Gateway = cfg.DesiredIPv4Gateway
			desiredIPv6Gateway = cfg.DesiredIPv6Gateway
			currentGatewayV4 = cfg.CurrentIPv4Gateway
			currentGatewayV6 = cfg.CurrentIPv6Gateway
			ipv4DNS = cfg.IPv4DNSServer
			ipv6DNS = cfg.IPv6DNSServer
			vlanID = cfg.VLANID
			pullSecretRefName = cfg.PullSecretRefName
			dnsIPFamily = cfg.DNSFilterOutFamily
			installInitMonitor = cfg.InstallInitMonitor
			installIpConfigurationService = cfg.InstallIPConfigurationService
		} else {
			pkgLog.Warnf("failed to unmarshal ip-config pre-pivot config: %v", jsonErr)
		}
	} else {
		pkgLog.Info("using command line flags")
	}

	pkgLog.WithFields(logrus.Fields{
		newStaterootNameFlag:              newStaterootName,
		installInitMonitorFlag:            installInitMonitor,
		installIpConfigurationServiceFlag: installIpConfigurationService,
		ipv4AddressFlag:                   ipv4Address,
		ipv4MachineNetworkFlag:            ipv4MachineNetwork,
		ipv6AddressFlag:                   ipv6Address,
		ipv6MachineNetworkFlag:            ipv6MachineNetwork,
		ipv4GatewayFlag:                   desiredIPv4Gateway,
		ipv6GatewayFlag:                   desiredIPv6Gateway,
		currentIPv4GatewayFlag:            currentGatewayV4,
		currentIPv6GatewayFlag:            currentGatewayV6,
		ipv4DNSFlag:                       ipv4DNS,
		ipv6DNSFlag:                       ipv6DNS,
		vlanIDFlag:                        vlanID,
		pullSecretRefNameFlag:             pullSecretRefName,
		dnsIPFamilyFlag:                   dnsIPFamily,
	}).Info("ip-config pre-pivot flags")

	ctx := context.Background()

	if err := validatePrePivotFlags(ctx, client); err != nil {
		return fmt.Errorf("pre-pivot flags validation failed: %w", err)
	}

	effectivePrimary, err := inferPrimaryStack()
	if err != nil {
		return fmt.Errorf("failed to infer primary IP stack: %w", err)
	}

	ipConfigs := buildIPConfigs(
		ipv4Address, ipv4MachineNetwork, desiredIPv4Gateway, currentGatewayV4, ipv4DNS,
		ipv6Address, ipv6MachineNetwork, desiredIPv6Gateway, currentGatewayV6, ipv6DNS,
		lo.FromPtr(effectivePrimary),
	)

	// Build ostree data only after we have the final new stateroot name.
	ostreeData, err := getOstreeData(newStaterootName, rpmClient, ostreeClient, pkgLog)
	if err != nil {
		return fmt.Errorf("failed to get ostree data: %w", err)
	}

	prePivotHandler := ipconfig.NewPrePivotHandler(
		pkgLog,
		opsInterface,
		ostreeData,
		ostreeClient,
		rpmClient,
		rbClient,
		client,
		ipConfigs,
		pullSecretRefName,
		vlanID,
		dnsIPFamily,
		common.LCAWorkspaceDir,
		common.MCDCurrentConfig,
		currentGatewayV4,
		currentGatewayV6,
	)

	if err := prePivotHandler.Run(ctx); err != nil {
		return fmt.Errorf("pre-pivot handler failed: %w", err)
	}

	if installInitMonitor {
		if err := installMonitorInitializationServiceInNewStateroot(
			opsInterface, ostreeData, pkgLog,
		); err != nil {
			return fmt.Errorf("failed to install monitor initialization service: %w", err)
		}
	}

	if installIpConfigurationService {
		if err := installIpConfigurationServiceInNewStateroot(ostreeData, opsInterface, pkgLog); err != nil {
			return fmt.Errorf("failed to install ip configuration service: %w", err)
		}

	}

	if err := copyLcaCliToNewStateroot(ostreeData, opsInterface, pkgLog); err != nil {
		return fmt.Errorf("failed to copy lca-cli binary to new stateroot: %w", err)
	}

	if err := removeIPCFileInNewStaterootIfExists(ostreeData, pkgLog); err != nil {
		return fmt.Errorf("failed to remove IPC file in new stateroot: %w", err)
	}

	return nil
}

// installMonitorInitializationServiceInNewStateroot installs and enables the IPC init monitor service
// within the new stateroot deployment.
func installMonitorInitializationServiceInNewStateroot(
	ops ops.Ops,
	ostreeData *ipconfig.OstreeData,
	logger *logrus.Logger,
) error {
	destinationFilePath := filepath.Join(ostreeData.NewStateroot.DeploymentDir, "etc/systemd/system", common.IPCInitMonitorService)
	logger.Infof("Creating service %s", common.IPCInitMonitorService)

	if err := os.MkdirAll(path.Dir(destinationFilePath), 0o755); err != nil {
		return fmt.Errorf("failed to create destination directory for %s: %w", common.IPCInitMonitorService, err)
	}
	if err := os.WriteFile(destinationFilePath, []byte(lcacli.LcaInitMonitorServiceFile), common.FileMode0600); err != nil {
		return fmt.Errorf("failed to write init monitor service file: %w", err)
	}

	logger.Infof("Enabling service %s", common.IPCInitMonitorService)
	if _, err := ops.SystemctlAction(
		"enable",
		"--root",
		ostreeData.NewStateroot.DeploymentDir,
		common.IPCInitMonitorService,
	); err != nil {
		return fmt.Errorf("failed enabling service %s: %w", common.IPCInitMonitorService, err)
	}

	initMonitorModeFile := filepath.Join(ostreeData.NewStateroot.Path, common.InitMonitorModeFile)
	if err := os.WriteFile(initMonitorModeFile, []byte("ipconfig"), common.FileMode0600); err != nil {
		return fmt.Errorf("failed to write init monitor mode file: %w", err)
	}

	return nil
}

// getOstreeData returns information about the current (old) and target (new)
// stateroots, including names, paths, deployment names, and deployment dirs.
func getOstreeData(
	newStaterootName string,
	rpmOstree rpmOstree.IClient,
	ostree intOstree.IClient,
	logger *logrus.Logger,
) (*ipconfig.OstreeData, error) {
	common.OstreeDeployPathPrefix = sysrootPath
	ostreeData := &ipconfig.OstreeData{}

	currentStaterootData, err := getCurrentStaterootData(rpmOstree, ostree)
	if err != nil {
		return nil, fmt.Errorf("failed to get current stateroot data: %w", err)
	}
	ostreeData.OldStateroot = currentStaterootData

	newStaterootData, err := getNewStaterootData(newStaterootName, ostree, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to get new stateroot data: %w", err)
	}
	ostreeData.NewStateroot = newStaterootData

	return ostreeData, nil
}

// getCurrentStaterootData queries the system for the current stateroot details.
func getCurrentStaterootData(rpmOstree rpmOstree.IClient, ostree intOstree.IClient) (*ipconfig.StaterootData, error) {
	staterootData := &ipconfig.StaterootData{}

	currentStaterootName, err := rpmOstree.GetCurrentStaterootName()
	if err != nil {
		return nil, fmt.Errorf("failed to get current stateroot name: %w", err)
	}
	staterootData.Name = currentStaterootName

	staterootData.Path = common.GetStaterootPath(currentStaterootName)

	oldDeploymentName, err := ostree.GetDeployment(currentStaterootName)
	if err != nil {
		return nil, fmt.Errorf("failed to get deployment for %s: %w", currentStaterootName, err)
	}
	staterootData.DeploymentName = oldDeploymentName

	oldDeploymentDir, err := ostree.GetDeploymentDir(currentStaterootName)
	if err != nil {
		return nil, fmt.Errorf("failed to get deployment dir for %s: %w", currentStaterootName, err)
	}
	staterootData.DeploymentDir = oldDeploymentDir

	return staterootData, nil
}

// getNewStaterootData resolves the metadata for the target new stateroot.
func getNewStaterootData(
	newStaterootName string,
	ostree intOstree.IClient,
	logger *logrus.Logger,
) (*ipconfig.StaterootData, error) {
	staterootData := &ipconfig.StaterootData{
		Name: newStaterootName,
		Path: common.GetStaterootPath(newStaterootName),
	}

	newDeploymentName, err := ostree.GetDeployment(newStaterootName)
	if err != nil {
		return nil, fmt.Errorf("failed to get deployment name for %s: %w", newStaterootName, err)
	}
	if newDeploymentName != "" {
		staterootData.DeploymentName = newDeploymentName
		logger.Infof("Found deployment for new stateroot before creation %s: %s", newStaterootName, newDeploymentName)
	} else {
		return staterootData, nil
	}

	newDeploymentDir, err := ostree.GetDeploymentDir(newStaterootName)
	if err != nil {
		return nil, fmt.Errorf("failed to get deployment dir for %s: %w", newStaterootName, err)
	}
	staterootData.DeploymentDir = newDeploymentDir

	return staterootData, nil
}

// validatePrePivotFlags validates all pre-pivot cmd flags including IP configs, VLAN and DNS family.
func validatePrePivotFlags(ctx context.Context, client runtimeClient.Client) error {
	if newStaterootName == "" {
		return fmt.Errorf("new-stateroot-name is required")
	}

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
		case common.DNSFamilyNone:
		default:
			return fmt.Errorf(
				"dns-ip-family must be one of: %s|%s|%s",
				common.IPv4FamilyName,
				common.IPv6FamilyName,
				common.DNSFamilyNone,
			)
		}
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
	ips, err := utils.GetNodeInternalIPs(ctx, client)
	if err != nil {
		return fmt.Errorf("failed to contact cluster API: %w", err)
	}

	clusterHasIPv4, clusterHasIPv6 := common.DetectClusterIPFamilies(ips)

	ipv4Provided := ipv4Address != "" || ipv4MachineNetwork != ""
	ipv6Provided := ipv6Address != "" || ipv6MachineNetwork != ""

	if ipv4Provided && !clusterHasIPv4 {
		return fmt.Errorf("specified IPv4, but the cluster does not have IPv4")
	}

	if ipv6Provided && !clusterHasIPv6 {
		return fmt.Errorf("specified IPv6, but the cluster does not have IPv6")
	}

	if dnsIPFamily != "" && dnsIPFamily != common.DNSFamilyNone {
		if !(clusterHasIPv4 && clusterHasIPv6) {
			return fmt.Errorf("dns-ip-family is supported only on dual-stack clusters")
		}
	}

	return nil
}

// validateIPFamilyConfigArgs validates IPv4/IPv6 arguments for consistency and correctness.
// It enforces that each family is either fully specified or omitted, and that
// addresses and gateways belong to their respective machine networks.
func validateIPFamilyConfigArgs() error {
	ipv4Any := ipv4Address != "" || ipv4MachineNetwork != "" || desiredIPv4Gateway != "" || currentGatewayV4 != "" || ipv4DNS != ""
	ipv4Core := ipv4Address != "" && ipv4MachineNetwork != ""
	ipv4None := !ipv4Any
	ipv6Any := ipv6Address != "" || ipv6MachineNetwork != "" || desiredIPv6Gateway != "" || currentGatewayV6 != "" || ipv6DNS != ""
	ipv6Core := ipv6Address != "" && ipv6MachineNetwork != ""
	ipv6None := !ipv6Any

	if (ipv4Any && !ipv4Core) || (ipv6Any && !ipv6Core) {
		return fmt.Errorf("both address and machine-network must be provided together for each IP family")
	}

	if ipv4None && ipv6None {
		return fmt.Errorf("at least one of IPv4 or IPv6 must be provided")
	}

	if ipv4Core {
		if err := utils.ValidateIPFamilyConfig(
			common.IPv4FamilyName,
			ipv4Address,
			ipv4MachineNetwork,
			desiredIPv4Gateway,
			ipv4DNS,
		); err != nil {
			return fmt.Errorf("invalid IPv4 config: %w", err)
		}
	}

	if ipv6Core {
		if err := utils.ValidateIPFamilyConfig(
			common.IPv6FamilyName,
			ipv6Address,
			ipv6MachineNetwork,
			desiredIPv6Gateway,
			ipv6DNS,
		); err != nil {
			return fmt.Errorf("invalid IPv6 config: %w", err)
		}
	}

	return nil
}

// InferPrimaryStack determines the primary IP family by reading the node's current primary IP
// and returns a pointer to "IPv4" or "IPv6".
func inferPrimaryStack() (*string, error) {
	data, err := os.ReadFile(PrimaryIPPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read primary IP: %w", err)
	}

	primaryIP := strings.TrimSpace(string(data))
	if primaryIP == "" {
		return nil, fmt.Errorf("primary IP not found")
	}

	ip := net.ParseIP(primaryIP)
	if ip == nil {
		return nil, fmt.Errorf("invalid primary IP: %s", primaryIP)
	}

	if ip.To4() != nil {
		return lo.ToPtr(common.IPv4FamilyName), nil
	}

	if ip.To16() != nil {
		return lo.ToPtr(common.IPv6FamilyName), nil
	}

	return nil, fmt.Errorf("invalid primary IP: %s", primaryIP)
}

// BuildIPConfigs creates the ordered slice of NetworkIPConfig with primary first.
func buildIPConfigs(
	ipv4Addr, ipv4Net, ipv4Gw, ipv4CurrentGw, ipv4DNS string,
	ipv6Addr, ipv6Net, ipv6Gw, ipv6CurrentGw, ipv6DNS string,
	primary string,
) []*ipconfig.NetworkIPConfig {
	var ipv4Config *ipconfig.NetworkIPConfig
	if ipv4Addr != "" && ipv4Net != "" {
		ipv4Config = &ipconfig.NetworkIPConfig{
			IP:             ipv4Addr,
			MachineNetwork: ipv4Net,
			DesiredGateway: ipv4Gw,
			CurrentGateway: ipv4CurrentGw,
			DNSServer:      ipv4DNS,
		}
	}

	var ipv6Config *ipconfig.NetworkIPConfig
	if ipv6Addr != "" && ipv6Net != "" {
		ipv6Config = &ipconfig.NetworkIPConfig{
			IP:             ipv6Addr,
			MachineNetwork: ipv6Net,
			DesiredGateway: ipv6Gw,
			CurrentGateway: ipv6CurrentGw,
			DNSServer:      ipv6DNS,
		}
	}

	ipConfigs := []*ipconfig.NetworkIPConfig{}
	switch primary {
	case common.IPv4FamilyName:
		if ipv4Config != nil {
			ipConfigs = append(ipConfigs, ipv4Config)
		}

		if ipv6Config != nil {
			ipConfigs = append(ipConfigs, ipv6Config)
		}
	case common.IPv6FamilyName:
		if ipv6Config != nil {
			ipConfigs = append(ipConfigs, ipv6Config)
		}

		if ipv4Config != nil {
			ipConfigs = append(ipConfigs, ipv4Config)
		}
	}

	return ipConfigs
}

func installIpConfigurationServiceInNewStateroot(
	ostreeData *ipconfig.OstreeData,
	ops ops.Ops,
	logger *logrus.Logger,
) error {
	destinationFilePath := filepath.Join(
		ostreeData.NewStateroot.DeploymentDir,
		"etc/systemd/system",
		common.IPConfigurationService,
	)
	logger.Infof("Creating service %s", common.IPConfigurationService)

	if err := os.MkdirAll(path.Dir(destinationFilePath), 0o755); err != nil {
		return fmt.Errorf("failed to create destination directory for %s: %w", common.IPConfigurationService, err)
	}

	if err := os.WriteFile(destinationFilePath, []byte(lcacli.IpConfigurationServiceFile), common.FileMode0600); err != nil {
		return fmt.Errorf("failed to write ip configuration service file: %w", err)
	}

	logger.Infof("Enabling service %s", common.IPConfigurationService)
	if _, err := ops.SystemctlAction(
		"enable",
		"--root",
		ostreeData.NewStateroot.DeploymentDir,
		common.IPConfigurationService,
	); err != nil {
		return fmt.Errorf("failed enabling service %s: %w", common.IPConfigurationService, err)
	}

	return nil
}

func copyLcaCliToNewStateroot(
	ostreeData *ipconfig.OstreeData,
	ops ops.Ops,
	logger *logrus.Logger,
) error {
	destinationFilePath := filepath.Join(ostreeData.NewStateroot.Path, common.LcaCliBinaryHostPath)
	logger.Infof("Copying lca-cli binary to %s", destinationFilePath)
	if err := ops.CopyFile(common.LcaCliBinaryHostPath, destinationFilePath, 0o777); err != nil {
		return fmt.Errorf("failed to copy lca-cli binary: %w", err)
	}

	return nil
}

func removeIPCFileInNewStaterootIfExists(
	ostreeData *ipconfig.OstreeData,
	logger *logrus.Logger,
) error {
	filePath := filepath.Join(ostreeData.NewStateroot.Path, common.IPCFilePath)
	if err := utils.RemoveListOfFiles(logger, []string{filePath}); err != nil {
		return fmt.Errorf("failed to remove IPC file in new stateroot: %w", err)
	}

	return nil
}
