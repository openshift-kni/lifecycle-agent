package ipconfig

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	ocp_config_v1 "github.com/openshift/api/config/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/recert"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"github.com/openshift-kni/lifecycle-agent/utils"
	machineconfigv1 "github.com/openshift/api/machineconfiguration/v1"
)

type RecertClusterData struct {
	Proxy                *ProxyConfig
	InstallConfig        string
	IngressCertificateCN string
	CurrentNodeIPs       []string
	CryptoDir            string
	PullSecretFile       string
}

// NetworkIPConfig is a minimal representation of an IP and its machine network.
type NetworkIPConfig struct {
	IP             string
	MachineNetwork string
	Gateway        string
	DNSServer      string
}

// IPConfigHandler handles the IP change process.
type IPConfigHandler struct {
	log               *logrus.Logger
	ops               ops.Ops
	executor          ops.Execute
	recertImage       string
	IPConfigs         []*NetworkIPConfig
	runtimeClient     runtimeclient.Client
	PullSecretRefName string
	VLANID            int
	DNSIPFamily       string
}

// NewIPConfig creates a new IPConfigHandler instance.
func NewIPConfig(
	log *logrus.Logger,
	ops ops.Ops,
	executor ops.Execute,
	runtimeClient runtimeclient.Client,
	recertImage string,
	ipConfigs []*NetworkIPConfig,
	pullSecretRefName string,
	vlanID int,
	dnsIPFamily string,
) *IPConfigHandler {
	return &IPConfigHandler{
		log:               log,
		ops:               ops,
		executor:          executor,
		recertImage:       recertImage,
		runtimeClient:     runtimeClient,
		IPConfigs:         ipConfigs,
		PullSecretRefName: pullSecretRefName,
		VLANID:            vlanID,
		DNSIPFamily:       dnsIPFamily,
	}
}

// ProxyConfig represents proxy configuration used during recert, including
// both spec values and effective status.
type ProxyConfig struct {
	HTTPProxy        string
	HTTPSProxy       string
	NoProxy          string
	StatusHTTPProxy  string
	StatusHTTPSProxy string
	StatusNoProxy    string
}

// Run executes the full IP configuration change workflow, including
// generating machine configuration, adjusting DNS overrides, stopping
// cluster services, running recert, and re-enabling services.
func (i *IPConfigHandler) Run() error {
	i.log.Info("IP config run started")

	ctx := context.Background()

	// Requires running cluster - start

	i.log.Info("Preparing recert cluster data")
	prepareRecertClusterData, err := i.prepareRecertClusterData(ctx)
	if err != nil {
		return fmt.Errorf("failed to prepare recert cluster data: %w", err)
	}

	i.log.Info("Creating network configuration")
	if err := i.CreateNetworkConfiguration(ctx); err != nil {
		return fmt.Errorf("failed to create network configuration: %w", err)
	}

	i.log.Info("Configuring dnsmasq override")
	if err := i.configureDNSMasq(ctx); err != nil {
		return fmt.Errorf("failed to configure dnsmasq override: %w", err)
	}

	// Requires running cluster - end

	i.log.Info("Stopping cluster services")
	if err := i.ops.StopClusterServices(); err != nil {
		return fmt.Errorf("failed to stop cluster services: %w", err)
	}

	i.log.Info("Running recert flow")
	if err := i.runRecert(prepareRecertClusterData); err != nil {
		return fmt.Errorf("failed to run recert flow: %w", err)
	}

	i.log.Info("Ensuring nodeip rerun service")
	if err := i.ensureNodeIPRerunService(i.IPConfigs[0].MachineNetwork); err != nil {
		return fmt.Errorf("failed to ensure nodeip rerun service: %w", err)
	}

	i.log.Info("Cleaning up nmstate residual state files")
	if err := i.cleanupNMStateAppliedFiles(); err != nil {
		return fmt.Errorf("failed to cleanup nmstate residual files: %w", err)
	}

	i.log.Info("Removing stale files for regeneration")
	if err := i.removeStaleFilesForRegeneration(); err != nil {
		return fmt.Errorf("failed to remove stale files for regeneration: %w", err)
	}

	i.log.Info("Enabling cluster services")
	if err := i.ops.EnableClusterServices(""); err != nil {
		return fmt.Errorf("failed to enable cluster services: %w", err)
	}

	i.log.Info("IP config run completed successfully")

	return nil
}

