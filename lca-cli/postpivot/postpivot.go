package postpivot

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	clusterconfig_api "github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/recert"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedclusterinfo"
	"github.com/openshift-kni/lifecycle-agent/utils"
	v1 "github.com/openshift/api/config/v1"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/samber/lo"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	cp "github.com/otiai10/copy"
	"github.com/sirupsen/logrus"
	etcdClient "go.etcd.io/etcd/client/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

type PostPivot struct {
	scheme     *runtime.Scheme
	log        *logrus.Logger
	ops        ops.Ops
	authFile   string
	workingDir string
	kubeconfig string
}

func NewPostPivot(scheme *runtime.Scheme, log *logrus.Logger, ops ops.Ops, authFile, workingDir, kubeconfig string) *PostPivot {
	return &PostPivot{
		scheme:     scheme,
		log:        log,
		ops:        ops,
		authFile:   authFile,
		workingDir: workingDir,
		kubeconfig: kubeconfig,
	}
}

var (
	dnsmasqOverrides   = "/etc/default/sno_dnsmasq_configuration_overrides"
	hostnameFile       = "/etc/hostname"
	nmConnectionFolder = common.NMConnectionFolder
	nodeIpFile         = "/run/nodeip-configuration/primary-ip"
	nodeIPHintFile     = "/etc/default/nodeip-configuration"
)

const (
	sshKeyEarlyAccessFile = "/home/core/.ssh/authorized_keys.d/ib-early-access"
	userCore              = "core"
	sshMachineConfig      = "99-%s-ssh"

	// TODO: change after all the components will move to blockDeviceLabel
	OldblockDeviceLabel    = "relocation-config"
	blockDeviceMountFolder = "/mnt/config"

	nmService      = "NetworkManager.service"
	dnsmasqService = "dnsmasq.service"
)

func (p *PostPivot) PostPivotConfiguration(ctx context.Context) error {

	if err := p.waitForConfiguration(ctx, filepath.Join(common.OptOpenshift, common.ClusterConfigDir), blockDeviceMountFolder); err != nil {
		return err
	}

	p.log.Info("Reading seed image info")
	seedClusterInfo, err := seedclusterinfo.ReadSeedClusterInfoFromFile(path.Join(common.SeedDataDir, common.SeedClusterInfoFileName))
	if err != nil {
		return fmt.Errorf("failed to get seed info from %s, err: %w", "", err)
	}

	p.log.Info("Reading seed reconfiguration info")
	seedReconfiguration, err := utils.ReadSeedReconfigurationFromFile(
		path.Join(p.workingDir, common.ClusterConfigDir, common.SeedClusterInfoFileName))
	if err != nil {
		return fmt.Errorf("failed to get cluster info from %s, err: %w", "", err)
	}

	if err := p.networkConfiguration(ctx, seedReconfiguration); err != nil {
		return fmt.Errorf("failed to configure networking, err: %w", err)
	}

	if err := utils.RunOnce("setSSHKey", p.workingDir, p.log, p.setSSHKey,
		seedReconfiguration, sshKeyEarlyAccessFile); err != nil {
		return fmt.Errorf("failed to run once setSSHKey for post pivot: %w", err)
	}

	if err := utils.RunOnce("pull-secret", p.workingDir, p.log, p.createPullSecretFile,
		seedReconfiguration.PullSecret, common.ImageRegistryAuthFile); err != nil {
		return fmt.Errorf("failed to run once pull-secret for post pivot: %w", err)
	}

	if seedReconfiguration.APIVersion != clusterconfig_api.SeedReconfigurationVersion {
		return fmt.Errorf("unsupported seed reconfiguration version %d", seedReconfiguration.APIVersion)
	}

	if err := utils.RunOnce("recert", p.workingDir, p.log, p.recert, ctx, seedReconfiguration, seedClusterInfo); err != nil {
		return fmt.Errorf("failed to run once recert for post pivot: %w", err)
	}

	if err := p.copyClusterConfigFiles(); err != nil {
		return fmt.Errorf("failed copy cluster config files: %w", err)
	}

	client, err := utils.CreateKubeClient(p.scheme, p.kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to create k8s client, err: %w", err)
	}

	if _, err := p.ops.SystemctlAction("enable", "kubelet", "--now"); err != nil {
		return fmt.Errorf("failed to enable kubelet: %w", err)
	}
	p.waitForApi(ctx, client)

	if err := p.deleteAllOldMirrorResources(ctx, client); err != nil {
		return fmt.Errorf("failed to all old mirror resources: %w", err)
	}

	if err := p.applyManifests(); err != nil {
		return fmt.Errorf("failed apply manifests: %w", err)
	}

	if err := p.changeRegistryInCSVDeployment(ctx, client, seedReconfiguration, seedClusterInfo); err != nil {
		return fmt.Errorf("failed change registry in CSV deployment: %w", err)
	}

	if err := utils.RunOnce("set_cluster_id", p.workingDir, p.log, p.setNewClusterID, ctx, client, seedReconfiguration); err != nil {
		return fmt.Errorf("failed to run once set_cluster_id for post pivot: %w", err)
	}

	// Restore lvm devices
	if err := utils.RunOnce("recover_lvm_devices", p.workingDir, p.log, p.recoverLvmDevices); err != nil {
		return fmt.Errorf("failed to run once recover_lvm_devices for post pivot: %w", err)
	}

	if _, err = p.ops.SystemctlAction("disable", "installation-configuration.service"); err != nil {
		return fmt.Errorf("failed to disable installation-configuration.service, err: %w", err)
	}

	return p.cleanup()
}

