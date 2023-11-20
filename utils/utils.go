package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/tools/clientcmd"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// WriteToFile write interface to file
func WriteToFile(data interface{}, filePath string) error {
	marshaled, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return os.WriteFile(filePath, marshaled, 0o644)
}

// RenderTemplateFile render template file
func RenderTemplateFile(srcTemplate string, params map[string]any, dest string, perm os.FileMode) error {
	templateData, err := os.ReadFile(srcTemplate)
	if err != nil {
		return fmt.Errorf("error occurred while trying to read %s: %w", srcTemplate, err)
	}

	tmpl := template.New("template")
	tmpl = template.Must(tmpl.Parse(string(templateData)))
	var buf bytes.Buffer
	if err = tmpl.Execute(&buf, params); err != nil {
		return fmt.Errorf("failed to render controller template: %w", err)
	}

	if err = os.WriteFile(dest, buf.Bytes(), perm); err != nil {
		return fmt.Errorf("error occurred while trying to write rendered data to %s: %w", dest, err)
	}
	return nil
}

func GetSNOMasterNode(ctx context.Context, client runtimeclient.Client) (*corev1.Node, error) {
	nodesList := &corev1.NodeList{}
	err := client.List(ctx, nodesList, &runtimeclient.ListOptions{LabelSelector: labels.SelectorFromSet(
		labels.Set{
			"node-role.kubernetes.io/master": "",
		},
	)})
	if err != nil {
		return nil, err
	}
	if len(nodesList.Items) != 1 {
		return nil, fmt.Errorf("we should have one master node in sno cluster, current number is %d", len(nodesList.Items))
	}
	return &nodesList.Items[0], nil
}

func ReadYamlOrJSONFile(filePath string, into interface{}) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	return decoder.Decode(into)
}

func IsIpv6(provideIp string) bool {
	ip := net.ParseIP(provideIp)
	if ip == nil {
		return false
	}
	return ip.To4() != nil
}

func CreateKubeClient(scheme *runtime.Scheme, kubeconfig string) (runtimeclient.Client, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	return runtimeclient.New(config, runtimeclient.Options{Scheme: scheme,
		WarningHandler: runtimeclient.WarningHandlerOptions{SuppressWarnings: true}})
}