// prepareRecertClusterData collects cluster data and artifacts required by the
// recert tool (install-config, crypto, ingress CN, node IPs, proxy, auth).
func (i *IPConfigHandler) prepareRecertClusterData(
	ctx context.Context,
) (*RecertClusterData, error) {
	proxy, err := i.prepareProxyConfigForRecert(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to compute proxy configuration: %w", err)
	}
	if proxy != nil {
		i.log.Info("Proxy is set")
	}

	cryptoDir := path.Join(common.LCAWorkspaceDir, common.KubeconfigCryptoDir)
	if err := i.createCryptoDir(cryptoDir); err != nil {
		return nil, fmt.Errorf("failed to create crypto directory: %w", err)
	}

	if err := i.collectKubeConfigCrypto(ctx, cryptoDir); err != nil {
		return nil, fmt.Errorf("failed to collect kubeconfig crypto: %w", err)
	}

	ingressCertificateCN, err := utils.GetIngressCertificateCN(ctx, i.runtimeClient)
	if err != nil {
		return nil, fmt.Errorf("failed to get ingress certificate CN: %w", err)
	}
	i.log.Info("Found ingress certificate CN")

	installConfig, err := utils.GetInstallConfig(ctx, i.runtimeClient)
	if err != nil {
		return nil, fmt.Errorf("failed to get install config: %w", err)
	}
	i.log.Info("Found install config")

	currentNodeIPs, err := utils.GetNodeInternalIPs(ctx, i.runtimeClient)
	if err != nil {
		return nil, fmt.Errorf("failed to get current node internal IPs: %w", err)
	}

	var pullSecretFile = common.ImageRegistryAuthFile
	if i.PullSecretRefName != "" {
		authPath, err := materializeAuthFileFromPullSecretRef(ctx, i.runtimeClient, i.PullSecretRefName)
		if err != nil {
			return nil, fmt.Errorf("failed to materialize pull secret file: %w", err)
		}
		defer os.Remove(common.PathOutsideChroot(authPath))
		pullSecretFile = authPath
	}

	return &RecertClusterData{
		Proxy:                proxy,
		InstallConfig:        installConfig,
		IngressCertificateCN: ingressCertificateCN,
		CryptoDir:            cryptoDir,
		CurrentNodeIPs:       currentNodeIPs,
		PullSecretFile:       pullSecretFile,
	}, nil
}

// prepareProxyConfigForRecert reads the cluster Proxy CR, and if proxy is configured,
// calculates the effective NoProxy by combining:
// - localhost defaults
// - new machine networks provided to ip-config
// - user-provided spec noProxy
// It then sets both spec and status proxy on the handler to be passed to recert.
func (i *IPConfigHandler) prepareProxyConfigForRecert(ctx context.Context) (*ProxyConfig, error) {
	proxy := &ocp_config_v1.Proxy{}
	if err := i.runtimeClient.Get(ctx, types.NamespacedName{Name: common.OpenshiftProxyCRName}, proxy); err != nil {
		return nil, fmt.Errorf("failed to get proxy: %w", err)
	}

	if proxy.Spec.HTTPProxy == "" && proxy.Spec.HTTPSProxy == "" && proxy.Spec.NoProxy == "" {
		return nil, nil
	}

	clusterName, err := utils.GetClusterName(ctx, i.runtimeClient)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster name: %w", err)
	}
	baseDomain, err := utils.GetClusterBaseDomain(ctx, i.runtimeClient)
	if err != nil {
		return nil, fmt.Errorf("failed to get base domain: %w", err)
	}

	set := sets.NewString(
		"127.0.0.1",
		"localhost",
		".svc",
		".cluster.local",
		fmt.Sprintf("api-int.%s.%s", clusterName, baseDomain),
	)

	for _, cfg := range i.IPConfigs {
		if cfg != nil && cfg.MachineNetwork != "" {
			set.Insert(cfg.MachineNetwork)
		}
	}

	if proxy.Spec.NoProxy != "" {
		for _, userValue := range strings.Split(proxy.Spec.NoProxy, ",") {
			userValue = strings.TrimSpace(userValue)
			if userValue != "" {
				set.Insert(userValue)
			}
		}
	}

	finalNoProxy := strings.Join(set.List(), ",")

	return &ProxyConfig{
		HTTPProxy:        proxy.Spec.HTTPProxy,
		HTTPSProxy:       proxy.Spec.HTTPSProxy,
		NoProxy:          finalNoProxy,
		StatusHTTPProxy:  proxy.Status.HTTPProxy,
		StatusHTTPSProxy: proxy.Status.HTTPSProxy,
		StatusNoProxy:    finalNoProxy,
	}, nil
}

