package ibi_preparation

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/containers/image/v5/pkg/sysregistriesv2"
	"github.com/go-logr/logr"
	"github.com/pelletier/go-toml"
	preinstallUtils "github.com/rh-ecosystem-edge/preinstall-utils/pkg"
	"github.com/sirupsen/logrus"

	"github.com/openshift-kni/lifecycle-agent/api/ibiconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	"github.com/openshift-kni/lifecycle-agent/internal/precache/workload"
	"github.com/openshift-kni/lifecycle-agent/internal/prep"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedclusterinfo"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

const (
	defaultRegistriesConfFile = "/etc/containers/registries.conf"
	rhcosOstreeIndex          = 1
	rhcosOstreePath           = "ostree/deploy/rhcos"
)

type IBIPrepare struct {
	log             *logrus.Logger
	ops             ops.Ops
	rpmostreeClient rpmostreeclient.IClient
	ostreeClient    ostreeclient.IClient
	cleanupDevice   preinstallUtils.CleanupDevice
	config          *ibiconfig.IBIPrepareConfig
}

func NewIBIPrepare(log *logrus.Logger, ops ops.Ops, rpmostreeClient rpmostreeclient.IClient,
	ostreeClient ostreeclient.IClient, cleanupDevice preinstallUtils.CleanupDevice, config *ibiconfig.IBIPrepareConfig) *IBIPrepare {
	return &IBIPrepare{
		log:             log,
		ops:             ops,
		rpmostreeClient: rpmostreeClient,
		ostreeClient:    ostreeClient,
		cleanupDevice:   cleanupDevice,
		config:          config,
	}
}

func (i *IBIPrepare) Run() error {
	// Pull seed image
	if err := i.diskPreparation(); err != nil {
		return fmt.Errorf("failed to prepare disk: %w", err)
	}

	i.log.Info("Pulling seed image")
	if _, err := i.ops.RunInHostNamespace("podman", "pull", "--authfile", common.IBIPullSecretFilePath, i.config.SeedImage); err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}

	// TODO: change to logrus after refactoring the code in controllers and moving to logrus
	log := logr.Logger{}
	common.OstreeDeployPathPrefix = "/mnt/"
	// Setup state root
	if err := prep.SetupStateroot(log, i.ops, i.ostreeClient, i.rpmostreeClient,
		i.config.SeedImage, i.config.SeedVersion, true, false); err != nil {
		return fmt.Errorf("failed to setup stateroot: %w", err)
	}

	if err := i.precacheFlow(common.ContainersListFilePath, common.IBISeedInfoFilePath, defaultRegistriesConfFile); err != nil {
		return fmt.Errorf("failed to precache: %w", err)
	}

	if err := i.postDeployment(common.IBIPostDeploymentScriptPath); err != nil {
		return fmt.Errorf("failed to run post deployment: %w", err)
	}

	if err := i.cleanupRhcosSysroot(); err != nil {
		return fmt.Errorf("failed to cleanup rhcos sysroot: %w", err)
	}

	return i.shutdownNode()
}

func (i *IBIPrepare) precacheFlow(imageListFile, seedInfoFile, registriesConfFile string) error {
	if i.config.PrecacheDisabled {
		i.log.Info("Precache disabled, skipping it")
		return nil
	}

	i.log.Info("Checking seed image info")
	seedInfo, err := seedclusterinfo.ReadSeedClusterInfoFromFile(seedInfoFile)
	if err != nil {
		return fmt.Errorf("failed to read seed info: %s, %w", common.PathOutsideChroot(seedInfoFile), err)
	}
	i.log.Info("Collected seed info for precache: ", fmt.Sprintf("%+v", seedInfo))

	i.log.Info("Getting mirror registry source registries")
	mirrorRegistrySources, err := mirrorRegistrySourceRegistries(registriesConfFile)
	if err != nil {
		return fmt.Errorf("failed to get mirror registry source registries: %w", err)
	}

	i.log.Info("Checking whether to override seed registry")
	shouldOverrideSeedRegistry, err := utils.ShouldOverrideSeedRegistry(seedInfo.MirrorRegistryConfigured, seedInfo.ReleaseRegistry, mirrorRegistrySources)
	if err != nil {
		return fmt.Errorf("failed to check ShouldOverrideSeedRegistry: %w", err)
	}
	i.log.Info("Should override seed registry", "shouldOverrideSeedRegistry", shouldOverrideSeedRegistry, "releaseRegistry", i.config.ReleaseRegistry)

	i.log.Info("Precaching images")
	imageList, err := prep.ReadPrecachingList(imageListFile, i.config.ReleaseRegistry, seedInfo.ReleaseRegistry, shouldOverrideSeedRegistry)
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

	if err := workload.Precache(imageList, common.IBIPullSecretFilePath, i.config.PrecacheBestEffort); err != nil {
		return fmt.Errorf("failed to start precache: %w", err)
	}

	return unchroot()
}

