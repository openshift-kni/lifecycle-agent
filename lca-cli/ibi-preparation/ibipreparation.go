package ibi_preparation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-logr/logr"
	"github.com/pelletier/go-toml"
	preinstallUtils "github.com/rh-ecosystem-edge/preinstall-utils/pkg"
	"github.com/sirupsen/logrus"
	"go.podman.io/image/v5/pkg/sysregistriesv2"

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
	VerifyAll(ctx context.Context, m *localimages.DigestManifest) error
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
	setupStateroot      = prep.SetupStateroot
	preflightSeedOSTree = func(i *IBIPrepare, ctx context.Context, manifest *localimages.DigestManifest,
		seedEntry localimages.ImageEntry) error {
		if err := i.importSeedToLiveStore(ctx, manifest, seedEntry); err != nil {
			return fmt.Errorf("failed to import seed for preflight: %w", err)
		}
		return prep.VerifySeedOSTree(logr.Logger{}, i.ops, i.ostreeClient, i.config.SeedImage)
	}
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

	// Connected (online) prepare sources images from the network; it has no ibi-status
	// partition and no result write-back, and its terminal behaviour (power-off on success)
	// is left byte-for-byte unchanged.
	if !i.config.Disconnected {
		if err := i.runPreparation(ctx, nil, localimages.ImageEntry{}, nil, nil); err != nil {
			return err
		}
		return i.shutdownNode()
	}

	// Disconnected (USB) prepare: establish the writable ibi-status (p4) partition and run
	// the re-entry guard FIRST -- before the p2 mount, manifest validation, and auth-file
	// setup -- so that every failure after this point (not only a core-prepare failure) is
	// recorded to prepare.json and takes the promised powered-on terminal action. p4 is a
	// mandatory precondition in both profiles: exactly one writable ibi-status device,
	// mountable and writable. The guard refuses to wipe over a completed install. Every
	// uncertain p4 state fails closed (halt powered-on without wiping); the partition stays
	// mounted so the result write-back at the end of the run reuses it.
	sp, err := i.mountStatusPartition()
	if err != nil {
		// Cannot establish exactly one writable p4 -> do not wipe, halt powered-on. There is
		// no usable partition to record a result on, so this is the single disconnected
		// failure path with no prepare.json.
		return i.haltPoweredOn(fmt.Errorf("ibi-status partition precondition failed: %w", err))
	}
	// Idempotent backup unmount for the error paths; finalizeDisconnected unmounts
	// explicitly before the terminal action on the normal paths.
	defer func() { _ = sp.unmount() }()

	blocked, guardErr := i.guardReentry(sp)
	if guardErr != nil {
		// An unreadable/malformed existing marker is never treated as safe to wipe.
		// Unmount p4 before the halt: isolating emergency.target may stop this prepare
		// service before the deferred unmount runs, and finalizeDisconnected already
		// unmounts explicitly before its terminal action.
		_ = sp.unmount()
		return i.haltPoweredOn(fmt.Errorf("ibi-status re-entry guard failed: %w", guardErr))
	}
	if blocked {
		// Completed install preserved and re-entry recorded: halt powered-on, no wipe.
		// Unmount p4 before the halt for the same reason as the guard-error path above.
		_ = sp.unmount()
		return i.haltPoweredOn(errReentryBlocked)
	}

	// With p4 mounted and the guard passed, every subsequent failure (p2 mount, manifest
	// validation, auth-file setup, or the core prepare) is finalized: prepare.json is written
	// and the node is halted powered-on.
	imagesExpected, prepErr := i.runDisconnectedPreparation(ctx)
	return i.finalizeDisconnected(sp, imagesExpected, prepErr)
}

