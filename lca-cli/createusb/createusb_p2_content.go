/*
Copyright 2026.

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

package createusb

// This file implements the create-usb self-contained durability-store stage (USB-based
// IBI proposal §6.4 step 4, §8.D): build the read-only containers/storage image store
// from the on-media closure, validate it against the target CRI-O overlay driver/layer
// format (the §6.6 compatibility gate), and stage it read-only on p2 at store/.
// install-only emits nothing.
//
// # Producer of the on-media durability store
//
// The store is the self-contained media's primary runtime durability artifact: a
// containers/storage overlay store that the installed node registers as a read-only
// additional image store so the digest-pinned closure resolves locally, with no registry
// and no network. It is built from the same digest-pinned image set BuildImageLayout
// (createusb_images.go) already mirrored into the p2 OCI layout and recorded in
// digests.json -- this stage reads that committed manifest and imports its closure into
// the store, so the store and the OCI layout can never drift.
//
// The import reuses the localimages consumer (the same in-process containers/image path
// ibi-preparation uses to populate the installed stateroot store), pointed at a store
// graphroot under p2Root instead of the stateroot. There is no external skopeo binary and
// no registry: the proposal's "skopeo copy oci:<layout> containers-storage:..." is
// realized in-process, symmetric with the layout producer and the prepare-side consumer.
//
// # Crash-safe staging and recoverable commit
//
// Mirroring createusb_images.go's model, the store is built into a fresh, build-unique
// staging directory under p2Root and validated before anything committed is touched. Only
// then is it committed by a recoverable swap: the previous store/ is moved aside, the staging
// store renamed into place, and the previous store restored if any step fails. So the
// committed store/ only ever appears complete and validated.
//
// The preservation guarantee is scoped to an in-process commit *failure*: if a rename returns
// an error mid-swap, the moved-aside previous store is restored before returning, so the last
// valid store survives. A crash mid-commit (the process dies after a moveAside, before the swap
// completes) is a different case and is not preserved in place: the previous store is left in a
// .store-backup-* backup, and the next build GCs that backup and rebuilds the store from the OCI
// layout + digests.json (the source of truth that always remains on the media) rather than
// restoring it. A rebuild that then fails is surfaced as a hard error out of
// BuildDurabilityStore, so media assembly (CNF-27171) must treat an unsuccessful rebuild as
// unusable media rather than shipping a store-less medium. Each build garbage-collects stale
// staging and backup artifacts first, so a crashed build self-heals on the next run instead of
// failing, and a re-run replaces a previous store/ rather than erroring on it.
//
// # Target architecture
//
// The media is built on a workstation whose architecture may differ from the node the media
// installs, and the OCI layout preserves every architecture. BuildDurabilityStore therefore
// selects the target node's architecture explicitly (via localimages.StoreConfig.Architecture)
// rather than defaulting to the build host's; a layout that carries no instance for the target
// architecture fails the build, so a cross-architecture build can never ship a store the node
// cannot run.
//
// # §6.6 compatibility gate
//
// The store is built with the overlay graph driver because that is the driver RHCOS/CRI-O
// use on the node; a store built with any other driver would not be consumable there. The
// load-bearing gate is opening the store with the overlay driver explicitly: if the build
// environment cannot produce an overlay store the open fails, failing the build rather than
// shipping an unusable store. validateOverlayStore then confirms the import populated the
// store and wrote the overlay driver/layer metadata CRI-O reads. If the overlay additional
// -image-store model is ever found unworkable, the documented alternative is the registry
// fallback (an ImageDigestMirrorSet plus an on-node registry) -- documented in
// docs/create-usb.md and deliberately not implemented here.
//
// # Wiring
//
// Like BuildImageLayout, BuildDurabilityStore is a self-contained, unit-tested building
// block. Wiring it into the end-to-end media-assembly flow (after the OCI layout +
// digests.json are committed) lands in the media-assembly story (CNF-27171).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/localimages"
)

const (
	// durabilityStoreDirName is the committed containers/storage graphroot staged directly
	// under the p2 media root, beside the OCI layout and digests.json. The installed node
	// consumes it after lca-cli ibi prepare copies and registers it on the installed disk.
	durabilityStoreDirName = "store"

	// storeStagingDirPrefix prefixes the fresh, build-unique directory the store is built
	// into before it is atomically renamed to store/. Stale entries with this prefix
	// (orphaned by a crashed build) are garbage-collected at the start of every build.
	storeStagingDirPrefix = ".store-staging-"

	// storeBackupPrefix prefixes the build-unique location the previous store/ is moved
	// aside to during a commit, so it can be restored if the swap fails. Stale backups (orphaned by a crash mid-commit) are garbage-collected at the start
	// of every build; the media is always rebuildable from the OCI layout + digests.json, so a
	// discarded stale backup is never load-bearing.
	storeBackupPrefix = ".store-backup-"

	// durabilityStoreDriver is the containers/storage graph driver the durability store is
	// built with. It is fixed to overlay: RHCOS/CRI-O use overlay for the node's persistent
	// container store (matching ibi-preparation's closureStoreDriver), so a store built with
	// any other driver could not be consumed there. Opening the store with this driver
	// explicitly is the build side of the §6.6 compatibility gate.
	durabilityStoreDriver = "overlay"

	// storeRunRootPrefix prefixes the temporary runroot a store open needs for its transient
	// locks/state. It is created outside p2Root (that tree is the media staging area) and
	// removed once the store is shut down.
	storeRunRootPrefix = "create-usb-store-runroot-"
)

// overlay graph-driver on-disk markers validateOverlayStore checks for. They are the
// directories/files containers/storage writes for an overlay store and that CRI-O reads
// back, so their presence is what confirms the import populated the store in the expected
// overlay driver/layer format rather than leaving an empty or incomplete directory.
const (
	overlayGraphDirName   = "overlay"
	overlayImagesJSONPath = "overlay-images/images.json"
	overlayLayersJSONPath = "overlay-layers/layers.json"
)

// StoreBuildResult is returned by BuildDurabilityStore once the read-only durability
// store has been staged under the p2 media root.
type StoreBuildResult struct {
	// StorePath is the committed containers/storage graphroot (<p2Root>/store).
	StorePath string
	// ImportedImages is the number of closure images imported into the store (the digest
	// manifest's image count; ImportAll is fail-fast, so a nil error means every one was
	// imported).
	ImportedImages int
	// StoreBytes is the on-media byte size of the staged store. It feeds the USB size budget
	// (CNF-27171); note the closure is carried twice on self-contained media -- once as the
	// OCI layout, once as this store.
	StoreBytes int64
}

// storeImporter is the subset of *localimages.Importer the durability-store build uses. It
// is an interface so the gating/validation/staging logic is unit-testable with a fake that
// does not open a real overlay store (which needs root and overlay support).
type storeImporter interface {
	ImportAll(ctx context.Context, mediaRoot string, m *localimages.DigestManifest) error
	Close() error
}

// storeBuildDeps bundles the injectable seams, mirroring imageBuildDeps/mediaBuildDeps.
type storeBuildDeps struct {
	// parseManifest reads the committed on-media digest manifest.
	parseManifest func(path string) (*localimages.DigestManifest, error)
	// newImporter opens a containers/storage importer targeting the store described by cfg.
	newImporter func(log *logrus.Logger, cfg localimages.StoreConfig) (storeImporter, error)
}

// defaultStoreBuildDeps returns the production seams backed by localimages, exactly as
// defaultImageBuildDeps wires the image seams in createusb_images.go.
func defaultStoreBuildDeps() storeBuildDeps {
	return storeBuildDeps{
		parseManifest: localimages.ParseDigestManifest,
		// On error NewImporter returns a nil *Importer, which this direct return converts to a
		// non-nil storeImporter wrapping a nil pointer; that is harmless because the sole caller
		// (importClosureIntoStore) checks the error and returns before using the importer.
		newImporter: func(log *logrus.Logger, cfg localimages.StoreConfig) (storeImporter, error) {
			return localimages.NewImporter(log, cfg)
		},
	}
}

// BuildDurabilityStore builds the read-only containers/storage durability store for
// self-contained media and stages it under p2Root at store/, validated against the target
// CRI-O overlay driver/layer format, for consumption by the installed node. It is built from the
// digest-pinned closure in the digests.json BuildImageLayout committed at the p2 root, so
// the store and the OCI layout carry the same image set.
//
// targetArch is the architecture of the node the media installs (e.g. "amd64", "arm64").
// It is selected explicitly from the multi-arch OCI layout rather than defaulting to the
// build host's architecture, so media built on a workstation of a different architecture
// still produces a store the installed node can consume; a layout that carries no instance
// for targetArch fails the build.
//
// install-only emits nothing: it ships no on-media durability store, so the call is a
// no-op returning a nil result.
//
// It is the entry point the media-assembly story (CNF-27171) wires in, after the OCI
// layout + digests.json are committed; it supplies targetArch.
func BuildDurabilityStore(ctx context.Context, log *logrus.Logger, m *Manifest, p2Root, targetArch string) (*StoreBuildResult, error) {
	return buildDurabilityStore(ctx, log, m, p2Root, targetArch, defaultStoreBuildDeps())
}

// buildDurabilityStore is the dependency-injected core of BuildDurabilityStore.
func buildDurabilityStore(ctx context.Context, log *logrus.Logger, m *Manifest, p2Root, targetArch string, deps storeBuildDeps) (result *StoreBuildResult, err error) {
	// install-only ships no on-media durability store (the node is not reconfigured
	// offline), so this stage emits nothing.
	if m.Mode != ModeSelfContained {
		log.Infof("create-usb: mode %q ships no durability store; nothing to stage", m.Mode)
		return nil, nil
	}

	// The store must carry the target node's architecture, not the build host's. A build on
	// a workstation of a different architecture would otherwise silently import the builder's
	// instance via copy.CopySystemImage, producing a valid-looking store the node cannot run,
	// so the target architecture is required and selected explicitly from the layout.
	targetArch = strings.TrimSpace(targetArch)
	if targetArch == "" {
		return nil, errors.New("target architecture is required to select the node's image instances from the layout")
	}

	// p2Root is normally created by the image-layout stage; create it defensively so this
	// stage does not depend on call order (mirroring BuildImageLayout).
	if err := os.MkdirAll(p2Root, 0o750); err != nil {
		return nil, fmt.Errorf("creating p2 root %q: %w", p2Root, err)
	}

	// Serialize against the p2 root for the whole stage, so a concurrent BuildImageLayout
	// against the same media root cannot GC or replace the OCI layout this import reads
	// (createusb_images.go takes the same lock).
	unlock, err := lockP2Root(p2Root)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Build the store from the same digest-pinned closure the OCI layout was mirrored from:
	// read the manifest BuildImageLayout committed at the p2 root. Its OCILayoutPath locates
	// the layout the importer copies out of (resolved under p2Root by localimages).
	manifestPath := filepath.Join(p2Root, digestManifestFileName)
	manifest, err := deps.parseManifest(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("reading on-media digest manifest %q (run the image-layout stage first): %w", manifestPath, err)
	}

	// Garbage-collect staging and backup artifacts orphaned by an earlier crashed build, so a
	// crash self-heals here rather than accumulating leftovers under p2Root.
	gcStaleStoreArtifacts(log, p2Root)

	// Build into a fresh, build-unique staging directory; it is never store/, so an
	// in-process failure or a crash before the commit below cannot leave a half-built store/
	// on the media. The committed defer keeps the staging directory only until it has become
	// store/.
	staging, err := os.MkdirTemp(p2Root, storeStagingDirPrefix)
	if err != nil {
		return nil, fmt.Errorf("creating durability store staging directory under %q: %w", p2Root, err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rmErr := os.RemoveAll(staging); rmErr != nil && err == nil {
			err = fmt.Errorf("cleaning up durability store staging directory %q: %w", staging, rmErr)
		}
	}()

	if err := importClosureIntoStore(ctx, log, manifest, p2Root, staging, targetArch, deps); err != nil {
		return nil, err
	}

	// §6.6 compatibility gate: confirm the populated store is in the overlay driver/layer
	// format the target CRI-O reads, before it is committed.
	if err := validateOverlayStore(staging); err != nil {
		return nil, fmt.Errorf("validating durability store against target CRI-O: %w", err)
	}

	// Size the staging store before the commit, so a sizing failure cannot leave a committed
	// store/ behind (nothing is committed until the swap below).
	storeBytes, err := dirSize(staging)
	if err != nil {
		return nil, fmt.Errorf("sizing durability store %q: %w", staging, err)
	}

	// Commit the store with a recoverable swap: the previous store/ is moved aside, the
	// validated staging store is renamed into place, and the previous store is restored if any
	// step returns an error. A crash mid-commit is recovered by rebuild-from-layout on the next
	// build (see the file header).
	storePath := filepath.Join(p2Root, durabilityStoreDirName)
	if err := commitStore(log, staging, storePath); err != nil {
		return nil, err
	}
	committed = true

	log.WithFields(logrus.Fields{
		"storePath":      storePath,
		"importedImages": len(manifest.Images),
		"storeBytes":     storeBytes,
	}).Info("create-usb: staged read-only durability store on p2")

	return &StoreBuildResult{
		StorePath:      storePath,
		ImportedImages: len(manifest.Images),
		StoreBytes:     storeBytes,
	}, nil
}

// importClosureIntoStore opens an overlay containers/storage store at storePath and imports
// the full digest-pinned closure from the on-media OCI layout (resolved under p2Root) into
// it. The store is opened with a temporary runroot outside p2Root (a store open needs a
// writable runroot and the media tree must carry only the graphroot) and shut down via the
// importer's Close before the caller validates/sizes the on-disk result.
func importClosureIntoStore(ctx context.Context, log *logrus.Logger, manifest *localimages.DigestManifest, p2Root, storePath, targetArch string, deps storeBuildDeps) error {
	runRoot, err := os.MkdirTemp("", storeRunRootPrefix)
	if err != nil {
		return fmt.Errorf("creating temporary store runroot: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(runRoot); rmErr != nil {
			log.Warnf("create-usb: failed to remove temporary store runroot %s: %v", runRoot, rmErr)
		}
	}()

	importer, err := deps.newImporter(log, localimages.StoreConfig{
		GraphDriverName: durabilityStoreDriver,
		GraphRoot:       storePath,
		RunRoot:         runRoot,
		// Select the target node's architecture explicitly: the build host may differ, and
		// the OCI layout preserves every architecture, so a host-default selection could
		// import the wrong instances.
		Architecture: targetArch,
	})
	if err != nil {
		// A failure to open an overlay store here is the §6.6 gate firing: the build
		// environment cannot produce the overlay store the node needs.
		return fmt.Errorf("opening %s durability store at %q: %w", durabilityStoreDriver, storePath, err)
	}
	// Close (store shutdown) must run before the caller reads the on-disk store, so its
	// metadata is flushed; a close failure after a successful import is surfaced so the
	// build does not report a store that was not cleanly committed.
	closed := false
	defer func() {
		if !closed {
			if cErr := importer.Close(); cErr != nil {
				log.Warnf("create-usb: failed to close durability store importer: %v", cErr)
			}
		}
	}()

	if err := importer.ImportAll(ctx, p2Root, manifest); err != nil {
		return fmt.Errorf("importing image closure into durability store: %w", err)
	}

	closed = true
	if err := importer.Close(); err != nil {
		return fmt.Errorf("closing durability store after import: %w", err)
	}
	return nil
}

// commitStore atomically swaps the validated staging store into store/, preserving the
// previous store until the new one is in place. It moves the previous store aside, renames
// the staging store into place, and restores the previous store if any step returns an error
// -- so an in-process commit *failure* never leaves the media without a valid store.
//
// A crash mid-swap is a different case and is not recovered in place: the previous store is
// left aside in a .store-backup-* backup that the next build GCs rather than restores. Builds
// are serialized by the p2 lock, so no concurrent build observes the swap window; the crashed
// build self-heals on the next build, which rebuilds the store from the OCI layout +
// digests.json (the source of truth that always remains on the media). A rebuild that itself
// fails is surfaced as a hard error, so media assembly must treat an unsuccessful rebuild as
// unusable media.
func commitStore(log *logrus.Logger, stagingStore, storePath string) error {
	p2Root := filepath.Dir(storePath)
	storeBackup := filepath.Join(p2Root, storeBackupPrefix+durabilityStoreDirName)

	// Move the previous store aside (a first build has none, backing up to ""), so it can be
	// restored if a later step fails.
	savedStore, err := moveAside(storePath, storeBackup)
	if err != nil {
		return fmt.Errorf("preserving previous durability store %q: %w", storePath, err)
	}

	// Swap in the new store; restore the previous store if it fails.
	if err := os.Rename(stagingStore, storePath); err != nil {
		restoreAside(log, savedStore, storePath)
		return fmt.Errorf("committing durability store to %q: %w", storePath, err)
	}

	// The new store is committed; discard the preserved previous store.
	discardAside(log, savedStore)
	return nil
}

// moveAside renames path to backup so a failed commit can restore it, returning the backup
// path (or "" when path does not exist and there is nothing to preserve). The backup is a
// sibling under the same p2 root, so the rename is atomic (same filesystem). A stale backup
// (orphaned by a crashed earlier commit) is GC'd before the build, but is removed defensively
// here so the rename cannot fail on an existing target.
func moveAside(path, backup string) (string, error) {
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("checking %q: %w", path, err)
	}
	if err := os.RemoveAll(backup); err != nil {
		return "", fmt.Errorf("removing stale backup %q: %w", backup, err)
	}
	if err := os.Rename(path, backup); err != nil {
		return "", fmt.Errorf("renaming %q aside to %q: %w", path, backup, err)
	}
	return backup, nil
}

// restoreAside renames a backup produced by moveAside back to its original path on a failed
// commit. A "" backup (nothing was preserved) is a no-op. It is best-effort: the commit is
// already failing and returning an error, so a restore failure is logged rather than masking
// the original error.
func restoreAside(log *logrus.Logger, backup, path string) {
	if backup == "" {
		return
	}
	if err := os.Rename(backup, path); err != nil {
		log.Warnf("create-usb: failed to restore previous durability artifact %s to %s: %v", backup, path, err)
	}
}

// discardAside removes a backup produced by moveAside once the commit has succeeded. A ""
// backup is a no-op. Best-effort: a leftover backup wastes media space but is GC'd by the
// next build and never misleads a consumer.
func discardAside(log *logrus.Logger, backup string) {
	if backup == "" {
		return
	}
	if err := os.RemoveAll(backup); err != nil {
		log.Warnf("create-usb: failed to remove durability artifact backup %s: %v", backup, err)
	}
}

// gcStaleStoreArtifacts removes staging and backup artifacts under p2Root left by a crashed
// earlier build (those with storeStagingDirPrefix or storeBackupPrefix). It is best-effort: a
// failure wastes media space but cannot leave a mismatched medium, so it is logged rather than
// returned. It must be called under the p2 lock so it never races a concurrent build's live
// staging or backup artifacts.
func gcStaleStoreArtifacts(log *logrus.Logger, p2Root string) {
	entries, err := os.ReadDir(p2Root)
	if err != nil {
		log.Warnf("create-usb: failed to scan for stale durability store artifacts: %v", err)
		return
	}
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasPrefix(name, storeStagingDirPrefix) && !strings.HasPrefix(name, storeBackupPrefix) {
			continue
		}
		path := filepath.Join(p2Root, name)
		if rmErr := os.RemoveAll(path); rmErr != nil {
			log.Warnf("create-usb: failed to remove stale durability store artifact %s: %v", path, rmErr)
		}
	}
}

// validateOverlayStore confirms the staged store was populated in the overlay
// containers/storage driver/layer format the target CRI-O reads: the overlay graph-driver
// directory exists, the image metadata records at least one image, and the overlay layer
// metadata exists. The store is opened with the overlay driver explicitly (so a wrong-driver
// store fails at open, not here); this check guards against an import that left the store
// empty or without its overlay metadata.
func validateOverlayStore(storePath string) error {
	graphDir := filepath.Join(storePath, overlayGraphDirName)
	if info, err := os.Stat(graphDir); err != nil {
		return fmt.Errorf("overlay graph-driver directory %q missing (store not in overlay layer format): %w", graphDir, err)
	} else if !info.IsDir() {
		return fmt.Errorf("overlay graph-driver path %q is not a directory", graphDir)
	}

	imagesJSON := filepath.Join(storePath, overlayImagesJSONPath)
	count, err := countStoredImages(imagesJSON)
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("durability store %q records no images", storePath)
	}

	layersJSON := filepath.Join(storePath, overlayLayersJSONPath)
	if _, err := os.Stat(layersJSON); err != nil {
		return fmt.Errorf("overlay layer metadata %q missing: %w", layersJSON, err)
	}

	return nil
}

// countStoredImages reads a containers/storage images.json and returns the number of image
// records it holds. The file is a JSON array of image objects; only its length is needed
// here (the per-image digest/name linkage is guaranteed by the importer's fail-fast copy).
func countStoredImages(imagesJSONPath string) (int, error) {
	data, err := os.ReadFile(imagesJSONPath) //nolint:gosec // imagesJSONPath is under the build-time store the tool just created
	if err != nil {
		return 0, fmt.Errorf("reading store image metadata %q: %w", imagesJSONPath, err)
	}
	var images []json.RawMessage
	if err := json.Unmarshal(data, &images); err != nil {
		return 0, fmt.Errorf("parsing store image metadata %q: %w", imagesJSONPath, err)
	}
	return len(images), nil
}