func (p *PostPivot) recert(ctx context.Context, seedReconfiguration *clusterconfig_api.SeedReconfiguration, seedClusterInfo *seedclusterinfo.SeedClusterInfo) error {
	if _, err := os.Stat(recert.SummaryFile); err == nil {
		return fmt.Errorf("found %s file, returning error, it means recert previously failed. "+
			"In case you still want to rerun it please remove the file", recert.SummaryFile)
	}

	p.log.Info("Create recert configuration file")
	kubeconfigCryptoDir := path.Join(p.workingDir, common.KubeconfigCryptoDir)
	if err := utils.SeedReconfigurationKubeconfigRetentionToCryptoDir(kubeconfigCryptoDir, &seedReconfiguration.KubeconfigCryptoRetention); err != nil {
		return fmt.Errorf("failed to populate crypto dir from seed reconfiguration: %w", err)
	}

	if err := recert.CreateRecertConfigFile(seedReconfiguration, seedClusterInfo, kubeconfigCryptoDir,
		p.workingDir); err != nil {
		return fmt.Errorf("failed to create recert config file: %w", err)
	}

	ctxWithTimeout, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	_ = wait.PollUntilContextCancel(ctxWithTimeout, time.Second, true, func(ctx context.Context) (bool, error) {
		p.log.Info("pulling recert image")
		if _, err := p.ops.RunInHostNamespace("podman", "pull", "--authfile", common.ImageRegistryAuthFile, seedClusterInfo.RecertImagePullSpec); err != nil {
			p.log.Warnf("failed to pull recert image, will retry, err: %s", err.Error())
			return false, nil
		}
		return true, nil
	})

	err := p.ops.RecertFullFlow(seedClusterInfo.RecertImagePullSpec, p.authFile,
		path.Join(p.workingDir, recert.RecertConfigFile),
		nil,
		func() error { return p.postRecertCommands(ctx, seedReconfiguration, seedClusterInfo) },
		"-v", fmt.Sprintf("%s:%s", p.workingDir, p.workingDir))
	if err != nil {
		return fmt.Errorf("failed recert full flow: %w", err)
	}

	return nil
}

