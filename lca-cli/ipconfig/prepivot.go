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

package ipconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path"
	"path/filepath"
	"strings"

	"github.com/samber/lo"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	intOstree "github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	"github.com/openshift-kni/lifecycle-agent/internal/recert"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmOstree "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

// RecertClusterData groups data required by the recert tool.
type RecertClusterData struct {
	InstallConfig        string
	IngressCertificateCN string
	CurrentNodeIPs       []string
	CryptoDir            string
}

// NetworkIPConfig is a minimal representation of an IP and its machine network.
type NetworkIPConfig struct {
	IP             string
	MachineNetwork string
	Gateway        string
	DNSServer      string
}

// OstreeData groups metadata about the old and new stateroots involved in the
// IP configuration pre-pivot flow.
type OstreeData struct {
	OldStateroot *StaterootData
	NewStateroot *StaterootData
}

// StaterootData describes a stateroot's identity and deployment metadata.
type StaterootData struct {
	Name           string
	Path           string
	DeploymentDir  string
	DeploymentName string
}

// PrePivotHandler coordinates the IP configuration pre-pivot steps by deploying
// the new stateroot, copying state, and setting the default deployment.
type PrePivotHandler struct {
	log                  *logrus.Logger
	ops                  ops.Ops
	ostreeData           *OstreeData
	ostree               intOstree.IClient
	rpm                  rpmOstree.IClient
	reboot               reboot.RebootIntf
	client               runtimeclient.Client
	ipConfigs            []*NetworkIPConfig
	pullSecretRefName    string
	vlanID               int
	dnsIPFamily          string
	hostWorkspaceDir     string
	mcdCurrentConfigPath string
}

// NewPrePivotHandler creates a new PrePivotHandler instance.
func NewPrePivotHandler(
	log *logrus.Logger,
	ops ops.Ops,
	ostreeData *OstreeData,
	ostree intOstree.IClient,
	rpm rpmOstree.IClient,
	reboot reboot.RebootIntf,
	client runtimeclient.Client,
	ipConfigs []*NetworkIPConfig,
	pullSecretRefName string,
	vlanID int,
	dnsIPFamily string,
	hostWorkspaceDir string,
	mcdCurrentConfigPath string,
) *PrePivotHandler {
	return &PrePivotHandler{
		log:                  log,
		ops:                  ops,
		ostreeData:           ostreeData,
		ostree:               ostree,
		rpm:                  rpm,
		reboot:               reboot,
		client:               client,
		ipConfigs:            ipConfigs,
		pullSecretRefName:    pullSecretRefName,
		vlanID:               vlanID,
		dnsIPFamily:          dnsIPFamily,
		hostWorkspaceDir:     hostWorkspaceDir,
		mcdCurrentConfigPath: mcdCurrentConfigPath,
	}
}

