package postpivot

import (
	"context"
	"encoding/json"
	"fmt"
	corev1 "k8s.io/api/core/v1"
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

const (
	nodeIpFile            = "/run/nodeip-configuration/primary-ip"
	dnsmasqOverrides      = "/etc/default/sno_dnsmasq_configuration_overrides"
	sshKeyEarlyAccessFile = "/home/core/.ssh/authorized_keys.d/ib-early-access"
	userCore              = "core"
	sshMachineConfig      = "99-%s-ssh"
)

func (p *PostPivot) PostPivotConfiguration(ctx context.Context) error {

	if err := p.waifForConfiguration(ctx, filepath.Join(common.OptOpenshift, common.ClusterConfigDir)); err != nil {
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
		return err
	}

	if seedReconfiguration.APIVersion != clusterconfig_api.SeedReconfigurationVersion {
		return fmt.Errorf("unsupported seed reconfiguration version %d", seedReconfiguration.APIVersion)
	}

	if err := utils.RunOnce("recert", p.workingDir, p.log, p.recert, ctx, seedReconfiguration, seedClusterInfo); err != nil {
		return err
	}

	if err := p.copyClusterConfigFiles(); err != nil {
		return err
	}

	client, err := utils.CreateKubeClient(p.scheme, p.kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to create k8s client, err: %w", err)
	}

	if _, err := p.ops.SystemctlAction("enable", "kubelet", "--now"); err != nil {
		return err
	}
	p.waitForApi(ctx, client)

	if err := p.deleteAllOldMirrorResources(ctx, client); err != nil {
		return err
	}

	if err := p.applyManifests(); err != nil {
		return err
	}

	if err := p.changeRegistryInCSVDeployment(ctx, client, seedReconfiguration, seedClusterInfo); err != nil {
		return err
	}

	if err := utils.RunOnce("set_cluster_id", p.workingDir, p.log, p.setNewClusterID, ctx, client, seedReconfiguration); err != nil {
		return err
	}

	// Restore lvm devices
	if err := utils.RunOnce("recover_lvm_devices", p.workingDir, p.log, p.recoverLvmDevices); err != nil {
		return err
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
		return err
	}

	ctxWithTimeout, _ := context.WithTimeout(ctx, 10*time.Minute)
	_ = wait.PollUntilContextCancel(ctxWithTimeout, time.Second, true, func(ctx context.Context) (bool, error) {
		p.log.Info("pulling recert image")
		if _, err := p.ops.RunInHostNamespace("podman", "pull", seedClusterInfo.RecertImagePullSpec); err != nil {
			p.log.Warnf("failed to pull recert image, will retry, err: %w", err)
			return false, nil
		}
		// TODO: add waiting for block device with label
		return true, nil
	})

	err := p.ops.RecertFullFlow(seedClusterInfo.RecertImagePullSpec, p.authFile,
		path.Join(p.workingDir, recert.RecertConfigFile),
		nil,
		func() error { return p.postRecertCommands(ctx, seedReconfiguration, seedClusterInfo) },
		"-v", fmt.Sprintf("%s:%s", p.workingDir, p.workingDir))
	return err
}

func (p *PostPivot) etcdPostPivotOperations(ctx context.Context, reconfigurationInfo *clusterconfig_api.SeedReconfiguration) error {
	p.log.Info("Start running etcd post pivot operations")
	cli, err := etcdClient.New(etcdClient.Config{
		Endpoints:   []string{common.EtcdDefaultEndpoint},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return err
	}
	defer cli.Close()

	// cleanup etcd keys
	p.log.Info("Cleaning up etcd keys from seed data")
	keysToDelete := []string{"/kubernetes.io/configmaps/openshift-etcd/etcd-endpoints"}

	for _, key := range keysToDelete {
		_, err = cli.Delete(ctx, key)
		if err != nil {
			return err
		}
	}

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
	dir, err := os.ReadDir(path.Join(p.workingDir, common.ClusterConfigDir, common.ManifestsDir))
	if err != nil {
		return err
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
			return err
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
	return utils.CopyFileIfExists(path.Join(p.workingDir, common.ClusterConfigDir, path.Base(common.CABundleFilePath)),
		common.CABundleFilePath)
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
		if err := client.Delete(ctx, &catalogSource); err != nil {
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
		return err
	}

	p.log.Info("Changing release registry in csv deployment")
	csvD, err := utils.GetCSVDeployment(ctx, client)
	if err != nil {
		return err
	}

	if seedReconfiguration.ReleaseRegistry == seedClusterInfo.ReleaseRegistry {
		p.log.Infof("No registry change occurred, skipping")
		return nil
	}

	p.log.Infof("Changing release registry from %s to %s", seedClusterInfo.ReleaseRegistry, seedReconfiguration.ReleaseRegistry)
	newImage, err := utils.ReplaceImageRegistry(csvD.Spec.Template.Spec.Containers[0].Image,
		seedReconfiguration.ReleaseRegistry, seedClusterInfo.ReleaseRegistry)
	if err != nil {
		return err
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
		return err
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
	return utils.RemoveListOfFolders(p.log, []string{p.workingDir, common.SeedDataDir})
}

func (p *PostPivot) setNewClusterID(ctx context.Context, client runtimeclient.Client, seedReconfiguration *clusterconfig_api.SeedReconfiguration) error {
	p.log.Info("Set new cluster id")
	clusterVersion := &v1.ClusterVersion{}
	if err := client.Get(ctx, types.NamespacedName{Name: "version"}, clusterVersion); err != nil {
		return err
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
		return err
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

// createPullSecretFile create pullSecretFile in  manifests folder, it will be applied with all other manifests
func (p *PostPivot) createPullSecretFile(pullSecret, pullSecretFile string) error {
	// TODO: Should return error in the future as cluster will not be operational without it
	if pullSecret == "" {
		p.log.Infof("Pull secret was not provided")
		return nil
	}

	p.log.Infof("Creating pull secret file %s", pullSecretFile)
	ps := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      common.PullSecretName,
			Namespace: common.OpenshiftConfigNamespace,
		},
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(pullSecret)},
		Type: corev1.SecretTypeDockerConfigJson,
	}
	typeMeta, err := utils.TypeMetaForObject(p.scheme, &ps)
	if err != nil {
		return fmt.Errorf("failed to create typeMetafor pull secret, err: %w", err)
	}
	ps.TypeMeta = *typeMeta

	if err := utils.MarshalToFile(ps, pullSecretFile); err != nil {
		return fmt.Errorf("failed to marshal pull secret into file, err: %w", err)
	}

	return nil
}

// waifForConfiguration
func (p *PostPivot) waifForConfiguration(ctx context.Context, configFolder string) error {
	// configFolder := path.Join(common.OptOpenshift, common.ClusterConfigDir)
	if _, err := os.Stat(configFolder); err == nil {
		return nil
	}
	label := "relocation-config"
	_ = wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		p.log.Info("waiting for block device with label %s or for configuration folder", label)
		if _, err := os.Stat(configFolder); err == nil {
			return true, nil
		}
		// TODO: add waiting for block device with label
		return false, nil
	})

	return nil
}

func (p *PostPivot) cleanupNMConnections(nmconnectionsFolder string) error {
	p.log.Infof("Removing seed nmconnection files")
	files, err := filepath.Glob(filepath.Join(nmconnectionsFolder, "*.nmconnection"))
	if err != nil {
		return fmt.Errorf("failed to find nmconnection files in %s, %w", nmconnectionsFolder, err)
	}
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			return fmt.Errorf("failed to remove %s", f)
		}
	}
	return nil
}

