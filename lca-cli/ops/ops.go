/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ops

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/recert"

	"github.com/openshift-kni/lifecycle-agent/utils"
	"github.com/openshift/assisted-image-service/pkg/isoeditor"
	cp "github.com/otiai10/copy"
)

const (
	podman = "podman"
)

var podmanRecertArgs = []string{
	"run", "--rm", "--network=host", "--privileged", "--replace",
}

// Ops is an interface for executing commands and actions in the host namespace
//
//go:generate mockgen -source=ops.go -package=ops -destination=mock_ops.go
type Ops interface {
	SystemctlAction(action string, args ...string) (string, error)
	RunSystemdAction(args ...string) (string, error)
	RunInHostNamespace(command string, args ...string) (string, error)
	RunBashInHostNamespace(command string, args ...string) (string, error)
	RunListOfCommands(cmds []*CMD) error
	ReadFile(filename string) ([]byte, error)
	WriteFile(filename string, data []byte, perm os.FileMode) error
	CopyFile(src, dest string, perm os.FileMode) error
	MkdirAll(path string, perm os.FileMode) error
	RemoveFile(path string) error
	RemoveAllFiles(path string) error
	ReadDir(path string) ([]os.DirEntry, error)
	StatFile(name string) (os.FileInfo, error)
	IsNotExist(err error) bool
	ForceExpireSeedCrypto(recertContainerImage, authFile string, hasKubeAdminPassword bool) error
	RestoreOriginalSeedCrypto(recertContainerImage, authFile string) error
	RunUnauthenticatedEtcdServer(authFile, name string) error
	StopEtcdServer(authfile, name string) error
	waitForEtcd(healthzEndpoint string) error
	RunRecert(recertContainerImage, authFile, recertConfigFile string, additionalPodmanParams ...string) error
	ExtractTarWithSELinux(srcPath, destPath string) error
	RemountSysroot() error
	RemountBoot() error
	ImageExists(img string) (bool, error)
	IsImageMounted(img string) (bool, error)
	UnmountAndRemoveImage(img string) error
	MountImage(img string) (string, error)
	RecertFullFlow(recertContainerImage, authFile, configFile string,
		preRecertOperations func() error, postRecertOperations func() error, additionalPodmanParams ...string) error
	ListBlockDevices() ([]BlockDevice, error)
	Mount(deviceName, mountFolder string) error
	Umount(deviceName string) error
	Chroot(chrootPath string) (func() error, error)
	CreateExtraPartition(installationDisk, extraPartitionLabel, extraPartitionStart string, extraPartitionNumber uint) error
	SetupContainersFolderCommands() error
	GetHostname() (string, error)
	CreateIsoWithEmbeddedIgnition(log logrus.FieldLogger, ignitionBytes []byte, baseIsoPath, outputIsoPath string) error
	GetContainerStorageTarget() (string, error)
	StopClusterServices() error
	EnableClusterServices() error
}

type CMD struct {
	command string
	args    []string
}