// Run executes the pre-pivot sequence: stop services, deploy/prepare the new
// stateroot, and re-enable services in the appropriate roots.
func (p *PrePivotHandler) Run(ctx context.Context) (err error) {
	p.log.Info("IP config pre-pivot started")

	p.log.Info("Fetching current kernel args")
	mcoData, err := p.ops.ReadFile(p.mcdCurrentConfigPath)
	if err != nil {
		err = fmt.Errorf("failed to read current mco config %s: %w", p.mcdCurrentConfigPath, err)
		return
	}

	kargs, err := utils.BuildKernelArgumentsFromMCOData(mcoData)
	if err != nil {
		err = fmt.Errorf("failed to get current kernel args: %w", err)
		return
	}

	p.log.Info("Getting pull secret data")
	pullSecretData, err := p.getPullSecretData(ctx)
	if err != nil {
		return fmt.Errorf("failed to get pull secret data: %w", err)
	}

	p.log.Info("Preparing recert cluster data")
	prepareRecertClusterData, err := p.prepareRecertClusterData(ctx)
	if err != nil {
		return fmt.Errorf("failed to prepare recert cluster data: %w", err)
	}

	p.log.Info("Creating recert configuration file")
	recertConfig, err := p.createRecertConfig(prepareRecertClusterData)
	if err != nil {
		return fmt.Errorf("failed to create recert configuration file: %w", err)
	}

	p.log.Info("Preparing network configuration")
	nmstateConfig, err := p.prepareNetworkConfiguration(ctx)
	if err != nil {
		return fmt.Errorf("failed to prepare network configuration: %w", err)
	}

	// all functions above should run before we stop the cluster services

	p.log.Info("Stopping cluster services")
	if err = p.ops.StopClusterServices(); err != nil {
		err = fmt.Errorf("failed to stop cluster services: %w", err)
		return
	}

	defer func() {
		p.log.Info("Enabling cluster services in old stateroot")
		if internalErr := p.ops.EnableClusterServices(); internalErr != nil {
			if err == nil {
				err = internalErr
			} else {
				err = fmt.Errorf("%s; also failed to enable cluster services: %w", err.Error(), internalErr)
			}
		}
	}()

	// should run after the cluster services stopped
	p.log.Info("Preparing new stateroot")
	if err = p.prepareNewStateroot(p.ostreeData, kargs); err != nil {
		return fmt.Errorf("failed to prepare new stateroot: %w", err)
	}

	// all functions below should run after the new stateroot created

	p.log.Info("Writing pull secret to new stateroot workspace")
	if err := p.writePullSecretToNewStateroot(p.ostreeData, pullSecretData); err != nil {
		return fmt.Errorf("failed to write pull secret to new stateroot workspace: %w", err)
	}

	p.log.Info("Writing recert configuration file to new stateroot workspace")
	if err := p.writeRecertConfigToNewStateroot(p.ostreeData, recertConfig); err != nil {
		return fmt.Errorf("failed to write recert configuration file to new stateroot workspace: %w", err)
	}

	p.log.Info("Writing nmstate configuration file to new stateroot workspace")
	if err := p.writeNMStateConfigToNewStateroot(p.ostreeData, nmstateConfig); err != nil {
		return fmt.Errorf("failed to write nmstate configuration file to new stateroot workspace: %w", err)
	}

	p.log.Info("Updating DNSMasq override in new stateroot")
	if err := p.updateDNSMasqOverrideIPInNewStateroot(); err != nil {
		return fmt.Errorf("failed to update DNSMasq override in new stateroot: %w", err)
	}

	p.log.Info("Removing stale files for regeneration in new stateroot")
	if err := p.removeStaleFilesInNewStaterootForRegeneration(); err != nil {
		return fmt.Errorf("failed to remove stale files for regeneration in new stateroot: %w", err)
	}

	p.log.Info("IP config pre-pivot done successfully")

	return nil
}

// prepareNewStateroot deploys the new stateroot if needed, copies data, and
// sets it as default (when supported).
func (p *PrePivotHandler) prepareNewStateroot(
	ostreeData *OstreeData,
	kernelArgs []string,
) error {
	if err := p.ensureSysrootWritable(); err != nil {
		return fmt.Errorf("failed to ensure sysroot writable: %w", err)
	}

	if ostreeData.NewStateroot.DeploymentName == "" {
		p.log.Infof("New stateroot %s is not deployed, deploying it", ostreeData.NewStateroot.Name)

		bootedCommit, err := p.getBootedCommit()
		if err != nil {
			return fmt.Errorf("failed to get booted commit: %w", err)
		}

		if err = p.deployNewStateroot(
			ostreeData,
			lo.FromPtr(bootedCommit),
			kernelArgs,
		); err != nil {
			return fmt.Errorf("failed to deploy new stateroot: %w", err)
		}
	} else {
		p.log.Infof("New stateroot %s is already deployed, skipping deploy", ostreeData.NewStateroot.Name)
	}

	if err := p.copyStateRootData(ostreeData); err != nil {
		return fmt.Errorf("failed to copy state root data: %w", err)
	}

	if err := p.setDefaultDeploymentIfEnabled(ostreeData.NewStateroot.Name); err != nil {
		return fmt.Errorf("failed to set default deployment: %w", err)
	}

	return nil
}

func (p *PrePivotHandler) ensureSysrootWritable() error {
	if err := p.ops.RemountSysroot(); err != nil {
		return fmt.Errorf("failed to remount /sysroot rw: %w", err)
	}
	return nil
}

// getBootedCommit returns the checksum of the currently booted rpm-ostree deployment.
func (p *PrePivotHandler) getBootedCommit() (*string, error) {
	status, err := p.rpm.QueryStatus()
	if err != nil {
		return nil, fmt.Errorf("failed to query rpm-ostree status: %w", err)
	}

	var bootedCommit string
	for _, d := range status.Deployments {
		if d.Booted {
			bootedCommit = d.Checksum
			break
		}
	}

	if bootedCommit == "" {
		return nil, fmt.Errorf("failed to determine booted deployment commit")
	}

	return &bootedCommit, nil
}