func (p *PostPivot) etcdPostPivotOperations(ctx context.Context, reconfigurationInfo *clusterconfig_api.SeedReconfiguration) error {
	p.log.Info("Start running etcd post pivot operations")
	cli, err := etcdClient.New(etcdClient.Config{
		Endpoints:   []string{common.EtcdDefaultEndpoint},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("failed start new etcd client: %w", err)
	}
	defer cli.Close()

	newEtcdIp := reconfigurationInfo.NodeIP
	if utils.IsIpv6(newEtcdIp) {
		newEtcdIp = fmt.Sprintf("[%s]", newEtcdIp)
	}

	members, err := cli.MemberList(ctx)
	if err != nil {
		return fmt.Errorf("failed to get etcd members list")
	}
	_, err = cli.MemberUpdate(ctx, members.Members[0].ID, []string{fmt.Sprintf("http://%s:2380", newEtcdIp)})
	if err != nil {
		return fmt.Errorf("failed to change etcd peer url, err: %w", err)
	}

	return nil
}

func (p *PostPivot) postRecertCommands(ctx context.Context, clusterInfo *clusterconfig_api.SeedReconfiguration, seedClusterInfo *seedclusterinfo.SeedClusterInfo) error {
	// TODO: remove after https://issues.redhat.com/browse/ETCD-503
	if err := p.etcdPostPivotOperations(ctx, clusterInfo); err != nil {
		return fmt.Errorf("failed to run post pivot etcd operations, err: %w", err)
	}

	// changing seed ip to new ip in all static pod files
	_, err := p.ops.RunBashInHostNamespace(fmt.Sprintf("find /etc/kubernetes/ -type f -print0 | xargs -0 sed -i \"s/%s/%s/g\"",
		seedClusterInfo.NodeIP, clusterInfo.NodeIP))
	if err != nil {
		return fmt.Errorf("failed to change seed ip to new ip in /etc/kubernetes, err: %w", err)
	}

	return nil
}

func (p *PostPivot) waitForApi(ctx context.Context, client runtimeclient.Client) {
	p.log.Info("Start waiting for api")
	_ = wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (done bool, err error) {
		p.log.Info("waiting for api")
		nodes := &v1.NodeList{}
		if err = client.List(ctx, nodes); err == nil {
			return true, nil
		}
		return false, nil
	})
}

func (p *PostPivot) applyManifests() error {
	p.log.Infof("Applying manifests from %s", path.Join(p.workingDir, common.ClusterConfigDir, common.ManifestsDir))
	mPath := path.Join(p.workingDir, common.ClusterConfigDir, common.ManifestsDir)
	dir, err := os.ReadDir(mPath)
	if err != nil {
		return fmt.Errorf("failed to read dir for extra manifest in %s: %w", mPath, err)
	}
	if len(dir) == 0 {
		p.log.Infof("No manifests to apply were found, skipping")
		return nil
	}

	args := []string{"--kubeconfig", p.kubeconfig, "apply", "-f"}

	_, err = p.ops.RunInHostNamespace("oc", append(args, path.Join(p.workingDir, common.ClusterConfigDir, common.ManifestsDir))...)
	if err != nil {
		return fmt.Errorf("failed to apply manifests, err: %w", err)
	}

	if _, err := os.Stat(path.Join(p.workingDir, common.ExtraManifestsDir)); err == nil {
		p.log.Infof("Applying extra manifests")
		_, err := p.ops.RunInHostNamespace("oc", append(args, path.Join(p.workingDir, common.ExtraManifestsDir))...)
		if err != nil {
			return fmt.Errorf("failed to apply extra manifests, err: %w", err)
		}
	}
	return nil
}

func (p *PostPivot) recoverLvmDevices() error {
	lvmConfigPath := path.Join(p.workingDir, common.LvmConfigDir)
	lvmDevicesPath := path.Join(lvmConfigPath, path.Base(common.LvmDevicesPath))

	p.log.Infof("Recovering lvm devices from %s", lvmDevicesPath)
	if err := cp.Copy(lvmDevicesPath, common.LvmDevicesPath); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to copy lvm devices devices in %s: %w", lvmDevicesPath, err)
		}
	}

	// Update the online record of PVs and activate all lvm devices in the VGs
	_, err := p.ops.RunInHostNamespace("pvscan", "--cache", "--activate", "ay")
	if err != nil {
		return fmt.Errorf("failed to scan and active lvm devices, err: %w", err)
	}

	return nil
}