func NewCMD(command string, args ...string) *CMD {
	return &CMD{command: command, args: args}
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

func (o *ops) RunSystemdAction(args ...string) (string, error) {
	o.log.Infof("Running systemd-run %s", args)
	output, err := o.hostCommandsExecutor.Execute("systemd-run", args...)
	if err != nil {
		err = fmt.Errorf("failed executing systemd-run with args %s: %w", args, err)
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
		return "", fmt.Errorf("failed to run %q in host namespace with args %s: %w", command, args, err)
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

	originalPasswordHash, err := utils.LoadKubeadminPasswordHash(common.BackupCertsDir)
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

	command := podman
	// nolint: gocritic
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

func (o *ops) StopEtcdServer(authfile, name string) error {
	o.log.Info("Stopping the unauthenticated etcd server")
	if _, err := o.RunInHostNamespace(podman, "stop", common.EtcdContainerName); err != nil {
		o.log.WithError(err).Errorf("failed to stop %s container.", common.EtcdContainerName)
	}
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
	command := podman

	// nolint: gocritic
	args := append(podmanRecertArgs, "--name", "recert",
		"-v", fmt.Sprintf("/etc:%s", recert.EtcMount),
		"-v", fmt.Sprintf("/etc/ssh:%s", recert.EtcSSHMount),
		"-v", fmt.Sprintf("/etc/kubernetes:%s", recert.EtcKubernetesMount),
		"-v", fmt.Sprintf("/var/lib/kubelet:%s", recert.VarLibKubeletMount),
		"-v", "/var/tmp:/var/tmp",
		"-v", fmt.Sprintf("/etc/machine-config-daemon:%s", recert.EtcMachineConfigDaemonMount),
		"-v", fmt.Sprintf("/etc/pki:%s", recert.EtcPKIMount),
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

// prepareSELinuxTar prepares a copy of tar executable with install_exec_t context.
// This type allows SELinux labeling of non-existing labels
func (o *ops) prepareSELinuxTar() (string, error) {
	tarPath := common.PathOutsideChroot("/usr/bin/tar")
	destDir := common.PathOutsideChroot("/var/tmp")
	newTarPath, err := utils.CopyToTempFile(tarPath, destDir, "tar-")
	if err != nil {
		return "", fmt.Errorf("failed to copy tar to temporary file: %w", err)
	}

	newTarPathInsideChroot, err := common.PathInsideChroot(newTarPath)
	if err != nil {
		return "", fmt.Errorf("failed to get path for file %s inside chroot: %w", newTarPath, err)
	}

	// Set SELinux attribute
	if _, err := o.hostCommandsExecutor.Execute("chcon", "-t", "install_exec_t", newTarPathInsideChroot); err != nil {
		return "", fmt.Errorf("failed to set SELinux context install_exec_t to %s: %w", newTarPath, err)
	}
	return newTarPath, nil
}

func (o *ops) ExtractTarWithSELinux(srcPath, destPath string) error {
	// Create a copy of /usr/bin/tar with extended permissions
	tarExec, err := o.prepareSELinuxTar()
	if err != nil {
		return fmt.Errorf("failed to create copy of tar with install_exec_t attribute: %w", err)
	}
	defer os.Remove(tarExec) // Cleanup temporary tar copy afterwards

	// Path as seen inside the chroot (without /host prepended)
	tarExecInsideChroot, err := common.PathInsideChroot(tarExec)
	if err != nil {
		return fmt.Errorf("failed to get path for file %s inside chroot: %w", tarExec, err)
	}

	tarArgs := []string{"xzf", srcPath, "-C", destPath}
	tarArgs = append(tarArgs, common.TarOpts...)

	if _, err = o.hostCommandsExecutor.Execute(tarExecInsideChroot, tarArgs...); err != nil {
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

func (o *ops) RemountBoot() error {
	if _, err := o.hostCommandsExecutor.Execute("mount", "/boot", "-o", "remount,rw"); err != nil {
		return fmt.Errorf("failed to remount boot: %w", err)
	}
	return nil
}

func (o *ops) ImageExists(img string) (bool, error) {
	_, err := o.hostCommandsExecutor.Execute(podman, "image", "exists", img)
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
	output, err := o.hostCommandsExecutor.Execute(podman, "image", "mount", "--format", "json", "--log-level", "error")
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
		podman, "rmi", img,
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
			podman, "image", "umount", img,
		); err != nil {
			return fmt.Errorf("failed to unmount image: %w", err)
		}
	}

	return o.removeImage(img)
}

func (o *ops) MountImage(img string) (string, error) {
	command := podman
	args := []string{"image", "mount", img}

	// In 4.18 crio got an update which removes all containers
	// that don't have a mount point on the host.
	// In order to workaround the issue we need to mount image in host namespace
	if _, ok := o.hostCommandsExecutor.(*chrootExecutor); ok {
		args = append([]string{command}, args...)
		args = append(nsenterArgs(), args...)
		command = nsenter
	}

	mountpoint, err := o.hostCommandsExecutor.Execute(command, args...)
	if err != nil {
		return mountpoint, fmt.Errorf("failed to mount seed image: %w", err)
	}
	return mountpoint, nil
}

func (o *ops) RecertFullFlow(recertContainerImage, authFile, configFile string,
	preRecertOperations func() error, postRecertOperations func() error, additionalPodmanParams ...string) error {
	if err := o.RunUnauthenticatedEtcdServer(authFile, common.EtcdContainerName); err != nil {
		return fmt.Errorf("failed to run etcd, err: %w", err)
	}

	defer o.StopEtcdServer(authFile, common.EtcdContainerName)

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

func (o *ops) Chroot(chrootPath string) (func() error, error) {
	root, err := os.Open("/")
	if err != nil {
		return nil, fmt.Errorf("failed to open root directory: %w", err)
	}

	if err := syscall.Chroot(chrootPath); err != nil {
		root.Close()
		return nil, fmt.Errorf("failed to chroot to %s, err: %w", chrootPath, err)
	}

	return func() error {
		defer root.Close()
		if err := root.Chdir(); err != nil {
			return fmt.Errorf("failed to change directory to root: %w", err)
		}
		if err := syscall.Chroot("."); err != nil {
			return fmt.Errorf("failed to unchroot: %w", err)
		}
		return nil
	}, nil
}

func (o *ops) RunListOfCommands(cmds []*CMD) error {
	for _, c := range cmds {
		if _, err := o.hostCommandsExecutor.Execute(c.command, c.args...); err != nil {
			return fmt.Errorf("failed to run %s with args %s: %w", c.command, c.args, err)
		}
	}
	return nil
}

// nolint: wrapcheck // this method intentionally returns the underlying os error directly
func (o *ops) ReadFile(filename string) ([]byte, error) {
	return os.ReadFile(filename)
}

// nolint: wrapcheck // this method intentionally returns the underlying os error directly
func (o *ops) WriteFile(filename string, data []byte, perm os.FileMode) error {
	return os.WriteFile(filename, data, perm)
}

// nolint: wrapcheck // this method intentionally returns the underlying os error directly
func (o *ops) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

// nolint: wrapcheck // this method intentionally returns the underlying copy error directly
func (o *ops) CopyFile(src, dest string, perm os.FileMode) error {
	return cp.Copy(src, dest, cp.Options{AddPermission: perm})
}

// nolint: wrapcheck // this method intentionally returns the underlying os error directly
func (o *ops) RemoveFile(path string) error {
	return os.Remove(path)
}

// nolint: wrapcheck // this method intentionally returns the underlying os error directly
func (o *ops) RemoveAllFiles(path string) error {
	return os.RemoveAll(path)
}

// nolint: wrapcheck // this method intentionally returns the underlying os error directly
func (o *ops) ReadDir(path string) ([]os.DirEntry, error) {
	return os.ReadDir(path)
}

// nolint: wrapcheck // this method intentionally returns the underlying os error directly
func (o *ops) StatFile(name string) (os.FileInfo, error) {
	return os.Stat(name)
}

func (o *ops) IsNotExist(err error) bool {
	return os.IsNotExist(err)
}

func (o *ops) CreateExtraPartition(installationDisk, extraPartitionLabel, extraPartitionStart string, extraPartitionNumber uint) error {
	o.log.Info("Creating extra partition")
	if _, err := o.RunBashInHostNamespace(
		"echo", "write", "|", "sfdisk", installationDisk); err != nil {
		return fmt.Errorf("failed to create extra partition: %w", err)
	}
	if _, err := o.RunInHostNamespace("sgdisk", "--new",
		fmt.Sprintf("%d:%s", extraPartitionNumber, extraPartitionStart),
		"--change-name", fmt.Sprintf("%d:%s", extraPartitionNumber, extraPartitionLabel),
		installationDisk); err != nil {
		return fmt.Errorf("failed to create extra partition: %w", err)
	}

	extraPartitionPath, err := o.RunBashInHostNamespace("lsblk", installationDisk, "--json", "-O", "|", "jq",
		fmt.Sprintf(".blockdevices[0].children[%d].path", extraPartitionNumber-1), "-r")
	if err != nil {
		return fmt.Errorf("failed to get extra partition path: %w", err)
	}

	var cmds []*CMD
	cmds = append(cmds, NewCMD("mkfs.xfs", "-f", extraPartitionPath))
	cmds = append(cmds, o.growRootPartitionCommands(installationDisk)...)
	cmds = append(cmds, NewCMD("mount",
		fmt.Sprintf("/dev/disk/by-partlabel/%s", extraPartitionLabel), common.ContainerStoragePath),
		NewCMD("restorecon", "-R", common.ContainerStoragePath))
	if err := o.RunListOfCommands(cmds); err != nil {
		return fmt.Errorf("failed to grow root partition: %w", err)
	}

	return nil
}

func (o *ops) SetupContainersFolderCommands() error {
	o.log.Info("Setting up containers folder")
	var cmds []*CMD
	cmds = append(cmds, NewCMD("chattr", "-i", "/mnt/"),
		NewCMD("mkdir", "-p", "/mnt/containers"),
		NewCMD("chattr", "+i", "/mnt/"),
		NewCMD("mount", "-o", "bind", "/mnt/containers", common.ContainerStoragePath),
		NewCMD("restorecon", "-R", common.ContainerStoragePath))
	if err := o.RunListOfCommands(cmds); err != nil {
		return fmt.Errorf("failed to setup containers folder: %w", err)
	}
	return nil
}

func (o *ops) growRootPartitionCommands(installationDisk string) []*CMD {
	var cmds []*CMD
	cmds = append(cmds, NewCMD("growpart", installationDisk, "4"),
		NewCMD("mount", "/dev/disk/by-partlabel/root", "/mnt"),
		NewCMD("mount", "/dev/disk/by-partlabel/boot", "/mnt/boot"),
		NewCMD("xfs_growfs", "/dev/disk/by-partlabel/root"))

	return cmds
}

func (o *ops) GetHostname() (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("failed to get hostname: %w", err)
	}
	return hostname, nil
}

func (o *ops) CreateIsoWithEmbeddedIgnition(log logrus.FieldLogger, ignitionBytes []byte, baseIsoPath, outputIsoPath string) error {
	ignitionc := &isoeditor.IgnitionContent{}
	ignitionc.Config = ignitionBytes
	reader, err := isoeditor.NewRHCOSStreamReader(baseIsoPath, ignitionc, nil, nil)
	if err != nil {
		return fmt.Errorf("failed to create reader for rhcos iso: %w", err)
	}
	log.Info("Creating IBI ISO with embedded ignition")
	file, err := os.Create(outputIsoPath)
	if err != nil {
		return fmt.Errorf("failed to create ibi iso file: %w", err)
	}
	defer file.Close()
	if _, err := io.Copy(file, reader); err != nil {
		return fmt.Errorf("failed to copy reader to file: %w", err)
	}

	if err := file.Sync(); err != nil {
		return fmt.Errorf("failed to sync file: %w", err)
	}
	return nil
}

func (o *ops) GetContainerStorageTarget() (string, error) {
	const containerStorageMountUnit = "var-lib-containers.mount"
	if _, err := o.SystemctlAction("is-active", containerStorageMountUnit); err != nil {
		// No active mount, return nil
		return "", nil
	}

	output, err := o.SystemctlAction("cat", containerStorageMountUnit)
	if err != nil {
		return "", fmt.Errorf("unable to systemctl cat %s: %w", containerStorageMountUnit, err)
	}

	// Parse the output to find the mountpoint target
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		field, value, _ := strings.Cut(scanner.Text(), "=")
		if field == "What" {
			return value, nil
		}

	}
	return "", fmt.Errorf("failed to find mountpoint target in %s", containerStorageMountUnit)
}

// StopClusterServices stops kubelet and crio services with proper container cleanup
func (o *ops) StopClusterServices() error {
	o.log.Info("Stop kubelet service")
	_, err := o.SystemctlAction("stop", "kubelet.service")
	if err != nil {
		return fmt.Errorf("failed to stop kubelet: %w", err)
	}

	o.log.Info("Disabling kubelet service")
	_, err = o.SystemctlAction("disable", "kubelet.service")
	if err != nil {
		return fmt.Errorf("failed to disable kubelet: %w", err)
	}

	o.log.Info("Stopping containers and CRI-O runtime.")
	crioSystemdStatus, err := o.SystemctlAction("is-active", "crio")
	var exitErr *exec.ExitError
	// If ExitCode is 3, the command succeeded and told us that crio is down
	if err != nil && errors.As(err, &exitErr) && exitErr.ExitCode() != 3 {
		return fmt.Errorf("failed to checking crio status: %w", err)
	}
	o.log.Info("crio status is ", crioSystemdStatus)
	if crioSystemdStatus == "active" {
		// CRI-O is active, so stop running containers with retry
		_ = wait.PollUntilContextCancel(context.TODO(), time.Second, true, func(ctx context.Context) (done bool, err error) {
			o.log.Info("Stop running containers")
			args := []string{"ps", "-q", "|", "xargs", "--no-run-if-empty", "--max-args", "1", "--max-procs", "10", "crictl", "stop", "--timeout", "5"}
			_, err = o.RunBashInHostNamespace("crictl", args...)
			if err != nil {
				return false, fmt.Errorf("failed to stop running containers: %w", err)
			}
			return true, nil
		})

		// Execute a D-Bus call to stop the CRI-O runtime
		o.log.Debug("Stopping CRI-O engine")
		_, err = o.SystemctlAction("stop", "crio.service")
		if err != nil {
			return fmt.Errorf("failed to stop crio engine: %w", err)
		}
		o.log.Info("Running containers and CRI-O engine stopped successfully.")
	} else {
		o.log.Info("Skipping running containers and CRI-O engine already stopped.")
	}

	return nil
}

func (o *ops) EnableClusterServices() error {
	_, err := o.SystemctlAction("enable", "kubelet.service")
	if err != nil {
		return fmt.Errorf("failed to enable kubelet: %w", err)
	}
	return nil
}