// deployNewStateroot initializes OSTree for the new stateroot and deploys the
// booted commit with the provided kernel args, updating the ostreeData fields.
func (p *PrePivotHandler) deployNewStateroot(
	ostreeData *OstreeData,
	bootedCommit string,
	kargs []string,
) error {
	if err := p.ostree.OSInit(ostreeData.NewStateroot.Name); err != nil {
		return fmt.Errorf("failed to initialize ostree for new stateroot %s: %w", ostreeData.NewStateroot.Name, err)
	}

	if err := p.ostree.Deploy(ostreeData.NewStateroot.Name, bootedCommit, kargs, p.rpm, false); err != nil {
		return fmt.Errorf("failed ostree admin deploy: %w", err)
	}

	deploymentName, err := p.ostree.GetDeployment(ostreeData.NewStateroot.Name)
	if err != nil {
		return fmt.Errorf("failed to get deployment for %s: %w", ostreeData.NewStateroot.Name, err)
	}
	ostreeData.NewStateroot.DeploymentName = deploymentName

	deploymentDir, err := p.ostree.GetDeploymentDir(ostreeData.NewStateroot.Name)
	if err != nil {
		return fmt.Errorf("failed to get deployment dir for %s: %w", ostreeData.NewStateroot.Name, err)
	}
	ostreeData.NewStateroot.DeploymentDir = deploymentDir

	return nil
}

// copyStateRootData copies important state from the old stateroot to the new one.
func (p *PrePivotHandler) copyStateRootData(ostreeData *OstreeData) error {
	if err := p.copyVar(
		ostreeData.OldStateroot.Path,
		ostreeData.NewStateroot.Path,
	); err != nil {
		return err
	}

	if err := p.copyEtc(
		ostreeData.OldStateroot.DeploymentDir,
		ostreeData.NewStateroot.DeploymentDir,
	); err != nil {
		return err
	}

	if err := p.copyDeploymentOrigin(
		ostreeData.OldStateroot.Path,
		ostreeData.NewStateroot.Path,
		ostreeData.OldStateroot.DeploymentName,
		ostreeData.NewStateroot.DeploymentName,
	); err != nil {
		return fmt.Errorf("failed to copy deployment origin: %w", err)
	}

	return nil
}

// setDefaultDeploymentIfEnabled sets the given stateroot as default when supported.
func (p *PrePivotHandler) setDefaultDeploymentIfEnabled(newStateroot string) error {
	if !p.ostree.IsOstreeAdminSetDefaultFeatureEnabled() {
		return fmt.Errorf("ostree admin set default feature is not enabled")
	}

	idx, err := p.rpm.GetDeploymentIndex(newStateroot)
	if err != nil {
		return fmt.Errorf("failed to get deployment index for %s: %w", newStateroot, err)
	}

	if err := p.ostree.SetDefaultDeployment(idx); err != nil {
		return fmt.Errorf("failed to set default deployment: %w", err)
	}

	return nil
}

// copyVar copies the var directory preserving SELinux contexts and attributes
func (p *PrePivotHandler) copyVar(oldSRPath, newSRPath string) error {
	// Copy var directory preserving SELinux contexts and attributes
	if _, err := p.ops.RunInHostNamespace(
		"bash", "-c",
		fmt.Sprintf(
			"cp -ar --preserve=context '%s/' '%s/'",
			filepath.Join(oldSRPath, "var"),
			newSRPath,
		),
	); err != nil {
		return fmt.Errorf("failed to copy var: %w", err)
	}
	return nil
}

// copyEtc copies the deployment's etc directory preserving SELinux contexts
func (p *PrePivotHandler) copyEtc(oldDeploymentDir, newDeploymentDir string) error {
	if _, err := p.ops.RunInHostNamespace(
		"bash", "-c",
		fmt.Sprintf(
			"cp -ar --preserve=context '%s/' '%s/'",
			filepath.Join(oldDeploymentDir, "etc"),
			newDeploymentDir,
		),
	); err != nil {
		return fmt.Errorf("failed to copy etc: %w", err)
	}
	return nil
}

