// Package localimages imports container images from a mounted OCI image layout
// directly into a target containers-storage store, under their canonical
// name-by-digest, with no registry and no registries.conf reference rewriting.
//
// It is the offline image-sourcing primitive for USB-based image-based install
// (IBI): the installer media carries an OCI layout plus a digest manifest, and
// this package copies the selected images into the target stateroot's
// containers-storage graphroot so they resolve locally by their canonical
// references.
//
// # Canonical by construction
//
// The destination reference is built as containers-storage:<name>@<digest>, so
// containers-storage records each image under its canonical repository name and
// digest. No registry is contacted, registries.conf is not consulted, and the
// ReleaseRegistry/ReplaceImageRegistry hostname rewriting (which stores images
// under non-canonical names that cannot be resolved offline) is deliberately
// not used.
//
// # Digest manifest schema
//
// Selection is driven by an on-media digest manifest:
//
//	{
//	  "version": 2,
//	  "ociLayoutPath": "oci",
//	  "images": [
//	    {
//	      "name": "quay.io/openshift-release-dev/ocp-release",
//	      "digest": "sha256:9a1f...c3",
//	      "ociTag": "ocp-release",
//	      "sources": ["quay.io/openshift-release-dev/ocp-release:4.15.0"]
//	    }
//	  ]
//	}
//
// ociLayoutPath is the layout directory relative to the media root; it is
// resolved so it always stays inside the media root (symlinks included). Each
// image is located by its ociTag (the required org.opencontainers.image.ref.name
// annotation in the layout index) and bound to its digest.
//
// sources (version 2) records the source references -- including tags -- that the
// seed requests for this image (as they appear in the seed's containers.list). It
// is the producer/consumer contract that lets a disconnected prepare verify the
// manifest covers every seed requirement by exact source reference, and register
// each tagged source as a local alias of the imported digest so a consumer that
// asks for name:tag (not name@digest) resolves the image locally. Version 1
// manifests carry no sources.
//
// # What entry.Digest represents
//
// entry.Digest is the digest of the top-level descriptor as it appears in the
// layout's index.json (manifests array). For OpenShift release content this is
// normally a multi-arch image index, but it may also be a single-platform image
// manifest.
//
// containers-storage stores single-platform images; it cannot hold a manifest
// list and all of its instances as a group. So for a multi-arch index the
// package imports the single instance matching the target platform: the host
// architecture/OS by default (for the on-node prepare path, where the host is
// the node being installed), or StoreConfig.Architecture when the caller builds
// media on a workstation whose architecture may differ from the target node's.
// That instance's layers are materialised. The index manifest is still recorded against the
// stored image, so the canonical name@<entry.Digest> reference resolves to it
// and round-trips end-to-end -- the index digest is preserved as a resolvable
// reference, even though the non-host platforms' layers are not copied. A
// single-platform descriptor is imported directly.
//
// # Selection and verification
//
// For each image the package locates the single index.json descriptor that
// matches both entry.Digest and the (mandatory) org.opencontainers.image.ref.name
// annotation equal to entry.OCITag, then copies that exact descriptor by its
// index position via oci/layout.NewIndexReference. A digested destination
// reference is used so copy.Image verifies the copied content against
// entry.Digest (for a single-platform descriptor, directly; for a multi-arch
// index, against the index manifest the selected instance belongs to), and the
// stored image is re-resolved after the copy to confirm it is present under its
// canonical digest.
//
// VerifyAll provides a standalone store-residency gate over a whole manifest: it
// confirms every image is recorded and resolvable in the store under its
// canonical name@digest AND that every source reference the seed requests for it
// (ImageEntry.Sources) resolves to that same imported image, which a disconnected
// prepare runs after ImportAll to catch an incomplete import before any terminal
// action. It is a residency check, not a second integrity pass -- content
// integrity is already enforced above at import time by the digest-verified copy.
package localimages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/signature"
	istorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	cstorage "go.podman.io/storage"
	// Register the containers-storage graph drivers (overlay, vfs, ...) so the
	// store can be opened at runtime. btrfs/devmapper are excluded via build
	// tags to keep the build CGO-light (see the Makefile build tags).
	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	_ "go.podman.io/storage/drivers/register"
)

