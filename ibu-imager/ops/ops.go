package ops

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

var podmanRecertArgs = []string{
	"run", "--rm", "--network=host", "--privileged", "--replace",
}

// Ops is an interface for executing commands and actions in the host namespace
//
//go:generate mockgen -source=ops.go -package=ops -destination=mock_ops.go
type Ops interface {
	SystemctlAction(action string, args ...string) (string, error)
	RunInHostNamespace(command string, args ...string) (string, error)
	RunBashInHostNamespace(command string, args ...string) (string, error)
	RunUnauthenticatedEtcdServer(authFile, name string) error
	GetImageFromPodDefinition(etcdStaticPodFile, containerImage string) (string, error)
	RunRecert(recertContainerImage, authFile, recertConfigFile string, additionalPodmanParams ...string) error
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

func (o *ops) GetImageFromPodDefinition(podFile, containerName string) (string, error) {
	o.log.Infof("Getting image from %s static pod file", podFile)
	pod := &corev1.Pod{}
	if err := utils.ReadYamlOrJSONFile(podFile, pod); err != nil {
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

func (o *ops) RunUnauthenticatedEtcdServer(authFile, name string) error {
	// Get etcdImage available for the current release, this is needed by recert to
	// run an unauthenticated etcd server for running successfully.
	etcdImage, err := o.GetImageFromPodDefinition(common.EtcdStaticPodFile, common.EtcdStaticPodContainer)
	if err != nil {
		return err
	}

	o.log.Info("Run unauthenticated etcd server for recert tool")

	command := "podman"
	args := append(podmanRecertArgs,
		"--authfile", authFile, "--detach",
		"--name", name,
		"--entrypoint", "etcd",
		"-v", "/var/lib/etcd:/store",
		etcdImage,
		"--name", "editor", "--data-dir", "/store")

	// Run the command and return an error if it occurs
	if _, err := o.RunInHostNamespace(command, args...); err != nil {
		return err
	}

	o.log.Info("Waiting for unauthenticated etcd start serving for recert tool")
	if err := o.waitForEtcd(common.EtcdDefaultEndpoint + "/health"); err != nil {
		return fmt.Errorf("failed to wait for unauthenticated etcd server: %w", err)
	}
	o.log.Info("Unauthenticated etcd server for recert is up and running")

	return nil
}

func (o *ops) waitForEtcd(healthzEndpoint string) error {
	timeout := time.After(1 * time.Minute)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for etcd")
		case <-ticker.C:
			resp, err := http.Get(healthzEndpoint)
			if err != nil {
				o.log.Infof("Waiting for etcd: %s", err)
				continue
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				o.log.Infof("Waiting for etcd, status: %d", resp.StatusCode)
				continue
			}

			return nil
		}
	}
}

func (o *ops) RunRecert(recertContainerImage, authFile, recertConfigFile string, additionalPodmanParams ...string) error {

	command := "podman"
	args := append(podmanRecertArgs, "--name", "recert",
		"-v", "/etc:/host-etc",
		"-v", "/etc/kubernetes:/kubernetes",
		"-v", "/var/lib/kubelet:/kubelet",
		"-v", "/var/tmp:/var/tmp",
		"-v", "/etc/machine-config-daemon:/machine-config-daemon",
		"-e", fmt.Sprintf("RECERT_CONFIG=%s", recertConfigFile),
	)
	if authFile != "" {
		args = append(args, "--authfile", authFile)
	}

	args = append(args, additionalPodmanParams...)
	args = append(args, recertContainerImage)
	if _, err := o.hostCommandsExecutor.Execute(command, args...); err != nil {
		return fmt.Errorf("failed to run recert tool container: %w", err)
	}

	return nil
}