// copyOrigin copies the .origin file between deployments preserving SELinux context
func (p *PrePivotHandler) copyDeploymentOrigin(oldSRPath, newSRPath, oldDeploymentName, newDeploymentName string) error {
	oldOriginPath := filepath.Join(oldSRPath, "deploy", fmt.Sprintf("%s.origin", oldDeploymentName))
	newOriginPath := filepath.Join(newSRPath, "deploy", fmt.Sprintf("%s.origin", newDeploymentName))

	if _, err := p.ops.RunInHostNamespace(
		"bash", "-c",
		fmt.Sprintf(
			"cp -a --preserve=context '%s' '%s'",
			oldOriginPath,
			newOriginPath,
		),
	); err != nil {
		return fmt.Errorf("failed to copy origin file: %w", err)
	}
	return nil
}

// prepareRecertClusterData collects cluster data and artifacts required by the
// recert tool (install-config, crypto, ingress CN, node IPs, auth).
func (p *PrePivotHandler) prepareRecertClusterData(
	ctx context.Context,
) (*RecertClusterData, error) {
	cryptoDir := path.Join(p.hostWorkspaceDir, common.KubeconfigCryptoDir)
	if err := p.createCryptoDir(cryptoDir); err != nil {
		return nil, fmt.Errorf("failed to create crypto directory: %w", err)
	}

	if err := p.collectKubeConfigCrypto(ctx, cryptoDir); err != nil {
		return nil, fmt.Errorf("failed to collect kubeconfig crypto: %w", err)
	}

	ingressCertificateCN, err := utils.GetIngressCertificateCN(ctx, p.client)
	if err != nil {
		return nil, fmt.Errorf("failed to get ingress certificate CN: %w", err)
	}
	p.log.Info("Found ingress certificate CN")

	installConfig, err := utils.GetInstallConfig(ctx, p.client)
	if err != nil {
		return nil, fmt.Errorf("failed to get install config: %w", err)
	}
	p.log.Info("Found install config")

	currentNodeIPs, err := utils.GetNodeInternalIPs(ctx, p.client)
	if err != nil {
		return nil, fmt.Errorf("failed to get current node internal IPs: %w", err)
	}

	return &RecertClusterData{
		InstallConfig:        installConfig,
		IngressCertificateCN: ingressCertificateCN,
		CryptoDir:            cryptoDir,
		CurrentNodeIPs:       currentNodeIPs,
	}, nil
}

func (p *PrePivotHandler) createRecertConfig(clusterData *RecertClusterData) (*recert.RecertConfig, error) {
	oldIPs := make([]string, len(p.ipConfigs))
	newIPs := make([]string, len(p.ipConfigs))
	newMachineNetworks := make([]string, len(p.ipConfigs))

	for i, cfg := range p.ipConfigs {
		oldIP, matchErr := selectIPOfSameFamily(cfg.IP, clusterData.CurrentNodeIPs)
		if matchErr != nil {
			return nil, fmt.Errorf("failed to select old IP to match new IP %s: %w", cfg.IP, matchErr)
		}
		oldIPs[i] = oldIP
		newIPs[i] = cfg.IP
		newMachineNetworks[i] = cfg.MachineNetwork
	}

	recertConfig, err := recert.CreateRecertConfigFileForIPConfig(
		oldIPs,
		newIPs,
		newMachineNetworks,
		clusterData.InstallConfig,
		clusterData.CryptoDir,
		clusterData.IngressCertificateCN,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create recert configuration file: %w", err)
	}

	return recertConfig, nil
}

func (p *PrePivotHandler) writeRecertConfigToNewStateroot(
	ostreeData *OstreeData,
	recertConfig *recert.RecertConfig,
) error {
	configPath := filepath.Join(
		ostreeData.NewStateroot.Path,
		common.LCAWorkspaceDir,
		recert.RecertConfigFile,
	)
	marshaled, err := json.Marshal(recertConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal recert config: %w", err)
	}
	if err := p.ops.WriteFile(configPath, marshaled, common.FileMode0600); err != nil {
		return fmt.Errorf("failed to write recert config file to %s: %w", configPath, err)
	}

	return nil
}

func (p *PrePivotHandler) createCryptoDir(cryptoDir string) error {
	if err := p.ops.MkdirAll(cryptoDir, 0o755); err != nil {
		return fmt.Errorf("failed to create crypto directory: %w", err)
	}
	return nil
}

func (p *PrePivotHandler) collectKubeConfigCrypto(ctx context.Context, cryptoDir string) error {
	p.log.Info("Collecting kubeconfig crypto")
	if err := utils.BackupKubeconfigCrypto(ctx, p.client, cryptoDir); err != nil {
		return fmt.Errorf("failed to collect kubeconfig crypto: %w", err)
	}
	return nil
}

