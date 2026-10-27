package ibi_preparation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"
	"go.podman.io/storage/pkg/archive"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/localimages"
)

const (
	// roStoreSELinuxType is the SELinux type applied to the persisted read-only additional
	// image store during prepare. The store copied from p2 inherits the media's mount-option
	// context (bin_t), which is a read-the-media label, not a container-storage one; CRI-O
	// reads an on-disk additional image store under the container-storage read-only content
	// type. The label is set during prepare (not post-boot) so CRI-O can read the store at the
	// moment offline recovery needs it; it is verified after pivot by the e2e recovery test.
	roStoreSELinuxType = "container_ro_file_t"

	// roStoreStagingSuffix names the staging directory the store is copied into before it is
	// atomically moved into place, so an interrupted copy never leaves a partial ro-store.
	roStoreStagingSuffix = ".staging"

	// Overlay-store on-disk markers used to validate the copied store is in the overlay layer
	// format CRI-O reads (the §6.6 compatibility gate, re-checked prepare-side).
	roStoreOverlayGraphDir   = "overlay"
	roStoreOverlayImagesJSON = "overlay-images/images.json"
	roStoreOverlayLayersJSON = "overlay-layers/layers.json"
)

// roStoreStorageConfDropIn is the storage.conf.d drop-in that registers the persisted store as
// a read-only additional image store at the post-pivot RODurabilityStorePath. The single %s is
// that absolute path. append=true is required: the vendored go.podman.io/storage conf.d loader
// otherwise has a drop-in array *replace* additionalimagestores, which would drop any entries
// an existing storage.conf already sets; append preserves them (merges into the effective
// config). The store is read-only by additional-image-store semantics plus its root-owned,
// relabelled on-disk form -- not by rewriting content modes.
const roStoreStorageConfDropIn = `# Written by lca-cli ibi prepare (CNF-27165) for self-contained USB-based IBI media.
#
# Registers the on-disk, read-only containers/storage durability store copied from the installer
# media as a read-only additional image store, so the installed node resolves the digest-pinned
# image closure locally with no registry and no network (the primary durability model). The copy
# survives kubelet GC and CRI-O reboot-time cleanup because both act only on the writable
# graphroot. append=true preserves any additionalimagestores already set in the effective config.
[storage.options]
additionalimagestores = ["%s", {append = true}]
`

// Seams overridable from tests. The real implementations copy a real containers-storage store
// and open it as an additional image store; tests replace them to exercise the persist
// orchestration (fail-closed, atomic seal, drop-in registration, ordering) without root, an
// overlay store, or SELinux.
var (
	// persistReadOnlyStoreFn lets Run()'s ordering and the install-only / connected no-op be
	// unit-tested without copying a real store.
	persistReadOnlyStoreFn = (*IBIPrepare).persistReadOnlyStore
	// copyReadOnlyStore copies the store tree preserving modes/metadata/xattrs/signatures.
	copyReadOnlyStore = defaultCopyReadOnlyStore
	// verifyPersistedStore confirms every manifest image resolves through the copied store
	// opened as a read-only additional image store (needs a real overlay store + root).
	verifyPersistedStore = defaultVerifyPersistedStore
)

