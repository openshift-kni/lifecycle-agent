package clusterconfig

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
	v1 "github.com/openshift/api/config/v1"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="apps",resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=clusterversions,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=networks,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=imagedigestmirrorsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=operator.openshift.io,resources=imagecontentsourcepolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=proxies,verbs=get;list;watch
// +kubebuilder:rbac:groups=machineconfiguration.openshift.io,resources=machineconfigs,verbs=get;list;watch

const (
	manifestDir = "manifests"

	pullSecretName = "pull-secret"

	idmsFileName  = "image-digest-mirror-set.json"
	icspsFileName = "image-content-source-policy-list.json"

	// ssh authorized keys file created by mco from ssh machine configs
	sshKeyFile = "/home/core/.ssh/authorized_keys.d/ignition"
)

var (
	hostPath                = common.Host
	listOfNetworkFilesPaths = []string{
		common.NMConnectionFolder,
	}
)

type UpgradeClusterConfigGatherer interface {
	FetchClusterConfig(ctx context.Context, ostreeVarDir string) error
	FetchLvmConfig(ctx context.Context, ostreeVarDir string) error
}

// UpgradeClusterConfigGather Gather ClusterConfig attributes from the kube-api
type UpgradeClusterConfigGather struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// FetchClusterConfig collects the current cluster's configuration and write it as JSON files into
// given filesystem directory.
func (r *UpgradeClusterConfigGather) FetchClusterConfig(ctx context.Context, ostreeVarDir string) error {
	r.Log.Info("Fetching cluster configuration")

	clusterConfigPath, err := r.configDir(ostreeVarDir)
	if err != nil {
		return err
	}
	manifestsDir := filepath.Join(clusterConfigPath, manifestDir)

	if err := r.fetchIDMS(ctx, manifestsDir); err != nil {
		return err
	}

	if err := r.fetchClusterInfo(ctx, clusterConfigPath); err != nil {
		return err
	}
	if err := r.fetchICSPs(ctx, manifestsDir); err != nil {
		return err
	}
	if err := r.fetchNetworkConfig(ostreeVarDir); err != nil {
		return err
	}

	r.Log.Info("Successfully fetched cluster configuration")
	return nil
}

func (r *UpgradeClusterConfigGather) getPullSecret(ctx context.Context) (string, error) {
	r.Log.Info("Fetching pull-secret")
	sd, err := utils.GetSecretData(ctx, common.PullSecretName, common.OpenshiftConfigNamespace, corev1.DockerConfigJsonKey, r.Client)
	if err != nil {
		return "", fmt.Errorf("failed to get pull-secret: %w", err)
	}
	return sd, nil
}

func (r *UpgradeClusterConfigGather) getSSHPublicKey() (string, error) {
	sshKey, err := os.ReadFile(filepath.Join(hostPath, sshKeyFile))
	if err != nil {
		return "", fmt.Errorf("failed to read sshKey: %w", err)
	}
	return string(sshKey), err
}

// getChronyConfig reads the chrony configuration from the node
// in case for some reason chrony configuration is not present on the node, it will return an empty string
// possible reasons for missing chrony configuration:
// cluster is not installed with assisted-service or user removed the configuration
func (r *UpgradeClusterConfigGather) getChronyConfig() (string, error) {
	chronyConfig, err := os.ReadFile(filepath.Join(hostPath, common.ChronyConfig))
	if os.IsNotExist(err) {
		r.Log.Info(fmt.Sprintf("chrony configuration %s doesn't exist, "+
			"skipping copy of it", common.ChronyConfig))
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to chrony config from node: %w", err)
	}
	return string(chronyConfig), err
}

func (r *UpgradeClusterConfigGather) getInfraID(ctx context.Context) (string, error) {
	infra, err := utils.GetInfrastructure(ctx, r.Client)
	if err != nil {
		return "", fmt.Errorf("failed to get infrastructure: %w", err)
	}

	return infra.Status.InfrastructureName, nil
}