func (p *PrePivotHandler) getPullSecretData(ctx context.Context) ([]byte, error) {
	if p.pullSecretRefName != "" {
		secret := &corev1.Secret{}
		if err := p.client.Get(ctx, types.NamespacedName{
			Namespace: common.LcaNamespace,
			Name:      p.pullSecretRefName,
		}, secret); err != nil {
			return nil, fmt.Errorf("failed to fetch pull secret %s/%s: %w", common.LcaNamespace, p.pullSecretRefName, err)
		}

		dockercfg, ok := secret.Data[corev1.DockerConfigJsonKey]
		if !ok || len(dockercfg) == 0 {
			return nil, fmt.Errorf("secret %s/%s missing key %s", common.LcaNamespace, p.pullSecretRefName, corev1.DockerConfigJsonKey)
		}

		return dockercfg, nil
	}

	pullSecretData, err := p.ops.ReadFile(common.ImageRegistryAuthFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read pull secret data from %s: %w", common.ImageRegistryAuthFile, err)
	}

	return pullSecretData, nil
}

func (p *PrePivotHandler) writePullSecretToNewStateroot(
	ostreeData *OstreeData,
	pullSecretData []byte,
) error {
	pullSecretPath := filepath.Join(
		ostreeData.NewStateroot.Path,
		common.IPConfigPullSecretFile,
	)
	if err := p.ops.WriteFile(pullSecretPath, pullSecretData, 0o600); err != nil {
		return fmt.Errorf("failed to write pull secret data to %s: %w", pullSecretPath, err)
	}

	return nil
}

