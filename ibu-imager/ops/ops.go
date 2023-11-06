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

const (
	// Default location for etcdStaticPodFile
	etcdStaticPodFile = "/etc/kubernetes/manifests/etcd-pod.yaml"
)

// Ops is an interface for executing commands and actions in the host namespace
//
//go:generate mockgen -source=ops.go -package=ops -destination=mock_ops.go
type Ops interface {
	SystemctlAction(action string, args ...string) (string, error)
	RunInHostNamespace(command string, args ...string) (string, error)
	RunBashInHostNamespace(command string, args ...string) (string, error)
	WaitForEtcd(endpoint string) error
	GetImageFromPodDefinition(containerImage string) (string, error)
}

type ops struct {
	log      *logrus.Logger
	executor Execute
}

// NewOps creates and returns an Ops interface for executing host namespace operations
func NewOps(log *logrus.Logger, executor Execute) Ops {
	return &ops{executor: executor, log: log}
}

func (o *ops) SystemctlAction(action string, args ...string) (string, error) {
	o.log.Infof("Running systemctl %s %s", action, args)
	output, err := o.RunInHostNamespace("systemctl", append([]string{action}, args...)...)
	if err != nil {
		err = fmt.Errorf("failed executing systemctl %s %s: %w", action, args, err)
	}
	return output, err
}

// RunInHostNamespace execute a command in the host environment via nsenter
func (o *ops) RunInHostNamespace(command string, args ...string) (string, error) {
	// nsenter is used here to launch processes inside the container in a way that makes said processes feel
	// and behave as if they're running on the host directly rather than inside the container
	commandBase := "nsenter"

	arguments := []string{
		"--target", "1",
		// Entering the cgroup namespace is not required for podman on CoreOS (where the
		// agent typically runs), but it's needed on some Fedora versions and
		// some other systemd based systems. Those systems are used to run dry-mode
		// agents for load testing. If this flag is not used, Podman will sometimes
		// have trouble creating a systemd cgroup slice for new containers.
		"--cgroup",
		// The mount namespace is required for podman to access the host's container
		// storage
		"--mount",
		// TODO: Document why we need the IPC namespace
		"--ipc",
		"--pid",
		"--",
		command,
	}

	arguments = append(arguments, args...)
	return o.executor.Execute(commandBase, arguments...)
}

func (o *ops) RunBashInHostNamespace(command string, args ...string) (string, error) {
	args = append([]string{command}, args...)
	return o.RunInHostNamespace("bash", "-c", strings.Join(args, " "))
}

// WaitForEtcd waits for the availability of the etcd server at the given healthz endpoint.
// It continuously checks the healthz endpoint to determine the server's availability.
// If the etcd server becomes accessible, it returns nil, indicating success. Otherwise, it returns an error.
func (o *ops) WaitForEtcd(healthzEndpoint string) error {
	// Use the default HTTP client
	client := http.DefaultClient

	for {
		// Send an HTTP GET request to the healthz endpoint
		resp, err := client.Get(healthzEndpoint)
		if err != nil {
			// If there was an error, wait for a short interval before retrying
			time.Sleep(1 * time.Second)
			o.log.Info(err)
			continue
		}

		// Ensure the response body is closed after reading
		defer resp.Body.Close()

		// If the server responds with HTTP status 200 (OK), it's up and running
		if resp.StatusCode == http.StatusOK {
			return nil
		}

		// Wait for a short interval before retrying
		time.Sleep(1 * time.Second)
	}
}

// GetImageFromPodDefinition reads a YAML file containing pod configuration data
// and extracts the image name for a given container named.
// It returns the image name if found, or an error if not found or encountered any issues.
func (o *ops) GetImageFromPodDefinition(containerImage string) (string, error) {

	// Define a struct to match the structure of the YAML data
	type PodConfig struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Spec struct {
			Containers []struct {
				Name  string `yaml:"name"`
				Image string `yaml:"image"`
			} `yaml:"containers"`
		} `yaml:"spec"`
	}

	// Read the YAML file
	yamlData, err := os.ReadFile(etcdStaticPodFile)
	if err != nil {
		return "", fmt.Errorf("error reading the YAML file: %w", err)
	}

	// Unmarshal the YAML data into the struct
	var podConfig PodConfig
	if err = yaml.Unmarshal(yamlData, &podConfig); err != nil {
		return "", fmt.Errorf("error unmarshaling YAML data: %w", err)
	}

	// Loop through the containers and find the one named "etcd"
	var etcdImage string
	for _, container := range podConfig.Spec.Containers {
		if container.Name == containerImage {
			etcdImage = container.Image
			return etcdImage, nil
		}
	}

	// Return an error if no "etcd" container found or no image specified in YAML definition.
	return "", fmt.Errorf("no 'etcd' container found or no image specified in YAML definition: %w", err)
}