const (
	// minSupportedManifestVersion is the oldest digest-manifest schema version this
	// package understands. Version 1 carries no per-image sources.
	minSupportedManifestVersion = 1
	// SourcesManifestVersion is the first schema version that records each image's
	// source references (ImageEntry.Sources). It is also the newest version this
	// package understands.
	SourcesManifestVersion = 2
	// maxSupportedManifestVersion is the newest digest-manifest schema version this
	// package understands.
	maxSupportedManifestVersion = SourcesManifestVersion
)

// ociLayoutIndexFile is the index file at the root of an OCI image layout.
const ociLayoutIndexFile = "index.json"

var (
	// ErrDigestNotFound is returned (wrapped) when a requested digest is not
	// present in the OCI layout index. No copy is attempted.
	ErrDigestNotFound = errors.New("requested digest not present in OCI layout")

	// ErrAmbiguousSelection is returned (wrapped) when a digest manifest entry
	// does not resolve to exactly one layout descriptor: more than one
	// descriptor matches, or the ociTag and digest resolve to different
	// descriptors. No copy is attempted.
	ErrAmbiguousSelection = errors.New("ambiguous image selection in OCI layout")
)

// DigestManifest is the parsed on-media digest manifest that selects which
// layout images to import.
type DigestManifest struct {
	// Version is the schema version; versions 1 and 2 are supported. Version 2
	// adds the per-image Sources mapping.
	Version int `json:"version"`
	// OCILayoutPath is the OCI layout directory, relative to the media root.
	OCILayoutPath string `json:"ociLayoutPath"`
	// Images is the set of images to import. Must be non-empty.
	Images []ImageEntry `json:"images"`
}

// ImageEntry selects a single image from the OCI layout.
type ImageEntry struct {
	// Name is the canonical repository name the image is stored under
	// (e.g. quay.io/openshift-release-dev/ocp-release).
	Name string `json:"name"`
	// Digest is the digest of the top-level index.json descriptor to import.
	// It may name a multi-arch image index; the instance matching the target
	// platform is imported, and the index digest remains the canonical
	// reference (see the package doc).
	Digest string `json:"digest"`
	// OCITag is the layout reference name (org.opencontainers.image.ref.name)
	// for the descriptor.
	OCITag string `json:"ociTag"`
	// Sources are the source references -- registry name[:tag] or name@digest --
	// that the seed requests for this image (as they appear in the seed's
	// containers.list). The media builder populates them in version 2 manifests.
	// They let a disconnected prepare (a) verify the manifest covers every image
	// the seed requires by exact source-reference match and (b) register each
	// tagged source as a local alias of the imported digest, so a consumer that
	// asks for name:tag (not name@digest) resolves the image locally. Empty in
	// version 1 manifests.
	Sources []string `json:"sources,omitempty"`
}

// ParseDigestManifest reads and validates the on-media digest manifest at path.
func ParseDigestManifest(path string) (*DigestManifest, error) {
	// #nosec G304 -- path is an operator-provided on-media manifest location.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading digest manifest %q: %w", path, err)
	}

	var m DigestManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing digest manifest %q: %w", path, err)
	}

	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("validating digest manifest %q: %w", path, err)
	}

	return &m, nil
}

// validate checks that the manifest is structurally complete.
func (m *DigestManifest) validate() error {
	if m.Version < minSupportedManifestVersion || m.Version > maxSupportedManifestVersion {
		return fmt.Errorf("unsupported version %d (want %d..%d)", m.Version, minSupportedManifestVersion, maxSupportedManifestVersion)
	}
	if m.OCILayoutPath == "" {
		return errors.New("ociLayoutPath is required")
	}
	if len(m.Images) == 0 {
		return errors.New("images must not be empty")
	}
	for i := range m.Images {
		if err := m.Images[i].validate(); err != nil {
			return fmt.Errorf("images[%d]: %w", i, err)
		}
	}
	return nil
}

// validate checks that an image entry is complete and carries a valid digest.
func (e *ImageEntry) validate() error {
	if e.Name == "" {
		return errors.New("name is required")
	}
	if e.OCITag == "" {
		return errors.New("ociTag is required")
	}
	if e.Digest == "" {
		return errors.New("digest is required")
	}
	if _, err := digest.Parse(e.Digest); err != nil {
		return fmt.Errorf("invalid digest %q: %w", e.Digest, err)
	}
	return nil
}

