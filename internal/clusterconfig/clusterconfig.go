package clusterconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/go-logr/logr"
	v1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"os"
	"path/filepath"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cro "github.com/RHsyseng/cluster-relocation-operator/api/v1beta1"
)

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=imagedigestmirrorsets,verbs=get;list;watch

const (
	PullSecretName                string = "pull-secret"
	ConfigNamespace               string = "openshift-config"
	upgradeConfigurationNamespace string = "upgrade-configuration"
	clusterConfigDir              string = "cluster-configuration"
	ImageSetName                  string = "mirror-ocp"
)

type UpdateConfigReconcilerOptions struct {
	DataDir string `envconfig:"DATA_DIR" default:"/data"`
}

// UpgradeClusterConfigGather Gather ClusterConfig attributes from the kube-api
type UpgradeClusterConfigGather struct {
	client.Client
	Log     logr.Logger
	Scheme  *runtime.Scheme
	Options *UpdateConfigReconcilerOptions
}

// FetchClusterConfig collect the current cluster config and write it as json files into data dir:
func (r *UpgradeClusterConfigGather) FetchClusterConfig(ctx context.Context) error {
	// TODO: Add the following
	// ssh keys
	// Other Machine configs?
	//additionalTrustedCA (from the images.config)
	// Other Certificates?
	// Catalog source
	// ACM registration?
	r.Log.Info("Fetching cluster configuration")
	pullSecret := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: PullSecretName, Namespace: ConfigNamespace}, pullSecret); err != nil {
		return err
	}
	pullSecret.Namespace = upgradeConfigurationNamespace
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
	IDMS := &v1.ImageDigestMirrorSet{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: ImageSetName}, IDMS); err != nil {
		if errors.IsNotFound(err) {
			r.Log.Info("Failed to find ImageDigestMirrorSet, skipping")
		} else {
			return err
		}
	}
	r.Log.Info("Successfully fetched cluster config")

	clusterConfig, err, err2 := r.generateClusterConfig(ingress, IDMS)
	if err2 != nil {
		return err2
	}
	if err = r.writeClusterConfig(clusterConfig, pullSecret, clusterID); err != nil {
		return err
	}
	return nil
}

func (r *UpgradeClusterConfigGather) generateClusterConfig(ingress *v1.Ingress, IDMS *v1.ImageDigestMirrorSet) (*cro.ClusterRelocation, error, error) {
	config := &cro.ClusterRelocation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: upgradeConfigurationNamespace,
		},
		Spec: cro.ClusterRelocationSpec{
			Domain: ingress.Spec.Domain,
			PullSecretRef: &corev1.SecretReference{
				Namespace: upgradeConfigurationNamespace,
				Name:      PullSecretName,
			},
			ImageDigestMirrors: IDMS.Spec.ImageDigestMirrors,
		},
	}

	typeMeta, err := r.typeMetaForObject(config)
	if err != nil {
		return nil, nil, err
	}
	config.TypeMeta = *typeMeta
	return config, err, nil
}

// configDirs returns the files directory for the given cluster config
func (r *UpgradeClusterConfigGather) configDirs(config *cro.ClusterRelocation) (string, error) {
	filesDir := filepath.Join(r.Options.DataDir, "namespaces", config.Namespace, config.Name, clusterConfigDir)
	if err := os.MkdirAll(filesDir, 0700); err != nil {
		return "", err
	}
	return filesDir, nil
}

// writeClusterConfig writes the required info based on the cluster config to the config cache dir
func (r *UpgradeClusterConfigGather) writeClusterConfig(config *cro.ClusterRelocation, pullSecret *corev1.Secret, clusterID v1.ClusterID) error {
	clusterConfigPath, err := r.configDirs(config)
	if err != nil {
		return err
	}

	if err = r.writeNamespaceToFile(filepath.Join(clusterConfigPath, "namespace.json")); err != nil {
		return err
	}
	if err = r.writeClusterRelocationToFile(config, filepath.Join(clusterConfigPath, "cluster-relocation.json")); err != nil {
		return err
	}
	if err = r.writeSecretToFile(pullSecret, filepath.Join(clusterConfigPath, "pullsecret.json")); err != nil {
		return err
	}
	if err = r.writeClusterIDToFile(clusterID, filepath.Join(clusterConfigPath, "cluster-id-override.json")); err != nil {
		return err
	}

	return nil
}

func (r *UpgradeClusterConfigGather) writeNamespaceToFile(filePath string) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: upgradeConfigurationNamespace,
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "Namespace",
		},
	}

	data, err := json.Marshal(ns)
	if err != nil {
		return fmt.Errorf("failed to marshal namespace: %w", err)
	}
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write namespace: %w", err)
	}
	return nil
}

func (r *UpgradeClusterConfigGather) writeClusterRelocationToFile(config *cro.ClusterRelocation, file string) error {
	data, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal cluster relocation: %w", err)
	}
	if err := os.WriteFile(file, data, 0644); err != nil {
		return fmt.Errorf("failed to write cluster relocation: %w", err)
	}

	return nil
}

func (r *UpgradeClusterConfigGather) writeSecretToFile(secret *corev1.Secret, filePath string) error {
	// override namespace
	secret.Namespace = upgradeConfigurationNamespace
	data, err := json.Marshal(secret)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filePath, data, 0644); err != nil {
		return err
	}
	return nil
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

	data, err := json.Marshal(clusterVersion)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filePath, data, 0644); err != nil {
		return err
	}
	return nil
}