func (p *PostPivot) copyClusterConfigFiles() error {
	if err := utils.CopyFileIfExists(path.Join(p.workingDir, common.ClusterConfigDir, path.Base(common.CABundleFilePath)),
		common.CABundleFilePath); err != nil {
		return fmt.Errorf("failed to copy cluster config file in %s: %w", common.ClusterConfigDir, err)
	}
	return nil
}

func (p *PostPivot) deleteAllOldMirrorResources(ctx context.Context, client runtimeclient.Client) error {
	p.log.Info("Deleting ImageContentSourcePolicy and ImageDigestMirrorSet if they exist")
	icsp := &operatorv1alpha1.ImageContentSourcePolicy{}
	if err := client.DeleteAllOf(ctx, icsp); err != nil {
		return fmt.Errorf("failed to delete all icsps %w", err)
	}

	idms := &v1.ImageDigestMirrorSet{}
	if err := client.DeleteAllOf(ctx, idms); err != nil {
		return fmt.Errorf("failed to delete all idms %w", err)
	}

	return p.deleteCatalogSources(ctx, client)
}

func (p *PostPivot) deleteCatalogSources(ctx context.Context, client runtimeclient.Client) error {
	p.log.Info("Deleting default catalog sources")
	catalogSources := &operatorsv1alpha1.CatalogSourceList{}
	allNamespaces := runtimeclient.ListOptions{Namespace: metav1.NamespaceAll}
	if err := client.List(ctx, catalogSources, &allNamespaces); err != nil {
		return fmt.Errorf("failed to list all catalogueSources %w", err)
	}

	for _, catalogSource := range catalogSources.Items {
		if err := client.Delete(ctx, &catalogSource); err != nil { //nolint:gosec
			return fmt.Errorf("failed to delete all catalogueSources %w", err)
		}
	}

	return nil
}

func (p *PostPivot) changeRegistryInCSVDeployment(ctx context.Context, client runtimeclient.Client,
	seedReconfiguration *clusterconfig_api.SeedReconfiguration, seedClusterInfo *seedclusterinfo.SeedClusterInfo) error {

	// in case we should not override we can skip
	if shouldOverride, err := utils.ShouldOverrideSeedRegistry(ctx, client,
		seedClusterInfo.MirrorRegistryConfigured, seedClusterInfo.ReleaseRegistry); !shouldOverride || err != nil {
		return err //nolint:wrapcheck
	}

	p.log.Info("Changing release registry in csv deployment")
	csvD, err := utils.GetCSVDeployment(ctx, client)
	if err != nil {
		return fmt.Errorf("failed to get CSV deployment: %w", err)
	}

	if seedReconfiguration.ReleaseRegistry == seedClusterInfo.ReleaseRegistry {
		p.log.Infof("No registry change occurred, skipping")
		return nil
	}

	p.log.Infof("Changing release registry from %s to %s", seedClusterInfo.ReleaseRegistry, seedReconfiguration.ReleaseRegistry)
	newImage, err := utils.ReplaceImageRegistry(csvD.Spec.Template.Spec.Containers[0].Image,
		seedReconfiguration.ReleaseRegistry, seedClusterInfo.ReleaseRegistry)
	if err != nil {
		return fmt.Errorf("failed to replace image registry from %s to %s: %w",
			seedClusterInfo.ReleaseRegistry, seedReconfiguration.ReleaseRegistry, err)
	}

	var newArgs []string
	for _, arg := range csvD.Spec.Template.Spec.Containers[0].Args {
		if strings.HasPrefix(arg, "--release-image") {
			arg = fmt.Sprintf("--release-image=%s", newImage)
		}
		newArgs = append(newArgs, arg)
	}
	newArgsStr, err := json.Marshal(newArgs)
	if err != nil {
		return fmt.Errorf("failed to marshall newArgs %s: %w", newArgs, err)
	}
	patch := []byte(fmt.Sprintf(`{"spec":{"template":{"spec": {"containers": [{"name": "%s", "image":"%s", "args": %s}]}}}}`,
		csvD.Spec.Template.Spec.Containers[0].Name, newImage, newArgsStr))

	p.log.Infof("Applying csv deploymet patch %s", string(patch))
	err = client.Patch(ctx, csvD, runtimeclient.RawPatch(types.StrategicMergePatchType, patch))
	if err != nil {
		return fmt.Errorf("failed to patch csv deployment, err: %w", err)
	}
	p.log.Infof("Done changing csv deployment")
	return nil
}

