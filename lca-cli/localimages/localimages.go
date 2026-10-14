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
//	  "version": 1,
//	  "ociLayoutPath": "oci",
//	  "images": [
//	    {
//	      "name": "quay.io/openshift-release-dev/ocp-release",
//	      "digest": "sha256:9a1f...c3",
//	      "ociTag": "ocp-release"
//	    }
//	  ]
//	}
//
// ociLayoutPath is the layout directory relative to the media root; it is
// resolved so it always stays inside the media root (symlinks included). Each
// image is located by its ociTag (the required org.opencontainers.image.ref.name
// annotation in the layout index) and bound to its digest.
//
// # What entry.Digest represents
//
// entry.Digest is the digest of the top-level descriptor as it appears in the
// layout's index.json (manifests array). For OpenShift release content this is
// normally a multi-arch image index, but it may also be a single-platform image
// manifest. The descriptor is imported as-is: a multi-arch index is preserved
// (no platform flattening, no re-digesting), so the stored object's digest
// equals entry.Digest exactly and the canonical name@digest reference round
// trips end-to-end.
//
// # Selection and verification
//
// For each image the package locates the single index.json descriptor that
// matches both entry.Digest and the (mandatory) org.opencontainers.image.ref.name
// annotation equal to entry.OCITag, then
// copies that exact descriptor by its index position via
// oci/layout.NewIndexReference. A digested destination reference is used so
// copy.Image independently verifies the copied bytes against entry.Digest, and
// the stored image is re-resolved after the copy to confirm it is present under
// its canonical digest.
package localimages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/oci/layout"
	"github.com/containers/image/v5/signature"
	istorage "github.com/containers/image/v5/storage"
	"github.com/containers/image/v5/types"
	cstorage "github.com/containers/storage"
	// Register the containers-storage graph drivers (overlay, vfs, ...) so the
	// store can be opened at runtime. btrfs/devmapper are excluded via build
	// tags to keep the build CGO-light (see the Makefile build tags).
	_ "github.com/containers/storage/drivers/register"
	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
)

// supportedManifestVersion is the only digest-manifest schema version this
// package understands.
const supportedManifestVersion = 1

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
	// Version is the schema version; only version 1 is supported.
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
	// It may name a multi-arch image index, which is imported as-is.
	Digest string `json:"digest"`
	// OCITag is the layout reference name (org.opencontainers.image.ref.name)
	// for the descriptor.
	OCITag string `json:"ociTag"`
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
	if m.Version != supportedManifestVersion {
		return fmt.Errorf("unsupported version %d (want %d)", m.Version, supportedManifestVersion)
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

	return &Importer{
		log:       log,
		store:     store,
		policyCtx: policyCtx,
		sysCtx:    &types.SystemContext{},
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

	// Import the descriptor as-is (preserve a multi-arch index when that is what
	// the digest names). The digested destination makes copy.Image verify the
	// copied bytes against e.Digest.
	if _, err := copy.Image(ctx, im.policyCtx, destRef, srcRef, &copy.Options{
		SourceCtx:          im.sysCtx,
		DestinationCtx:     im.sysCtx,
		ImageListSelection: copy.CopyAllImages,
	}); err != nil {
		return fmt.Errorf("copying %s@%s into containers-storage: %w", e.Name, e.Digest, err)
	}

	// Post-condition: the image is resolvable in the target store under its
	// canonical digest.
	if _, _, err := istorage.ResolveReference(destRef); err != nil {
		return fmt.Errorf("verifying stored image %s@%s: %w", e.Name, e.Digest, err)
	}

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