// runDisconnectedPreparation runs the disconnected preflight -- p2 discovery/mount, manifest
// and seed validation, seed OSTree integrity, and the synthetic authfile -- and then the shared core prepare. It is
// invoked only after the ibi-status (p4) partition is mounted and the re-entry guard has
// passed, so every error it returns is routed through finalizeDisconnected (recorded to
// prepare.json, followed by the powered-on terminal action) rather than aborting Run bare.
// The two hard preconditions (p2 discovery/mount and manifest/seed validation) still run
// before diskPreparation touches the installation disk, so a missing/ambiguous device or a
// missing/malformed/seed-mismatched manifest or corrupt seed fails while the disk is still intact. It returns
// the number of images the on-media manifest expects (0 until the manifest is parsed; used
// only to populate the success counts) and the first error encountered.
func (i *IBIPrepare) runDisconnectedPreparation(ctx context.Context) (imagesExpected int, err error) {
	// Preflight precondition 1: discover p2 by label and mount it read-only.
	unmountP2, err := i.mountLocalImages()
	if err != nil {
		return 0, fmt.Errorf("failed to mount local images partition: %w", err)
	}
	// Keep p2 mounted through the entire import; this deferred unmount covers the error paths
	// (best-effort cleanup while the run is already failing, and the retry of a failed
	// explicit final unmount). On the happy path p2 is unmounted explicitly as the last p2
	// action in runPreparation and this deferred call then becomes a no-op (unmount is
	// idempotent).
	defer func() { _ = unmountP2() }()

	// Preflight precondition 2: parse the manifest and resolve the seed entry, before any
	// disk write.
	manifest, seedEntry, err := i.preflightValidateManifest()
	if err != nil {
		return 0, fmt.Errorf("failed to validate local images manifest: %w", err)
	}

	// The seed must survive SELinux-aware extraction and OSTree verification in this
	// live ISO before cleanupDisk or coreos-installer can replace the original disk.
	// diskPreparation later mounts a fresh /var/lib/containers, so the seed is
	// imported again there for SetupStateroot.
	i.log.Info("Verifying seed OSTree repository before preparing the installation disk")
	if err := preflightSeedOSTree(i, ctx, manifest, seedEntry); err != nil {
		return len(manifest.Images), fmt.Errorf("seed preflight failed: %w", err)
	}

	// In a disconnected prepare there is no operator-provided pull secret, but the precache is
	// still passed common.IBIPullSecretFilePath, so a synthetic empty authfile must exist.
	// Provision it non-destructively and remove it on completion (including the error path via
	// defer).
	removeAuthFile, err := setupPrepareAuthFile(common.PathOutsideChroot(common.IBIPullSecretFilePath))
	if err != nil {
		return len(manifest.Images), fmt.Errorf("failed to set up prepare auth file: %w", err)
	}
	// Covers the error paths. On the success path the authfile is removed explicitly before
	// the final sync in runPreparation (so the removal is flushed to disk before shutdown);
	// cleanup is idempotent, so this deferred call then becomes a no-op.
	defer removeAuthFile()

	if err := i.runPreparation(ctx, manifest, seedEntry, unmountP2, removeAuthFile); err != nil {
		return len(manifest.Images), err
	}
	return len(manifest.Images), nil
}