// StoreConfig makes the target containers-storage location explicit. The zero
// value targets the live/default store (as configured by the host).
type StoreConfig struct {
	// GraphDriverName is the storage driver (e.g. "overlay", "vfs").
	GraphDriverName string
	// GraphRoot is the containers-storage graphroot
	// (e.g. the stateroot's /var/lib/containers/storage).
	GraphRoot string
	// RunRoot is the containers-storage run root.
	RunRoot string
	// DriverOptions are optional graph-driver options.
	DriverOptions []string
	// Architecture, when non-empty, selects the image instance for this
	// architecture from a multi-arch index (types.SystemContext.ArchitectureChoice)
	// instead of the build host's architecture. create-usb sets it because the media
	// is built on a workstation whose architecture may differ from the target node's;
	// the on-node prepare leaves it empty so the host architecture (the node's own) is
	// used. It does not participate in isDefault: it is a copy-time platform selector,
	// not a store-location option.
	Architecture string
}

// isDefault reports whether the config targets the live/default store.
func (c StoreConfig) isDefault() bool {
	return c.GraphDriverName == "" && c.GraphRoot == "" && c.RunRoot == "" && len(c.DriverOptions) == 0
}

// Importer imports OCI-layout images into a containers-storage store under their
// canonical name-by-digest. Create one with NewImporter and release it with
// Close.
type Importer struct {
	log       *logrus.Logger
	store     cstorage.Store
	policyCtx *signature.PolicyContext
	sysCtx    *types.SystemContext
}

// NewImporter constructs an Importer targeting the store described by cfg. An
// empty cfg targets the live/default store. Callers must Close the returned
// Importer.
func NewImporter(log *logrus.Logger, cfg StoreConfig) (*Importer, error) {
	if log == nil {
		return nil, errors.New("logger is required")
	}

	var opts cstorage.StoreOptions
	if cfg.isDefault() {
		defaults, err := cstorage.DefaultStoreOptions()
		if err != nil {
			return nil, fmt.Errorf("loading default containers-storage options: %w", err)
		}
		opts = defaults
	} else {
		opts = cstorage.StoreOptions{
			GraphDriverName:    cfg.GraphDriverName,
			GraphRoot:          cfg.GraphRoot,
			RunRoot:            cfg.RunRoot,
			GraphDriverOptions: cfg.DriverOptions,
		}
	}

	store, err := cstorage.GetStore(opts)
	if err != nil {
		return nil, fmt.Errorf("opening containers-storage store: %w", err)
	}

	// Accept-anything signature policy: the source is a trusted local OCI
	// layout on the installer media, reached over a credential-less local
	// transport, so no signature verification is performed.
	policy := &signature.Policy{
		Default: []signature.PolicyRequirement{signature.NewPRInsecureAcceptAnything()},
	}
	policyCtx, err := signature.NewPolicyContext(policy)
	if err != nil {
		if _, shutErr := store.Shutdown(false); shutErr != nil {
			log.WithError(shutErr).Warn("failed to shut down containers-storage store")
		}
		return nil, fmt.Errorf("creating signature policy context: %w", err)
	}

	// ArchitectureChoice, when set, makes copy.CopySystemImage select the instance for
	// the caller's target architecture from a multi-arch index rather than the build
	// host's. An empty value preserves the host-architecture default (the on-node
	// prepare path, where host == node).
	return &Importer{
		log:       log,
		store:     store,
		policyCtx: policyCtx,
		sysCtx:    &types.SystemContext{ArchitectureChoice: cfg.Architecture},
	}, nil
}

// Close releases the signature policy context and shuts down the store. It is
// safe to call on a partially-initialised Importer.
func (im *Importer) Close() error {
	var errs []error
	if im.policyCtx != nil {
		if err := im.policyCtx.Destroy(); err != nil {
			errs = append(errs, fmt.Errorf("destroying signature policy context: %w", err))
		}
		im.policyCtx = nil
	}
	if im.store != nil {
		if _, err := im.store.Shutdown(false); err != nil {
			errs = append(errs, fmt.Errorf("shutting down containers-storage store: %w", err))
		}
		im.store = nil
	}
	return errors.Join(errs...)
}