func (r *UpgradeClusterConfigGather) GetKubeadminPasswordHash(ctx context.Context) (string, error) {
	kubeadminPasswordHash, err := utils.GetSecretData(ctx, "kubeadmin", "kube-system", "kubeadmin", r.Client)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			return "", fmt.Errorf("failed to get kubeadmin password hash: %w", err)
		}

		// No kubeadmin password secret found, this is fine (see
		// https://docs.openshift.com/container-platform/4.14/authentication/remove-kubeadmin.html)
		//
		// An empty string will signal to the seed LCA that it should delete
		// the kubeadmin password secret of the seed cluster, to ensure we
		// don't accept a password in the reconfigured seed.
		return "", nil

	}

	return kubeadminPasswordHash, nil
}

type ServerSSHKey struct {
	FileName    string
	FileContent string
}

func (r *UpgradeClusterConfigGather) GetServerSSHKeys(ctx context.Context) ([]ServerSSHKey, error) {
	sshKeys := []ServerSSHKey{}

	matches, err := filepath.Glob(filepath.Join(hostPath, common.SSHServerKeysDirectory, "ssh_host_*_key*"))
	if err != nil {
		return nil, fmt.Errorf("failed to glob server SSH keys: %w", err)
	}

	for _, match := range matches {
		fileContent, err := os.ReadFile(match)
		if err != nil {
			return nil, fmt.Errorf("failed to read server SSH key file %s: %w", match, err)
		}

		sshKeys = append(sshKeys, ServerSSHKey{
			FileName:    filepath.Base(match),
			FileContent: string(fileContent),
		})
	}

	return sshKeys, nil
}

type AdditionalTrustBundle struct {
	// The contents of the "user-ca-bundle" configmap in the "openshift-config" namespace
	UserCaBundle string `json:"userCaBundle"`

	// The Proxy CR trustedCA configmap name
	ProxyConfigmapName string `json:"proxyConfigmapName"`

	// The contents of the ProxyConfigmapName configmap. Must equal
	// UserCaBundle if ProxyConfigmapName is "user-ca-bundle"
	ProxyConfigmapBundle string `json:"proxyConfigmapBundle"`
}

func newAdditionalTrustBundle(userCaBundle, proxyConfigmapName, proxyConfigmapBundle string) (*AdditionalTrustBundle, error) {
	if proxyConfigmapName == common.ClusterAdditionalTrustBundleName && userCaBundle != proxyConfigmapBundle {
		return nil, fmt.Errorf("proxyConfigmapName is %s, but userCaBundle and proxyConfigmapBundle differ", common.ClusterAdditionalTrustBundleName)
	}

	return &AdditionalTrustBundle{
		UserCaBundle:         userCaBundle,
		ProxyConfigmapName:   proxyConfigmapName,
		ProxyConfigmapBundle: proxyConfigmapBundle,
	}, nil
}

func (r *UpgradeClusterConfigGather) GetAdditionalTrustBundle(ctx context.Context) (*AdditionalTrustBundle, error) {
	clusterAdditionalTrustBundle, err := utils.GetAdditionalTrustBundleFromConfigmap(ctx, r.Client, common.ClusterAdditionalTrustBundleName)
	if err != nil {
		return nil, fmt.Errorf("failed to get additional trust bundle from configmap: %w", err)
	}

	proxy := v1.Proxy{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: common.OpenshiftProxyCRName}, &proxy); err != nil {
		return nil, fmt.Errorf("failed to get proxy: %w", err)

	}

	proxyCaBundle := ""
	switch proxy.Spec.TrustedCA.Name {
	case "":
		// No proxy CA bundle
	case common.ClusterAdditionalTrustBundleName:
		// Proxy is using the cluster's CA bundle
		proxyCaBundle = clusterAdditionalTrustBundle
	default:
		// Proxy is using a different CA bundle, retrieve it from the named configmap
		proxyCaBundle, err = utils.GetAdditionalTrustBundleFromConfigmap(ctx, r.Client, proxy.Spec.TrustedCA.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to get additional trust bundle from configmap: %w", err)
		}

		if proxyCaBundle == "" {
			// This is a very weird but probably valid OCP configuration that we prefer to not support in LCA
			return nil, fmt.Errorf("proxy trustedCA configmap %s/%s exists but is empty", common.OpenshiftConfigNamespace, proxy.Spec.TrustedCA.Name)
		}
	}

	additionalTrustBundle, err := newAdditionalTrustBundle(clusterAdditionalTrustBundle, proxy.Spec.TrustedCA.Name, proxyCaBundle)
	if err != nil {
		return nil, fmt.Errorf("failed to create additional trust bundle: %w", err)
	}

	return additionalTrustBundle, err
}