func (p *PostPivot) cleanup() error {
	p.log.Info("Cleaning up")
	listOfDirs := []string{p.workingDir, common.SeedDataDir}
	if err := utils.RemoveListOfFolders(p.log, listOfDirs); err != nil {
		return fmt.Errorf("failed to cleanup in postpivot %s: %w", listOfDirs, err)
	}
	return nil
}

func (p *PostPivot) setNewClusterID(ctx context.Context, client runtimeclient.Client, seedReconfiguration *clusterconfig_api.SeedReconfiguration) error {
	p.log.Info("Set new cluster id")
	clusterVersion := &v1.ClusterVersion{}
	if err := client.Get(ctx, types.NamespacedName{Name: "version"}, clusterVersion); err != nil {
		return fmt.Errorf("failed to get cluseterVersion: %w", err)
	}

	if seedReconfiguration.ClusterID == "" {
		// TODO: Generate a fresh cluster ID in case it is empty
		return fmt.Errorf("cluster id is empty, automatic generation is not yet implemented")
	}

	patch := []byte(fmt.Sprintf(`{"spec":{"clusterID":"%s"}}`, seedReconfiguration.ClusterID))

	p.log.Infof("Applying csv deploymet patch %s", string(patch))
	err := client.Patch(ctx, clusterVersion, runtimeclient.RawPatch(types.MergePatchType, patch))
	if err != nil {
		return fmt.Errorf("failed to patch cluster id in clusterversion, err: %w", err)
	}
	return nil
}

// setDnsMasqConfiguration sets new configuration for dnsmasq and forcedns dispatcher script.
// It points them to new ip, cluster name and domain.
// For new configuration to apply we must restart NM and dnsmasq
func (p *PostPivot) setDnsMasqConfiguration(seedReconfiguration *clusterconfig_api.SeedReconfiguration,
	dnsmasqOverridesFiles string) error {
	p.log.Info("Setting new dnsmasq and forcedns dispatcher script configuration")
	config := []string{
		fmt.Sprintf("SNO_CLUSTER_NAME_OVERRIDE=%s", seedReconfiguration.ClusterName),
		fmt.Sprintf("SNO_BASE_DOMAIN_OVERRIDE=%s", seedReconfiguration.BaseDomain),
		fmt.Sprintf("SNO_DNSMASQ_IP_OVERRIDE=%s", seedReconfiguration.NodeIP),
	}

	if err := os.WriteFile(dnsmasqOverridesFiles, []byte(strings.Join(config, "\n")), 0o600); err != nil {
		return fmt.Errorf("failed to configure dnsmasq and forcedns dispatcher script, err %w", err)
	}

	return nil
}

// setNodeIPIfNotProvided will run nodeip configuration service on demand in case seedReconfiguration node ip is empty
// nodeip-configuration service in on charge of setting kubelet and crio ip, this ip we will take as NodeIP
func (p *PostPivot) setNodeIPIfNotProvided(ctx context.Context,
	seedReconfiguration *clusterconfig_api.SeedReconfiguration, ipFile string) error {
	if seedReconfiguration.NodeIP != "" {
		return nil
	}

	if _, err := os.Stat(ipFile); err != nil {
		_, err := p.ops.SystemctlAction("start", "nodeip-configuration")
		if err != nil {
			return fmt.Errorf("failed to start nodeip-configuration service, err %w", err)
		}

		p.log.Info("Start waiting for nodeip service to choose node ip")
		_ = wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (done bool, err error) {
			if _, err := os.Stat(ipFile); err != nil {
				return false, nil
			}
			return true, nil
		})
	}

	b, err := os.ReadFile(ipFile)
	if err != nil {
		return fmt.Errorf("failed to read ip from %s, err: %w", ipFile, err)
	}
	ip := net.ParseIP(string(b))
	if ip == nil {
		return fmt.Errorf("failed to parse ip %s from %s", string(b), ipFile)
	}

	seedReconfiguration.NodeIP = ip.String()
	return nil
}