// ImportAll imports every image in the manifest. It is fail-fast: it stops at
// the first failure and returns an error naming the offending image's name and
// digest.
func (im *Importer) ImportAll(ctx context.Context, mediaRoot string, m *DigestManifest) error {
	for i := range m.Images {
		e := m.Images[i]
		if err := im.ImportImage(ctx, mediaRoot, m, e); err != nil {
			return fmt.Errorf("importing %s@%s: %w", e.Name, e.Digest, err)
		}
		im.log.WithFields(logrus.Fields{"name": e.Name, "digest": e.Digest}).Info("imported local image")
	}
	return nil
}

// VerifyAll reports whether every image in the manifest is resident in this
// Importer's store under its canonical name@digest reference, and whether every
// source reference the seed requests for it (ImageEntry.Sources) resolves to that
// same imported image. It is the store-residency / resolvability gate a
// disconnected prepare runs after ImportAll to confirm the installed node's store
// holds the full image closure -- addressable by both its canonical reference and
// every reference the seed's containers.list actually requests -- before any
// terminal action (power-off / reboot). It is a correctness gate, not a
// best-effort check.
//
// It is a residency check, not a fresh integrity check: content integrity is
// already enforced at import time by copy.Image's digest-verified copy (see
// "Selection and verification" above). VerifyAll confirms each manifest image and
// its source references are recorded and resolvable in the store; it does not
// re-hash stored layer content.
//
// Every entry is checked (not fail-fast): the returned error names every
// missing/unresolvable image and reports the verified count. It returns nil only
// when all images resolve. The logic is deliberately factored here -- rather than
// inline in the prepare flow -- so a future check-usb media-verification tool can
// reuse the exact same store-residency contract.
func (im *Importer) VerifyAll(ctx context.Context, m *DigestManifest) error {
	// Guard the standalone contract: an empty (or nil) manifest must not pass
	// vacuously. ParseDigestManifest already rejects an empty Images list, but a
	// future caller (e.g. check-usb) may build the manifest another way.
	if m == nil || len(m.Images) == 0 {
		return errors.New("cannot verify store residency: digest manifest has no images")
	}

	var failures []string
	for i := range m.Images {
		e := m.Images[i]
		if err := im.verifyImage(e); err != nil {
			im.log.WithFields(logrus.Fields{"name": e.Name, "digest": e.Digest}).
				WithError(err).Warn("manifest image not resolvable in store")
			failures = append(failures, err.Error())
		}
	}

	verified := len(m.Images) - len(failures)
	if len(failures) > 0 {
		return fmt.Errorf("verified %d/%d images resident; %d not resolvable: %s",
			verified, len(m.Images), len(failures), strings.Join(failures, "; "))
	}
	im.log.Infof("verified %d/%d images resident in the store", verified, len(m.Images))
	return nil
}

// verifyImage reports whether a single manifest entry is resident in the store
// under its canonical name@digest reference AND that every source reference the
// seed requests for it (e.Sources) resolves in the same store to that same
// imported image. It uses the same containers-storage resolve path
// (ParseStoreReference + ResolveReference) that ImportImage runs as its post-copy
// post-condition. It does not re-hash stored content.
//
// Verifying the sources -- not only the canonical name@digest -- closes a gap a
// canonical-only check leaves open. registerSourceAliases skips digest-pinned
// sources on the assumption they already resolve by their canonical reference,
// which only holds when the source repository equals e.Name. A manifest may name a
// digest-pinned source in a repository different from Name: its canonical image is
// resident (name@digest resolves) and would pass a canonical-only check, yet the
// reference the seed's containers.list actually requests resolves to nothing. By
// requiring every source to resolve to the imported image, verifyImage (via
// VerifyAll) fails such a manifest before any terminal action instead of leaving
// the node short an image it can never pull offline.
func (im *Importer) verifyImage(e ImageEntry) error {
	if err := e.validate(); err != nil {
		return fmt.Errorf("invalid image entry: %w", err)
	}

	canonical := e.Name + "@" + e.Digest
	storedImage, err := im.resolveStoredImage(canonical)
	if err != nil {
		return err
	}

	// Every source reference the seed requests for this image must resolve to the
	// same stored image. A source that does not resolve -- or resolves to a
	// different image -- means the reference the node will request at runtime is
	// unavailable (or bound to the wrong content) offline.
	for _, src := range e.Sources {
		srcImage, err := im.resolveStoredImage(src)
		if err != nil {
			return fmt.Errorf("source reference %q of %s is not resident: %w", src, canonical, err)
		}
		if srcImage.ID != storedImage.ID {
			return fmt.Errorf("source reference %q of %s resolves to image %s, want the imported image %s",
				src, canonical, srcImage.ID, storedImage.ID)
		}
	}
	return nil
}