// runPreparation performs the core prepare work shared by the connected and disconnected
// flows: it writes the image to disk, acquires and deploys the seed, precaches, runs the
// post-deployment hooks, flushes to disk, and (disconnected) unmounts the p2 local-images
// partition last. It returns the first error encountered without taking any terminal
// action; the caller decides the terminal action (and, for the disconnected flow, records
// the result on the ibi-status partition first).
func (i *IBIPrepare) runPreparation(ctx context.Context, manifest *localimages.DigestManifest,
	seedEntry localimages.ImageEntry, unmountP2 func() error, removeAuthFile func()) error {
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

	if err := i.precacheFlow(common.ContainersListFilePath, common.IBISeedInfoFilePath, defaultRegistriesConfFile, manifest); err != nil {
		return fmt.Errorf("failed to precache: %w", err)
	}

	// Disconnected: mandatory image self-check. After import + precache, confirm every
	// image in the on-media manifest is resident in the installed stateroot store before
	// any terminal action. A missing/unresolvable image means the closure import was
	// incomplete, so fail Run() here -- before postDeployment, cleanupRhcosSysroot, the
	// final sync, the explicit p2 unmount and shutdownNode -- rather than powering off /
	// rebooting a node short an image it can never pull offline. This is a correctness
	// gate, not optional hardening. The deferred best-effort cleanup (p2 unmount +
	// authfile removal) still runs on this error path, as on any other.
	if i.config.Disconnected {
		i.log.Info("Running mandatory image self-check against the on-media manifest")
		if err := i.verifyImportedClosure(ctx, manifest); err != nil {
			return fmt.Errorf("image self-check failed: %w", err)
		}
	}

	if err := i.postDeployment(common.IBIPostDeploymentScriptPath); err != nil {
		return fmt.Errorf("failed to run post deployment: %w", err)
	}

	if err := i.cleanupRhcosSysroot(); err != nil {
		return fmt.Errorf("failed to cleanup rhcos sysroot: %w", err)
	}

	// Self-contained disconnected media carries a pre-built read-only containers/storage
	// durability store on p2. Copy it onto the installed disk, seal it read-only, and register it
	// as a read-only additional image store -- while the stateroot is still mounted and before the
	// final sync and p2 unmount (the handoff the unmount-p2 comment below reserves), so the node
	// recovers GC-evicted/corrupted images locally with no registry and no network. install-only
	// and the connected flow carry no such artifact and are a clean no-op.
	if i.config.Disconnected && i.config.Profile == ibiconfig.ProfileSelfContained {
		i.log.Info("Persisting read-only additional image store into the installed stateroot")
		if err := persistReadOnlyStoreFn(i, ctx, manifest); err != nil {
			return fmt.Errorf("failed to persist read-only image store: %w", err)
		}
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
	// idempotent, so the deferred unmount registered above then becomes a no-op on
	// success. A failed final unmount is a failed lifecycle step: surface it as Run's
	// result regardless of i.config.Shutdown rather than reporting success (or shutting
	// down) with p2 still mounted. The deferred unmount above then retries on the way out.
	if unmountP2 != nil {
		if err := unmountP2(); err != nil {
			return fmt.Errorf("failed to unmount local images partition after import: %w", err)
		}
	}

	return nil
}

// mountLocalImages discovers the p2 installer-media partition by its filesystem
// label (common.LocalImagesFSLabel) and makes it available read-only at
// i.config.LocalImagesPath. Discovery is a hard precondition with no network
// fallback: if zero or more than one device carries the label the function fails,
// and because Run calls it before diskPreparation the installation disk is still
// untouched. It returns an idempotent unmount closure so the caller can both defer
// it (error paths) and invoke it explicitly as the last p2 action on the happy path.
// The closure returns the umount error so the caller can surface a failed final
// unmount as Run's result.
//
// On USB boot the embedded ignition's label-based systemd .mount unit already owns
// this partition, mounted read-only at i.config.LocalImagesPath. In that case this
// function reuses the existing mount and returns a no-op cleanup, so the systemd-owned
// mount is never torn down from under the unit: the mount is left exactly as the boot
// unit established it. If the mount point is instead occupied by another device, or the
// expected device is mounted read-write, that is an unsafe state and the function fails
// before diskPreparation touches the installation disk. When nothing is mounted there
// (e.g. the connected/online path or a manual invocation) it mounts the device
// read-only itself, as before.
func (i *IBIPrepare) mountLocalImages() (func() error, error) {
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
	expectedSource := "/dev/" + deviceName

	// If the systemd .mount unit already owns the mount point, reuse it rather than
	// mounting again. The check reads the host mount namespace so a container-local
	// view cannot report a false answer.
	mount, err := i.inspectHostMount(i.config.LocalImagesPath)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect existing mount at %s: %w", i.config.LocalImagesPath, err)
	}
	if mount.mounted {
		if mount.source != expectedSource {
			return nil, fmt.Errorf(
				"%s is already mounted from %s, not the discovered ibi-images device %s; refusing to proceed",
				i.config.LocalImagesPath, mount.source, expectedSource)
		}
		if !mount.readOnly {
			return nil, fmt.Errorf(
				"%s is already mounted read-write from %s; the ibi-images partition must be read-only; refusing to proceed",
				i.config.LocalImagesPath, expectedSource)
		}
		i.log.Infof("Reusing existing read-only mount of %s at %s (owned by the boot-time .mount unit)",
			expectedSource, i.config.LocalImagesPath)
		// The mount is owned by the systemd unit; never unmount it from here.
		return func() error { return nil }, nil
	}

	if err := i.ops.MountReadOnly(deviceName, i.config.LocalImagesPath); err != nil {
		return nil, fmt.Errorf("failed to mount local images device %s read-only: %w", deviceName, err)
	}

	var unmounted bool
	unmount := func() error {
		if unmounted {
			return nil
		}
		if err := i.ops.Umount(deviceName); err != nil {
			// Leave unmounted false so a subsequent (deferred) call retries; a second
			// umount of an already-unmounted device would otherwise error, which is why
			// the flag is only set once the unmount actually succeeds.
			i.log.Warnf("failed to unmount local images device %s: %v", deviceName, err)
			return fmt.Errorf("failed to unmount local images device %s: %w", deviceName, err)
		}
		unmounted = true
		return nil
	}
	return unmount, nil
}

