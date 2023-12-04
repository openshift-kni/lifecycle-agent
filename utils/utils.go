package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"text/template"

	cp "github.com/otiai10/copy"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/tools/clientcmd"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// MarshalToFile marshals anything and writes it to the given file path
func MarshalToFile(data any, filePath string) error {
	marshaled, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return os.WriteFile(filePath, marshaled, 0o644)
}

// TypeMetaForObject returns the given object's TypeMeta or an error otherwise.
func TypeMetaForObject(scheme *runtime.Scheme, o runtime.Object) (*metav1.TypeMeta, error) {
	gvks, unversioned, err := scheme.ObjectKinds(o)
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
	return ip.To4() == nil
}

func CreateKubeClient(scheme *runtime.Scheme, kubeconfig string) (runtimeclient.Client, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	return runtimeclient.New(config, runtimeclient.Options{Scheme: scheme,
		WarningHandler: runtimeclient.WarningHandlerOptions{SuppressWarnings: true}})
}

func RunOnce(name, directory string, log *logrus.Logger, f any, args ...any) error {
	doneFile := path.Join(directory, name+".done")
	_, err := os.Stat(doneFile)
	if err == nil || !os.IsNotExist(err) {
		log.Info(fmt.Sprintf("%s already exists, skipping", doneFile))
		return nil
	}

	fValue := reflect.ValueOf(f)

	var fArgs []reflect.Value
	for _, arg := range args {
		fArgs = append(fArgs, reflect.ValueOf(arg))
	}

	resultValues := fValue.Call(fArgs)
	if len(resultValues) > 0 {
		errVal, ok := resultValues[0].Interface().(error)
		if ok {
			return errVal
		}
	}

	_, err = os.Create(doneFile)
	if err != nil {
		return err
	}

	return nil
}

func ReadImageFromStaticPodDefinition(podFile, containerName string) (string, error) {
	pod := &corev1.Pod{}
	if err := ReadYamlOrJSONFile(podFile, pod); err != nil {
		return "", fmt.Errorf("failed to read %s pod static file, err: %w", podFile, err)
	}

	var etcdImage string
	for _, container := range pod.Spec.Containers {
		if container.Name == containerName {
			etcdImage = container.Image
			return etcdImage, nil
		}
	}

	return "", fmt.Errorf("no '%s' container found or no image specified in %s", containerName, podFile)
}

func HandleFilesWithCallback(folder string, action func(string) error) error {
	return filepath.Walk(folder, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		return action(path)
	})
}

func CopyFileIfExists(source, dest string) error {
	return cp.Copy(source, dest, cp.Options{OnError: func(src, dest string, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}})
}
