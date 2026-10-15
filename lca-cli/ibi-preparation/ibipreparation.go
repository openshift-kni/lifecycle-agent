package ibi_preparation

import (
	"context"
	"errors"
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
	"github.com/openshift-kni/lifecycle-agent/lca-cli/localimages"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedclusterinfo"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

const (
	defaultRegistriesConfFile = "/etc/containers/registries.conf"
	rhcosOstreeIndex          = 1
	rhcosOstreePath           = "ostree/deploy/rhcos"

	// closureStoreDriver is the containers-storage graph driver used when opening
	// the installed stateroot's store to import the image closure into. It matches
	// the driver RHCOS uses for the node's persistent /var/lib/containers store.
	closureStoreDriver = "overlay"
)

// imageImporter is the subset of *localimages.Importer that the prepare flow uses.
// It is an interface so unit tests can substitute a fake importer without opening a
// real containers-storage store.
type imageImporter interface {
	ImportImage(ctx context.Context, mediaRoot string, m *localimages.DigestManifest, e localimages.ImageEntry) error
	ImportAll(ctx context.Context, mediaRoot string, m *localimages.DigestManifest) error
	Close() error
}

// Seams overridable from tests. The real implementations parse the on-media
// manifest and open a containers-storage store; tests replace them to exercise the
// disconnected wiring without touching the filesystem or a real store.
var (
	parseDigestManifest = localimages.ParseDigestManifest
	newImageImporter    = func(log *logrus.Logger, cfg localimages.StoreConfig) (imageImporter, error) {
		return localimages.NewImporter(log, cfg)
	}
	// setupStateroot is seamed so Run()'s import ordering can be unit-tested without
	// performing the real (heavy, filesystem-dependent) stateroot setup.
	setupStateroot = prep.SetupStateroot
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
	ctx := context.Background()

	// Disconnected (offline) prepare sources every image from the on-media OCI layout
	// on the USB "p2" partition instead of the network. The two hard preconditions --
	// p2 discovery/mount and manifest/seed validation -- run first, before
	// diskPreparation touches the installation disk, so a missing/ambiguous device or a
	// missing/malformed/seed-mismatched manifest fails while the disk is still intact
	// (there is no network fallback). The connected flow is left byte-for-byte unchanged.
	var (
		manifest  *localimages.DigestManifest
		seedEntry localimages.ImageEntry
		unmountP2 func()
	)
	if i.config.Disconnected {
		// Preflight precondition 1: discover p2 by label and mount it read-only.
		unmount, err := i.mountLocalImages()
		if err != nil {
			return fmt.Errorf("failed to mount local images partition: %w", err)
		}
		unmountP2 = unmount
		// Keep p2 mounted through the entire import; this deferred unmount covers the
		// error paths. On the happy path p2 is unmounted explicitly as the last p2
		// action after the final sync (the installed store handoff reads it after
		// precache), and this deferred call then becomes a no-op (unmount is idempotent).
		defer unmountP2()

		// Preflight precondition 2: parse the manifest and resolve the seed entry,
		// before any disk write.
		m, e, err := i.preflightValidateManifest()
		if err != nil {
			return fmt.Errorf("failed to validate local images manifest: %w", err)
		}
		manifest, seedEntry = m, e
	}

	// In a disconnected prepare there is no operator-provided pull secret, but Run() still
	// passes common.IBIPullSecretFilePath to the precache, so a synthetic empty
	// authfile must exist. Provision it non-destructively and remove it on completion (including
	// the error path via defer). The connected flow is untouched.
	var removeAuthFile func()
	if i.config.Disconnected {
		cleanup, err := setupPrepareAuthFile(common.PathOutsideChroot(common.IBIPullSecretFilePath))
		if err != nil {
			return fmt.Errorf("failed to set up prepare auth file: %w", err)
		}
		removeAuthFile = cleanup
		// Covers the error paths. On the success path the authfile is removed explicitly
		// before the final sync below (so the removal is flushed to disk before shutdown);
		// cleanup is idempotent, so this deferred call then becomes a no-op.
		defer removeAuthFile()
	}

	if err := i.diskPreparation(); err != nil {
		return fmt.Errorf("failed to prepare disk: %w", err)
	}

	// Acquire the seed image. Disconnected: import the single seed entry (validated in
	// preflight) from the p2 layout into the live/default store. Connected: pull it from
	// the network as before. The seed is a bootstrap input consumed by SetupStateroot
	// below; the live store is ephemeral and dropped on reboot.
	if i.config.Disconnected {
		i.log.Info("Importing seed image from local images layout")
		if err := i.importSeedToLiveStore(ctx, manifest, seedEntry); err != nil {
			return fmt.Errorf("failed to import seed image: %w", err)
		}
	} else {
		i.log.Info("Pulling seed image")
		if _, err := i.ops.RunInHostNamespace("podman", "pull", "--authfile", common.IBIPullSecretFilePath, i.config.SeedImage); err != nil {
			return fmt.Errorf("failed to pull image: %w", err)
		}
	}

	// TODO: change to logrus after refactoring the code in controllers and moving to logrus
	log := logr.Logger{}
	common.OstreeDeployPathPrefix = "/mnt/"
	// Setup state root
	if err := setupStateroot(log, i.ops, i.ostreeClient, i.rpmostreeClient,
		i.config.SeedImage, i.config.SeedVersion, true); err != nil {
		return fmt.Errorf("failed to setup stateroot: %w", err)
	}

	// Disconnected: import the full release/workload closure into the installed
	// stateroot's graphroot before precache, so precache finds every image already
	// present and performs no network pull. This must run after SetupStateroot (the
	// installed stateroot now exists) and before precacheFlow, and outside the /host
	// chroot precacheFlow enters -- the p2 layout is only visible in the live-ISO
	// namespace.
	if i.config.Disconnected {
		i.log.Info("Importing image closure into installed stateroot")
		if err := i.importClosureToStateroot(ctx, manifest); err != nil {
			return fmt.Errorf("failed to import image closure: %w", err)
		}
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

	// Remove the synthetic authfile before the final sync so its removal is persisted to disk.
	// shutdownNode may terminate the process before the deferred cleanup runs, and even if the
	// defer does run it would execute after this final sync, leaving the removal unflushed.
	if removeAuthFile != nil {
		removeAuthFile()
	}

	i.log.Info("Syncing filesystems to disk")
	if _, err := i.ops.RunInHostNamespace("sync"); err != nil {
		return fmt.Errorf("failed to sync filesystems: %w", err)
	}

	// Unmount p2 last: only after the final sync and the installed store handoff, so
	// additionalimagestores never points at a path that has already gone away. This is
	// idempotent, so the deferred unmount registered above then becomes a no-op.
	if unmountP2 != nil {
		unmountP2()
	}

	return i.shutdownNode()
}

// mountLocalImages discovers the p2 installer-media partition by its filesystem
// label (common.LocalImagesFSLabel) and mounts it read-only at
// i.config.LocalImagesPath. Discovery is a hard precondition with no network
// fallback: if zero or more than one device carries the label the function fails,
// and because Run calls it before diskPreparation the installation disk is still
// untouched. It returns an idempotent unmount closure so the caller can both defer
// it (error paths) and invoke it explicitly as the last p2 action on the happy path.
func (i *IBIPrepare) mountLocalImages() (func(), error) {
	blockDevices, err := i.ops.ListBlockDevices()
	if err != nil {
		return nil, fmt.Errorf("failed to list block devices: %w", err)
	}

	matches := make([]string, 0, 1)
	for _, bd := range blockDevices {
		if bd.Label == common.LocalImagesFSLabel {
			matches = append(matches, bd.Name)
		}
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("expected exactly one block device labelled %q for local images, found %d",
			common.LocalImagesFSLabel, len(matches))
	}
	deviceName := matches[0]

	if err := i.ops.MountReadOnly(deviceName, i.config.LocalImagesPath); err != nil {
		return nil, fmt.Errorf("failed to mount local images device %s read-only: %w", deviceName, err)
	}

	var unmounted bool
	unmount := func() {
		if unmounted {
			return
		}
		if err := i.ops.Umount(deviceName); err != nil {
			// Leave unmounted false so a subsequent (deferred) call retries; a second
			// umount of an already-unmounted device would otherwise error, which is why
			// the flag is only set once the unmount actually succeeds.
			i.log.Warnf("failed to unmount local images device %s: %v", deviceName, err)
			return
		}
		unmounted = true
	}
	return unmount, nil
}

// preflightValidateManifest parses the on-media digest manifest and resolves the
// seed entry whose canonical Name@Digest equals i.config.SeedImage. It runs in
// preflight, before diskPreparation, so a missing, malformed, or seed-mismatched
// manifest fails while the installation disk is still untouched.
//
// Resolving the seed here also pins seed addressability: the importer later writes
// the seed under entry.Name+"@"+entry.Digest, which must be exactly the reference
// SetupStateroot resolves (i.config.SeedImage, passed on to ops.MountImage). A
// SeedImage that is not present in the manifest as a canonical name@digest entry is
// rejected now rather than silently imported as an unaddressable reference.
func (i *IBIPrepare) preflightValidateManifest() (*localimages.DigestManifest, localimages.ImageEntry, error) {
	manifest, err := parseDigestManifest(i.config.ExpectedDigestsPath)
	if err != nil {
		return nil, localimages.ImageEntry{}, fmt.Errorf("failed to parse digest manifest %q: %w", i.config.ExpectedDigestsPath, err)
	}

	for _, e := range manifest.Images {
		if e.Name+"@"+e.Digest == i.config.SeedImage {
			return manifest, e, nil
		}
	}

	return nil, localimages.ImageEntry{}, fmt.Errorf(
		"seed image %q not found as a canonical name@digest entry in digest manifest %q",
		i.config.SeedImage, i.config.ExpectedDigestsPath)
}

// importSeedToLiveStore imports the single seed entry from the mounted p2 layout
// into the live/default containers-storage (zero-value StoreConfig). The seed is a
// bootstrap input consumed by SetupStateroot and then dropped with the ephemeral
// live store on reboot, so only the seed -- not the full closure -- is imported here.
func (i *IBIPrepare) importSeedToLiveStore(ctx context.Context, manifest *localimages.DigestManifest, seedEntry localimages.ImageEntry) error {
	importer, err := newImageImporter(i.log, localimages.StoreConfig{})
	if err != nil {
		return fmt.Errorf("failed to open live containers-storage: %w", err)
	}
	defer func() {
		if err := importer.Close(); err != nil {
			i.log.Warnf("failed to close live containers-storage importer: %v", err)
		}
	}()

	if err := importer.ImportImage(ctx, i.config.LocalImagesPath, manifest, seedEntry); err != nil {
		return fmt.Errorf("failed to import seed image %s@%s into live store: %w", seedEntry.Name, seedEntry.Digest, err)
	}
	return nil
}

// importClosureToStateroot imports the full release/workload closure from the
// mounted p2 layout into the installed stateroot's containers-storage graphroot,
// addressed by explicit graphroot (the stateroot's /var/lib/containers/storage
// under the /mnt deploy prefix). A second store open needs a writable runroot, so a
// temporary one is created and removed afterwards; the importer is closed (via defer)
// to release its store locks before the later sync/unmount.
func (i *IBIPrepare) importClosureToStateroot(ctx context.Context, manifest *localimages.DigestManifest) error {
	osname := common.GetStaterootName(i.config.SeedVersion)
	graphRoot := filepath.Join(common.GetStaterootPath(osname), common.ContainerStoragePath, "storage")

	runRoot, err := os.MkdirTemp("", "ibi-closure-runroot-")
	if err != nil {
		return fmt.Errorf("failed to create temporary runroot: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(runRoot); err != nil {
			i.log.Warnf("failed to remove temporary runroot %s: %v", runRoot, err)
		}
	}()

	importer, err := newImageImporter(i.log, localimages.StoreConfig{
		GraphDriverName: closureStoreDriver,
		GraphRoot:       graphRoot,
		RunRoot:         runRoot,
	})
	if err != nil {
		return fmt.Errorf("failed to open installed stateroot containers-storage at %s: %w", graphRoot, err)
	}
	defer func() {
		if err := importer.Close(); err != nil {
			i.log.Warnf("failed to close installed stateroot importer: %v", err)
		}
	}()

	if err := importer.ImportAll(ctx, i.config.LocalImagesPath, manifest); err != nil {
		return fmt.Errorf("failed to import image closure into installed stateroot store %s: %w", graphRoot, err)
	}
	return nil
}

// setupPrepareAuthFile ensures a synthetic empty authfile exists at path for the
// disconnected precache, without disturbing any operator-provided file.
// It creates the file only if it is absent (atomically, via O_EXCL) and returns a
// cleanup func that removes the file ONLY if this call created it. A pre-existing file
// (operator-provided or a leftover from an interrupted run) is left byte-for-byte
// untouched and is never removed.
func setupPrepareAuthFile(path string) (func(), error) {
	noop := func() {}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return noop, fmt.Errorf("failed to create authfile dir: %w", err)
	}
	// O_EXCL => atomically create only if absent; tells us definitively whether THIS run
	// created the file (no TOCTOU, safe after a crashed run).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // path is provided by the caller (Run passes the validated IBI pull-secret path)
	if errors.Is(err, os.ErrExist) {
		// Pre-existing file (operator-provided or leftover): leave it untouched.
		return noop, nil
	}
	if err != nil {
		return noop, fmt.Errorf("failed to create synthetic authfile %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(common.IBIEmptyPullSecret); err != nil {
		_ = os.Remove(path) // roll back our own partial create
		return noop, fmt.Errorf("failed to write synthetic authfile: %w", err)
	}
	return func() { _ = os.Remove(path) }, nil
}

func (i *IBIPrepare) precacheFlow(imageListFile, seedInfoFile, registriesConfFile string) (retErr error) {
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
	// Restore the root on every return path. Otherwise an error after chrooting would leave
	// the process rooted at /host while Run()'s deferred authfile cleanup (an absolute /host
	// path) executes, resolving it under the wrong root and leaking the synthetic authfile.
	// Preserve any earlier error; only surface the unchroot failure when the flow succeeded.
	defer func() {
		if unchrootErr := unchroot(); retErr == nil {
			retErr = unchrootErr
		}
	}()

	if err := os.MkdirAll(filepath.Dir(precache.StatusFile), 0o700); err != nil {
		return fmt.Errorf("failed to create status file dir, err %w", err)
	}

	if err := workload.Precache(imageList, common.IBIPullSecretFilePath, i.config.PrecacheBestEffort); err != nil {
		return fmt.Errorf("failed to start precache: %w", err)
	}

	return nil
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

	// syscall.Chroot changes the root but not the working directory, so the cwd still points
	// outside the new root. Move into the new root so relative paths passed to subsequent
	// commands (e.g. the precache --authfile common.IBIPullSecretFilePath) resolve against it
	// instead of escaping to the pre-chroot directory. On failure, undo the chroot.
	if err := os.Chdir("/"); err != nil {
		if unchrootErr := unchroot(); unchrootErr != nil {
			i.log.Warnf("failed to restore root after chdir error: %v", unchrootErr)
		}
		return nil, fmt.Errorf("failed to change directory to chroot root %s: %w", chrootPath, err)
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
	content, err := os.ReadFile(registriesConfFile) //nolint:gosec // registriesConfFile path is validated
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
