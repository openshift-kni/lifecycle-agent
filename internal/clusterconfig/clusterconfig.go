package clusterconfig

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	cro "github.com/RHsyseng/cluster-relocation-operator/api/v1beta1"
	"github.com/go-logr/logr"
	v1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/clusterinfo"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=imagedigestmirrorsets,verbs=get;list;watch

const (
	pullSecretName                = "pull-secret"
	configNamespace               = "openshift-config"
	upgradeConfigurationNamespace = "upgrade-configuration"
	clusterConfigDir              = "/opt/openshift/cluster-configuration"
	imageSetName                  = "mirror-ocp"
	clusterIDFileName             = "cluster-id-override.json"
	croFileName                   = "cluster-relocation.json"
	croName                       = "cluster"
	pullSecretFileName            = "pullsecret.json"
	clusterInfoFileName           = "clusterinfo/manifest.json"
)

// UpgradeClusterConfigGather Gather ClusterConfig attributes from the kube-api
type UpgradeClusterConfigGather struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// FetchClusterConfig collect the current cluster config and write it as json files into data dir:
func (r *UpgradeClusterConfigGather) FetchClusterConfig(ctx context.Context, ostreeDir string) error {
	// TODO: Add the following
	// ssh keys
	// Other Machine configs?
	// additionalTrustedCA (from the images.config)
	// Other Certificates?
	// Catalog source
	// ACM registration?
	r.Log.Info("Fetching cluster configuration")
	pullSecret := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: pullSecretName, Namespace: configNamespace}, pullSecret); err != nil {
		return err
	}
	clusterVersion := &v1.ClusterVersion{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: "version"}, clusterVersion); err != nil {
		return err
	}
	clusterID := clusterVersion.Spec.ClusterID
	ingress := &v1.Ingress{}
	if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: "cluster"}, ingress); err != nil {
		return err
	}
	// TODO: we might need a list here and perhaps we want to apply these as extra-manifests
	idms := &v1.ImageDigestMirrorSet{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: imageSetName}, idms); err != nil {
		if errors.IsNotFound(err) {
			r.Log.Info("ImageDigestMirrorSet", imageSetName, "was not found, skipping")
		} else {
			return err
		}
	}
	r.Log.Info("Successfully fetched cluster config")

	clusterConfig, err := r.generateClusterConfig(ingress, idms)
	if err != nil {
		return err
	}

	cmClient := clusterinfo.NewClusterInfoClient(r.Client)
	clusterData, err := cmClient.CreateClusterInfo(ctx)
	if err != nil {
		return err
	}

	if err := r.writeClusterConfig(clusterConfig, pullSecret, clusterID, ostreeDir, clusterData); err != nil {
		return err
	}
	return nil
}

func (r *UpgradeClusterConfigGather) generateClusterConfig(ingress *v1.Ingress, idms *v1.ImageDigestMirrorSet) (*cro.ClusterRelocation, error) {
	config := &cro.ClusterRelocation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      croName,
			Namespace: upgradeConfigurationNamespace,
		},
		Spec: cro.ClusterRelocationSpec{
			Domain: ingress.Spec.Domain,
			PullSecretRef: &corev1.SecretReference{
				Namespace: upgradeConfigurationNamespace,
				Name:      pullSecretName,
			},
			ImageDigestMirrors: idms.Spec.ImageDigestMirrors,
		},
	}

	typeMeta, err := r.typeMetaForObject(config)
	if err != nil {
		return nil, err
	}
	config.TypeMeta = *typeMeta
	return config, nil
}

// configDirs returns the files directory for the given cluster config
func (r *UpgradeClusterConfigGather) configDirs(dir string) (string, error) {
	filesDir := filepath.Join(dir, clusterConfigDir)
	r.Log.Info("Creating cluster configuration folder", "folder", filesDir)
	if err := os.MkdirAll(filepath.Join(filesDir, filepath.Dir(clusterInfoFileName)), 0o700); err != nil {
		return "", err
	}
	return filesDir, nil
}

// writeClusterConfig writes the required info based on the cluster config to the config cache dir
func (r *UpgradeClusterConfigGather) writeClusterConfig(config *cro.ClusterRelocation, pullSecret *corev1.Secret,
	clusterID v1.ClusterID, dir string, clusterData *clusterinfo.ClusterInfo) error {
	clusterConfigPath, err := r.configDirs(dir)
	if err != nil {
		return err
	}

	if err := r.writeNamespaceToFile(filepath.Join(clusterConfigPath, "namespace.json")); err != nil {
		return err
	}
	if err := r.writeClusterRelocationToFile(config, filepath.Join(clusterConfigPath, croFileName)); err != nil {
		return err
	}
	if err := r.writeSecretToFile(pullSecret, filepath.Join(clusterConfigPath, pullSecretFileName)); err != nil {
		return err
	}
	if err := r.writeClusterIDToFile(clusterID, filepath.Join(clusterConfigPath, clusterIDFileName)); err != nil {
		return err
	}
	if err := utils.WriteToFile(clusterData, filepath.Join(clusterConfigPath, clusterInfoFileName)); err != nil {
		return err
	}

	return nil
}

func (r *UpgradeClusterConfigGather) writeNamespaceToFile(filePath string) error {
	r.Log.Info("Writing namespace file", "path", filePath)
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: upgradeConfigurationNamespace,
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "Namespace",
		},
	}
	return utils.WriteToFile(ns, filePath)
}

func (r *UpgradeClusterConfigGather) writeClusterRelocationToFile(config *cro.ClusterRelocation, filePath string) error {
	r.Log.Info("Writing cluster relocation to file", "path", filePath)
	return utils.WriteToFile(config, filePath)
}

func (r *UpgradeClusterConfigGather) writeSecretToFile(secret *corev1.Secret, filePath string) error {
	// override namespace
	r.Log.Info("Writing secret to file", "path", filePath)
	secret.Namespace = upgradeConfigurationNamespace
	return utils.WriteToFile(secret, filePath)
}

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

func (r *UpgradeClusterConfigGather) writeClusterIDToFile(clusterID v1.ClusterID, filePath string) error {
	// We just want to override the clusterID
	r.Log.Info("Writing clusterversion to file", "path", filePath)
	clusterVersion := &v1.ClusterVersion{
		ObjectMeta: metav1.ObjectMeta{
			Name: "version",
		},
		Spec: v1.ClusterVersionSpec{
			ClusterID: clusterID,
		},
	}
	typeMeta, err := r.typeMetaForObject(clusterVersion)
	if err != nil {
		return err
	}
	clusterVersion.TypeMeta = *typeMeta
	return utils.WriteToFile(clusterVersion, filePath)
}