func (p *PrePivotHandler) prepareNetworkConfiguration(ctx context.Context) (*string, error) {
	logger := p.log.WithContext(ctx)

	if len(p.ipConfigs) == 0 {
		return nil, fmt.Errorf("no IP configurations provided")
	}

	ips := make([]string, 0, len(p.ipConfigs))
	machineNetworks := make([]string, 0, len(p.ipConfigs))
	var ipv4Gateway, ipv6Gateway, ipv4DNS, ipv6DNS string

	for _, cfg := range p.ipConfigs {
		if cfg == nil {
			return nil, fmt.Errorf("nil IP configuration encountered")
		}

		if cfg.IP == "" || cfg.MachineNetwork == "" {
			return nil, fmt.Errorf("ip configuration must include both IP and machine network")
		}

		if _, _, err := net.ParseCIDR(cfg.MachineNetwork); err != nil {
			return nil, fmt.Errorf("invalid machine network %q: %w", cfg.MachineNetwork, err)
		}

		ips = append(ips, cfg.IP)
		machineNetworks = append(machineNetworks, cfg.MachineNetwork)

		switch ipFamilyOfString(cfg.IP) {
		case common.IPv4FamilyName:
			ipv4Gateway = cfg.Gateway
			ipv4DNS = cfg.DNSServer
		case common.IPv6FamilyName:
			ipv6Gateway = cfg.Gateway
			ipv6DNS = cfg.DNSServer
		}
	}

	iface, err := p.detectBrExNetworkInterface()
	if err != nil {
		return nil, fmt.Errorf("failed to detect %s network interface: %w", BridgeExternalName, err)
	}
	logger.Infof("Detected %s network interface: %s", BridgeExternalName, iface)

	nmstateConfig, err := utils.GenerateNMState(
		iface,
		ips,
		machineNetworks,
		ipv4Gateway,
		ipv6Gateway,
		ipv4DNS,
		ipv6DNS,
		p.vlanID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to generate nmstate configuration: %w", err)
	}

	return &nmstateConfig, nil
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

// detectBrExNetworkInterface discovers the *physical* host interface that is
// plugged into the external OVS bridge (`br-ex`). The IP configured on this
// interface becomes the node's primary/external IP (for API, egress, etc.),
// so we must reliably identify it before generating network MachineConfigs.
// To do this we query `ovs-vsctl list-ports br-ex` and ignore OVS-internal
// patch ports, returning only the real NIC that backs `br-ex`.
func (p *PrePivotHandler) detectBrExNetworkInterface() (string, error) {
	p.log.Infof("Detecting %s network interface", BridgeExternalName)

	if output, err := p.ops.RunInHostNamespace("ovs-vsctl", "list-ports", BridgeExternalName); err == nil {
		ports := strings.Fields(output)
		for _, port := range ports {
			// Skip OVS patch ports; we only care about the underlying host NIC
			// Example output:
			//   ovs-vsctl list-ports br-ex
			//   ens3
			//   patch-br-ex_test-infra-cluster-06d0a16b-master-0-to-br-int
			if !strings.Contains(port, BridgeExternalName) {
				p.log.Infof("Found interface via ovs-vsctl: %s", port)
				return port, nil
			}
		}
	} else {
		p.log.Debugf("failed to query ovs-vsctl list-ports %s: %v", BridgeExternalName, err)
	}

	return "", fmt.Errorf("no connected network interface found")
}

func (p *PrePivotHandler) writeNMStateConfigToNewStateroot(
	ostreeData *OstreeData,
	nmstateConfig *string,
) error {
	nmstateConfigPath := filepath.Join(
		ostreeData.NewStateroot.Path,
		common.LCAWorkspaceDir,
		common.NmstateConfigFileName,
	)
	if err := p.ops.WriteFile(nmstateConfigPath, []byte(lo.FromPtr(nmstateConfig)), common.FileMode0600); err != nil {
		return fmt.Errorf("failed to write nmstate config to %s: %w", nmstateConfigPath, err)
	}

	return nil
}

// getDNSOverrideIP selects the IP to use for dnsmasq overrides based on the
// configured IP family (if any), falling back to the first available IP.
func (p *PrePivotHandler) getDNSOverrideIP() (string, error) {
	overrideIP := ""
	for _, cfg := range p.ipConfigs {
		if cfg != nil && cfg.IP != "" && ipFamilyOfString(cfg.IP) == p.dnsIPFamily {
			overrideIP = cfg.IP
			break
		}
	}

	if overrideIP == "" && len(p.ipConfigs) > 0 && p.ipConfigs[0] != nil {
		overrideIP = p.ipConfigs[0].IP
	}

	if overrideIP == "" {
		return "", fmt.Errorf("no IP available to configure dnsmasq overrides")
	}

	return overrideIP, nil
}

// updateDNSMasqOverrideIP updates or appends the dnsmasq override env variable
// in the dnsmasq overrides file without disturbing other configuration lines.
func (p *PrePivotHandler) updateDNSMasqOverrideIPInNewStateroot() error {
	overrideIP, err := p.getDNSOverrideIP()
	if err != nil {
		return fmt.Errorf("failed to get DNS override IP: %w", err)
	}

	var lines []string
	if existing, err := p.ops.ReadFile(common.DnsmasqOverrides); err == nil {
		trimmed := strings.TrimRight(string(existing), "\n")
		if trimmed != "" {
			lines = strings.Split(trimmed, "\n")
		}
	} else if !p.ops.IsNotExist(err) {
		return fmt.Errorf("failed to read existing dnsmasq overrides: %w", err)
	}

	keyPrefix := common.DnsmasqOverrideEnvKeyIP + "="
	newLine := fmt.Sprintf("%s%s", keyPrefix, overrideIP)
	updated := false

	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), keyPrefix) {
			lines[i] = newLine
			updated = true
			break
		}
	}

	if !updated {
		lines = append(lines, newLine)
	}

	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}

	dnsmasqOverridesPath := filepath.Join(p.ostreeData.NewStateroot.DeploymentDir, common.DnsmasqOverrides)
	if err := p.ops.WriteFile(dnsmasqOverridesPath, []byte(content), common.FileMode0600); err != nil {
		return fmt.Errorf("failed to set dnsmasq overrides: %w", err)
	}

	return nil
}

// removeStaleFilesForRegeneration deletes known files so that components
// regenerate their state safely after IP changes.
func (p *PrePivotHandler) removeStaleFilesInNewStaterootForRegeneration() error {
	files := []string{
		filepath.Join(p.ostreeData.NewStateroot.Path, common.OvnIcEtcFolder),
		filepath.Join(p.ostreeData.NewStateroot.DeploymentDir, common.MultusCerts),
		filepath.Join(p.ostreeData.NewStateroot.DeploymentDir, common.OvsConfDb),
		filepath.Join(p.ostreeData.NewStateroot.DeploymentDir, common.OvsConfDbLock),
	}

	if err := utils.RemoveListOfFiles(p.log, files); err != nil {
		return fmt.Errorf("failed to remove stale files for regeneration in new stateroot %s: %w", p.ostreeData.NewStateroot.Path, err)
	}

	return nil
}
