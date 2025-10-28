package postpivot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/openshift-kni/lifecycle-agent/internal/extramanifest"

	"github.com/opencontainers/selinux/go-selinux/label"

	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	clusterconfig_api "github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/backuprestore"
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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/dynamic"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	cp "github.com/otiai10/copy"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	nmConnectionFolder = common.NMConnectionFolder
	nodePrimaryIPFile  = "/run/nodeip-configuration/primary-ip"
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

	localhost         = "localhost"
	kubeletConfigFile = "/etc/systemd/system/kubelet.service.d/20-nodenet.conf"
)

// seedClusterInfoNodeIPs Handles backward compatibility with the old seed cluster info file,
// containing a single IP address (NodeIP).
func seedClusterInfoNodeIPs(seedClusterInfo *seedclusterinfo.SeedClusterInfo) []string {
	if len(seedClusterInfo.NodeIPs) > 0 {
		return seedClusterInfo.NodeIPs
	}

	if seedClusterInfo.NodeIP != "" {
		return []string{seedClusterInfo.NodeIP}
	}

	return []string{}
}

// seedReconfigurationNodeIPs Handles backward compatibility with the old seed reconfiguration file,
// containing a single IP address (NodeIP).
func seedReconfigurationNodeIPs(seedReconfiguration *clusterconfig_api.SeedReconfiguration) []string {
	if len(seedReconfiguration.NodeIPs) > 0 {
		return seedReconfiguration.NodeIPs
	}

	if seedReconfiguration.NodeIP != "" {
		return []string{seedReconfiguration.NodeIP}
	}

	return []string{}
}