func (p *PostPivot) setNodeIPHint(ctx context.Context, nodeIp, nodeIPConfFile string) error {
	if nodeIp == "" {
		p.log.Info("Node ip was not provided, skipping node ip hint changes")
		return nil
	}
	var hintSubnet net.IP
	p.log.Infof("Searching for subnet that matches %s", nodeIp)
	// retry is required as installation service runs before
	// network online and the ip might require some time to be configured
	ctxWithTimeout, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	err := wait.PollUntilContextCancel(ctxWithTimeout, time.Second, true, func(ctx context.Context) (done bool, err error) {
		addresses, err := p.ops.ListNodeAddresses()
		if err != nil {
			p.log.Warnf("failed to list node addresses, will retry. err: %s", err.Error())
			return false, nil
		}

		for _, addr := range addresses {
			ip, subnet, _ := net.ParseCIDR(addr.String())
			if nodeIp == ip.String() {
				hintSubnet, _, _ = net.ParseCIDR(subnet.String())
				break
			}
		}
		// should retry in case subnet was not found
		if hintSubnet == nil {
			p.log.Infof("Failed to find subnet that matches %s, will retry", nodeIp)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("failed to find subnet that matches %s, please check provided ip, err: %w", nodeIp, err)
	}

	p.log.Infof("Writing subnet %s into %s", hintSubnet.String(), nodeIPHintFile)
	if err := os.WriteFile(nodeIPConfFile, []byte(fmt.Sprintf("KUBELET_NODEIP_HINT=%s",
		hintSubnet.String())), 0o600); err != nil {
		return fmt.Errorf("failed to write nmstate config to %s, err %w", nodeIPConfFile, err)
	}

	return nil
}

// setSSHKey  sets ssh public key provided by user in 2 operations:
// 1. as file in order to give early access to the node
// 2. creates 2 machine configs in manifests dir that will be applied when cluster is up
func (p *PostPivot) setSSHKey(seedReconfiguration *clusterconfig_api.SeedReconfiguration, sshKeyFile string) error {
	if seedReconfiguration.SSHKey == "" {
		p.log.Infof("No ssh public key was provided, skipping")
		return nil
	}

	p.log.Infof("Creating file %s with ssh keys for early connection", sshKeyFile)
	if err := os.WriteFile(sshKeyFile, []byte(seedReconfiguration.SSHKey), 0o600); err != nil {
		return fmt.Errorf("failed to write ssh key to file, err %w", err)
	}

	p.log.Infof("Setting %s user ownership on %s", userCore, sshKeyFile)
	if _, err := p.ops.RunInHostNamespace("chown", userCore, sshKeyFile); err != nil {
		return fmt.Errorf("failed to set %s user ownership on %s, err :%w", userCore, sshKeyFile, err)
	}

	return p.createSSHKeyMachineConfigs(seedReconfiguration.SSHKey)
}

func (p *PostPivot) createSSHKeyMachineConfigs(sshKey string) error {
	p.log.Info("Creating worker and master machine configs with provided ssh key, in order to override seed's")
	ignConfig := map[string]any{
		"ignition": map[string]string{"version": "3.2.0"},
		"passwd": map[string]any{
			"users": []any{
				map[string]any{
					"name":              userCore,
					"sshAuthorizedKeys": []string{sshKey},
				}},
		},
	}
	rawExt, err := utils.ConvertToRawExtension(ignConfig)
	if err != nil {
		return fmt.Errorf("failed to convert ign config to raw ext: %w", err)
	}

	for _, role := range []string{"master", "worker"} {
		mc := &mcfgv1.MachineConfig{
			TypeMeta: metav1.TypeMeta{
				APIVersion: mcfgv1.SchemeGroupVersion.String(),
				Kind:       "MachineConfig",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf(sshMachineConfig, role),
				Labels: map[string]string{
					"machineconfiguration.openshift.io/role": role,
				},
			},
			Spec: mcfgv1.MachineConfigSpec{
				Config: rawExt,
			},
		}
		if err := utils.MarshalToFile(mc, path.Join(p.workingDir, common.ClusterConfigDir,
			common.ManifestsDir, fmt.Sprintf(sshMachineConfig, role))); err != nil {
			return fmt.Errorf("failed to marshal ssh key into file for role %s, err: %w", role, err)
		}
	}
	return nil
}

// applyNMStateConfiguration is applying nmstate yaml provided as string in seedReconfiguration.
// It uses nmstatectl apply <file> command that will return error in case configuration is not successful
func (p *PostPivot) applyNMStateConfiguration(seedReconfiguration *clusterconfig_api.SeedReconfiguration) error {
	if seedReconfiguration.RawNMStateConfig == "" {
		p.log.Infof("NMState config is empty, skipping")
		return nil
	}
	nmFile := path.Join(p.workingDir, "nmstate.yaml")
	p.log.Infof("Applying nmstate config %s", seedReconfiguration.RawNMStateConfig)
	if err := os.WriteFile(nmFile, []byte(seedReconfiguration.RawNMStateConfig), 0o600); err != nil {
		return fmt.Errorf("failed to write nmstate config to %s, err %w", nmFile, err)
	}
	if _, err := p.ops.RunInHostNamespace("nmstatectl", "apply", nmFile); err != nil {
		return fmt.Errorf("failed to apply nmstate config %s, err: %w", seedReconfiguration.RawNMStateConfig, err)
	}

	return nil
}

// createPullSecretFile creates auth file on filesystem in order to be able to pull images
func (p *PostPivot) createPullSecretFile(pullSecret, pullSecretFile string) error {
	// TODO: Should return error in the future as cluster will not be operational without it
	if pullSecret == "" {
		return fmt.Errorf("pull secret was not provided")
	}

	p.log.Infof("Writing provided pull secret to %s", pullSecretFile)
	if err := os.WriteFile(pullSecretFile, []byte(pullSecret), 0o600); err != nil {
		return fmt.Errorf("failed to write pull secret to %s, err %w", pullSecret, err)
	}

	return nil
}

// waitForConfiguration waits till configuration folder exists or till device with label "relocation-config" is added.
// in case device was provided we are mounting it and copying files to configFolder
func (p *PostPivot) waitForConfiguration(ctx context.Context, configFolder, blockDeviceMountFolder string) error {
	err := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		p.log.Infof("waiting for block device with label %s or for configuration folder %s",
			clusterconfig_api.BlockDeviceLabel, configFolder)
		if _, err := os.Stat(configFolder); err == nil {
			return true, nil
		}
		blockDevices, err := p.ops.ListBlockDevices()
		if err != nil {
			p.log.Infof("Failed to list block devices with error %s, will retry", err.Error())
			return false, nil
		}
		for _, bd := range blockDevices {
			// TODO: change after all the components will move to clusterconfig_api.BlockDeviceLabel
			if lo.Contains([]string{clusterconfig_api.BlockDeviceLabel, OldblockDeviceLabel}, bd.Label) {
				// in case of error while mounting device we exit wait and return the error
				if err := p.setupConfigurationFolder(bd.Name, blockDeviceMountFolder, filepath.Dir(configFolder)); err != nil {
					return true, err
				}
				return true, nil
			}
		}
		return false, nil
	})

	if err != nil {
		return fmt.Errorf("failed to wait for configuration: %w", err)
	}

	return nil
}

