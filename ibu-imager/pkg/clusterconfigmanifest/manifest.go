package clusterconfigmanifest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	v1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type ClusterManifest struct {
	Version     string `json:"version,omitempty"`
	Domain      string `json:"domain,omitempty"`
	ClusterName string `json:"cluster_name,omitempty"`
}

type installConfigMetadata struct {
	Name string `json:"name"`
}

type basicInstallConfig struct {
	BaseDomain string                `json:"baseDomain"`
	Metadata   installConfigMetadata `json:"metadata"`
}

type ManifestClient struct {
	client runtimeclient.Client
}

func NewManifestClient(client runtimeclient.Client) *ManifestClient {
	return &ManifestClient{
		client: client,
	}
}

func (m *ManifestClient) CreateClusterManifest(ctx context.Context) (*ClusterManifest, error) {
	clusterVersion := &v1.ClusterVersion{}
	if err := m.client.Get(ctx, types.NamespacedName{Name: "version"}, clusterVersion); err != nil {
		return nil, err
	}
	installConfig, err := m.getInstallConfig(ctx)
	if err != nil {
		return nil, err
	}
	return &ClusterManifest{
		ClusterName: installConfig.Metadata.Name,
		Domain:      installConfig.BaseDomain,
		Version:     clusterVersion.Status.Desired.Version,
	}, nil
}

func (m *ManifestClient) CreateClusterManifestAsJson(ctx context.Context) ([]byte, error) {
	manifest, err := m.CreateClusterManifest(ctx)
	if err != nil {
		return nil, err
	}
	return json.Marshal(manifest)
}

func (m *ManifestClient) getInstallConfig(ctx context.Context) (*basicInstallConfig, error) {
	cm := &corev1.ConfigMap{}
	err := m.client.Get(ctx, types.NamespacedName{Name: "cluster-config-v1", Namespace: "kube-system"}, cm)
	if err != nil {
		return nil, err
	}

	data, ok := cm.Data["install-config"]
	if !ok {
		return nil, fmt.Errorf("did not find key install-config in configmap")
	}

	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(data)), 4096)
	instConf := &basicInstallConfig{}
	if err := decoder.Decode(instConf); err != nil {
		return nil, fmt.Errorf("failed to decode install config, err: %w", err)
	}
	return instConf, nil
}
