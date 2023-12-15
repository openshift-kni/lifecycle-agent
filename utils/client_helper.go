package utils

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/clusterinfo"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	v1 "github.com/openshift/api/config/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

func GetSecretData(ctx context.Context, name, namespace, key string, client runtimeclient.Client) (string, error) {
	secret := &corev1.Secret{}
	if err := client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, secret); err != nil {
		return "", err
	}

	data, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("did not find key %s in Secret %s/%s", key, name, namespace)
	}

	return string(data), nil
}

func GetConfigMapData(ctx context.Context, name, namespace, key string, client runtimeclient.Client) (string, error) {
	cm := &corev1.ConfigMap{}
	if err := client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, cm); err != nil {
		return "", err
	}

	data, ok := cm.Data[key]
	if !ok {
		return "", fmt.Errorf("did not find key %s in ConfigMap", key)
	}

	return data, nil
}

func BackupCertificates(ctx context.Context, client runtimeclient.Client, certDir string) error {
	if err := os.MkdirAll(certDir, os.ModePerm); err != nil {
		return fmt.Errorf("error creating %s: %w", certDir, err)
	}

	adminKubeConfigClientCA, err := GetConfigMapData(ctx, "admin-kubeconfig-client-ca", "openshift-config", "ca-bundle.crt", client)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path.Join(certDir, "admin-kubeconfig-client-ca.crt"), []byte(adminKubeConfigClientCA), 0o644); err != nil {
		return err
	}

	for _, cert := range common.CertPrefixes {
		servingSignerKey, err := GetSecretData(ctx, cert, "openshift-kube-apiserver-operator", "tls.key", client)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path.Join(certDir, cert+".key"), []byte(servingSignerKey), 0o644); err != nil {
			return err
		}
	}

	ingressOperatorKey, err := GetSecretData(ctx, "router-ca", "openshift-ingress-operator", "tls.key", client)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path.Join(certDir, "ingresskey-ingress-operator.key"), []byte(ingressOperatorKey), 0o644); err != nil {
		return err
	}
	return nil
}

func CreateClusterInfo(ctx context.Context, client runtimeclient.Client) (*clusterinfo.ClusterInfo, error) {
	clusterVersion := &v1.ClusterVersion{}
	if err := client.Get(ctx, types.NamespacedName{Name: "version"}, clusterVersion); err != nil {
		return nil, err
	}

	installConfig, err := getInstallConfig(ctx, client)
	if err != nil {
		return nil, err
	}

	node, err := GetSNOMasterNode(ctx, client)
	if err != nil {
		return nil, err
	}
	ip, err := getNodeInternalIP(*node)
	if err != nil {
		return nil, err
	}
	hostname, err := getNodeHostname(*node)
	if err != nil {
		return nil, err
	}

	releaseRegistry, err := GetReleaseRegistry(ctx, client)
	if err != nil {
		return nil, err
	}

	return &clusterinfo.ClusterInfo{
		ClusterName:     installConfig.Metadata.Name,
		Domain:          installConfig.BaseDomain,
		Version:         clusterVersion.Status.Desired.Version,
		ClusterID:       string(clusterVersion.Spec.ClusterID),
		MasterIP:        ip,
		ReleaseRegistry: releaseRegistry,
		Hostname:        hostname,
	}, nil
}

// TODO: add dual stuck support
func getNodeInternalIP(node corev1.Node) (string, error) {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address, nil
		}
	}
	return "", fmt.Errorf("failed to find node internal ip address")
}

func getNodeHostname(node corev1.Node) (string, error) {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeHostName {
			return addr.Address, nil
		}
	}
	return "", fmt.Errorf("failed to find node hostname")
}

func getInstallConfig(ctx context.Context, client runtimeclient.Client) (*clusterinfo.BasicInstallConfig, error) {
	cm := &corev1.ConfigMap{}
	err := client.Get(ctx, types.NamespacedName{Name: common.InstallConfigCM, Namespace: common.InstallConfigCMNamespace}, cm)
	if err != nil {
		return nil, err
	}

	data, ok := cm.Data["install-config"]
	if !ok {
		return nil, fmt.Errorf("did not find key install-config in configmap")
	}

	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(data)), 4096)
	instConf := &clusterinfo.BasicInstallConfig{}
	if err := decoder.Decode(instConf); err != nil {
		return nil, fmt.Errorf("failed to decode install config, err: %w", err)
	}
	return instConf, nil
}

func GetCSVDeployment(ctx context.Context, client runtimeclient.Client) (*appsv1.Deployment, error) {
	deployment := &appsv1.Deployment{}
	if err := client.Get(ctx,
		types.NamespacedName{
			Name:      common.CsvDeploymentName,
			Namespace: common.CsvDeploymentNamespace},
		deployment); err != nil {
		return nil, fmt.Errorf("failed to get cluster version deployment, err: %w", err)
	}

	return deployment, nil
}

func GetReleaseRegistry(ctx context.Context, client runtimeclient.Client) (string, error) {
	deployment, err := GetCSVDeployment(ctx, client)
	if err != nil {
		return "", err
	}

	return strings.Split(deployment.Spec.Template.Spec.Containers[0].Image, "/")[0], nil
}

func ReadClusterInfoFromFile(path string) (*clusterinfo.ClusterInfo, error) {
	data := &clusterinfo.ClusterInfo{}
	err := ReadYamlOrJSONFile(path, data)
	return data, err
}