// setupConfigurationFolder mounts device to mountFolder and copies everything to configFolder
func (p *PostPivot) setupConfigurationFolder(deviceName, mountFolder, configFolder string) error {
	p.log.Infof("Running setup of configuration folder")
	if err := os.MkdirAll(configFolder, 0o700); err != nil {
		return fmt.Errorf("failed to create %s, err: %w", configFolder, err)
	}
	defer os.RemoveAll(mountFolder)

	if err := p.ops.Mount(deviceName, mountFolder); err != nil {
		return fmt.Errorf("failed to mount %s: %w", mountFolder, err)
	}
	defer p.ops.Umount(deviceName)

	if err := utils.CopyFileIfExists(mountFolder, configFolder); err != nil {
		return fmt.Errorf("failed to copy contert of %s to %s, err: %w", mountFolder, configFolder, err)
	}

	return nil
}

// setHostname writes provided hostname to hostnameFile
func (p *PostPivot) setHostname(hostname, hostnameFile string) error {
	p.log.Infof("Writing new hostname %s into %s", hostname, hostnameFile)
	if err := os.WriteFile(hostnameFile, []byte(hostname), 0o600); err != nil {
		return fmt.Errorf("failed to configure hostname, err %w", err)
	}
	return nil
}

// copyNMConnectionFiles as part of the process nmconnection files can be provided and we should copy them into etc folder
func (p *PostPivot) copyNMConnectionFiles(source, dest string) error {
	p.log.Infof("Copying nmconnection files if they were provided")
	err := utils.CopyFileIfExists(source, dest)
	if err != nil {
		return fmt.Errorf("failed to copy nmconnection files from %s to %s, err: %w", source, dest, err)
	}

	return nil
}

