package ipconfig

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	runtimeClient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	intOstree "github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ipconfig"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmOstree "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/utils"
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
	pullSecretRefNameFlag  = "pull-secret-ref-name"
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
		if err := runIPConfigChange(); err != nil {
			pkgLog.Fatalf("Error executing ip-config run: %v", err)
		}
	},
}

// runIPConfigChange coordinates the ip-config "run" flow:
// - writes initial status, loads flags/config, validates inputs
// - infers the primary IP family and builds handler dependencies
// - applies network changes, finalizes status, and reboots
func runIPConfigChange() error {
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

	if err := validateRunFlags(); err != nil {
		return err
	}

	effectivePrimary, err := inferPrimaryStack()
	if err != nil {
		return err
	}

	ipConfigs := buildIPConfigs(
		ipv4Address, ipv4MachineNetwork, ipv4Gateway, ipv4DNS,
		ipv6Address, ipv6MachineNetwork, ipv6Gateway, ipv6DNS,
		lo.FromPtr(effectivePrimary),
	)

	if recertImage == "" {
		recertImage = common.DefaultRecertImage
	}

	var hostCommandsExecutor ops.Execute
	if _, err := os.Stat(common.Host); err == nil {
		hostCommandsExecutor = ops.NewChrootExecutor(pkgLog, true, common.Host)
	} else {
		hostCommandsExecutor = ops.NewRegularExecutor(pkgLog, true)
	}

	opsInterface := ops.NewOps(pkgLog, hostCommandsExecutor)

	k8sConfig, err := clientcmd.BuildConfigFromFlags("", common.PathOutsideChroot(common.KubeconfigFile))
	if err != nil {
		return fmt.Errorf("failed to create k8s config: %w", err)
	}

	client, err := runtimeClient.New(k8sConfig, runtimeClient.Options{Scheme: ipConfigScheme})
	if err != nil {
		return fmt.Errorf("failed to create runtime client: %w", err)
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

	if err := ipConfigHandler.Run(); err != nil {
		internalErr := common.FinalizeIPConfigStatus(
			common.IPConfigRunStatusFile,
			common.IPConfigPhaseFailed,
			fmt.Sprintf("ip-config run failed: %v", err),
		)
		if internalErr != nil {
			return fmt.Errorf("failed to finalize IP config run status: %w", internalErr)
		}

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

// validateIPFamilyConfigArgs validates IPv4/IPv6 arguments for consistency and correctness.
// It enforces that each family is either fully specified or omitted, and that
// addresses and gateways belong to their respective machine networks.
func validateIPFamilyConfigArgs(
	ipv4Addr,
	ipv4Net,
	ipv6Addr,
	ipv6Net,
	ipv4Gw,
	ipv6Gw,
	ipv4DNSStr,
	ipv6DNSStr string,
) error {
	ipv4All := ipv4Addr != "" && ipv4Net != "" && ipv4Gw != "" && ipv4DNSStr != ""
	ipv4None := ipv4Addr == "" && ipv4Net == "" && ipv4Gw == "" && ipv4DNSStr == ""
	ipv6All := ipv6Addr != "" && ipv6Net != "" && ipv6Gw != "" && ipv6DNSStr != ""
	ipv6None := ipv6Addr == "" && ipv6Net == "" && ipv6Gw == "" && ipv6DNSStr == ""

	if (!ipv4All && !ipv4None) || (!ipv6All && !ipv6None) {
		return fmt.Errorf("both address and machine-network must be provided together for each IP family")
	}

	if ipv4None && ipv6None {
		return fmt.Errorf("at least one of IPv4 or IPv6 must be provided")
	}

	if ipv4All {
		if err := validateIPFamilyConfig(
			common.IPv4FamilyName,
			ipv4Addr,
			ipv4Net,
			ipv4Gw,
			ipv4DNSStr,
		); err != nil {
			return fmt.Errorf("invalid IPv4 config: %w", err)
		}
	}

	if ipv6All {
		if err := validateIPFamilyConfig(
			common.IPv6FamilyName,
			ipv6Addr,
			ipv6Net,
			ipv6Gw,
			ipv6DNSStr,
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

// inferPrimaryStack determines the primary IP family by reading the node's
// current primary IP and returns "IPv4" or "IPv6".
func inferPrimaryStack() (*string, error) {
	data, err := os.ReadFile(utils.PrimaryIPPath)
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

// buildIPConfigs creates the ordered slice of NetworkIPConfig with primary first.
func buildIPConfigs(
	ipv4Addr, ipv4Net, ipv4Gw, ipv4DNS string,
	ipv6Addr, ipv6Net, ipv6Gw, ipv6DNS string,
	primary string,
) []*ipconfig.NetworkIPConfig {
	var ipv4Config *ipconfig.NetworkIPConfig
	if ipv4Addr != "" && ipv4Net != "" {
		ipv4Config = &ipconfig.NetworkIPConfig{IP: ipv4Addr, MachineNetwork: ipv4Net, Gateway: ipv4Gw, DNSServer: ipv4DNS}
	}

	var ipv6Config *ipconfig.NetworkIPConfig
	if ipv6Addr != "" && ipv6Net != "" {
		ipv6Config = &ipconfig.NetworkIPConfig{IP: ipv6Addr, MachineNetwork: ipv6Net, Gateway: ipv6Gw, DNSServer: ipv6DNS}
	}

	ipConfigs := []*ipconfig.NetworkIPConfig{}
	if primary == common.IPv4FamilyName && ipv4Config != nil {
		ipConfigs = append(ipConfigs, ipv4Config)
		if ipv6Config != nil {
			ipConfigs = append(ipConfigs, ipv6Config)
		}
	} else if primary == common.IPv6FamilyName && ipv6Config != nil {
		ipConfigs = append(ipConfigs, ipv6Config)
		if ipv4Config != nil {
			ipConfigs = append(ipConfigs, ipv4Config)
		}
	}

	return ipConfigs
}

// validateRunFlags validates all run cmd flags including IP configs, VLAN and DNS family.
func validateRunFlags() error {
	if err := validateIPFamilyConfigArgs(
		ipv4Address, ipv4MachineNetwork, ipv6Address, ipv6MachineNetwork,
		ipv4Gateway, ipv6Gateway, ipv4DNS, ipv6DNS,
	); err != nil {
		return fmt.Errorf("invalid IP config arguments: %w", err)
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

// validateIPFamilyConfig performs family-specific validation for addr, CIDR, gateway and DNS.
func validateIPFamilyConfig(
	family string,
	addr string,
	networkCIDR string,
	gateway string,
	dnsServer string,
) error {
	ip := net.ParseIP(addr)
	if ip == nil {
		return fmt.Errorf("invalid %s address: %s", strings.ToUpper(family), addr)
	}
	isIPv4 := family == common.IPv4FamilyName
	if isIPv4 && ip.To4() == nil {
		return fmt.Errorf("invalid %s address: %s", strings.ToUpper(family), addr)
	}
	if !isIPv4 && ip.To4() != nil {
		return fmt.Errorf("invalid %s address: %s", strings.ToUpper(family), addr)
	}

	_, ipNet, err := net.ParseCIDR(networkCIDR)
	if err != nil {
		return fmt.Errorf("invalid %s machine network CIDR: %s", strings.ToLower(family), networkCIDR)
	}
	if isIPv4 && ipNet.IP.To4() == nil {
		return fmt.Errorf("%s-machine-network must be an %s CIDR: %s", family, strings.ToUpper(family), networkCIDR)
	}
	if !isIPv4 && ipNet.IP.To4() != nil {
		return fmt.Errorf("%s-machine-network must be an %s CIDR: %s", family, strings.ToUpper(family), networkCIDR)
	}
	if !ipNet.Contains(ip) {
		return fmt.Errorf("%s address %s is not within machine network %s", strings.ToUpper(family), addr, networkCIDR)
	}

	if gateway != "" {
		gw := net.ParseIP(gateway)
		if gw == nil {
			return fmt.Errorf("invalid %s gateway: %s", strings.ToUpper(family), gateway)
		}
		if isIPv4 && gw.To4() == nil {
			return fmt.Errorf("invalid %s gateway: %s", strings.ToUpper(family), gateway)
		}
		if !isIPv4 && gw.To4() != nil {
			return fmt.Errorf("invalid %s gateway: %s", strings.ToUpper(family), gateway)
		}
		if !ipNet.Contains(gw) {
			return fmt.Errorf("%s gateway %s is not within machine network %s", strings.ToUpper(family), gateway, networkCIDR)
		}
	}

	if dnsServer != "" {
		dns := net.ParseIP(dnsServer)
		if dns == nil {
			return fmt.Errorf("invalid %s DNS server: %s", strings.ToUpper(family), dnsServer)
		}
		if isIPv4 && dns.To4() == nil {
			return fmt.Errorf("invalid %s DNS server: %s", strings.ToUpper(family), dnsServer)
		}
		if !isIPv4 && dns.To4() != nil {
			return fmt.Errorf("invalid %s DNS server: %s", strings.ToUpper(family), dnsServer)
		}
	}

	return nil
}

// validateIPv4Config validates IPv4 address, CIDR, gateway and DNS inputs.
func validateIPv4Config(ipv4Addr, ipv4Net, ipv4Gw, ipv4DNSStr string) error {
	return validateIPFamilyConfig(common.IPv4FamilyName, ipv4Addr, ipv4Net, ipv4Gw, ipv4DNSStr)
}

// validateIPv6Config validates IPv6 address, CIDR, gateway and DNS inputs.
func validateIPv6Config(ipv6Addr, ipv6Net, ipv6Gw, ipv6DNSStr string) error {
	return validateIPFamilyConfig(common.IPv6FamilyName, ipv6Addr, ipv6Net, ipv6Gw, ipv6DNSStr)
}