func (r *UpgradeClusterConfigGather) GetProxy(ctx context.Context) (*seedreconfig.Proxy, *seedreconfig.Proxy, error) {
	proxy := v1.Proxy{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: common.OpenshiftProxyCRName}, &proxy); err != nil {
		return nil, nil, fmt.Errorf("failed to get proxy: %w", err)
	}

	if proxy.Spec.HTTPProxy == "" && proxy.Spec.HTTPSProxy == "" && proxy.Spec.NoProxy == "" {
		return nil, nil, nil
	}

	return &seedreconfig.Proxy{
			HTTPProxy:  proxy.Spec.HTTPProxy,
			HTTPSProxy: proxy.Spec.HTTPSProxy,
			NoProxy:    proxy.Spec.NoProxy,
		},
		&seedreconfig.Proxy{
			HTTPProxy:  proxy.Status.HTTPProxy,
			HTTPSProxy: proxy.Status.HTTPSProxy,
			NoProxy:    proxy.Status.NoProxy,
		},
		nil

}

func (r *UpgradeClusterConfigGather) GetInstallConfig(ctx context.Context) (string, error) {
	configmap := corev1.ConfigMap{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: common.InstallConfigCMNamespace, Name: common.InstallConfigCM}, &configmap); err != nil {
		return "", fmt.Errorf("failed to get install-config configmap: %w", err)
	}
	return configmap.Data[common.InstallConfigCMInstallConfigDataKey], nil

}

func serverSSHKeysToReconfigServerSSHKeys(serverSSHKeys []ServerSSHKey) []seedreconfig.ServerSSHKey {
	return lo.Map(serverSSHKeys, func(serverSSHKey ServerSSHKey, _ int) seedreconfig.ServerSSHKey {
		return seedreconfig.ServerSSHKey{
			FileName:    serverSSHKey.FileName,
			FileContent: serverSSHKey.FileContent,
		}
	})
}

func SeedReconfigurationFromClusterInfo(
	clusterInfo *utils.ClusterInfo,
	kubeconfigCryptoRetention *seedreconfig.KubeConfigCryptoRetention,
	sshKey string,
	infraID string,
	pullSecret string,
	kubeadminPasswordHash string,
	proxy,
	statusProxy *seedreconfig.Proxy,
	installConfig string,
	chronyConfig string,
	additionalTrustBundle *AdditionalTrustBundle,
	serverSSHKeys []ServerSSHKey,
) *seedreconfig.SeedReconfiguration {
	return &seedreconfig.SeedReconfiguration{
		APIVersion:                seedreconfig.SeedReconfigurationVersion,
		BaseDomain:                clusterInfo.BaseDomain,
		ClusterName:               clusterInfo.ClusterName,
		ClusterID:                 clusterInfo.ClusterID,
		InfraID:                   infraID,
		NodeIPs:                   clusterInfo.NodeIPs,
		ReleaseRegistry:           clusterInfo.ReleaseRegistry,
		Hostname:                  clusterInfo.Hostname,
		KubeconfigCryptoRetention: *kubeconfigCryptoRetention,
		SSHKey:                    sshKey,
		ServerSSHKeys:             serverSSHKeysToReconfigServerSSHKeys(serverSSHKeys),
		PullSecret:                pullSecret,
		KubeadminPasswordHash:     kubeadminPasswordHash,
		Proxy:                     proxy,
		StatusProxy:               statusProxy,
		InstallConfig:             installConfig,
		MachineNetworks:           clusterInfo.MachineNetworks,
		ChronyConfig:              chronyConfig,
		AdditionalTrustBundle: seedreconfig.AdditionalTrustBundle{
			UserCaBundle:         additionalTrustBundle.UserCaBundle,
			ProxyConfigmapName:   additionalTrustBundle.ProxyConfigmapName,
			ProxyConfigmapBundle: additionalTrustBundle.ProxyConfigmapBundle,
		},
		NodeLabels: clusterInfo.NodeLabels,
	}
}

