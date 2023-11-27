package clusterconfig

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
	v1 "github.com/openshift/api/config/v1"
	mcv1 "github.com/openshift/api/machineconfiguration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/clusterinfo"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=clusterversions,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=imagedigestmirrorsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=imagecontentpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=proxies,verbs=get;list;watch
// +kubebuilder:rbac:groups=machineconfiguration.openshift.io,resources=machineconfigs,verbs=get;list;watch

const (
	manifestDir = "manifests"

	proxyName     = "cluster"
	proxyFileName = "proxy.json"

	pullSecretName     = "pull-secret"
	pullSecretFileName = "pullsecret.json"

	configNamespace = "openshift-config"

	clusterIDFileName = "cluster-id-override.json"

	idmsFileName  = "image-digest-mirror-set.json"
	icspsFileName = "image-content-policy-list.json"

	caBundleCMName   = "user-ca-bundle"
	caBundleFileName = caBundleCMName + ".json"
)

var (
	machineConfigNames = []string{
		"99-master-ssh",
		"99-worker-ssh",
	}
)

// UpgradeClusterConfigGather Gather ClusterConfig attributes from the kube-api
type UpgradeClusterConfigGather struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// FetchClusterConfig collects the current cluster's configuration and write it as JSON files into
// given filesystem directory.
func (r *UpgradeClusterConfigGather) FetchClusterConfig(ctx context.Context, ostreeDir string) error {
	// TODO: Add the following
	// Other Machine configs?
	// additionalTrustedCA (from the images.config)
	// Other Certificates?
	// Catalog source
	// ACM registration?
	r.Log.Info("Fetching cluster configuration")

	clusterConfigPath, err := r.configDirs(ostreeDir)
	if err != nil {
		return err
	}
	manifestsDir := filepath.Join(clusterConfigPath, manifestDir)

	if err := r.fetchPullSecret(ctx, manifestsDir); err != nil {
		return err
	}
	if err := r.fetchProxy(ctx, manifestsDir); err != nil {
		return err
	}
	if err := r.fetchIDMS(ctx, manifestsDir); err != nil {
		return err
	}
	if err := r.fetchClusterVersion(ctx, manifestsDir); err != nil {
		return err
	}
	if err := r.fetchClusterInfo(ctx, clusterConfigPath); err != nil {
		return err
	}
	if err := r.fetchMachineConfigs(ctx, manifestsDir); err != nil {
		return err
	}
	if err := r.fetchCABundle(ctx, manifestsDir); err != nil {
		return err
	}
	if err := r.fetchICSPs(ctx, manifestsDir); err != nil {
		return err
	}

	r.Log.Info("Successfully fetched cluster configuration")
	return nil
}

func (r *UpgradeClusterConfigGather) fetchPullSecret(ctx context.Context, manifestsDir string) error {
	r.Log.Info("Fetching pull-secret", "name", pullSecretName, "namespace", configNamespace)

	secret := corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: pullSecretName, Namespace: configNamespace}, &secret); err != nil {
		return err
	}

	s := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secret.Name,
			Namespace: secret.Namespace,
		},
		Data: secret.Data,
		Type: secret.Type,
	}
	typeMeta, err := r.typeMetaForObject(&s)
	if err != nil {
		return err
	}
	s.TypeMeta = *typeMeta

	filePath := filepath.Join(manifestsDir, pullSecretFileName)
	r.Log.Info("Writing pull-secret to file", "path", filePath)
	return utils.MarshalToFile(s, filePath)
}

func (r *UpgradeClusterConfigGather) fetchProxy(ctx context.Context, manifestsDir string) error {
	r.Log.Info("Fetching cluster-wide proxy", "name", proxyName)

	proxy := v1.Proxy{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: proxyName}, &proxy); err != nil {
		return err
	}

	p := v1.Proxy{
		ObjectMeta: metav1.ObjectMeta{
			Name: proxy.Name,
		},
		Spec: proxy.Spec,
	}
	typeMeta, err := r.typeMetaForObject(&p)
	if err != nil {
		return err
	}
	p.TypeMeta = *typeMeta

	filePath := filepath.Join(manifestsDir, proxyFileName)
	r.Log.Info("Writing proxy to file", "path", filePath)
	return utils.MarshalToFile(p, filePath)
}

func (r *UpgradeClusterConfigGather) fetchMachineConfigs(ctx context.Context, manifestsDir string) error {
	for _, machineConfigName := range machineConfigNames {
		r.Log.Info("Fetching MachineConfig", "name", machineConfigName)

		machineConfig := mcv1.MachineConfig{}
		if err := r.Client.Get(ctx, types.NamespacedName{Name: machineConfigName}, &machineConfig); err != nil {
			return err
		}

		mc := mcv1.MachineConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name:   machineConfig.Name,
				Labels: machineConfig.Labels,
			},
			Spec: machineConfig.Spec,
		}
		machineConfigTypeMeta, err := r.typeMetaForObject(&machineConfig)
		if err != nil {
			return err
		}
		mc.TypeMeta = *machineConfigTypeMeta

		filePath := filepath.Join(manifestsDir, machineConfig.Name+".json")
		r.Log.Info("Writing MachineConfig to file", "path", filePath)
		if err := utils.MarshalToFile(mc, filePath); err != nil {
			return err
		}
	}
	return nil
}

func (r *UpgradeClusterConfigGather) fetchClusterInfo(ctx context.Context, clusterConfigPath string) error {
	r.Log.Info("Fetching ClusterInfo")

	cmClient := clusterinfo.NewClusterInfoClient(r.Client)
	clusterInfo, err := cmClient.CreateClusterInfo(ctx)
	if err != nil {
		return err
	}

	filePath := filepath.Join(clusterConfigPath, common.ClusterInfoFileName)

	r.Log.Info("Writing ClusterInfo to file", "path", filePath)
	return utils.MarshalToFile(clusterInfo, filePath)
}