// runRecert writes a recert configuration and executes the full recert flow
// to regenerate certificates and manifests for the new IP configuration.
func (i *IPConfigHandler) runRecert(clusterData *RecertClusterData) error {
	i.log.Info("Creating recert configuration file")

	oldIPs := make([]string, len(i.IPConfigs))
	newIPs := make([]string, len(i.IPConfigs))
	newMachineNetworks := make([]string, len(i.IPConfigs))

	for i, cfg := range i.IPConfigs {
		oldIP, matchErr := selectIPOfSameFamily(cfg.IP, clusterData.CurrentNodeIPs)
		if matchErr != nil {
			return fmt.Errorf("failed to select old IP to match new IP %s: %w", cfg.IP, matchErr)
		}
		oldIPs[i] = oldIP
		newIPs[i] = cfg.IP
		newMachineNetworks[i] = cfg.MachineNetwork
	}

	if clusterData.Proxy == nil {
		clusterData.Proxy = &ProxyConfig{}
	}

	if err := recert.CreateRecertConfigFileForIPConfig(
		oldIPs,
		newIPs,
		newMachineNetworks,
		clusterData.InstallConfig,
		clusterData.CryptoDir,
		clusterData.IngressCertificateCN,
		common.LCAWorkspaceDir,
		clusterData.Proxy.HTTPProxy,
		clusterData.Proxy.HTTPSProxy,
		clusterData.Proxy.NoProxy,
		clusterData.Proxy.StatusHTTPProxy,
		clusterData.Proxy.StatusHTTPSProxy,
		clusterData.Proxy.StatusNoProxy,
	); err != nil {
		return fmt.Errorf("failed to create recert configuration file: %w", err)
	}

	i.log.Info("Starting recert full flow")

	err := i.ops.RecertFullFlow(
		i.recertImage,
		clusterData.PullSecretFile,
		path.Join(common.LCAWorkspaceDir, recert.RecertConfigFile),
		nil,
		nil,
		"-v", fmt.Sprintf("%s:%s", common.LCAWorkspaceDir, common.LCAWorkspaceDir),
	)
	if err != nil {
		return fmt.Errorf("failed recert full flow: %w", err)
	}

	return nil
}

// selectIPOfSameFamily picks the first candidate IP that matches the family (IPv4/IPv6) of newIP
func selectIPOfSameFamily(newIP string, candidates []string) (string, error) {
	family := ipFamilyOfString(newIP)
	for _, c := range candidates {
		if ipFamilyOfString(c) == family {
			return c, nil
		}
	}
	return "", fmt.Errorf("no %s NodeInternalIP found", family)
}

// ipFamilyOfString returns "IPv6" if the IP contains a colon, otherwise "IPv4"
func ipFamilyOfString(ip string) string {
	if strings.Contains(ip, ":") {
		return common.IPv6FamilyName
	}
	return common.IPv4FamilyName
}

