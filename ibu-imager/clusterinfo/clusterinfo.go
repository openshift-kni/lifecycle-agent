package clusterinfo

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	v1 "github.com/openshift/api/config/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/utils"
)

const (
	// InstallConfigCM cm name
	InstallConfigCM = "cluster-config-v1"
	// InstallConfigCMNamespace cm namespace
	InstallConfigCMNamespace = "kube-system"

	CsvDeploymentName      = "cluster-version-operator"
	CsvDeploymentNamespace = "openshift-cluster-version"
)

// ClusterInfo struct that describe current cluster critical info
type ClusterInfo struct {
	Version         string `json:"version,omitempty"`
	Domain          string `json:"domain,omitempty"`
	ClusterName     string `json:"cluster_name,omitempty"`
	ClusterID       string `json:"cluster_id,omitempty"`
	MasterIP        string `json:"master_ip,omitempty"`
	ReleaseRegistry string `json:"release_registry,omitempty"`
}

type installConfigMetadata struct {
	Name string `json:"name"`
}

type basicInstallConfig struct {
	BaseDomain string                `json:"baseDomain"`
	Metadata   installConfigMetadata `json:"metadata"`
}

// InfoClient client to create cluster info object
type InfoClient struct {
	client runtimeclient.Client
}

func NewClusterInfoClient(client runtimeclient.Client) *InfoClient {
	return &InfoClient{
		client: client,
	}
}

// CreateClusterInfo create cluster info
func (m *InfoClient) CreateClusterInfo(ctx context.Context) (*ClusterInfo, error) {
	clusterVersion := &v1.ClusterVersion{}
	if err := m.client.Get(ctx, types.NamespacedName{Name: "version"}, clusterVersion); err != nil {
		return nil, err
	}

	installConfig, err := m.getInstallConfig(ctx)
	if err != nil {
		return nil, err
	}

	ip, err := m.getNodeInternalIP(ctx)
	if err != nil {
		return nil, err
	}

	releaseRegistry, err := m.getReleaseRegistry(ctx)
	if err != nil {
		return nil, err
	}

	return &ClusterInfo{
		ClusterName:     installConfig.Metadata.Name,
		Domain:          installConfig.BaseDomain,
		Version:         clusterVersion.Status.Desired.Version,
		ClusterID:       string(clusterVersion.Spec.ClusterID),
		MasterIP:        ip,
		ReleaseRegistry: releaseRegistry,
	}, nil
}

func (m *InfoClient) GetSecretData(ctx context.Context, name, namespace, key string) (string, error) {
	secret := &corev1.Secret{}
	if err := m.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, secret); err != nil {
		return "", err
	}

	data, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("did not find key %s in Secret %s/%s", key, name, namespace)
	}

	return string(data), nil
}

func (m *InfoClient) GetConfigMapData(ctx context.Context, name, namespace, key string) (string, error) {
	cm := &corev1.ConfigMap{}
	if err := m.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, cm); err != nil {
		return "", err
	}

	data, ok := cm.Data[key]
	if !ok {
		return "", fmt.Errorf("did not find key %s in ConfigMap", key)
	}

	return data, nil
}

// TODO: add dual stuck support
func (m *InfoClient) getNodeInternalIP(ctx context.Context) (string, error) {
	node, err := utils.GetSNOMasterNode(ctx, m.client)
	if err != nil {
		return "", err
	}
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address, nil
		}
	}

	return "", fmt.Errorf("failed to find node internal ip address")
}

func (m *InfoClient) getInstallConfig(ctx context.Context) (*basicInstallConfig, error) {
	cm := &corev1.ConfigMap{}
	err := m.client.Get(ctx, types.NamespacedName{Name: InstallConfigCM, Namespace: InstallConfigCMNamespace}, cm)
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

func (m *InfoClient) GetCSVDeployment(ctx context.Context) (*appsv1.Deployment, error) {
	deployment := &appsv1.Deployment{}
	if err := m.client.Get(ctx,
		types.NamespacedName{
			Name:      CsvDeploymentName,
			Namespace: CsvDeploymentNamespace},
		deployment); err != nil {
		return nil, fmt.Errorf("failed to get cluster version deployment, err: %w", err)
	}

	return deployment, nil
}

func (m *InfoClient) getReleaseRegistry(ctx context.Context) (string, error) {
	deployment, err := m.GetCSVDeployment(ctx)
	if err != nil {
		return "", err
	}

	return strings.Split(deployment.Spec.Template.Spec.Containers[0].Image, "/")[0], nil
}

func ReadClusterInfoFromFile(path string) (*ClusterInfo, error) {
	data := &ClusterInfo{}
	err := utils.ReadYamlOrJSONFile(path, data)
	return data, err
}