func (r *UpgradeClusterConfigGather) fetchClusterVersion(ctx context.Context, manifestsDir string) error {
	r.Log.Info("Fetching ClusterVersion", "name", "version")
	clusterVersion := &v1.ClusterVersion{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: "version"}, clusterVersion); err != nil {
		return err
	}

	cv := v1.ClusterVersion{
		ObjectMeta: metav1.ObjectMeta{
			Name: "version",
		},
		Spec: v1.ClusterVersionSpec{
			ClusterID: clusterVersion.Spec.ClusterID,
		},
	}
	typeMeta, err := r.typeMetaForObject(&cv)
	if err != nil {
		return err
	}
	cv.TypeMeta = *typeMeta

	filePath := filepath.Join(manifestsDir, clusterIDFileName)
	r.Log.Info("Writing ClusterVersion with only the ClusterID field to file", "path", filePath)
	return utils.MarshalToFile(cv, filePath)
}

func (r *UpgradeClusterConfigGather) fetchIDMS(ctx context.Context, manifestsDir string) error {
	r.Log.Info("Fetching IDMS")
	idms, err := r.getIDMSs(ctx)
	if err != nil {
		return err
	}

	if len(idms.Items) < 1 {
		r.Log.Info("ImageDigestMirrorSetList is empty, skipping")
		return nil
	}

	filePath := filepath.Join(manifestsDir, idmsFileName)
	r.Log.Info("Writing IDMS to file", "path", filePath)
	return utils.MarshalToFile(idms, filePath)
}

// configDirs creates and returns the directory for the given cluster configuration.
func (r *UpgradeClusterConfigGather) configDirs(ostreeDir string) (string, error) {
	filesDir := filepath.Join(ostreeDir, common.OptOpenshift, common.ClusterConfigDir)
	r.Log.Info("Creating cluster configuration folder and subfolder", "folder", filesDir)
	if err := os.MkdirAll(filepath.Join(filesDir, manifestDir), 0o700); err != nil {
		return "", err
	}
	return filesDir, nil
}

// typeMetaForObject returns the given object's TypeMeta or an error otherwise.
func (r *UpgradeClusterConfigGather) typeMetaForObject(o runtime.Object) (*metav1.TypeMeta, error) {
	gvks, unversioned, err := r.Scheme.ObjectKinds(o)
	if err != nil {
		return nil, err
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

func (r *UpgradeClusterConfigGather) cleanObjectMetadata(o client.Object) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      o.GetName(),
		Namespace: o.GetNamespace(),
		Labels:    o.GetLabels(),
	}
}

func (r *UpgradeClusterConfigGather) getIDMSs(ctx context.Context) (v1.ImageDigestMirrorSetList, error) {
	idmsList := v1.ImageDigestMirrorSetList{}
	currentIdms := v1.ImageDigestMirrorSetList{}
	if err := r.Client.List(ctx, &currentIdms); err != nil {
		return v1.ImageDigestMirrorSetList{}, err
	}

	for _, idms := range currentIdms.Items {
		obj := v1.ImageDigestMirrorSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      idms.Name,
				Namespace: idms.Namespace,
			},
			Spec: idms.Spec,
		}
		typeMeta, err := r.typeMetaForObject(&currentIdms)
		if err != nil {
			return v1.ImageDigestMirrorSetList{}, err
		}
		idms.TypeMeta = *typeMeta

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
	iscpsList := &v1.ImageContentPolicyList{}
	currentIcps := &v1.ImageContentPolicyList{}
	if err := r.Client.List(ctx, currentIcps); err != nil {
		return err
	}

	for _, icp := range currentIcps.Items {
		obj := v1.ImageContentPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      icp.Name,
				Namespace: icp.Namespace,
			},
			Spec: icp.Spec,
		}
		typeMeta, err := r.typeMetaForObject(&icp)
		if err != nil {
			return err
		}
		icp.TypeMeta = *typeMeta
		iscpsList.Items = append(iscpsList.Items, obj)
	}
	typeMeta, err := r.typeMetaForObject(iscpsList)
	if err != nil {
		return err
	}
	iscpsList.TypeMeta = *typeMeta

	if err := utils.MarshalToFile(iscpsList, filepath.Join(manifestsDir, icspsFileName)); err != nil {
		return fmt.Errorf("failed to write icsps to %s, err: %w",
			filepath.Join(manifestsDir, icspsFileName), err)
	}

	return nil
}

func (r *UpgradeClusterConfigGather) fetchCABundle(ctx context.Context, manifestsDir string) error {
	r.Log.Info("Fetching user ca bundle")
	caBundle := &corev1.ConfigMap{}
	err := r.Client.Get(ctx, types.NamespacedName{Name: caBundleCMName,
		Namespace: configNamespace}, caBundle)
	if err != nil && errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get ca bundle cm, err: %w", err)
	}

	typeMeta, err := r.typeMetaForObject(caBundle)
	if err != nil {
		return err
	}
	caBundle.TypeMeta = *typeMeta
	caBundle.ObjectMeta = r.cleanObjectMetadata(caBundle)

	if err := utils.MarshalToFile(caBundle, filepath.Join(manifestsDir, caBundleFileName)); err != nil {
		return fmt.Errorf("failed to write user ca bundle to %s, err: %w",
			filepath.Join(manifestsDir, caBundleFileName), err)
	}

	return nil
}
