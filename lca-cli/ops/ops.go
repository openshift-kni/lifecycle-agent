package ops

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/recert"
	"github.com/openshift-kni/lifecycle-agent/utils"
	"github.com/sirupsen/logrus"
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
	ForceExpireSeedCrypto(recertContainerImage, authFile string, hasKubeAdminPassword bool) error
	RestoreOriginalSeedCrypto(recertContainerImage, authFile string) error
	RunUnauthenticatedEtcdServer(authFile, name string) error
	waitForEtcd(healthzEndpoint string) error
	RunRecert(recertContainerImage, authFile, recertConfigFile string, additionalPodmanParams ...string) error
	ExtractTarWithSELinux(srcPath, destPath string) error
	RemountSysroot() error
	ImageExists(img string) (bool, error)
	IsImageMounted(img string) (bool, error)
	UnmountAndRemoveImage(img string) error
	RecertFullFlow(recertContainerImage, authFile, configFile string,
		preRecertOperations func() error, postRecertOperations func() error, additionalPodmanParams ...string) error
	ListBlockDevices() ([]BlockDevice, error)
	Mount(deviceName, mountFolder string) error
	Umount(deviceName string) error
	ListNodeAddresses() ([]net.Addr, error)
}

type BlockDevice struct {
	Name  string
	Label string
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
	execute, err := o.hostCommandsExecutor.Execute("bash", "-c", strings.Join(args, " "))
	if err != nil {
		return "", fmt.Errorf("failed to run bash in host namespace with args %s: %w", args, err)
	}
	return execute, nil
}

func (o *ops) RunInHostNamespace(command string, args ...string) (string, error) {
	execute, err := o.hostCommandsExecutor.Execute(command, args...)
	if err != nil {
		return "", fmt.Errorf("failed to run in host namespace with args %s: %w", args, err)
	}
	return execute, nil
}

func (o *ops) ForceExpireSeedCrypto(recertContainerImage, authFile string, hasKubeAdminPassword bool) error {
	o.log.Info("Running recert --force-expire tool and saving a summary without sensitive data")
	// Run recert tool to force expiration of seed cluster certificates, and save a summary without sensitive data.
	// This pre-check is also useful for validating that a cluster can be re-certified error-free before turning it
	// into a seed image.
	o.log.Info("Run recert --force-expire tool")
	recertConfigFile := path.Join(common.BackupDir, recert.RecertConfigFile)
	if err := recert.CreateRecertConfigFileForSeedCreation(recertConfigFile, hasKubeAdminPassword); err != nil {
		return fmt.Errorf("failed to create %s file: %w", recertConfigFile, err)
	}
	if err := o.RecertFullFlow(recertContainerImage, authFile, recertConfigFile, nil, nil); err != nil {
		return err
	}

	o.log.Info("Recert --force-expire tool ran and summary created successfully.")
	return nil
}

func (o *ops) RestoreOriginalSeedCrypto(recertContainerImage, authFile string) error {
	o.log.Info("Running recert --extend-expiration tool to restore original seed crypto")
	o.log.Info("Run recert --extend-expiration tool")
	recertConfigFile := path.Join(common.BackupCertsDir, recert.RecertConfigFile)

	originalPasswordHash, err := utils.LoadKubeadminPasswordHash(common.BackupDir)
	if err != nil {
		return fmt.Errorf("failed to load kubeadmin password hash: %w", err)
	}

	if err := recert.CreateRecertConfigFileForSeedRestoration(recertConfigFile, originalPasswordHash); err != nil {
		return fmt.Errorf("failed to create %s file", recertConfigFile)
	}

	postRecertOp := func() error {
		o.log.Infof("Removing %s folder", common.BackupCertsDir)
		if err := os.RemoveAll(common.BackupCertsDir); err != nil {
			return fmt.Errorf("error removing %s: %w", common.BackupCertsDir, err)
		}
		return nil
	}

	if err := o.RecertFullFlow(recertContainerImage, authFile, recertConfigFile, nil, postRecertOp); err != nil {
		return err
	}

	o.log.Info("Certificates in seed SNO cluster restored successfully.")
	return nil
}