// persistReadOnlyStore copies the pre-built read-only containers/storage durability store staged
// on p2 (common.LocalImagesStorePath) onto the installed disk, validates it against the on-media
// digest manifest, seals it read-only at the post-pivot /var/lib/containers/ro-store path, and
// registers it in the effective storage.conf via a conf.d drop-in. It runs only for
// self-contained disconnected media, while the stateroot is still mounted and before p2 is
// unmounted (so additionalimagestores never points at a path that has already gone away), and it
// fails closed: self-contained media that is missing or carries an incomplete store is an error,
// never a silent install-only-style no-op.
//
// The store copy and its config registration are atomic as a pair: the store is copied to a
// staging dir, verified, labelled, then atomically moved into place, and only then is the drop-in
// written. An interrupted copy leaves no ro-store and no storage.conf entry; a failure writing the
// drop-in rolls the store back.
func (i *IBIPrepare) persistReadOnlyStore(ctx context.Context, manifest *localimages.DigestManifest) error {
	// The store is staged at <p2 mount>/store; read it from the configured mount path so it
	// tracks LocalImagesPath (equal to common.LocalImagesStorePath in production).
	srcStore := filepath.Join(i.config.LocalImagesPath, common.LocalImagesStoreDirName)

	// Fail closed: self-contained media must carry a complete overlay durability store. A
	// missing/malformed store is worse than none, so reject it rather than degrade silently.
	if err := validateSourceStore(srcStore); err != nil {
		return fmt.Errorf("self-contained durability store on media is missing or incomplete: %w", err)
	}

	osname := common.GetStaterootName(i.config.SeedVersion)
	destStore := filepath.Join(common.GetStaterootPath(osname), common.ContainerStoragePath, common.RODurabilityStoreDirName)
	staging := destStore + roStoreStagingSuffix

	// Clear any leftover staging/partial tree from an interrupted earlier run so the
	// stage -> move is clean. staging and destStore share a parent dir (same filesystem), so
	// the move below is an atomic rename.
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("failed to clear read-only store staging dir %q: %w", staging, err)
	}
	committed := false
	defer func() {
		if !committed {
			if err := os.RemoveAll(staging); err != nil {
				i.log.Warnf("failed to clean up read-only store staging dir %q: %v", staging, err)
			}
		}
	}()

	// Copy the pre-built store onto the installed disk, preserving modes/metadata/xattrs and the
	// image signatures embedded in the overlay metadata; a lossy copy would silently drop those
	// signatures and break offline signature verification at the moment recovery runs.
	i.log.Infof("Copying read-only durability store from %s to %s", srcStore, staging)
	if err := copyReadOnlyStore(srcStore, staging); err != nil {
		return fmt.Errorf("failed to copy read-only store from %q to %q: %w", srcStore, staging, err)
	}

	// Validate the copied store: overlay layer format (re-checked against the target prepare-side)
	// and every manifest image resolvable through it as a read-only additional image store.
	if err := validateOverlayStoreFormat(staging); err != nil {
		return fmt.Errorf("copied read-only store is not a valid overlay store: %w", err)
	}
	if err := verifyPersistedStore(ctx, i.log, staging, manifest); err != nil {
		return fmt.Errorf("copied read-only store failed digest-manifest validation: %w", err)
	}

	// Label for the target container-storage policy during prepare (replacing the media's
	// mount-option bin_t), to be verified after pivot. The host namespace sees the stateroot at
	// its /mnt path, as the sysroot cleanup does.
	i.log.Infof("Applying SELinux type %s to read-only store %s", roStoreSELinuxType, staging)
	if _, err := i.ops.RunInHostNamespace("chcon", "-R", "-t", roStoreSELinuxType, staging); err != nil {
		return fmt.Errorf("failed to apply SELinux label to read-only store %q: %w", staging, err)
	}

	// Atomic publish: move the verified, labelled store into place. Remove any pre-existing store
	// first so a re-run is idempotent; the rename within the same filesystem is atomic.
	if err := os.RemoveAll(destStore); err != nil {
		return fmt.Errorf("failed to remove existing read-only store %q: %w", destStore, err)
	}
	if err := os.Rename(staging, destStore); err != nil {
		return fmt.Errorf("failed to move read-only store into place at %q: %w", destStore, err)
	}
	committed = true
	i.log.Infof("Sealed read-only durability store at %s", destStore)

	// Register the store only after it is in place and verified, so an interrupted prepare never
	// leaves a storage.conf entry pointing at an incomplete/absent store. If the drop-in write
	// fails, roll the store back so the (store, config) pair stays all-or-nothing.
	if err := i.registerReadOnlyStore(osname); err != nil {
		if rmErr := os.RemoveAll(destStore); rmErr != nil {
			i.log.Warnf("failed to roll back read-only store %q after drop-in write failure: %v", destStore, rmErr)
		}
		return fmt.Errorf("failed to register read-only store: %w", err)
	}
	return nil
}

// registerReadOnlyStore writes the storage.conf.d drop-in that registers the persisted store as a
// read-only additional image store. The drop-in is written onto the installed deployment's /etc
// (resolved via the ostree deployment dir) so it is the effective config post-pivot, and it is
// written atomically (temp file + rename) so a reader never observes a half-written config.
func (i *IBIPrepare) registerReadOnlyStore(osname string) error {
	deploymentDir, err := i.ostreeClient.GetDeploymentDir(osname)
	if err != nil {
		return fmt.Errorf("failed to resolve deployment dir for stateroot %q: %w", osname, err)
	}
	if deploymentDir == "" {
		return fmt.Errorf("no deployment found for stateroot %q", osname)
	}

	dropInDir := filepath.Join(deploymentDir, common.StorageConfDropInDir)
	// 0o755: /etc/containers/storage.conf.d is a world-readable config directory by convention
	// (it holds no secrets, only the public additional-image-store registration), matching the
	// mode it carries on a stock RHCOS node.
	if err := os.MkdirAll(dropInDir, 0o755); err != nil { //nolint:gosec // public config dir, no secrets
		return fmt.Errorf("failed to create storage.conf.d dir %q: %w", dropInDir, err)
	}
	dropInPath := filepath.Join(dropInDir, common.RODurabilityStoreDropInName)
	// Remove any stale drop-in from an earlier interrupted run first, so a failure writing the new
	// one never leaves an outdated registration pointing at a store this run is about to roll back.
	if err := os.Remove(dropInPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove stale storage.conf drop-in %q: %w", dropInPath, err)
	}
	content := fmt.Sprintf(roStoreStorageConfDropIn, common.RODurabilityStorePath)
	if err := writeFileAtomic(dropInPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("failed to write storage.conf drop-in %q: %w", dropInPath, err)
	}
	i.log.Infof("Registered read-only additional image store %s via %s", common.RODurabilityStorePath, dropInPath)
	return nil
}