// resolveStoredImage parses ref as a containers-storage reference in this
// Importer's store and resolves it to the stored image, returning an error if ref
// is not resident. It does not re-hash stored content.
func (im *Importer) resolveStoredImage(ref string) (*cstorage.Image, error) {
	storeRef, err := istorage.Transport.ParseStoreReference(im.store, ref)
	if err != nil {
		return nil, fmt.Errorf("building containers-storage reference for %s: %w", ref, err)
	}
	_, img, err := istorage.ResolveReference(storeRef)
	if err != nil {
		return nil, fmt.Errorf("resolving stored image %s: %w", ref, err)
	}
	return img, nil
}

// ImportImage selects the layout descriptor matching both e.Digest and e.OCITag,
// copies that exact descriptor into containers-storage as e.Name@e.Digest, and
// verifies the stored image is present under its canonical digest.
func (im *Importer) ImportImage(ctx context.Context, mediaRoot string, m *DigestManifest, e ImageEntry) error {
	if err := e.validate(); err != nil {
		return fmt.Errorf("invalid image entry: %w", err)
	}

	// Keep the layout directory inside the media root. ociLayoutPath is a
	// relative path from an on-media manifest; SecureJoin resolves it (symlinks
	// included) so the result can never escape mediaRoot, e.g. a "../.." prefix
	// is clamped rather than allowed to select a layout outside the mounted
	// media.
	srcDir, err := securejoin.SecureJoin(mediaRoot, m.OCILayoutPath)
	if err != nil {
		return fmt.Errorf("resolving OCI layout path %q under media root %q: %w", m.OCILayoutPath, mediaRoot, err)
	}

	index, err := loadLayoutIndex(srcDir)
	if err != nil {
		return err
	}

	pos, err := selectManifestIndex(index, e)
	if err != nil {
		return err
	}

	// Copy the exact descriptor by position, so the bytes copied are the ones
	// whose digest we matched -- not whatever a tag might point at.
	srcRef, err := layout.NewIndexReference(srcDir, pos)
	if err != nil {
		return fmt.Errorf("building OCI layout reference for %s[%d]: %w", srcDir, pos, err)
	}

	// Canonical by construction: containers-storage:<name>@<digest>. The store
	// (driver/graphroot/runroot) is taken from this Importer's store.
	destRef, err := istorage.Transport.ParseStoreReference(im.store, e.Name+"@"+e.Digest)
	if err != nil {
		return fmt.Errorf("building containers-storage reference for %s@%s: %w", e.Name, e.Digest, err)
	}

	// containers-storage stores single-platform images and cannot hold a
	// manifest list as a group, so for a multi-arch index copy the single
	// instance matching the target platform (the host architecture/OS). The
	// index manifest is still recorded against the stored image, so the digested
	// destination (name@<index-digest>) resolves and copy.Image verifies the
	// copied content against e.Digest. A single-platform descriptor is copied
	// directly.
	if _, err := copy.Image(ctx, im.policyCtx, destRef, srcRef, &copy.Options{
		SourceCtx:          im.sysCtx,
		DestinationCtx:     im.sysCtx,
		ImageListSelection: copy.CopySystemImage,
	}); err != nil {
		return fmt.Errorf("copying %s@%s into containers-storage: %w", e.Name, e.Digest, err)
	}

	// Post-condition: the image is resolvable in the target store under its
	// canonical digest.
	_, storedImage, err := istorage.ResolveReference(destRef)
	if err != nil {
		return fmt.Errorf("verifying stored image %s@%s: %w", e.Name, e.Digest, err)
	}

	// Register the seed's requested tagged references as local aliases of the
	// imported digest, so a consumer that asks for name:tag (not name@digest)
	// resolves the image locally. The media builder records them in e.Sources.
	if err := im.registerSourceAliases(storedImage, e); err != nil {
		return fmt.Errorf("registering source aliases for %s@%s: %w", e.Name, e.Digest, err)
	}

	return nil
}

