package ops

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// Ops is an interface for executing commands and actions in the host namespace
//
//go:generate mockgen -source=ops.go -package=ops -destination=mock_ops.go
type Ops interface {
	SystemctlAction(action string, args ...string) (string, error)
	RunInHostNamespace(command string, args ...string) (string, error)
	RunBashInHostNamespace(command string, args ...string) (string, error)
	WaitForEtcd(endpoint string) error
	GetImageFromPodDefinition(etcdStaticPodFile, containerImage string) (string, error)
}

type ops struct {
	log                  *logrus.Logger
	hostCommandsExecutor Execute
}

// NewOps creates and returns an Ops interface for executing host namespace operations
func NewOps(log *logrus.Logger, hostCommandsExecutor Execute) Ops {
	return &ops{hostCommandsExecutor: hostCommandsExecutor, log: log}
}

func (o *ops) SystemctlAction(action string, args ...string) (string, error) {
	o.log.Infof("Running systemctl %s %s", action, args)
	output, err := o.hostCommandsExecutor.Execute("systemctl", append([]string{action}, args...)...)
	if err != nil {
		err = fmt.Errorf("failed executing systemctl %s %s: %w", action, args, err)
	}
	return output, err
}

func (o *ops) RunBashInHostNamespace(command string, args ...string) (string, error) {
	args = append([]string{command}, args...)
	return o.hostCommandsExecutor.Execute("bash", "-c", strings.Join(args, " "))
}

func (o *ops) RunInHostNamespace(command string, args ...string) (string, error) {
	return o.hostCommandsExecutor.Execute(command, args...)
}

func (o *ops) WaitForEtcd(healthzEndpoint string) error {
	for {
		resp, err := http.Get(healthzEndpoint)
		if err != nil {
			o.log.Infof("Waiting for etcd: %s", err)
			time.Sleep(1 * time.Second)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			o.log.Infof("Waiting for etcd, status: %d", resp.StatusCode)
			time.Sleep(1 * time.Second)
			continue
		}

		return nil
	}
}

func (o *ops) GetImageFromPodDefinition(podFile, containerImage string) (string, error) {
	type PodConfig struct {
		Spec struct {
			Containers []struct {
				Name  string `yaml:"name"`
				Image string `yaml:"image"`
			} `yaml:"containers"`
		} `yaml:"spec"`
	}

	yamlData, err := os.ReadFile(podFile)
	if err != nil {
		return "", fmt.Errorf("error reading the YAML file: %w", err)
	}

	var podConfig PodConfig
	if err = yaml.Unmarshal(yamlData, &podConfig); err != nil {
		return "", fmt.Errorf("error unmarshaling YAML data: %w", err)
	}

	var etcdImage string
	for _, container := range podConfig.Spec.Containers {
		if container.Name == containerImage {
			etcdImage = container.Image
			return etcdImage, nil
		}
	}

	return "", fmt.Errorf("no 'etcd' container found or no image specified in YAML definition: %w", err)
}