func (p *PostPivot) setHostname(hostname, hostnameFile string) error {
	p.log.Infof("Writing new hostname %s into %s", hostname, hostnameFile)
	if err := os.WriteFile(hostnameFile, []byte(hostname), 0o600); err != nil {
		return fmt.Errorf("failed to configure hostname, err %w", err)
	}
	return nil
}

func (p *PostPivot) networkConfiguration(ctx context.Context, seedReconfiguration *clusterconfig_api.SeedReconfiguration) error {

	if err := p.cleanupNMConnections(common.NMConnectionFolder); err != nil {
		return err
	}

	p.log.Infof("Copying nmconnection files if they were provided")
	err := utils.CopyFileIfExists(filepath.Join(common.NetworkDir, "system-connections"), common.NMConnectionFolder)
	if err != nil {
		return fmt.Errorf("failed to nmconnection files, err: %w", err)
	}

	if err := p.applyNMStateConfiguration(seedReconfiguration); err != nil {
		return err
	}

	if err := p.setNodeIPIfNotProvided(ctx, seedReconfiguration, nodeIpFile); err != nil {
		return err
	}

	if err := p.setDnsMasqConfiguration(seedReconfiguration, dnsmasqOverrides); err != nil {
		return err
	}

	if err := p.setHostname(seedReconfiguration.Hostname, "/etc/hostname"); err != nil {
		return err
	}

	if _, err := p.ops.SystemctlAction("restart", "NetworkManager.service"); err != nil {
		return fmt.Errorf("failed to restart network manager service, err %w", err)
	}

	if _, err := p.ops.SystemctlAction("restart", "dnsmasq.service"); err != nil {
		return fmt.Errorf("failed to restart dnsmasq service, err %w", err)
	}

	return nil
}