// detectBrExNetworkInterface discovers the physical interface connected to
// the external OVS bridge (br-ex), excluding patch ports.
func (i *IPConfigHandler) detectBrExNetworkInterface() (string, error) {
	i.log.Infof("Detecting %s network interface", BridgeExternalName)

	if output, err := i.executor.Execute("ovs-vsctl", "list-ports", BridgeExternalName); err == nil {
		ports := strings.Fields(string(output))
		for _, port := range ports {
			// We want the actual port used by the node, not the patch port
			// Example output:
			// sudo ovs-vsctl list-ports br-ex
			// ens3
			// patch-br-ex_test-infra-cluster-06d0a16b-master-0-to-br-int
			if !strings.Contains(port, BridgeExternalName) {
				i.log.Infof("Found interface via ovs-vsctl: %s", port)
				return port, nil
			}
		}
	} else {
		i.log.Debugf("failed to query ovs-vsctl list-ports %s: %v", BridgeExternalName, err)
	}

	return "", fmt.Errorf("no connected network interface found")
}

// ensureNodeIPRerunService installs and enables a one-shot systemd service
// to re-run nodeip detection after reboot, using a hint derived from the new network.
func (i *IPConfigHandler) ensureNodeIPRerunService(newMachineNetwork string) error {
	i.log.Infof("Installing one-shot nodeip rerun service at %s", utils.NodeipRerunUnitPath)

	baseIP := strings.Split(newMachineNetwork, "/")[0]
	hintSed := strings.ReplaceAll(baseIP, "/", "\\/")

	templateData := &utils.NodeIPRerunServiceTemplateData{
		BaseIP:  baseIP,
		HintSed: hintSed,
	}

	unitContent, err := utils.GenerateNodeIPRerunService(templateData)
	if err != nil {
		return fmt.Errorf("failed to generate nodeip rerun service content: %w", err)
	}

	if err := os.WriteFile(common.PathOutsideChroot(utils.NodeipRerunUnitPath), []byte(unitContent), 0644); err != nil {
		return fmt.Errorf("failed to write nodeip rerun service file: %w", err)
	}

	if _, err := i.executor.Execute("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("failed to reload systemd daemon: %w", err)
	}

	if _, err := i.executor.Execute("systemctl", "enable", NodeipRerunUnitName); err != nil {
		return fmt.Errorf("failed to enable %s: %w", NodeipRerunUnitName, err)
	}

	i.log.Info("Nodeip rerun service configured successfully")
	return nil
}

// configureDNSMasq writes a dnsmasq override to prefer the selected IP
// (optionally constrained to a specific IP family) and updates MCO filter.
func (i *IPConfigHandler) configureDNSMasq(ctx context.Context) error {
	overrideIP := ""
	for _, cfg := range i.IPConfigs {
		if cfg != nil && cfg.IP != "" && ipFamilyOfString(cfg.IP) == i.DNSIPFamily {
			overrideIP = cfg.IP
			break
		}
	}

	if overrideIP == "" && len(i.IPConfigs) > 0 && i.IPConfigs[0] != nil {
		overrideIP = i.IPConfigs[0].IP
	}

	if overrideIP == "" {
		return fmt.Errorf("no IP available to configure dnsmasq overrides")
	}
	i.log.Infof("Setting new dnsmasq configuration for %s ", overrideIP)

	config := []string{
		fmt.Sprintf("%s=%s", common.DnsmasqOverrideEnvKey, overrideIP),
	}

	if err := os.WriteFile(
		common.PathOutsideChroot(common.DnsmasqOverrides),
		[]byte(strings.Join(config, "\n")),
		common.FileMode0600,
	); err != nil {
		return fmt.Errorf("failed to set dnsmasq overrides: %w", err)
	}

	if i.DNSIPFamily != "" {
		if err := common.SetDNSMasqFilterInMachineConfig(ctx, i.runtimeClient, i.DNSIPFamily); err != nil {
			return fmt.Errorf("failed to update dnsmasq filter in machine config: %w", err)
		}
	}

	i.log.Infof("DNSMasq override configured with IP: %s", overrideIP)

	return nil
}