// RunUnauthenticatedEtcdServer Run unauthenticated etcd server for the recert tool.
// This runs a small (fake) unauthenticated etcd server backed by the actual etcd database,
// which is required before running the recert tool.
func (o *ops) RunUnauthenticatedEtcdServer(authFile, name string) error {
	// Get etcdImage available for the current release, this is needed by recert to
	// run an unauthenticated etcd server for running successfully.
	o.log.Infof("Getting image from %s static pod file", common.EtcdStaticPodFile)
	etcdImage, err := utils.ReadImageFromStaticPodDefinition(common.EtcdStaticPodFile, common.EtcdStaticPodContainer)
	if err != nil {
		return fmt.Errorf("failed to get image from %s static pod file: %w", common.EtcdStaticPodFile, err)
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
	if err := o.waitForEtcd("http://" + common.EtcdDefaultEndpoint + "/health"); err != nil {
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
			resp, err := http.Get(healthzEndpoint) //nolint:gosec
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
	o.log.Info("Start running recert")
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

func (o *ops) ExtractTarWithSELinux(srcPath, destPath string) error {
	if _, err := o.hostCommandsExecutor.Execute(
		"tar", "xzf", srcPath, "-C", destPath, "--selinux",
	); err != nil {
		return fmt.Errorf("failed to extract tar with SELinux with sourcePath %s and destPath %s: %w", srcPath, destPath, err)
	}
	return nil
}

func (o *ops) RemountSysroot() error {
	if _, err := o.hostCommandsExecutor.Execute("mount", "/sysroot", "-o", "remount,rw"); err != nil {
		return fmt.Errorf("failed to remount sysroot: %w", err)
	}
	return nil
}

func (o *ops) ImageExists(img string) (bool, error) {
	_, err := o.hostCommandsExecutor.Execute("podman", "image", "exists", img)
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			if exitError.ExitCode() == 1 {
				return false, nil
			}
			return false, fmt.Errorf("failed to run podman image exists: %w", err)
		} else {
			return false, fmt.Errorf("failed to run podman image exists with an unknown erorr: %w", err)
		}
	}
	return true, nil
}

type PodmanImage struct {
	Repositories []string `json:"Repositories"`
}

// IsImageMounted checkes whether certain image is mounted
// pass in the full address with tag e.g: quay.io/openshift/lifecycle-agent-operator:latest
func (o *ops) IsImageMounted(imgName string) (bool, error) {
	output, err := o.hostCommandsExecutor.Execute("podman", "image", "mount", "--format", "json")
	if err != nil {
		return false, fmt.Errorf("failed to mount podamn image: %w", err)
	}
	var images []PodmanImage
	if err := json.Unmarshal([]byte(output), &images); err != nil {
		return false, fmt.Errorf("failed to unmarshall podamn images: %w", err)
	}
	for _, img := range images {
		for _, tag := range img.Repositories {
			if tag == imgName {
				return true, nil
			}
		}
	}
	return false, nil
}

func (o *ops) removeImage(img string) error {
	exist, err := o.ImageExists(img)
	if err != nil {
		return fmt.Errorf("failed to check if image exist: %w", err)
	}
	if !exist {
		return nil
	}
	if _, err := o.hostCommandsExecutor.Execute(
		"podman", "rmi", img,
	); err != nil {
		return fmt.Errorf("failed to remove image: %w", err)
	}
	return nil
}

func (o *ops) UnmountAndRemoveImage(img string) error {
	if mounted, err := o.IsImageMounted(img); err != nil {
		return fmt.Errorf("failed to check if image is mounted: %w", err)
	} else if mounted {
		if _, err := o.hostCommandsExecutor.Execute(
			"podman", "image", "umount", img,
		); err != nil {
			return fmt.Errorf("failed to unmount image: %w", err)
		}
	}

	return o.removeImage(img)
}

func (o *ops) RecertFullFlow(recertContainerImage, authFile, configFile string,
	preRecertOperations func() error, postRecertOperations func() error, additionalPodmanParams ...string) error {
	if err := o.RunUnauthenticatedEtcdServer(authFile, common.EtcdContainerName); err != nil {
		return fmt.Errorf("failed to run etcd, err: %w", err)
	}

	defer func() {
		o.log.Info("Killing the unauthenticated etcd server")
		if _, err := o.RunInHostNamespace("podman", "stop", common.EtcdContainerName); err != nil {
			o.log.WithError(err).Errorf("failed to kill %s container.", common.EtcdContainerName)
		}
	}()

	if preRecertOperations != nil {
		if err := preRecertOperations(); err != nil {
			return err
		}
	}

	if err := o.RunRecert(recertContainerImage, authFile, configFile,
		additionalPodmanParams...); err != nil {
		return err
	}

	if postRecertOperations != nil {
		if err := postRecertOperations(); err != nil {
			return err
		}
	}

	return nil
}

// ListBlockDevices runs lsblk command and not using go library cause
// each library that i was looking into doesn't show label for block device and shows labels only for partitions
func (o *ops) ListBlockDevices() ([]BlockDevice, error) {
	o.log.Info("Listing block devices")
	lsblkOutput, err := o.RunInHostNamespace("lsblk", "-f",
		"--json", "--output", "NAME,LABEL")
	if err != nil {
		return nil, fmt.Errorf("failed to run lsblk, err: %w", err)
	}
	blockDevices := map[string][]BlockDevice{}
	err = json.Unmarshal([]byte(lsblkOutput), &blockDevices)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal lsblk output, err %w", err)
	}
	blockDeviceList, ok := blockDevices["blockdevices"]
	if !ok {
		return nil, fmt.Errorf("failed to find blockdevices in lsblk output")
	}

	return blockDeviceList, nil
}

func (o *ops) Mount(deviceName, mountFolder string) error {
	o.log.Infof("Mounting %s into %s", deviceName, mountFolder)
	if err := os.MkdirAll(mountFolder, 0o700); err != nil {
		return fmt.Errorf("failed to create %s, err: %w", mountFolder, err)
	}
	if _, err := o.RunInHostNamespace("mount", fmt.Sprintf("/dev/%s", deviceName), mountFolder); err != nil {
		return fmt.Errorf("failed to mount %s into %s, err: %w", deviceName, mountFolder, err)
	}
	return nil
}

func (o *ops) Umount(deviceName string) error {
	o.log.Infof("Unmounting %s", deviceName)
	if _, err := o.RunInHostNamespace("umount", fmt.Sprintf("/dev/%s", deviceName)); err != nil {
		return fmt.Errorf("failed to unmount %s, err: %w", deviceName, err)
	}
	return nil
}

// ListNodeAddresses return a list of all IP addresses currently configured on the host
func (o *ops) ListNodeAddresses() ([]net.Addr, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("failed to get node interfaces, err: %w", err)
	}
	var addresses []net.Addr
	for _, iface := range ifaces {
		ips, err := iface.Addrs()
		if err != nil {
			o.log.Warnf("Failing to get addressed for %s interface", iface.Name)
			continue
		}
		addresses = append(addresses, ips...)
	}

	return addresses, nil
}