func (r *UpgradeClusterConfigGather) fetchClusterInfo(ctx context.Context, clusterConfigPath string) error {
	r.Log.Info("Fetching ClusterInfo")

	clusterInfo, err := utils.GetClusterInfo(ctx, r.Client)
	if err != nil {
		return fmt.Errorf("failed to get cluster info: %w", err)
	}

	seedReconfigurationKubeconfigRetention, err := utils.SeedReconfigurationKubeconfigRetentionFromCluster(ctx, r.Client)
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig retention from crypto dir: %w", err)
	}

	sshKey, err := r.getSSHPublicKey()
	if err != nil {
		return fmt.Errorf("failed to get ssh key: %w", err)
	}

	infraID, err := r.getInfraID(ctx)
	if err != nil {
		return err
	}

	pullSecret, err := r.getPullSecret(ctx)
	if err != nil {
		return err
	}

	kubeadminPasswordHash, err := r.GetKubeadminPasswordHash(ctx)
	if err != nil {
		return err
	}

	serverSSHKeys, err := r.GetServerSSHKeys(ctx)
	if err != nil {
		return err
	}

	proxy, statusProxy, err := r.GetProxy(ctx)
	if err != nil {
		return err
	}

	additionalTrustBundle, err := r.GetAdditionalTrustBundle(ctx)
	if err != nil {
		return err
	}

	installConfig, err := r.GetInstallConfig(ctx)
	if err != nil {
		return err
	}

	chronyConfig, err := r.getChronyConfig()
	if err != nil {
		return err
	}

	seedReconfiguration := SeedReconfigurationFromClusterInfo(clusterInfo, seedReconfigurationKubeconfigRetention,
		sshKey,
		infraID,
		pullSecret,
		kubeadminPasswordHash,
		proxy,
		statusProxy,
		installConfig,
		chronyConfig,
		additionalTrustBundle,
		serverSSHKeys,
	)

	filePath := filepath.Join(clusterConfigPath, common.SeedReconfigurationFileName)
	r.Log.Info("Writing ClusterInfo to file", "path", filePath)
	if err := utils.MarshalToFile(seedReconfiguration, filePath); err != nil {
		return fmt.Errorf("failed to write cluster info to %s: %w", filePath, err)
	}

	return nil
}

func (r *UpgradeClusterConfigGather) fetchIDMS(ctx context.Context, manifestsDir string) error {
	r.Log.Info("Fetching IDMS")
	idms, err := r.getIDMSs(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch IDMS")
	}

	if len(idms.Items) < 1 {
		r.Log.Info("ImageDigestMirrorSetList is empty, skipping")
		return nil
	}

	filePath := filepath.Join(manifestsDir, idmsFileName)
	r.Log.Info("Writing IDMS to file", "path", filePath)
	if err := utils.MarshalToFile(idms, filePath); err != nil {
		return fmt.Errorf("failed to write IDMS to file %s: %w", filePath, err)
	}

	return nil
}

// configDirs creates and returns the directory for the given cluster configuration.
func (r *UpgradeClusterConfigGather) configDir(ostreeVarDir string) (string, error) {
	filesDir := filepath.Join(ostreeVarDir, common.OptOpenshift, common.ClusterConfigDir)
	r.Log.Info("Creating cluster configuration folder and subfolder", "folder", filesDir)
	if err := os.MkdirAll(filepath.Join(filesDir, manifestDir), 0o700); err != nil {
		return "", fmt.Errorf("failed to create cluster configuration folder and subfolder in %s: %w", manifestDir, err)
	}
	return filesDir, nil
}