// cleanupNMStateAppliedFiles removes residual nmstate files that may interfere
// with subsequent network reconfiguration.
func (i *IPConfigHandler) cleanupNMStateAppliedFiles() error {
	i.log.Info("Cleaning up nmstate residual state files")

	filesToRemove := []string{
		common.PathOutsideChroot("/etc/nmstate/openshift/applied"),
		common.PathOutsideChroot("/etc/nmstate/cluster.yml"),
		common.PathOutsideChroot("/etc/nmstate/cluster.applied"),
	}

	for _, file := range filesToRemove {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			i.log.Warnf("Failed to remove %s: %v", file, err)
		} else if err == nil {
			i.log.Infof("Removed nmstate file: %s", file)
		}
	}

	i.log.Info("Cleaned up nmstate residual state on node")
	return nil
}

// removeStaleFilesForRegeneration deletes known files so that components
// regenerate their state safely after IP changes.
func (i *IPConfigHandler) removeStaleFilesForRegeneration() error {
	i.log.Infof("Removing stale files for regeneration")
	files := []string{
		common.OvnIcEtcFolder,
		common.MultusCerts,
		common.OvsConfDb,
		common.OvsConfDbLock,
	}
	if err := utils.RemoveListOfFiles(i.log, files); err != nil {
		return fmt.Errorf("failed to remove stale files for regeneration in %v: %w", files, err)
	}
	return nil
}

// createMachineConfig creates a machine config for IP configuration changes
func (i *IPConfigHandler) createMachineConfig(interfaceName string) (*machineconfigv1.MachineConfig, error) {
	newIPs := make([]string, len(i.IPConfigs))
	newMachineNetworks := make([]string, len(i.IPConfigs))
	var ipv4Gw, ipv6Gw, ipv4DNS, ipv6DNS string
	for i, cfg := range i.IPConfigs {
		newIPs[i] = cfg.IP
		newMachineNetworks[i] = cfg.MachineNetwork
		if strings.Contains(cfg.IP, ":") {
			if cfg.Gateway != "" {
				ipv6Gw = cfg.Gateway
			}
			if cfg.DNSServer != "" {
				ipv6DNS = cfg.DNSServer
			}
		} else {
			if cfg.Gateway != "" {
				ipv4Gw = cfg.Gateway
			}
			if cfg.DNSServer != "" {
				ipv4DNS = cfg.DNSServer
			}
		}
	}

	nmstateConfig, err := utils.GenerateNMState(
		interfaceName,
		newIPs,
		newMachineNetworks,
		ipv4Gw,
		ipv6Gw,
		ipv4DNS,
		ipv6DNS,
		i.VLANID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to generate NMState config: %w", err)
	}

	encodedContent := base64.StdEncoding.EncodeToString([]byte(nmstateConfig))
	ignitionConfig, err := utils.GenerateIgnitionNMState(&utils.IgnitionNMStateTemplateData{
		EncodedContent: encodedContent,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to generate ignition config: %w", err)
	}

	mc := &machineconfigv1.MachineConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: BrExMachineConfigName,
			Labels: map[string]string{
				"machineconfiguration.openshift.io/role": "master",
			},
		},
		Spec: machineconfigv1.MachineConfigSpec{
			Config: runtime.RawExtension{
				Raw: []byte(ignitionConfig),
			},
		},
	}

	return mc, nil
}

// applyNetworkConfigurationMachineConfig creates or updates the MachineConfig
// that applies the nmstate configuration for the detected interface.
func (i *IPConfigHandler) applyNetworkConfigurationMachineConfig(ctx context.Context, interfaceName string) error {
	i.log.Info("Applying machine config for IP changes")

	mc, err := i.createMachineConfig(interfaceName)
	if err != nil {
		return fmt.Errorf("failed to create machine config: %w", err)
	}

	if err := i.runtimeClient.Create(ctx, mc); err != nil {
		existingMC := &machineconfigv1.MachineConfig{}
		if getErr := i.runtimeClient.Get(ctx, types.NamespacedName{Name: mc.Name}, existingMC); getErr == nil {
			// If spec is identical, treat as idempotent and skip update
			if string(existingMC.Spec.Config.Raw) == string(mc.Spec.Config.Raw) {
				i.log.Infof("Machine config %s already up to date; skipping update", mc.Name)
				return nil
			}

			mc.ResourceVersion = existingMC.ResourceVersion
			if updateErr := i.runtimeClient.Update(ctx, mc); updateErr != nil {
				return fmt.Errorf("failed to update existing machine config: %w", updateErr)
			}
			i.log.Infof("Updated existing machine config: %s", mc.Name)
		} else {
			return fmt.Errorf("failed to create machine config: %w", err)
		}
	} else {
		i.log.Infof("Created machine config: %s", mc.Name)
	}

	return nil
}