// hostMount describes the state of a mount point as seen in the host mount namespace.
type hostMount struct {
	// mounted is true when the mount point carries a mount.
	mounted bool
	// source is the backing device the kernel reports for the mount (e.g. /dev/sdb2).
	source string
	// readOnly is true when the mount carries the ro option.
	readOnly bool
}

// inspectHostMount reports whether mountpoint is mounted in the host mount namespace and,
// if so, its backing device and whether it is read-only. It reads the host's
// /proc/self/mounts through the host namespace (RunInHostNamespace), never this process's
// own (possibly container) mount table, so a container mount namespace cannot give a false
// answer about what the boot-time unit mounted on the host.
func (i *IBIPrepare) inspectHostMount(mountpoint string) (hostMount, error) {
	out, err := i.ops.RunInHostNamespace("cat", "/proc/self/mounts")
	if err != nil {
		return hostMount{}, fmt.Errorf("failed to read host mount table: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		// Each /proc/self/mounts line is: <source> <mountpoint> <fstype> <options> <dump> <pass>.
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		// The mount point is escaped in /proc/self/mounts (space -> \040 etc.); the
		// ibi-images mount point contains no such characters, so a direct compare is exact.
		if fields[1] != mountpoint {
			continue
		}
		m := hostMount{mounted: true, source: fields[0]}
		for _, opt := range strings.Split(fields[3], ",") {
			if opt == "ro" {
				m.readOnly = true
				break
			}
		}
		return m, nil
	}
	return hostMount{}, nil
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

	// Disconnected prepare requires a v2 manifest: only v2 records each image's
	// source references (ImageEntry.Sources), which the coverage check matches
	// against the seed's containers.list by exact source reference and the closure
	// import registers as local tag aliases. A v1 manifest cannot satisfy that
	// contract, so fail now -- before any disk write -- rather than after importing
	// the closure. The media builder must emit a v2 manifest.
	if manifest.Version < localimages.SourcesManifestVersion {
		return nil, localimages.ImageEntry{}, fmt.Errorf(
			"disconnected prepare requires a v%d digest manifest with source references, but %q is v%d",
			localimages.SourcesManifestVersion, i.config.ExpectedDigestsPath, manifest.Version)
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

// podmanStoreInfo is the subset of `podman info --format json` the prepare flow
// consumes to resolve the host containers-storage store.
type podmanStoreInfo struct {
	Store struct {
		GraphDriverName string `json:"graphDriverName"`
		GraphRoot       string `json:"graphRoot"`
		RunRoot         string `json:"runRoot"`
	} `json:"store"`
}

// hostSeedStoreConfig resolves the containers-storage configuration of the exact
// store that ops.MountImage reads the seed from, so the seed import lands where
// SetupStateroot later looks for it.
//
// A zero-value StoreConfig would open the Go-resolved default store
// (cstorage.DefaultStoreOptions), which is resolved against this process's own
// (container) environment and can differ from the store the host-namespace
// `podman image mount` uses -- leaving the seed imported into a store nothing
// reads. Instead, ask the host podman itself (`podman info`) for its resolved
// driver/graphroot/runroot. The paths podman reports are host-root-relative;
// they are translated into this process's mount namespace with PathOutsideChroot
// because the importer opens the store directly in-process (as the closure import
// does for the installed stateroot) rather than through the host namespace.
func (i *IBIPrepare) hostSeedStoreConfig() (localimages.StoreConfig, error) {
	out, err := i.ops.RunInHostNamespace("podman", "info", "--format", "json")
	if err != nil {
		return localimages.StoreConfig{}, fmt.Errorf("failed to query host podman store configuration: %w", err)
	}

	var info podmanStoreInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return localimages.StoreConfig{}, fmt.Errorf("failed to parse host podman store configuration: %w", err)
	}
	if info.Store.GraphDriverName == "" || info.Store.GraphRoot == "" || info.Store.RunRoot == "" {
		return localimages.StoreConfig{}, fmt.Errorf(
			"host podman reported an incomplete store configuration: driver=%q graphRoot=%q runRoot=%q",
			info.Store.GraphDriverName, info.Store.GraphRoot, info.Store.RunRoot)
	}

	return localimages.StoreConfig{
		GraphDriverName: info.Store.GraphDriverName,
		GraphRoot:       common.PathOutsideChroot(info.Store.GraphRoot),
		RunRoot:         common.PathOutsideChroot(info.Store.RunRoot),
	}, nil
}

// importSeedToLiveStore imports the single seed entry from the mounted p2 layout
// into the host containers-storage store that ops.MountImage reads -- resolved
// from the host podman's own configuration (hostSeedStoreConfig), not assumed
// defaults. The seed is a bootstrap input consumed by SetupStateroot and then
// dropped with the ephemeral live store on reboot, so only the seed -- not the
// full closure -- is imported here.
func (i *IBIPrepare) importSeedToLiveStore(ctx context.Context, manifest *localimages.DigestManifest, seedEntry localimages.ImageEntry) error {
	storeCfg, err := i.hostSeedStoreConfig()
	if err != nil {
		return fmt.Errorf("failed to resolve host containers-storage configuration: %w", err)
	}

	importer, err := newImageImporter(i.log, storeCfg)
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

// withInstalledStaterootStore opens the installed stateroot's containers-storage
// graphroot (the stateroot's /var/lib/containers/storage under the /mnt deploy
// prefix) and invokes fn with an importer bound to it. A second store open needs a
// writable runroot, so a temporary one is created and removed afterwards; the
// importer is closed (via defer) to release its store locks before the later
// sync/unmount. It is the single store-open path shared by the closure import and
// the post-import self-check, so both read exactly the same store; the self-check
// relies on the import's importer having been closed first (this helper closes it)
// to avoid a double-open lock.
func (i *IBIPrepare) withInstalledStaterootStore(fn func(importer imageImporter) error) error {
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

	return fn(importer)
}

// importClosureToStateroot imports the full release/workload closure from the
// mounted p2 layout into the installed stateroot's containers-storage graphroot.
func (i *IBIPrepare) importClosureToStateroot(ctx context.Context, manifest *localimages.DigestManifest) error {
	return i.withInstalledStaterootStore(func(importer imageImporter) error {
		if err := importer.ImportAll(ctx, i.config.LocalImagesPath, manifest); err != nil {
			return fmt.Errorf("failed to import image closure into installed stateroot store: %w", err)
		}
		return nil
	})
}

// verifyImportedClosure runs the mandatory image self-check: it re-opens the
// installed stateroot's containers-storage store (the same store the closure was
// imported into) and confirms every image in the on-media manifest is resident
// under its canonical name@digest and that every source reference the seed
// requests for it resolves to that same imported image. A missing/unresolvable
// image -- or a seed source reference that does not resolve -- means the closure
// import was incomplete; returning an error here fails Run() before any terminal
// action so an incomplete node is never powered off / rebooted.
//
// The store it reads is the installed graphroot on the installation disk -- read
// directly in-process, not via the /host chroot the precache step uses -- so it is
// the authoritative installed-node residency gate and does not depend on p2 being
// mounted. The residency check itself lives in localimages.VerifyAll so it can
// later move to the proposed check-usb media-verification tooling.
func (i *IBIPrepare) verifyImportedClosure(ctx context.Context, manifest *localimages.DigestManifest) error {
	return i.withInstalledStaterootStore(func(importer imageImporter) error {
		if err := importer.VerifyAll(ctx, manifest); err != nil {
			return fmt.Errorf("verifying imported closure in the installed stateroot store: %w", err)
		}
		return nil
	})
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

func (i *IBIPrepare) precacheFlow(imageListFile, seedInfoFile, registriesConfFile string, manifest *localimages.DigestManifest) (retErr error) {
	if i.config.PrecacheDisabled {
		i.log.Info("Precache disabled, skipping it")
		return nil
	}

	imageList, err := i.precacheImageList(imageListFile, seedInfoFile, registriesConfFile, manifest)
	if err != nil {
		return err
	}

	// Disconnected: the full closure was imported into the installed stateroot store up front,
	// and its residency is verified directly in that store by the mandatory self-check
	// (verifyImportedClosure) immediately after this step. The seed-to-manifest coverage check
	// has already run above while precacheImageList built the set. The live-ISO podman presence
	// check (workload.Precache) inspects the live ISO's /host containers store, not the
	// installed stateroot store, so for a disconnected prepare it is both redundant with the
	// self-check and misleading -- it can report present against the live store while the
	// installed store is short an image. Skip it, and the /host chroot it requires, so
	// disconnected prepare verifies residency only where it matters (the installed store) and
	// does not depend on /host existing at all.
	if i.config.Disconnected {
		i.log.Infof("Disconnected: skipping the live-ISO podman precache presence check; %d image(s) will be verified directly in the installed stateroot store", len(imageList))
		return nil
	}

	// Connected: run the precache presence/pull check in the host namespace. Change root
	// directory to /host.
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

	if err := workload.Precache(imageList, common.IBIPullSecretFilePath, i.config.PrecacheBestEffort, i.config.Disconnected); err != nil {
		return fmt.Errorf("failed to start precache: %w", err)
	}

	return nil
}

// precacheImageList builds the set of image references precache must ensure are
// present locally.
//
// Connected: the seed's containers.list, with the usual registries.conf-driven
// registry override applied so the images resolve to the registry they will be
// pulled from.
//
// Disconnected: every image in the on-media digest manifest, by canonical
// name@digest (see disconnectedPrecacheList). The manifest is already
// profile-scoped by create-usb, so this covers the full closure the node needs.
// Presence is checked with `podman image exists`; nothing is pulled.
func (i *IBIPrepare) precacheImageList(imageListFile, seedInfoFile, registriesConfFile string, manifest *localimages.DigestManifest) ([]string, error) {
	if i.config.Disconnected {
		return i.disconnectedPrecacheList(imageListFile, manifest)
	}

	i.log.Info("Checking seed image info")
	seedInfo, err := seedclusterinfo.ReadSeedClusterInfoFromFile(seedInfoFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read seed info: %s, %w", common.PathOutsideChroot(seedInfoFile), err)
	}
	i.log.Info("Collected seed info for precache: ", fmt.Sprintf("%+v", seedInfo))

	i.log.Info("Getting mirror registry source registries")
	mirrorRegistrySources, err := mirrorRegistrySourceRegistries(registriesConfFile)
	if err != nil {
		return nil, fmt.Errorf("failed to get mirror registry source registries: %w", err)
	}

	i.log.Info("Checking whether to override seed registry")
	shouldOverrideSeedRegistry, err := utils.ShouldOverrideSeedRegistry(seedInfo.MirrorRegistryConfigured, seedInfo.ReleaseRegistry, mirrorRegistrySources)
	if err != nil {
		return nil, fmt.Errorf("failed to check ShouldOverrideSeedRegistry: %w", err)
	}
	i.log.Info("Should override seed registry", "shouldOverrideSeedRegistry", shouldOverrideSeedRegistry, "releaseRegistry", i.config.ReleaseRegistry)

	i.log.Info("Precaching images")
	imageList, err := prep.ReadPrecachingList(imageListFile, i.config.ReleaseRegistry, seedInfo.ReleaseRegistry, shouldOverrideSeedRegistry)
	if err != nil {
		return nil, fmt.Errorf("failed to read pre-caching image file: %s, %w", common.PathOutsideChroot(imageListFile), err)
	}
	return imageList, nil
}

// disconnectedPrecacheList returns the disconnected precache presence set: one
// canonical name@digest reference per entry in the on-media digest manifest. The
// closure import (ImportAll) stored every manifest image under its canonical
// digest, so `podman image exists <name@digest>` resolves each locally.
//
// Driving the set from the whole manifest -- not just the seed's containers.list --
// is what makes precache cover the full profile-scoped closure: for install-only
// media that is the seed closure plus ExtraPrecacheImages and recert/lca-cli; for
// self-contained media it is the entire p3 site closure as well. Because create-usb
// already emits a profile-scoped manifest, this needs no on-node profile branching.
// Verifying only the seed's containers.list (as the first disconnected
// implementation did) would silently leave those extra images unverified.
//
// As a retained defense-in-depth invariant, the seed's own containers.list must
// still be fully covered by the manifest (verifyManifestCoversSeed) before the set
// is built: a manifest that does not satisfy every seed requirement by exact source
// reference, or a missing manifest, is a hard failure rather than a silently
// incomplete precache.
func (i *IBIPrepare) disconnectedPrecacheList(imageListFile string, manifest *localimages.DigestManifest) ([]string, error) {
	if manifest == nil {
		return nil, errors.New("disconnected precache requires the on-media digest manifest")
	}

	required, err := readSeedRequiredImages(imageListFile)
	if err != nil {
		return nil, err
	}

	if err := verifyManifestCoversSeed(required, manifest); err != nil {
		return nil, err
	}

	precacheSet := make([]string, 0, len(manifest.Images))
	for _, e := range manifest.Images {
		precacheSet = append(precacheSet, e.Name+"@"+e.Digest)
	}

	i.log.Infof("Disconnected precache will verify %d image(s) from the on-media manifest", len(precacheSet))
	return precacheSet, nil
}

// readSeedRequiredImages reads the seed's containers.list verbatim (no registry
// override): the references are compared against the manifest's canonical entries,
// so rewriting their registry here would defeat the coverage check.
func readSeedRequiredImages(imageListFile string) ([]string, error) {
	content, err := os.ReadFile(common.PathOutsideChroot(imageListFile)) //nolint:gosec // imageListFile is the validated seed containers.list path
	if err != nil {
		return nil, fmt.Errorf("failed to read pre-caching image file: %s, %w", common.PathOutsideChroot(imageListFile), err)
	}
	var images []string
	for _, line := range strings.Split(string(content), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			images = append(images, line)
		}
	}
	return images, nil
}

// verifyManifestCoversSeed fails unless every image the seed requires
// (containers.list) maps, by exact source reference, to exactly one entry in the
// imported closure (the v2 digest manifest). Each manifest entry records the
// source references it satisfies (ImageEntry.Sources, populated by the media
// builder); a seed reference is covered when it equals one of those sources on
// exactly one entry -- and because the closure import is fail-fast over every
// manifest entry, that entry's digest is necessarily imported.
//
// Matching is by exact source reference, never by repository name. A name-only
// match (repo covers repo:anytag) cannot establish that the manifest entry holds
// the image the seed asked for under that specific tag, so it could bind a tag to
// the wrong digest and report a successful disconnected prepare with the wrong
// image. A reference that matches no source (uncovered) or sources on more than
// one entry (ambiguous) is a hard failure: an unmapped or mis-mapped seed
// requirement would otherwise pass precache while leaving the node short -- or
// serving the wrong digest for -- an image it needs.
func verifyManifestCoversSeed(required []string, manifest *localimages.DigestManifest) error {
	sourceEntryCount := make(map[string]int)
	for _, e := range manifest.Images {
		for _, src := range e.Sources {
			sourceEntryCount[src]++
		}
	}

	var uncovered, ambiguous []string
	for _, ref := range required {
		switch sourceEntryCount[ref] {
		case 0:
			uncovered = append(uncovered, ref)
		case 1:
			// Exactly one manifest entry lists this source: unambiguous match.
		default:
			ambiguous = append(ambiguous, ref)
		}
	}

	var problems []string
	if len(uncovered) > 0 {
		problems = append(problems, fmt.Sprintf("digest manifest does not cover %d image(s) required by the seed: %s",
			len(uncovered), strings.Join(uncovered, ", ")))
	}
	if len(ambiguous) > 0 {
		problems = append(problems, fmt.Sprintf("%d seed image(s) map ambiguously to multiple manifest entries: %s",
			len(ambiguous), strings.Join(ambiguous, ", ")))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
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