// typeMetaForObject returns the given object's TypeMeta or an error otherwise.
func (r *UpgradeClusterConfigGather) typeMetaForObject(o runtime.Object) (*metav1.TypeMeta, error) {
	gvks, unversioned, err := r.Scheme.ObjectKinds(o)
	if err != nil {
		return nil, fmt.Errorf("failed to get objectKinds: %w", err)
	}
	if unversioned || len(gvks) == 0 {
		return nil, fmt.Errorf("unable to find API version for object")
	}
	// if there are multiple assume the last is the most recent
	gvk := gvks[len(gvks)-1]
	return &metav1.TypeMeta{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
	}, nil
}

func (r *UpgradeClusterConfigGather) getIDMSs(ctx context.Context) (v1.ImageDigestMirrorSetList, error) {
	idmsList := v1.ImageDigestMirrorSetList{}
	currentIdms := v1.ImageDigestMirrorSetList{}
	if err := r.Client.List(ctx, &currentIdms); err != nil {
		return v1.ImageDigestMirrorSetList{}, fmt.Errorf("failed to list ImageDigestMirrorSet: %w", err)
	}

	for _, idms := range currentIdms.Items {
		obj := v1.ImageDigestMirrorSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      idms.Name,
				Namespace: idms.Namespace,
			},
			Spec: idms.Spec,
		}
		typeMeta, err := r.typeMetaForObject(&idms) //nolint:gosec
		if err != nil {
			return v1.ImageDigestMirrorSetList{}, err
		}
		obj.TypeMeta = *typeMeta
		obj.ObjectMeta.Labels = idms.Labels
		obj.ObjectMeta.Annotations = idms.Annotations
		idmsList.Items = append(idmsList.Items, obj)
	}
	typeMeta, err := r.typeMetaForObject(&idmsList)
	if err != nil {
		return v1.ImageDigestMirrorSetList{}, err
	}
	idmsList.TypeMeta = *typeMeta

	return idmsList, nil
}

func (r *UpgradeClusterConfigGather) fetchICSPs(ctx context.Context, manifestsDir string) error {
	r.Log.Info("Fetching ICSPs")
	iscpsList := &operatorv1alpha1.ImageContentSourcePolicyList{}
	currentIcps := &operatorv1alpha1.ImageContentSourcePolicyList{}
	if err := r.Client.List(ctx, currentIcps); err != nil {
		return fmt.Errorf("failed list ImageContentSourcePolicy: %w", err)
	}

	if len(currentIcps.Items) < 1 {
		r.Log.Info("ImageContentPolicyList is empty, skipping")
		return nil
	}

	for _, icp := range currentIcps.Items {
		obj := operatorv1alpha1.ImageContentSourcePolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      icp.Name,
				Namespace: icp.Namespace,
			},
			Spec: icp.Spec,
		}
		typeMeta, err := r.typeMetaForObject(&icp) //nolint:gosec
		if err != nil {
			return err
		}
		obj.TypeMeta = *typeMeta
		obj.ObjectMeta.Labels = icp.Labels
		obj.ObjectMeta.Annotations = icp.Annotations
		iscpsList.Items = append(iscpsList.Items, obj)
	}
	typeMeta, err := r.typeMetaForObject(iscpsList)
	if err != nil {
		return err
	}
	iscpsList.TypeMeta = *typeMeta

	filePath := filepath.Join(manifestsDir, icspsFileName)
	r.Log.Info("Writing ICSPs to file", "path", filePath)
	if err := utils.MarshalToFile(iscpsList, filePath); err != nil {
		return fmt.Errorf("failed to write icsps to %s, err: %w", filePath, err)
	}

	return nil
}

// gather network files and copy them
func (r *UpgradeClusterConfigGather) fetchNetworkConfig(ostreeDir string) error {
	r.Log.Info("Fetching node network files")
	dir := filepath.Join(ostreeDir, filepath.Join(common.OptOpenshift, common.NetworkDir))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create network folder %s, err %w", dir, err)
	}

	for _, path := range listOfNetworkFilesPaths {
		r.Log.Info("Copying network files", "file", path, "to", dir)
		err := utils.CopyFileIfExists(filepath.Join(hostPath, path), filepath.Join(dir, filepath.Base(path)))
		if err != nil {
			return fmt.Errorf("failed to copy %s to %s, err %w", path, dir, err)
		}
	}
	r.Log.Info("Done fetching node network files")
	return nil
}