// registerSourceAliases adds each tagged source reference in e.Sources as an
// additional name on the stored image, so the reference the seed actually
// requests (e.g. name:tag) resolves locally to the imported digest. Digest-pinned
// sources are skipped: when the source repository is e.Name the image is already
// stored under that canonical name@digest, so it resolves without an alias, and
// adding a name the image already carries is a harmless no-op (the store dedupes
// names). A digest-pinned source in a different repository is not aliased here and
// will not resolve; VerifyAll's source-resolution check catches that and fails the
// prepare rather than leaving the reference unavailable offline.
func (im *Importer) registerSourceAliases(storedImage *cstorage.Image, e ImageEntry) error {
	if storedImage == nil {
		return fmt.Errorf("stored image %s@%s resolved to nil", e.Name, e.Digest)
	}

	aliases := make([]string, 0, len(e.Sources))
	for _, src := range e.Sources {
		// A digest-pinned source (repo@sha256:...) already resolves to the stored
		// image by its canonical reference, so it needs no alias.
		if strings.Contains(src, "@") {
			continue
		}
		aliases = append(aliases, src)
	}
	if len(aliases) == 0 {
		return nil
	}

	if err := im.store.AddNames(storedImage.ID, aliases); err != nil {
		return fmt.Errorf("adding aliases %v to image %s: %w", aliases, storedImage.ID, err)
	}
	im.log.WithFields(logrus.Fields{"image": e.Name + "@" + e.Digest, "aliases": aliases}).
		Info("registered local image aliases")
	return nil
}

// loadLayoutIndex reads and parses the index.json at the root of an OCI layout.
func loadLayoutIndex(srcDir string) (*imgspecv1.Index, error) {
	indexPath := filepath.Join(srcDir, ociLayoutIndexFile)
	// #nosec G304 -- srcDir derives from the operator-provided media root and
	// the manifest's ociLayoutPath.
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return nil, fmt.Errorf("reading OCI layout index %q: %w", indexPath, err)
	}

	var index imgspecv1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("parsing OCI layout index %q: %w", indexPath, err)
	}
	return &index, nil
}

// selectManifestIndex returns the zero-based position in index.Manifests of the
// single descriptor that matches entry e. A descriptor matches when its digest
// equals e.Digest and it carries an org.opencontainers.image.ref.name
// annotation equal to e.OCITag. The tag is mandatory: an untagged descriptor
// never matches, so an image cannot be imported for a tag it does not identify.
//
// It returns an error wrapping ErrDigestNotFound when no descriptor carries the
// digest, and ErrAmbiguousSelection when more than one descriptor matches or the
// digest is present only under a descriptor that is untagged or tagged
// differently from e.OCITag.
func selectManifestIndex(index *imgspecv1.Index, e ImageEntry) (int, error) {
	matches := make([]int, 0, 1)
	digestPresent := false

	for i := range index.Manifests {
		d := index.Manifests[i]
		if d.Digest.String() != e.Digest {
			continue
		}
		digestPresent = true

		// The requested tag is mandatory: a descriptor only matches when it
		// carries an org.opencontainers.image.ref.name annotation equal to
		// e.OCITag. An untagged descriptor does not identify the requested tag
		// and is rejected, so an image is never imported for a tag it does not
		// carry.
		tag := d.Annotations[imgspecv1.AnnotationRefName]
		if tag != e.OCITag {
			// Digest is present but not under the requested tag (untagged or
			// tagged differently).
			continue
		}
		matches = append(matches, i)
	}

	switch {
	case len(matches) == 1:
		return matches[0], nil
	case len(matches) > 1:
		return 0, fmt.Errorf("%w: %d descriptors match %s with tag %q", ErrAmbiguousSelection, len(matches), e.Digest, e.OCITag)
	case digestPresent:
		return 0, fmt.Errorf("%w: digest %s is present but not under tag %q", ErrAmbiguousSelection, e.Digest, e.OCITag)
	default:
		return 0, fmt.Errorf("%w: digest %s (tag %q)", ErrDigestNotFound, e.Digest, e.OCITag)
	}
}