func (p *PostPivot) PostPivotConfiguration(ctx context.Context) error {

	if err := p.waitForConfiguration(ctx, filepath.Join(common.OptOpenshift, common.ClusterConfigDir), blockDeviceMountFolder); err != nil {
		return err
	}

	p.log.Info("Reading seed image info")
	seedClusterInfo, err := seedclusterinfo.ReadSeedClusterInfoFromFile(path.Join(common.SeedDataDir, common.SeedClusterInfoFileName))
	if err != nil {
		return fmt.Errorf("failed to get seed info from %s, err: %w", "", err)
	}
	seedClusterInfo.NodeIPs = seedClusterInfoNodeIPs(seedClusterInfo)
	p.log.Infof("Seed cluster info node IPs: %v", seedClusterInfo.NodeIPs)

	p.log.Info("Reading seed reconfiguration info")
	seedReconfiguration, err := utils.ReadSeedReconfigurationFromFile(
		path.Join(p.workingDir, common.ClusterConfigDir, common.SeedReconfigurationFileName))
	if err != nil {
		return fmt.Errorf("failed to get cluster info from %s, err: %w", "", err)
	}
	seedReconfiguration.NodeIPs = seedReconfigurationNodeIPs(seedReconfiguration)
	p.log.Infof("Seed reconfiguration node IPs: %v", seedReconfiguration.NodeIPs)

	if err := utils.RunOnce("setSSHKey", p.workingDir, p.log, p.setSSHKey,
		seedReconfiguration.SSHKey, sshKeyEarlyAccessFile); err != nil {
		return fmt.Errorf("failed to run once setSSHKey for post pivot: %w", err)
	}

	if err := utils.RunOnce("pull-secret", p.workingDir, p.log, p.createPullSecretFile,
		seedReconfiguration.PullSecret, common.ImageRegistryAuthFile); err != nil {
		return fmt.Errorf("failed to run once pull-secret for post pivot: %w", err)
	}

	if err := p.networkConfiguration(ctx, seedReconfiguration); err != nil {
		return fmt.Errorf("failed to configure networking, err: %w", err)
	}

	if err := validateIPAndMachineNetworkConsistency(seedClusterInfo, seedReconfiguration); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	if seedReconfiguration.APIVersion != clusterconfig_api.SeedReconfigurationVersion {
		return fmt.Errorf("unsupported seed reconfiguration version %d", seedReconfiguration.APIVersion)
	}

	if err := p.setProxyAndProxyStatus(seedReconfiguration, seedClusterInfo); err != nil {
		return fmt.Errorf("failed to set no proxy: %w", err)
	}

	// NOTE: This must be done before we run recert, as otherwise recert could
	// fail to pull in absence of a precached recert image
	if err := p.establishEarlyCertificateTrust(seedReconfiguration); err != nil {
		return fmt.Errorf("failed copy cluster config files: %w", err)
	}

	if err := utils.RunOnce("recert", p.workingDir, p.log, p.recert, ctx, seedReconfiguration, seedClusterInfo); err != nil {
		return fmt.Errorf("failed to run once recert for post pivot: %w", err)
	}

	if err := p.applyServerSSHKeys(seedReconfiguration.ServerSSHKeys); err != nil {
		return fmt.Errorf("failed to apply server ssh keys: %w", err)
	}

	client, err := utils.CreateKubeClient(p.scheme, p.kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to create k8s client, err: %w", err)
	}

	// init new logger for CreateDynamicClient (others are using logrus)
	opts := zap.Options{Development: true}
	l := zap.New(zap.UseFlagOptions(&opts))
	dynamicClient, err := utils.CreateDynamicClient(p.kubeconfig, false, l.WithName("post-pivot-dynamic-client"))
	if err != nil {
		return fmt.Errorf("failed to create k8s dynamic client, err: %w", err)
	}

	if _, err := p.ops.SystemctlAction("enable", "kubelet", "--now"); err != nil {
		return fmt.Errorf("failed to enable kubelet: %w", err)
	}
	p.waitForApi(ctx, client)

	if err := p.deleteAllOldMirrorResources(ctx, client); err != nil {
		return fmt.Errorf("failed to all old mirror resources: %w", err)
	}

	mPath := path.Join(p.workingDir, common.ClusterConfigDir, common.ManifestsDir)
	if err := p.applyManifests(ctx, mPath, dynamicClient, client.RESTMapper()); err != nil {
		return fmt.Errorf("failed apply manifests: %w", err)
	}

	emPath := path.Join(p.workingDir, common.ExtraManifestsDir)
	if _, err := os.Stat(emPath); err == nil {
		if err := p.applyManifests(ctx, emPath, dynamicClient, client.RESTMapper()); err != nil {
			return fmt.Errorf("failed apply extra manifests: %w", err)
		}
	}

	if err := p.changeRegistryInCSVDeployment(ctx, client, seedReconfiguration, seedClusterInfo); err != nil {
		return fmt.Errorf("failed change registry in CSV deployment: %w", err)
	}

	// Restore OADP secrets and DPA if exists
	if err := p.restoreOadpSecrets(ctx, client); err != nil {
		return fmt.Errorf("failed to restore OADP secrets: %w", err)
	}
	if err := p.restoreOadpDataProtectionApplication(ctx, client); err != nil {
		return fmt.Errorf("failed to restore OADP DataProtectionApplication: %w", err)
	}

	if err := utils.RunOnce("set_cluster_id", p.workingDir, p.log, p.setNewClusterID, ctx, client, seedReconfiguration); err != nil {
		return fmt.Errorf("failed to run once set_cluster_id for post pivot: %w", err)
	}

	if err := utils.RunOnce("set_node_labels", p.workingDir, p.log, p.setNodeLabels, ctx, client, seedReconfiguration.NodeLabels, 5*time.Minute); err != nil {
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

	if _, err := p.ops.RunInHostNamespace("podman", "image", "exists", seedClusterInfo.RecertImagePullSpec); err != nil {
		ctxWithTimeout, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		_ = wait.PollUntilContextCancel(ctxWithTimeout, time.Second, true, func(ctx context.Context) (bool, error) {
			p.log.Info("pulling recert image")
			command := "podman"
			if seedReconfiguration.Proxy != nil {
				command = fmt.Sprintf("HTTP_PROXY=%s HTTPS_PROXY=%s NO_PROXY=%s %s",
					seedReconfiguration.Proxy.HTTPProxy, seedReconfiguration.Proxy.HTTPSProxy,
					seedReconfiguration.Proxy.NoProxy, command)
			}
			if _, err := p.ops.RunBashInHostNamespace(command, "pull", "--authfile", common.ImageRegistryAuthFile, seedClusterInfo.RecertImagePullSpec); err != nil {
				p.log.Warnf("failed to pull recert image, will retry, err: %s", err.Error())
				return false, nil
			}
			return true, nil
		})
	}

	err := p.ops.RecertFullFlow(seedClusterInfo.RecertImagePullSpec, p.authFile,
		path.Join(p.workingDir, recert.RecertConfigFile),
		nil,
		func() error { return p.postRecertCommands() },
		"-v", fmt.Sprintf("%s:%s", p.workingDir, p.workingDir))
	if err != nil {
		return fmt.Errorf("failed recert full flow: %w", err)
	}

	return nil
}

func (p *PostPivot) applyServerSSHKeys(serverSSHKeys []clusterconfig_api.ServerSSHKey) error {
	if len(serverSSHKeys) == 0 {
		p.log.Infof("No server ssh keys were provided, fresh keys already regenerated by recert, skipping")
		return nil
	}

	if err := removeOldServerSSHKeys(); err != nil {
		return fmt.Errorf("failed to remove old server SSH keys: %w", err)
	}

	for _, serverSSHKey := range serverSSHKeys {
		if err := createServerSSHKeyFile(serverSSHKey); err != nil {
			return fmt.Errorf("failed to create server SSH key file: %w", err)
		}
	}

	return nil
}

func removeOldServerSSHKeys() error {
	files, err := filepath.Glob("/etc/ssh/ssh_host_*_key*")
	if err != nil {
		return fmt.Errorf("failed to glob existing server SSH keys: %w", err)
	}

	for _, file := range files {
		if err := os.Remove(file); err != nil {
			return fmt.Errorf("failed to remove existing server SSH key %s: %w", file, err)
		}
	}
	return nil
}

func createServerSSHKeyFile(serverSSHKey clusterconfig_api.ServerSSHKey) error {
	mode := 0o600
	if strings.HasSuffix(serverSSHKey.FileName, ".pub") {
		mode = 0o644
	}

	fullKeyPath := filepath.Join(common.SSHServerKeysDirectory, serverSSHKey.FileName)
	// nolint: gosec
	if err := os.WriteFile(fullKeyPath, []byte(serverSSHKey.FileContent), fs.FileMode(mode)); err != nil {
		return fmt.Errorf("failed to write server SSH key %s: %w", fullKeyPath, err)
	}

	if err := label.SetFileLabel(fullKeyPath, "system_u:object_r:sshd_key_t:s0"); err != nil {
		return fmt.Errorf("failed to set selinux label on server SSH key %s: %w", fullKeyPath, err)
	}

	const rootUID = 0
	const rootGID = 0
	if err := os.Chown(fullKeyPath, rootUID, rootGID); err != nil {
		return fmt.Errorf("failed to change ownership of server SSH key %s to root gid/uid: %w", fullKeyPath, err)
	}

	return nil
}

func (p *PostPivot) setProxyAndProxyStatus(seedReconfig *clusterconfig_api.SeedReconfiguration, seedClusterInfo *seedclusterinfo.SeedClusterInfo) error {
	if seedReconfig.Proxy != nil != seedClusterInfo.HasProxy {
		return fmt.Errorf("seedReconfig and seedClusterInfo proxy configuration mismatch")
	}

	// In case of no proxy, we don't need to set it
	// In case of status was set it means that we already have all relevant info as in IBU case
	// and we should not change anything
	if seedReconfig.Proxy == nil || seedReconfig.StatusProxy != nil {
		return nil
	}

	set := sets.NewString(
		"127.0.0.1",
		"localhost",
		".svc",
		".cluster.local",
		fmt.Sprintf("api-int.%s.%s", seedReconfig.ClusterName, seedReconfig.BaseDomain),
	)

	machineNetworks := getMachineNetworksFromSeedReconfig(seedReconfig)
	if len(machineNetworks) == 0 {
		return fmt.Errorf("machineNetworks is empty, must be provided in case of proxy")
	}

	for _, machineNetwork := range machineNetworks {
		set.Insert(machineNetwork)
	}

	for _, nss := range append(seedClusterInfo.ServiceNetworks, seedClusterInfo.ClusterNetworks...) {
		set.Insert(nss)
	}

	// nolint: gocritic
	if len(seedReconfig.Proxy.NoProxy) > 0 {
		for _, userValue := range strings.Split(seedReconfig.Proxy.NoProxy, ",") {
			if userValue != "" {
				set.Insert(userValue)
			}
		}
	}
	seedReconfig.Proxy.NoProxy = strings.Join(set.List(), ",")
	// as we are using same logic as network operator
	// proxy and proxy status are the same
	seedReconfig.StatusProxy = seedReconfig.Proxy

	return nil
}

func (p *PostPivot) postRecertCommands() error {
	// changing seed ip to new ip in all static pod files
	_, err := p.ops.RunBashInHostNamespace("update-ca-trust")
	if err != nil {
		return fmt.Errorf("failed to run update-ca-trust after recert: %w", err)
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

func (p *PostPivot) applyManifests(ctx context.Context, mPath string, dynamicClient dynamic.Interface, restMapper meta.RESTMapper) error {
	p.log.Infof("Applying manifests from %s", mPath)
	mFiles, err := os.ReadDir(mPath)
	if err != nil {
		return fmt.Errorf("failed to read dir for extra manifests in %s: %w", mPath, err)
	}
	if len(mFiles) == 0 {
		p.log.Infof("No manifests to apply were found, skipping")
		return nil
	}

	for _, mFile := range mFiles {
		decoder, err := utils.GetYamlOrJsonDecoder(filepath.Join(mPath, mFile.Name()))
		if err != nil {
			return fmt.Errorf("failed to read manifest %s: %w", mFile.Name(), err)
		}
		for {
			var obj map[string]interface{}
			// decode until EOF since we might get multiple definitions in a single file
			err := decoder.Decode(&obj)
			if errors.Is(err, io.EOF) {
				// End of file, break the loop
				break
			}
			if err != nil {
				return fmt.Errorf("failed to decode manifest %s: %w", mFile.Name(), err)
			}
			err = p.handleManifest(ctx, mPath, dynamicClient, restMapper, obj, mFile)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *PostPivot) handleManifest(ctx context.Context, mPath string, dynamicClient dynamic.Interface, restMapper meta.RESTMapper, obj map[string]interface{}, mFile os.DirEntry) error {
	if manifests, ok := obj["items"]; ok {
		for _, m := range manifests.([]interface{}) {
			manifest := unstructured.Unstructured{}
			manifest.Object = m.(map[string]interface{})
			if err := extramanifest.ApplyExtraManifest(ctx, dynamicClient, restMapper, &manifest, false); err != nil {
				return fmt.Errorf("failed to apply manifest: %w", err)
			}
		}
	} else {
		manifest := unstructured.Unstructured{}
		manifest.Object = obj
		if err := extramanifest.ApplyExtraManifest(ctx, dynamicClient, restMapper, &manifest, false); err != nil {
			return fmt.Errorf("failed to apply manifest: %w", err)
		}
	}

	p.log.Infof("manifest applied: %s", filepath.Join(mPath, mFile.Name()))
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

func (p *PostPivot) restoreOadpSecrets(ctx context.Context, client runtimeclient.Client) error {
	p.log.Infof("Restoring OADP secrets from %s", backuprestore.OadpSecretPath)

	secretYamlDir := backuprestore.OadpSecretPath
	secretYamls, err := os.ReadDir(secretYamlDir)
	if err != nil {
		if os.IsNotExist(err) {
			p.log.Infof("No OADP secrets to restore")
			return nil
		}
		return fmt.Errorf("failed to read OADP secret dir %s: %w", secretYamlDir, err)
	}
	if len(secretYamls) == 0 {
		p.log.Infof("No OADP secrets to restore")
		return nil
	}

	for _, secretYaml := range secretYamls {
		secretYamlPath := filepath.Join(secretYamlDir, secretYaml.Name())
		if secretYaml.IsDir() {
			continue
		}

		secret := &corev1.Secret{}
		if err := utils.ReadYamlOrJSONFile(secretYamlPath, secret); err != nil {
			return fmt.Errorf("failed to read secret from %s: %w", secretYamlPath, err)
		}

		if err := backuprestore.CreateOrUpdateSecret(ctx, secret, client); err != nil {
			return fmt.Errorf("failed to restore OADP secret: %w", err)
		}
		p.log.Infof("OADP Secret %s applied", secret.GetName())
	}
	return nil
}

func (p *PostPivot) restoreOadpDataProtectionApplication(ctx context.Context, client runtimeclient.Client) error {
	p.log.Infof("Restoring OADP DataProtectionApplication from %s", backuprestore.OadpDpaPath)
	dpa, err := backuprestore.ReadOadpDataProtectionApplication(backuprestore.OadpDpaPath)
	if err != nil {
		return fmt.Errorf("failed to get stored DataProtectionApplication: %w", err)
	}

	if dpa == nil {
		p.log.Infof("No OADP DataProtectionApplication to restore")
		return nil
	}

	if err := backuprestore.CreateOrUpdateDataProtectionAppliation(ctx, dpa, client); err != nil {
		return fmt.Errorf("failed to restore DataProtectionApplication: %w", err)
	}
	p.log.Infof("OADP DataProtectionApplication %s applied", dpa.GetName())
	return nil
}

// establishEarlyCertificateTrust writes the user ca bundle to the trust store
// and runs update-ca-trust to trust the required certificates before we pull
// the recert image. This is required because the recert image is not
// necessarily cached on the host and we need to pull it from the registry
// which might require trusting the registry's certificate. This is actually
// somewhat redundant because recert itself already does the same thing (among
// other things), but we need to do it before we can even run recert.
func (p *PostPivot) establishEarlyCertificateTrust(seedReconfiguration *clusterconfig_api.SeedReconfiguration) error {
	if seedReconfiguration.AdditionalTrustBundle.UserCaBundle != "" {
		if err := os.WriteFile(common.CABundleFilePath, []byte(seedReconfiguration.AdditionalTrustBundle.UserCaBundle), 0o600); err != nil {
			return fmt.Errorf("failed to write user ca bundle to %s: %w", common.CABundleFilePath, err)
		}
	}

	_, err := p.ops.RunBashInHostNamespace("update-ca-trust")
	if err != nil {
		return fmt.Errorf("failed to run update-ca-trust after early certificate trust: %w", err)
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

	mirrorRegistrySources, err := utils.GetMirrorRegistrySourceRegistries(ctx, client)
	if err != nil {
		return err //nolint:wrapcheck
	}

	// in case we should not override we can skip
	if shouldOverride, err := utils.ShouldOverrideSeedRegistry(
		seedClusterInfo.MirrorRegistryConfigured, seedClusterInfo.ReleaseRegistry, mirrorRegistrySources); !shouldOverride || err != nil {
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
	if len(seedReconfiguration.NodeIPs) == 0 {
		return fmt.Errorf("at least one IP is required to set up DNSMASQ")
	}
	primaryIP := seedReconfiguration.NodeIPs[0]
	p.log.Infof("Setting new dnsmasq and forcedns dispatcher script configuration for %s", primaryIP)

	config := []string{
		fmt.Sprintf("SNO_CLUSTER_NAME_OVERRIDE=%s", seedReconfiguration.ClusterName),
		fmt.Sprintf("SNO_BASE_DOMAIN_OVERRIDE=%s", seedReconfiguration.BaseDomain),
		fmt.Sprintf("SNO_DNSMASQ_IP_OVERRIDE=%s", primaryIP),
	}

	if err := os.WriteFile(dnsmasqOverridesFiles, []byte(strings.Join(config, "\n")), 0o600); err != nil {
		return fmt.Errorf("failed to configure dnsmasq and forcedns dispatcher script, err %w", err)
	}

	return nil
}

// setNodeIPsIfNotProvided will run nodeip configuration service on demand in case seedReconfiguration node ip is empty
// nodeip-configuration service is in charge of setting kubelet and crio ip, this ip we will take as NodeIPs
func (p *PostPivot) setNodeIPsIfNotProvided(
	ctx context.Context,
	seedReconfiguration *clusterconfig_api.SeedReconfiguration,
) error {
	if len(seedReconfiguration.NodeIPs) != 0 {
		p.log.Infof("Node IPs are already set to %v, skipping", seedReconfiguration.NodeIPs)
		return nil
	}

	// The existence of the primary IP file is the condition for whether the nodeip-configuration
	// service has already run. While the service also updates the kubelet config file with the chosen IPs, that file
	// might exist beforehand. The primary IP file is created only *after* the IPs are written
	// to the kubelet config file, making it a reliable indicator of completion.
	// We use the primary IP file as a definitive check, then read from the kubelet config file.
	// See: https://github.com/openshift/baremetal-runtimecfg/blob/e898adc576b343a214aff860e349f9bba3a125d4/cmd/runtimecfg/node-ip.go#L107
	// and: https://github.com/openshift/baremetal-runtimecfg/blob/e898adc576b343a214aff860e349f9bba3a125d4/cmd/runtimecfg/node-ip.go#L148
	if _, err := os.Stat(nodePrimaryIPFile); err != nil {
		_, err := p.ops.SystemctlAction("start", "nodeip-configuration")
		if err != nil {
			return fmt.Errorf("failed to start nodeip-configuration service, err %w", err)
		}

		p.log.Info("Start waiting for nodeip service to choose node ip")
		_ = wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (done bool, err error) {
			if _, err := os.Stat(nodePrimaryIPFile); err != nil {
				return false, nil
			}
			return true, nil
		})
	}

	b, err := os.ReadFile(kubeletConfigFile)
	if err != nil {
		return fmt.Errorf("failed to read kubelet config from %s, err: %w", kubeletConfigFile, err)
	}

	content := string(b)
	nodeIPs, err := parseKubeletNodeIPs(content)
	if err != nil {
		return fmt.Errorf("failed to parse node IPs from kubelet config: %w", err)
	}

	p.log.Infof("Node IPs are set to %v", nodeIPs)

	// the IPs slice is ordered such that the primary IP is the first one.
	// It is important for both DNSMASQ and recert.
	seedReconfiguration.NodeIPs = nodeIPs

	return nil
}

// parseKubeletNodeIPs parses KUBELET_NODE_IP and KUBELET_NODE_IPS from kubelet service config
func parseKubeletNodeIPs(content string) ([]string, error) {
	var nodeIPs []string

	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "Environment=") {
			// Parse environment variables from the line
			// Format: Environment="KUBELET_NODE_IP=192.168.127.172" "KUBELET_NODE_IPS=192.168.127.172,1001:db9::3c"
			if match := regexp.MustCompile(`"KUBELET_NODE_IPS=([^"]+)"`).FindStringSubmatch(line); len(match) > 1 {
				nodeIPsStr := match[1]
				nodeIPs = strings.Split(nodeIPsStr, ",")
				for i, ip := range nodeIPs {
					nodeIPs[i] = strings.TrimSpace(ip)
				}
			} else if match := regexp.MustCompile(`"KUBELET_NODE_IP=([^"]+)"`).FindStringSubmatch(line); len(match) > 1 {
				// Fall back to single IP if KUBELET_NODE_IPS is not present
				nodeIPs = []string{strings.TrimSpace(match[1])}
			}
		}
	}

	if len(nodeIPs) == 0 {
		return nil, fmt.Errorf("no KUBELET_NODE_IP or KUBELET_NODE_IPS found in kubelet config")
	}

	return nodeIPs, nil
}

// setSSHKey  sets ssh public key provided by user in 2 operations:
// 1. as file in order to give early access to the node
// 2. creates 2 machine configs in manifests dir that will be applied when cluster is up
func (p *PostPivot) setSSHKey(sshKey, sshKeyFile string) error {
	if sshKey == "" {
		p.log.Infof("No ssh public key was provided, skipping")
		return nil
	}

	p.log.Infof("Creating file %s with ssh keys for early connection", sshKeyFile)
	if err := os.WriteFile(sshKeyFile, []byte(sshKey), 0o600); err != nil {
		return fmt.Errorf("failed to write ssh key to file, err %w", err)
	}

	p.log.Infof("Setting %s user ownership on %s", userCore, sshKeyFile)
	if _, err := p.ops.RunInHostNamespace("chown", userCore, sshKeyFile); err != nil {
		return fmt.Errorf("failed to set %s user ownership on %s, err :%w", userCore, sshKeyFile, err)
	}

	return p.createSSHKeyMachineConfigs(sshKey)
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
		fname := fmt.Sprintf(sshMachineConfig, role) + ".json"
		if err := utils.MarshalToFile(mc, path.Join(p.workingDir, common.ClusterConfigDir,
			common.ManifestsDir, fname)); err != nil {
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

// setHostname set provided hostname in case it was provided, in case it was not provided we will get hostname from kernel
// retuning error in case hostname is localhost
func (p *PostPivot) setHostname(hostname string) (string, error) {
	if hostname != "" && hostname != localhost {
		p.log.Infof("Setting new hostname %s", hostname)
		if _, err := p.ops.RunInHostNamespace("hostnamectl", "set-hostname", hostname); err != nil {
			return "", fmt.Errorf("failed to set hostname %s, err %w", hostname, err)
		}
		return hostname, nil
	}
	if hostname == localhost {
		return "", fmt.Errorf("provided hostname is %s and it is invalid, please provide a valid hostname", localhost)
	}

	osHostname, err := p.ops.GetHostname()
	if err != nil {
		return "", fmt.Errorf("failed to get hostname from os, err %w", err)
	}
	if osHostname == localhost {
		return "", fmt.Errorf("os hostname is %s and it is invalid, please provide a valid hostname", localhost)
	}
	p.log.Infof("No hostname was provided, taking %s as hostname from os", osHostname)
	return osHostname, nil
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

// setNodeIpHint writes provided subnets to nodeIPHintFile
func (p *PostPivot) setNodeIpHint(machineNetworks []string) error {
	if len(machineNetworks) == 0 {
		p.log.Infof("No machine network was provided, skipping setting node ip hint")
		return nil
	}

	var ips []string
	for _, machineNetwork := range machineNetworks {
		ip, _, err := net.ParseCIDR(machineNetwork)
		if err != nil {
			return fmt.Errorf("failed to parse machine network %s, err: %w", machineNetwork, err)
		}
		ips = append(ips, ip.String())
	}

	// Join IPs with spaces for dual-stack support: "KUBELET_NODEIP_HINT=<ip1> <ip2>"
	// This is used as an argument in
	// https://github.com/openshift/machine-config-operator/blob/5a97a9a29a99e93f15b55242fefb7b41da4a4a82/templates/common/_base/units/nodeip-configuration.service.yaml#L34
	// which is later used in
	// https://github.com/openshift/baremetal-runtimecfg/blob/e898adc576b343a214aff860e349f9bba3a125d4/cmd/runtimecfg/node-ip.go#L112
	ipsHint := strings.Join(ips, " ")
	p.log.Infof("Writing machine network IPs %s into %s", ipsHint, nodeIPHintFile)

	if err := os.WriteFile(nodeIPHintFile, []byte(fmt.Sprintf("KUBELET_NODEIP_HINT=%s", ipsHint)), 0o600); err != nil {
		return fmt.Errorf("failed to write machine network IPs %s to %s, err %w", ipsHint, nodeIPHintFile, err)
	}
	return nil
}

// networkConfiguration is on charge of configuring network prior installation process.
// This function should include all network configurations of post-pivot flow.
// It's logic currently includes:
// 1. Copying provided nmconnection files to NM sysconnections to setup network provided by user
// 2. Apply provided NMstate configurations
// 3. In case ip was not provided by user we should run set ip logic that can be found in setNodeIPIfNotProvided
// 4. Override seed dnsmasq params
// 5. Restart NM and dnsmasq in order to apply provided configurations
func (p *PostPivot) networkConfiguration(ctx context.Context, seedReconfiguration *clusterconfig_api.SeedReconfiguration) error {
	var err error
	if err := p.copyNMConnectionFiles(
		path.Join(p.workingDir, common.NetworkDir, "system-connections"), nmConnectionFolder); err != nil {
		return err
	}

	if err := utils.RunOnce("apply-static-network", p.workingDir, p.log, p.applyNMStateConfiguration,
		seedReconfiguration); err != nil {
		return fmt.Errorf("failed to apply static network: %w", err)
	}

	if _, err := p.ops.SystemctlAction("restart", nmService); err != nil {
		return fmt.Errorf("failed to restart network manager service, err %w", err)
	}

	seedReconfiguration.Hostname, err = p.setHostname(seedReconfiguration.Hostname)
	if err != nil {
		return err
	}

	if err := p.setNodeIpHint(getMachineNetworksFromSeedReconfig(seedReconfiguration)); err != nil {
		return err
	}

	if err := p.setNodeIPsIfNotProvided(ctx, seedReconfiguration); err != nil {
		return err
	}

	if err := p.setDnsMasqConfiguration(seedReconfiguration, dnsmasqOverrides); err != nil {
		return err
	}

	if _, err := p.ops.SystemctlAction("restart", dnsmasqService); err != nil {
		return fmt.Errorf("failed to restart dnsmasq service, err %w", err)
	}

	return nil
}

func (p *PostPivot) setNodeLabels(ctx context.Context, client runtimeclient.Client, nodeLabels map[string]string, timeout time.Duration) error {
	labels := map[string]string{}
	// no need to set default labels, we search only for custom ones
	defaultLabels := map[string]string{
		"beta.kubernetes.io/arch":               "",
		"beta.kubernetes.io/os":                 "",
		"kubernetes.io/arch":                    "",
		"kubernetes.io/hostname":                "",
		"kubernetes.io/os":                      "",
		"node-role.kubernetes.io/control-plane": "",
		"node-role.kubernetes.io/master":        "",
		"node.openshift.io/os_id":               "",
		"node-role.kubernetes.io/worker":        "",
	}
	for k, v := range nodeLabels {
		if _, ok := defaultLabels[k]; !ok {
			labels[k] = v
		}
	}

	if len(labels) == 0 {
		p.log.Infof("No custom node labels were provided, skipping")
		return nil
	}

	labelsAsString, err := json.Marshal(labels)
	if err != nil {
		return fmt.Errorf("failed to marshal node labels %w", err)
	}
	p.log.Infof("Patching node with new labels %s", labelsAsString)
	data := []byte(`{"metadata": {"labels": ` + string(labelsAsString) + `}}`)
	err = wait.PollUntilContextTimeout(ctx, 1*time.Second, timeout, false, func(ctx context.Context) (done bool, err error) {

		node, err := utils.GetSNOMasterNode(ctx, client)
		if err != nil {
			p.log.Warnf("failed to get node, will retry, err: %v", err)
			return false, nil
		}
		if err := client.Patch(context.Background(), node, runtimeclient.RawPatch(types.MergePatchType, data)); err != nil {
			p.log.Warnf("failed to patch node with new labels, will retry, err: %v", err)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("failed to patch node with new labels %w", err)
	}
	return nil
}

// getMachineNetworksFromSeedReconfig returns machine networks with backward compatibility
func getMachineNetworksFromSeedReconfig(seedReconfig *clusterconfig_api.SeedReconfiguration) []string {
	// Prefer the new list field if it's populated
	if len(seedReconfig.MachineNetworks) > 0 {
		return seedReconfig.MachineNetworks
	}
	// Fall back to the old single field for backward compatibility
	if seedReconfig.MachineNetwork != "" {
		return []string{seedReconfig.MachineNetwork}
	}
	return []string{}
}

// validateIPAndMachineNetworkConsistency validates the amount and family order of node IPs and machine networks
// across seed cluster info and seed reconfiguration, according to the following rules:
// 1) seedClusterInfo.NodeIPs and seedReconfiguration.NodeIPs must have the same length, and at each index the IP family must match
// 2) seedReconfiguration.MachineNetworks length must equal seedReconfiguration.NodeIPs length, and at each index the family must match
// 3) If seedClusterInfo.MachineNetworks is non-empty, it must have the same length as seedReconfiguration.MachineNetworks, and at each index the family must match
func validateIPAndMachineNetworkConsistency(seedClusterInfo *seedclusterinfo.SeedClusterInfo, seedReconfiguration *clusterconfig_api.SeedReconfiguration) error {
	seedIPs := seedClusterInfo.NodeIPs
	reconfigIPs := seedReconfiguration.NodeIPs

	if len(seedIPs) == 0 || len(reconfigIPs) == 0 {
		return fmt.Errorf("node IPs must be provided in both seedClusterInfo and seedReconfiguration")
	}

	if len(seedIPs) != len(reconfigIPs) {
		return fmt.Errorf("node IPs count mismatch: seed has %d, reconfiguration has %d", len(seedIPs), len(reconfigIPs))
	}

	for i := range seedIPs {
		seedFam, err := ipFamilyFromIP(seedIPs[i])
		if err != nil {
			return fmt.Errorf("invalid seed node IP at index %d: %w", i, err)
		}
		reconfFam, err := ipFamilyFromIP(reconfigIPs[i])
		if err != nil {
			return fmt.Errorf("invalid reconfiguration node IP at index %d: %w", i, err)
		}
		if seedFam != reconfFam {
			return fmt.Errorf("node IP family mismatch at index %d: seed has %s, reconfiguration has %s", i, seedFam, reconfFam)
		}
	}

	// Rule 2: machine networks in reconfiguration must match the number and order (by family) of IPs
	reconfigMNs := getMachineNetworksFromSeedReconfig(seedReconfiguration)
	if len(reconfigMNs) != len(reconfigIPs) {
		return fmt.Errorf("machineNetworks count (%d) must equal node IPs count (%d)", len(reconfigMNs), len(reconfigIPs))
	}
	for i := range reconfigMNs {
		mnFam, err := ipFamilyFromCIDR(reconfigMNs[i])
		if err != nil {
			return fmt.Errorf("invalid reconfiguration machineNetwork at index %d: %w", i, err)
		}
		ipFam, _ := ipFamilyFromIP(reconfigIPs[i])
		if mnFam != ipFam {
			return fmt.Errorf("machineNetwork family at index %d (%s) must match node IP family (%s)", i, mnFam, ipFam)
		}
	}

	// Rule 3: if seedClusterInfo has machine networks, validate count and family order against reconfiguration
	if len(seedClusterInfo.MachineNetworks) > 0 {
		if len(seedClusterInfo.MachineNetworks) != len(reconfigMNs) {
			return fmt.Errorf("seed machineNetworks count (%d) must equal reconfiguration machineNetworks count (%d)", len(seedClusterInfo.MachineNetworks), len(reconfigMNs))
		}
		for i := range seedClusterInfo.MachineNetworks {
			seedMNFam, err := ipFamilyFromCIDR(seedClusterInfo.MachineNetworks[i])
			if err != nil {
				return fmt.Errorf("invalid seed machineNetwork at index %d: %w", i, err)
			}
			reconfMNFam, err := ipFamilyFromCIDR(reconfigMNs[i])
			if err != nil {
				return fmt.Errorf("invalid reconfiguration machineNetwork at index %d: %w", i, err)
			}
			if seedMNFam != reconfMNFam {
				return fmt.Errorf("machineNetwork family mismatch at index %d: seed has %s, reconfiguration has %s", i, seedMNFam, reconfMNFam)
			}
		}
	}

	return nil
}

func ipFamilyFromIP(ipStr string) (string, error) {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return "", fmt.Errorf("invalid IP %q", ipStr)
	}
	if ip.To4() != nil {
		return "ipv4", nil
	}
	return "ipv6", nil
}

func ipFamilyFromCIDR(cidr string) (string, error) {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("invalid CIDR %q: %w", cidr, err)
	}
	if ip.To4() != nil {
		return "ipv4", nil
	}
	return "ipv6", nil
}