// CreateNetworkConfiguration applies MachineConfig to create nmstate configuration
// and waits for rollout on MCP/master and the node.
func (i *IPConfigHandler) CreateNetworkConfiguration(ctx context.Context) error {
	iface, err := i.detectBrExNetworkInterface()
	if err != nil {
		return fmt.Errorf("failed to detect %s network interface: %w", BridgeExternalName, err)
	}
	i.log.Infof("Detected %s network interface: %s", BridgeExternalName, iface)

	if err := i.applyNetworkConfigurationMachineConfig(ctx, iface); err != nil {
		return fmt.Errorf("failed to apply network configuration machine config: %w", err)
	}

	if err := i.waitForMCPMasterUpdated(ctx); err != nil {
		return err
	}

	if err := i.waitForNodeToApplyRenderedMC(ctx); err != nil {
		return err
	}

	return nil
}

// waitForMCPMasterUpdated mirrors the shell logic: first detect updating/rendered change,
// then wait for Updated=True and Degraded!=True with new rendered.
func (i *IPConfigHandler) waitForMCPMasterUpdated(ctx context.Context) error {
	i.log.Info("Waiting for MachineConfigPool/master to roll out a new rendered configuration")

	// Fetch initial rendered name
	mcp := &machineconfigv1.MachineConfigPool{}
	if err := i.runtimeClient.Get(ctx, types.NamespacedName{Name: MCPMasterName}, mcp); err != nil {
		return fmt.Errorf("failed to get mcp/master: %w", err)
	}
	prevRendered := ""
	if mcp.Status.Configuration.Name != "" {
		prevRendered = mcp.Status.Configuration.Name
	}
	if prevRendered != "" {
		i.log.Infof("Current MCP/master rendered: %s", prevRendered)
	}

	deadlineCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	phase1 := func() (bool, error) {
		if err := i.runtimeClient.Get(deadlineCtx, types.NamespacedName{Name: MCPMasterName}, mcp); err != nil {
			i.log.Warnf("failed to get mcp/master: %v", err)
			return false, nil
		}
		rendered := mcp.Status.Configuration.Name
		updating := mcpConditionStatus(mcp.Status.Conditions, MCPConditionUpdating)
		if rendered != prevRendered || updating == "True" {
			return true, nil
		}
		return false, nil
	}

	if err := wait.PollUntilContextTimeout(deadlineCtx, 5*time.Second, 15*time.Minute, true, func(ctx context.Context) (bool, error) {
		return phase1()
	}); err != nil {
		return fmt.Errorf("timed out waiting for MCP master to begin rollout: %w", err)
	}

	// Phase 2: wait for Updated=True, Degraded!=True and rendered changed
	if err := wait.PollUntilContextTimeout(deadlineCtx, 10*time.Second, 15*time.Minute, true, func(ctx context.Context) (bool, error) {
		if err := i.runtimeClient.Get(ctx, types.NamespacedName{Name: MCPMasterName}, mcp); err != nil {
			i.log.Warnf("failed to get mcp/master: %v", err)
			return false, nil
		}
		rendered := mcp.Status.Configuration.Name
		updated := mcpConditionStatus(mcp.Status.Conditions, MCPConditionUpdated)
		degraded := mcpConditionStatus(mcp.Status.Conditions, MCPConditionDegraded)
		if rendered != "" && rendered != prevRendered && updated == "True" && degraded != "True" {
			i.log.Infof("MachineConfigPool master rolled out new rendered %s (previous %s)", rendered, prevRendered)
			return true, nil
		}
		return false, nil
	}); err != nil {
		return fmt.Errorf("timed out waiting for MCP master to reach Updated with new rendered config: %w", err)
	}

	return nil
}