func (i *IBIPrepare) shutdownNode() error {
	if !i.config.Shutdown {
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
	if i.config.SkipDiskCleanup {
		i.log.Info("Skipping disk cleanup")
		return
	}
	i.log.Infof("Cleaning up %s disk", i.config.InstallationDisk)
	// We don't want to fail the process if the cleanup fails as the installation still can succeed
	if err := i.cleanupDevice.CleanupInstallDevice(i.config.InstallationDisk); err != nil {
		i.log.Errorf("failed to cleanup installation disk %s, though installation will continue"+
			", error : %v", i.config.InstallationDisk, err)
	}
}

func (i *IBIPrepare) diskPreparation() error {
	i.log.Info("Start preparing disk")

	i.cleanupDisk()

	i.log.Info("Writing image to disk")
	if _, err := i.ops.RunInHostNamespace("coreos-installer",
		append([]string{"install", i.config.InstallationDisk}, i.config.CoreosInstallerArgs...)...); err != nil {
		return fmt.Errorf("failed to write image to disk: %w", err)
	}

	if i.config.UseContainersFolder {
		if err := i.ops.SetupContainersFolderCommands(); err != nil {
			return fmt.Errorf("failed to setup containers folder: %w", err)
		}
	} else {
		if err := i.ops.CreateExtraPartition(i.config.InstallationDisk, i.config.ExtraPartitionLabel,
			i.config.ExtraPartitionStart, i.config.ExtraPartitionNumber); err != nil {
			return fmt.Errorf("failed to create extra partition: %w", err)
		}
	}

	i.log.Info("Disk was successfully prepared")

	return nil
}

func (i *IBIPrepare) postDeployment(scriptPath string) error {
	if _, err := os.Stat(scriptPath); err == nil {
		i.log.Info("Running post deployment script")
		if _, err := i.ops.RunBashInHostNamespace(scriptPath); err != nil {
			return fmt.Errorf("failed to run post deployment script: %w", err)
		}
	}
	return nil
}

// cleanupRhcosSysroot cleanups ostree that was written by us as part of diskPreparation
// as each new deployed ostree takes index 0, we know that the one we created earlier will be "1"
// that's why we can set it hardcoded
func (i *IBIPrepare) cleanupRhcosSysroot() error {
	if err := i.ostreeClient.Undeploy(rhcosOstreeIndex); err != nil {
		return fmt.Errorf("failed to undeploy sysroot written by coreos installer command with index %d: %w", 1, err)
	}
	// We set hardcoded value here cause we know exact path for written sysroot
	rhcosysrootPath := filepath.Join(common.OstreeDeployPathPrefix, rhcosOstreePath)
	if _, err := os.Stat(rhcosysrootPath); err != nil {
		i.log.Warnf("rhcos sysroot %s doesn't exists but it was expected", rhcosysrootPath)
		return nil
	}
	if _, err := i.ops.RunBashInHostNamespace("rm", "-rf", rhcosysrootPath); err != nil {
		return fmt.Errorf("removing sysroot %s failed: %w", rhcosysrootPath, err)
	}
	return nil
}

func mirrorRegistrySourceRegistries(registriesConfFile string) ([]string, error) {
	content, err := os.ReadFile(registriesConfFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read registry config file: %w", err)
	}

	config := &sysregistriesv2.V2RegistriesConf{}

	if err := toml.Unmarshal(content, config); err != nil {
		return nil, fmt.Errorf("failed to parse registry config: %w", err)
	}

	sources := make([]string, 0, len(config.Registries))
	for _, registry := range config.Registries {
		if registry.Location != "" {
			sources = append(sources, utils.ExtractRegistryFromImage(registry.Location))
		}
	}

	return sources, nil
}