// validateSourceStore fails closed when the expected p2 durability store is absent or not a valid
// overlay containers/storage store, before any store is copied onto the disk.
func validateSourceStore(storePath string) error {
	info, err := os.Stat(storePath)
	if err != nil {
		return fmt.Errorf("durability store %q not found: %w", storePath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("durability store path %q is not a directory", storePath)
	}
	return validateOverlayStoreFormat(storePath)
}

// validateOverlayStoreFormat confirms storePath holds a containers/storage store in the overlay
// layer format CRI-O reads: the overlay graph-driver directory, an images.json listing at least
// one image, and the layer metadata. It is the prepare-side half of the §6.6 compatibility gate,
// belt-and-suspenders over the build-time check CNF-27170 performs.
func validateOverlayStoreFormat(storePath string) error {
	graphDir := filepath.Join(storePath, roStoreOverlayGraphDir)
	if info, err := os.Stat(graphDir); err != nil {
		return fmt.Errorf("overlay graph-driver directory %q missing (store not in overlay layer format): %w", graphDir, err)
	} else if !info.IsDir() {
		return fmt.Errorf("overlay graph-driver path %q is not a directory", graphDir)
	}

	imagesJSON := filepath.Join(storePath, roStoreOverlayImagesJSON)
	count, err := countStoredImages(imagesJSON)
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("durability store %q records no images", storePath)
	}

	layersJSON := filepath.Join(storePath, roStoreOverlayLayersJSON)
	if _, err := os.Stat(layersJSON); err != nil {
		return fmt.Errorf("overlay layer metadata %q missing: %w", layersJSON, err)
	}
	return nil
}

// countStoredImages reads a containers/storage images.json and returns the number of image
// records it holds. The file is a JSON array of image objects; only its length is needed here.
func countStoredImages(imagesJSONPath string) (int, error) {
	data, err := os.ReadFile(imagesJSONPath) //nolint:gosec // imagesJSONPath is under the store copied from the trusted installer media
	if err != nil {
		return 0, fmt.Errorf("reading store image metadata %q: %w", imagesJSONPath, err)
	}
	var images []json.RawMessage
	if err := json.Unmarshal(data, &images); err != nil {
		return 0, fmt.Errorf("parsing store image metadata %q: %w", imagesJSONPath, err)
	}
	return len(images), nil
}

// defaultCopyReadOnlyStore copies the store tree with the same metadata-preserving archive copier
// create-usb uses to stage the store onto p2 (go.podman.io/storage/pkg/archive). It preserves
// symlinks (the overlay link farm), hardlinks, numeric ownership, capability/IMA/user.* xattrs
// and special mode bits, and errors if any required attribute cannot be restored -- so a store
// that cannot be copied faithfully fails the prepare rather than sealing a subtly-broken store.
func defaultCopyReadOnlyStore(srcRoot, dstRoot string) error {
	if err := archive.NewDefaultArchiver().CopyWithTar(srcRoot, dstRoot); err != nil {
		return fmt.Errorf("metadata-preserving copy of %q to %q: %w", srcRoot, dstRoot, err)
	}
	return nil
}

// defaultVerifyPersistedStore opens the copied store the way the installed node will -- as a
// read-only additional image store attached to a throwaway empty primary overlay store -- and
// confirms every image in the on-media manifest resolves through it under its canonical
// name@digest. This is the prepare-side proof that the sealed store is not merely present but
// usable for offline recovery; the full force-evict/recover check is the e2e (STORY-017).
func defaultVerifyPersistedStore(ctx context.Context, log *logrus.Logger, storePath string, manifest *localimages.DigestManifest) error {
	graphRoot, err := os.MkdirTemp("", "ibi-ro-store-verify-graph-")
	if err != nil {
		return fmt.Errorf("creating temporary verify graphroot: %w", err)
	}
	defer func() { _ = os.RemoveAll(graphRoot) }()
	runRoot, err := os.MkdirTemp("", "ibi-ro-store-verify-run-")
	if err != nil {
		return fmt.Errorf("creating temporary verify runroot: %w", err)
	}
	defer func() { _ = os.RemoveAll(runRoot) }()

	importer, err := localimages.NewImporter(log, localimages.StoreConfig{
		GraphDriverName: closureStoreDriver,
		GraphRoot:       graphRoot,
		RunRoot:         runRoot,
		DriverOptions:   []string{"imagestore=" + storePath},
	})
	if err != nil {
		return fmt.Errorf("opening copied store %q as a read-only additional image store: %w", storePath, err)
	}
	defer func() {
		if err := importer.Close(); err != nil {
			log.Warnf("failed to close read-only store verify importer: %v", err)
		}
	}()

	if err := importer.VerifyAll(ctx, manifest); err != nil {
		return fmt.Errorf("verifying copied store against digest manifest: %w", err)
	}
	return nil
}

// writeFileAtomic writes data to path via a temp file in the same directory followed by a rename,
// so a reader never observes a partially-written file and an interrupted write leaves either the
// old file or the new one, never a truncated one.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return fmt.Errorf("creating temp file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Best-effort removal of the temp file; a no-op once the rename below succeeds.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp file %q: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("setting mode on temp file %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming %q to %q: %w", tmpName, path, err)
	}
	return nil
}
