package ibi_preparation

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
	preinstallUtils "github.com/rh-ecosystem-edge/preinstall-utils/pkg"
	"github.com/sirupsen/logrus"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	"github.com/openshift-kni/lifecycle-agent/internal/precache/workload"
	"github.com/openshift-kni/lifecycle-agent/internal/prep"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
)

const imageListFile = "var/tmp/imageListFile"

type IBIPrepare struct {
	log                        *logrus.Logger
	ops                        ops.Ops
	authFile                   string
	seedImage                  string
	rpmostreeClient            rpmostreeclient.IClient
	ostreeClient               ostreeclient.IClient
	seedExpectedVersion        string
	pullSecretFile             string
	precacheBestEffort         bool
	precacheDisabled           bool
	shutdown                   bool
	installationDisk           string
	shouldCreateExtraPartition bool
	extraPartitionLabel        string
	extraPartitionStart        string
	extraPartitionNumber       int
	cleanupDevice              preinstallUtils.CleanupDevice
	skipDiskCleanup            bool
}

func NewIBIPrepare(log *logrus.Logger, ops ops.Ops, rpmostreeClient rpmostreeclient.IClient,
	ostreeClient ostreeclient.IClient, cleanupDevice preinstallUtils.CleanupDevice,
	seedImage, authFile, pullSecretFile,
	seedExpectedVersion, installationDisk, extraPartitionLabel, extraPartitionStart string,
	precacheBestEffort, precacheDisabled, shutdown, shouldCreateExtraPartition, skipDiskCleanup bool,
	extraPartitionNumber int) *IBIPrepare {
	return &IBIPrepare{
		log:                        log,
		ops:                        ops,
		authFile:                   authFile,
		pullSecretFile:             pullSecretFile,
		seedImage:                  seedImage,
		rpmostreeClient:            rpmostreeClient,
		ostreeClient:               ostreeClient,
		seedExpectedVersion:        seedExpectedVersion,
		precacheDisabled:           precacheDisabled,
		precacheBestEffort:         precacheBestEffort,
		shutdown:                   shutdown,
		installationDisk:           installationDisk,
		shouldCreateExtraPartition: shouldCreateExtraPartition,
		extraPartitionLabel:        extraPartitionLabel,
		extraPartitionStart:        extraPartitionStart,
		extraPartitionNumber:       extraPartitionNumber,
		cleanupDevice:              cleanupDevice,
		skipDiskCleanup:            skipDiskCleanup,
	}
}

func (i *IBIPrepare) Run() error {
	// Pull seed image
	if err := i.diskPreparation(); err != nil {
		return fmt.Errorf("failed to prepare disk: %w", err)
	}

	i.log.Info("Pulling seed image")
	if _, err := i.ops.RunInHostNamespace("podman", "pull", "--authfile", i.authFile, i.seedImage); err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}

	// TODO: change to logrus after refactoring the code in controllers and moving to logrus
	log := logr.Logger{}
	common.OstreeDeployPathPrefix = "/mnt/"
	// Setup state root
	if err := prep.SetupStateroot(log, i.ops, i.ostreeClient, i.rpmostreeClient,
		i.seedImage, i.seedExpectedVersion, imageListFile, true); err != nil {
		return fmt.Errorf("failed to setup stateroot: %w", err)
	}

	if err := i.precacheFlow(imageListFile); err != nil {
		return fmt.Errorf("failed to precache: %w", err)
	}

	return i.shutdownNode()
}

func (i *IBIPrepare) precacheFlow(imageListFile string) error {
	// TODO: add support for mirror registry
	if i.precacheDisabled {
		i.log.Info("Precache disabled, skipping it")
		return nil
	}

	i.log.Info("Precaching imaging")
	imageList, err := prep.ReadPrecachingList(imageListFile, "", "", false)
	if err != nil {
		err = fmt.Errorf("failed to read pre-caching image file: %s, %w", common.PathOutsideChroot(imageListFile), err)
		return err
	}

	// Change root directory to /host
	unchroot, err := i.chrootIfPathExists(common.Host)
	if err != nil {
		return fmt.Errorf("failed to chroot to %s, err: %w", common.Host, err)

	}

	if err := os.MkdirAll(filepath.Dir(precache.StatusFile), 0o700); err != nil {
		return fmt.Errorf("failed to create status file dir, err %w", err)
	}

	if err := workload.Precache(imageList, i.pullSecretFile, i.precacheBestEffort); err != nil {
		return fmt.Errorf("failed to start precache: %w", err)
	}

	return unchroot()
}

func (i *IBIPrepare) shutdownNode() error {
	if !i.shutdown {
		i.log.Info("Skipping shutdown")
		return nil
	}
	i.log.Info("Shutting down the host")
	if _, err := i.ops.RunInHostNamespace("shutdown", "now"); err != nil {
		return fmt.Errorf("failed to shutdown the host: %w", err)
	}
	return nil
}

// chrootIfPathExists chroots to the given path if it exists
// in case path doesn't exist there is no need to chroot
func (i *IBIPrepare) chrootIfPathExists(chrootPath string) (func() error, error) {
	if _, err := os.Stat(chrootPath); err != nil {
		i.log.Info("Path doesn't exist, skipping chroot", "path", chrootPath)
		return func() error { return nil }, nil
	}

	unchroot, err := i.ops.Chroot(chrootPath)
	if err != nil {
		return nil, fmt.Errorf("failed to chroot to %s, err: %w", common.Host, err)

	}

	i.log.Infof("chroot %s successful", chrootPath)
	return unchroot, nil
}

func (i *IBIPrepare) cleanupDisk() {
	if i.skipDiskCleanup {
		i.log.Info("Skipping disk cleanup")
		return
	}
	i.log.Infof("Cleaning up %s disk", i.installationDisk)
	// We don't want to fail the process if the cleanup fails as the installation still can succeed
	if err := i.cleanupDevice.CleanupInstallDevice(i.installationDisk); err != nil {
		i.log.Errorf("failed to cleanup installation disk %s, though installation will continue"+
			", error : %v", i.installationDisk, err)
	}
}

func (i *IBIPrepare) diskPreparation() error {
	i.log.Info("Start preparing disk")

	i.cleanupDisk()

	i.log.Info("Writing image to disk")
	if _, err := i.ops.RunInHostNamespace("coreos-installer", "install", i.installationDisk); err != nil {
		return fmt.Errorf("failed to write image to disk: %w", err)
	}

	if i.shouldCreateExtraPartition {
		if err := i.ops.CreateExtraPartition(i.installationDisk, i.extraPartitionLabel,
			i.extraPartitionStart, i.extraPartitionNumber); err != nil {
			return fmt.Errorf("failed to create extra partition: %w", err)
		}
	} else {
		if err := i.ops.SetupContainersFolderCommands(); err != nil {
			return fmt.Errorf("failed to setup containers folder: %w", err)
		}
	}

	i.log.Info("Disk was successfully prepared")

	return nil
}