// waitForNodeToApplyRenderedMC waits for the single node to have desired/current annotations equal to MCP rendered.
func (i *IPConfigHandler) waitForNodeToApplyRenderedMC(ctx context.Context) error {
	nodeName, err := utils.GetLocalNodeName(ctx, i.runtimeClient)
	if err != nil {
		return err
	}

	i.log.Infof("Waiting for node %s to apply new MachineConfig", nodeName)

	deadlineCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	// Helper to fetch current MCP rendered name (may change during wait)
	getRendered := func(ctx context.Context) string {
		mcp := &machineconfigv1.MachineConfigPool{}
		if err := i.runtimeClient.Get(ctx, types.NamespacedName{Name: MCPMasterName}, mcp); err != nil {
			return ""
		}
		return mcp.Status.Configuration.Name
	}

	return wait.PollUntilContextTimeout(deadlineCtx, 10*time.Second, 15*time.Minute, true, func(ctx context.Context) (bool, error) {
		rendered := getRendered(ctx)
		node := &corev1.Node{}
		if err := i.runtimeClient.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
			i.log.Warnf("failed to get node %s: %v", nodeName, err)
			return false, nil
		}
		ann := node.GetAnnotations()
		desired := ann[MachineConfigDesiredAnnoKey]
		current := ann[MachineConfigCurrentAnnoKey]

		if rendered != "" {
			if desired == rendered && current == rendered {
				i.log.Infof("Node %s desiredConfig/currentConfig match MCP configuration %s", nodeName, rendered)
				return true, nil
			}
		} else {
			if desired != "" && desired == current {
				i.log.Infof("Node %s currentConfig equals desiredConfig (%s)", nodeName, desired)
				return true, nil
			}
		}
		return false, nil
	})
}

// mcpConditionStatus returns the Status string for a given MCP condition type if present, otherwise empty string.
func mcpConditionStatus(conds []machineconfigv1.MachineConfigPoolCondition, condType string) string {
	for _, c := range conds {
		if string(c.Type) == condType {
			return string(c.Status)
		}
	}
	return ""
}

func (i *IPConfigHandler) createCryptoDir(cryptoDir string) error {
	if err := os.MkdirAll(cryptoDir, 0o755); err != nil {
		return fmt.Errorf("failed to create crypto directory: %w", err)
	}
	return nil
}

func (i *IPConfigHandler) collectKubeConfigCrypto(ctx context.Context, cryptoDir string) error {
	i.log.Info("Collecting kubeconfig crypto")
	if err := utils.BackupKubeconfigCrypto(ctx, i.runtimeClient, cryptoDir); err != nil {
		return fmt.Errorf("failed to collect kubeconfig crypto: %w", err)
	}
	return nil
}

// materializeAuthFileFromPullSecretRef fetches the dockerconfigjson secret by name in the LCA namespace
// and writes it to an auth file under the LCA workspace, returning the path to the file.
func materializeAuthFileFromPullSecretRef(
	ctx context.Context,
	client runtimeclient.Client,
	secretName string,
) (string, error) {
	secret := &corev1.Secret{}
	if err := client.Get(ctx, types.NamespacedName{
		Namespace: common.LcaNamespace,
		Name:      secretName,
	}, secret); err != nil {
		return "", fmt.Errorf("failed to fetch pull secret %s/%s: %w", common.LcaNamespace, secretName, err)
	}
	dockercfg, ok := secret.Data[corev1.DockerConfigJsonKey]
	if !ok || len(dockercfg) == 0 {
		return "", fmt.Errorf("secret %s/%s missing key %s", common.LcaNamespace, secretName, corev1.DockerConfigJsonKey)
	}
	authPath := path.Join(common.LCAWorkspaceDir, "recert-pull-secret.json")
	if err := os.WriteFile(common.PathOutsideChroot(authPath), dockercfg, 0o600); err != nil {
		return "", fmt.Errorf("failed to write pull secret auth file: %w", err)
	}
	return authPath, nil
}