// physicalNetworkConfigurations is on charge of configuring host network prior installation process.
// 1. Copying provided nmconnection files to NM sysconnections to setup network provided by user
// 2. Apply provided NMstate configurations
// 3. We should restart NM after adding connection files in order for sure to set static ip
func (p *PostPivot) physicalNetworkConfigurations(seedReconfiguration *clusterconfig_api.SeedReconfiguration) error {
	if err := p.copyNMConnectionFiles(
		path.Join(p.workingDir, common.NetworkDir, "system-connections"), nmConnectionFolder); err != nil {
		return err
	}

	if err := p.applyNMStateConfiguration(seedReconfiguration); err != nil {
		return err
	}

	if _, err := p.ops.SystemctlAction("restart", nmService); err != nil {
		return fmt.Errorf("failed to restart network manager service, err %w", err)
	}
	return nil
}

// networkConfiguration is in charge of configuring the network prior installation process.
// This function should include all network configurations of post-pivot flow.
// It's logic currently includes:
// 1. Setup host network
// 2. Override nodeip-hint file
// 3. In case ip was not provided by user we should run set ip logic that can be found in setNodeIPIfNotProvided
// 4. Override seed dnsmasq params
// 5. Restart NM and dnsmasq in order to apply provided configurations
func (p *PostPivot) networkConfiguration(ctx context.Context, seedReconfiguration *clusterconfig_api.SeedReconfiguration) error {
	if err := p.physicalNetworkConfigurations(seedReconfiguration); err != nil {
		return err
	}

	if err := p.setNodeIPIfNotProvided(ctx, seedReconfiguration, nodeIpFile); err != nil {
		return err
	}

	if err := p.setNodeIPHint(ctx, seedReconfiguration.NodeIP, nodeIPHintFile); err != nil {
		return err
	}

	if err := p.setDnsMasqConfiguration(seedReconfiguration, dnsmasqOverrides); err != nil {
		return err
	}

	if err := p.setHostname(seedReconfiguration.Hostname, hostnameFile); err != nil {
		return err
	}

	if _, err := p.ops.SystemctlAction("restart", nmService); err != nil {
		return fmt.Errorf("failed to restart network manager service, err %w", err)
	}

	if _, err := p.ops.SystemctlAction("restart", dnsmasqService); err != nil {
		return fmt.Errorf("failed to restart dnsmasq service, err %w", err)
	}

	return nil
}
