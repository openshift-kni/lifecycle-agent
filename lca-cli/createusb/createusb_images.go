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

// This file implements the create-usb image-build stage (USB-based IBI proposal
// §6.4 steps 1, 2, 6 and §8.E): resolve the profile's image set to digests,
// mirror it into the p2 OCI image layout, and emit the on-media digest manifest
// that doubles as the prepare precache list and the self-check reference, plus a
// resulting-size report.
//
// # Producer of the localimages schema
//
// The consumer of the digest manifest already exists on the branch: package
// lca-cli/localimages (CNF-27168) defines localimages.DigestManifest /
// localimages.ImageEntry and imports images from the OCI layout by matching
// digest + ociTag against the layout index.json. This file is the producer side:
// it constructs and marshals the localimages types directly so the producer and
// consumer can never drift, and the emitted file is validated by round-tripping
// it through localimages.ParseDigestManifest before it is published.
//
// # Multi-arch preservation
//
// The consumer imports a single host-platform instance (copy.CopySystemImage) and
// relies on the index descriptor digest resolving in the target store. The
// producer therefore mirrors with copy.CopyAllImages so the full multi-arch index
// is preserved in the layout and the recorded digest is the top-level index.json
// descriptor digest (paired with the org.opencontainers.image.ref.name annotation),
// exactly the pair localimages selection matches on.
//
// # Trust
//
// Mirroring uses an accept-anything signature policy (as localimages does). Pinning
// each reference to a digest guarantees content-addressing/immutability, not
// publisher identity or provenance.
//
// # Wiring
//
// Build remains a documented not-implemented stub; the end-to-end media assembly
// that wires BuildImageLayout into the live-ISO/partitioning flow lands in
// CNF-27171. The functions here are self-contained, unit-tested building blocks.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/docker"
	"github.com/containers/image/v5/oci/layout"
	"github.com/containers/image/v5/signature"
	"github.com/containers/image/v5/types"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/localimages"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

const (
	// ociLayoutDirName is both the OCI layout directory name under the p2 media root
	// and the value recorded as DigestManifest.OCILayoutPath (relative to the media
	// root). The consumer resolves it with SecureJoin against the media root.
	ociLayoutDirName = "oci"

	// digestManifestFileName is the on-media digest-manifest name written at the p2
	// root; the value later recorded as IBIPrepareConfig.ExpectedDigestsPath.
	digestManifestFileName = "digests.json"

	// digestManifestSuffix is appended to the --output path for the build-side copy
	// (<output>.digests.json) used for auditing.
	digestManifestSuffix = ".digests.json"

	// digestManifestVersion is the schema version emitted; it must match the only
	// version localimages understands.
	digestManifestVersion = 1

	// ociStagingPrefix prefixes the temporary staging directory created inside the
	// p2 root so the completed layout can be published with an atomic same-filesystem
	// rename, and so a failed build leaves no stale output.
	ociStagingPrefix = ".oci-staging-"
)

// ociTagInvalidChars matches every run of characters that is not allowed in the
// alphanumeric portion of an org.opencontainers.image.ref.name component.
var ociTagInvalidChars = regexp.MustCompile(`[^A-Za-z0-9]+`)

// ImageSetOptions carries the authoritative build inputs the resolver needs beyond
// the manifest. recert and lca-cli are guaranteed from these inputs (not from
// incidental seed containers.list contents) and deduplicated against the seed list.
type ImageSetOptions struct {
	// AuthFile is the workstation --auth-file used for the seed-image pull and the
	// registry mirror (build-time only; never written to the media).
	AuthFile string
	// RecertImage is the authoritative recert pull-spec (from --recert-image /
	// RELATED_IMAGE_RECERT_IMAGE / the default). Required.
	RecertImage string
	// LCAImage is the authoritative lca-cli pull-spec (from --lca-image). The
	// runtime resolver that reads the running operator pod is unavailable on a build
	// workstation, so an explicit value is the honest source. Required.
	LCAImage string
}

// SizeReport is the resulting-size estimate for the USB-size budget. It is an
// estimate: it excludes the final filesystem/ISO overhead applied by the later
// media-assembly story (CNF-27171).
type SizeReport struct {
	// OCILayoutBytes is the byte subtotal of the staged OCI image layout.
	OCILayoutBytes int64
	// StagedBytes is the full staged-p2 byte total (all staged bytes).
	StagedBytes int64
}

// LayoutBuildResult is returned by BuildImageLayout once the OCI layout and both
// digest-manifest copies have been published.
type LayoutBuildResult struct {
	// Images is the published digest-manifest image set.
	Images []localimages.ImageEntry
	// Size is the resulting-size estimate.
	Size SizeReport
}

// ---- injectable seams ----
//
// Registry and seed-image interactions are hidden behind these interfaces so the
// pure resolution/mirroring/emission logic is unit-testable with fakes; the real
// implementations (which need network and a seed image) are exercised by the
// build-stage orchestration in CNF-27171 and by the integration test.

// seedClosureReader reads the seed image's embedded /containers.list at build time.
// The seed image is built FROM scratch; COPY . /, so containers.list sits at the
// image root (prepare reads it the same way from the mounted seed image).
type seedClosureReader interface {
	ReadContainersList(seedImage, authFile string) ([]string, error)
}

// imageMirrorer copies a single image reference into an OCI image layout under
// ociTag, preserving the full multi-arch index, and returns the digest of the
// top-level index.json descriptor that now carries the ociTag annotation. A
// reference that cannot be reached/resolved fails the copy (and therefore the
// build); the returned digest is the pinned, content-addressed reference.
type imageMirrorer interface {
	MirrorToLayout(ctx context.Context, imageRef, destLayoutDir, ociTag, authFile string) (string, error)
}

// p3ImageScanner returns the images referenced by the rendered p3 config-tree
// (self-contained mode). CNF-27169 supplies the real scanner; until then the
// default returns an explicit not-implemented error so self-contained builds fail
// loudly instead of silently shipping media missing its p3 images.
type p3ImageScanner interface {
	ScanP3Images(m *Manifest) ([]string, error)
}

// imageBuildDeps bundles the injectable seams.
type imageBuildDeps struct {
	seedReader seedClosureReader
	mirrorer   imageMirrorer
	p3Scanner  p3ImageScanner
}

// defaultImageBuildDeps returns the production seams backed by containers/image and
// the host ops helpers.
func defaultImageBuildDeps(log *logrus.Logger) imageBuildDeps {
	return imageBuildDeps{
		seedReader: &opsSeedClosureReader{
			ops: ops.NewOps(log, ops.NewRegularExecutor(log, false)),
			log: log,
		},
		mirrorer:  &containersImageMirrorer{},
		p3Scanner: notImplementedP3Scanner{},
	}
}

// BuildImageLayout resolves the manifest's image set, mirrors it into the p2 OCI
// image layout under p2Root, and publishes the digest manifest beside the output
// (<output>.digests.json) and at the p2 root (digests.json). Publication is atomic:
// everything is staged in a fresh temporary directory inside p2Root and published
// with a same-filesystem rename, so a failed build leaves no stale layout or
// manifest. It is the entry point the media-assembly story (CNF-27171) wires in.
func BuildImageLayout(ctx context.Context, log *logrus.Logger, m *Manifest, opts ImageSetOptions, outputPath, p2Root string) (*LayoutBuildResult, error) {
	return buildImageLayout(ctx, log, m, opts, outputPath, p2Root, defaultImageBuildDeps(log))
}

// buildImageLayout is the dependency-injected core of BuildImageLayout.
func buildImageLayout(ctx context.Context, log *logrus.Logger, m *Manifest, opts ImageSetOptions, outputPath, p2Root string, deps imageBuildDeps) (result *LayoutBuildResult, err error) {
	refs, err := resolveImageSet(m, opts, deps)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(p2Root, 0o750); err != nil {
		return nil, fmt.Errorf("creating p2 root %q: %w", p2Root, err)
	}

	// Stage inside p2Root so the completed layout can be published with an atomic
	// same-filesystem rename. The staging directory is always removed, so a failed
	// build leaves no partial output alongside the (untouched) final paths.
	stagingDir, err := os.MkdirTemp(p2Root, ociStagingPrefix)
	if err != nil {
		return nil, fmt.Errorf("creating staging directory under %q: %w", p2Root, err)
	}
	defer func() {
		if rmErr := os.RemoveAll(stagingDir); rmErr != nil && err == nil {
			err = fmt.Errorf("cleaning up staging directory %q: %w", stagingDir, rmErr)
		}
	}()

	stagingLayout := filepath.Join(stagingDir, ociLayoutDirName)
	entries, err := mirrorToOCILayout(ctx, refs, stagingLayout, opts.AuthFile, deps)
	if err != nil {
		return nil, err
	}

	// Validate every entry against the completed staged layout before publishing, so
	// a producer bug is caught here rather than at prepare time on the node.
	if err := verifyLayoutEntries(stagingLayout, entries); err != nil {
		return nil, err
	}

	size, err := computeSizeReport(stagingDir, stagingLayout)
	if err != nil {
		return nil, err
	}

	// Marshal and validate the manifest bytes before anything is published, so a bad
	// manifest fails the build while the final paths still hold their previous
	// (untouched) content.
	manifestData, err := marshalDigestManifest(entries)
	if err != nil {
		return nil, err
	}
	if err := validateDigestManifestBytes(manifestData, stagingDir); err != nil {
		return nil, err
	}

	logSizeReport(log, size)

	// Publish from the pre-validated content. The layout is swapped in without
	// destroying the previous one first (it is moved aside and only removed once the
	// new layout is in place), and each manifest copy is written with an atomic
	// temp-file-then-rename. Every published byte was validated above, so a failure
	// here never leaves a corrupt or partially-written file.
	finalLayout := filepath.Join(p2Root, ociLayoutDirName)
	if err := publishLayout(stagingLayout, finalLayout); err != nil {
		return nil, err
	}

	p2Path := filepath.Join(p2Root, digestManifestFileName)
	besideOutput := outputPath + digestManifestSuffix
	for _, path := range []string{p2Path, besideOutput} {
		if err := publishFile(path, manifestData); err != nil {
			return nil, err
		}
	}

	return &LayoutBuildResult{Images: entries, Size: size}, nil
}

// resolveImageSet builds the image reference set for the manifest's mode
// (USB-based IBI §6.4 step 1).
//
//   - install-only: the seed closure (the seed image's embedded containers.list) plus
//     recert and lca-cli from the authoritative inputs, unioned with
//     extraPrecacheImages and deduplicated. recert and lca-cli are guaranteed present
//     as a checked invariant, independent of the incidental containers.list contents.
//   - self-contained: the p3-scan union point is wired through the p3ImageScanner seam;
//     until CNF-27169 supplies the scanner it returns an explicit not-implemented
//     error so self-contained builds fail loudly rather than shipping media missing
//     its p3 images.
func resolveImageSet(m *Manifest, opts ImageSetOptions, deps imageBuildDeps) ([]string, error) {
	switch m.Mode {
	case ModeSelfContained:
		// Wire the p3 union point here; the scanner currently fails loudly.
		images, err := deps.p3Scanner.ScanP3Images(m)
		if err != nil {
			return nil, fmt.Errorf("resolving self-contained image set: %w", err)
		}
		return images, nil
	case ModeInstallOnly:
		return resolveInstallOnlyImageSet(m, opts, deps)
	default:
		return nil, fmt.Errorf("cannot resolve image set for unsupported mode %q", m.Mode)
	}
}

// resolveInstallOnlyImageSet implements the install-only closure.
func resolveInstallOnlyImageSet(m *Manifest, opts ImageSetOptions, deps imageBuildDeps) ([]string, error) {
	// recert and lca-cli are guaranteed from authoritative inputs, so an empty value
	// fails the build rather than silently shipping media without them.
	if strings.TrimSpace(opts.RecertImage) == "" {
		return nil, fmt.Errorf("recert image is required (set --recert-image or %s)", common.RecertImageEnvKey)
	}
	if strings.TrimSpace(opts.LCAImage) == "" {
		return nil, fmt.Errorf("lca-cli image is required (set --lca-image)")
	}

	seedList, err := deps.seedReader.ReadContainersList(m.SeedImage, opts.AuthFile)
	if err != nil {
		return nil, fmt.Errorf("reading seed image closure: %w", err)
	}

	var refs []string
	for _, img := range seedList {
		if img = strings.TrimSpace(img); img != "" {
			refs = utils.AppendToListIfNotExists(refs, img)
		}
	}

	// Guarantee recert and lca-cli independently of the incidental containers.list
	// contents: they are added from the authoritative, build-validated inputs above
	// and deduplicated against the seed list here. The empty-input guard at the top of
	// this function is the invariant that enforces their presence; the append makes
	// them part of the closure whether or not the seed list already carried them.
	refs = utils.AppendToListIfNotExists(refs, opts.RecertImage)
	refs = utils.AppendToListIfNotExists(refs, opts.LCAImage)

	if m.ExtraPrecacheImages != nil {
		for _, img := range *m.ExtraPrecacheImages {
			if img = strings.TrimSpace(img); img != "" {
				refs = utils.AppendToListIfNotExists(refs, img)
			}
		}
	}

	return refs, nil
}

// mirrorToOCILayout mirrors every reference into the OCI layout at stagingLayoutDir
// (USB-based IBI §6.4 step 2 + §5.2 digest-pin), returning one ImageEntry per image
// carrying the name, the pinned top-level index digest, and a deterministic unique
// ociTag. Any reference that cannot be mirrored/pinned fails the build with a
// wrapped error naming the offending image; no partial manifest is produced.
func mirrorToOCILayout(ctx context.Context, refs []string, stagingLayoutDir, authFile string, deps imageBuildDeps) ([]localimages.ImageEntry, error) {
	if len(refs) == 0 {
		return nil, errors.New("no images to mirror: the resolved image set is empty")
	}

	entries := make([]localimages.ImageEntry, 0, len(refs))
	usedTags := make(map[string]struct{}, len(refs))
	for i, ref := range refs {
		tag := uniqueOCITag(ref, i, usedTags)
		dgst, err := deps.mirrorer.MirrorToLayout(ctx, ref, stagingLayoutDir, tag, authFile)
		if err != nil {
			return nil, fmt.Errorf("failed to mirror and digest-pin image %q: %w", ref, err)
		}
		entries = append(entries, localimages.ImageEntry{
			Name:   nameForRef(ref),
			Digest: dgst,
			OCITag: tag,
		})
	}
	return entries, nil
}

// marshalDigestManifest marshals the digest manifest for the given entries.
// Together with validateDigestManifestBytes and publishFile it produces the two
// byte-identical on-media copies (<output>.digests.json for build-side auditing and
// digests.json at the p2 root as the self-check/precache reference) from
// pre-validated content.
func marshalDigestManifest(entries []localimages.ImageEntry) ([]byte, error) {
	manifest := localimages.DigestManifest{
		Version:       digestManifestVersion,
		OCILayoutPath: ociLayoutDirName,
		Images:        entries,
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("marshaling digest manifest: %w", err)
	}
	return data, nil
}

// validateDigestManifestBytes confirms the exact bytes that will be published parse
// and validate under the consumer's parser, guaranteeing producer/consumer schema
// parity. The validation file is created under scratchDir and removed afterwards.
func validateDigestManifestBytes(data []byte, scratchDir string) error {
	tmp, err := os.CreateTemp(scratchDir, ".digests-validate-")
	if err != nil {
		return fmt.Errorf("creating digest-manifest validation file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing digest-manifest validation file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing digest-manifest validation file: %w", err)
	}
	if _, err := localimages.ParseDigestManifest(tmpPath); err != nil {
		return fmt.Errorf("emitted digest manifest failed validation: %w", err)
	}
	return nil
}

// publishFile atomically writes data to path by writing to a temp file in the same
// directory and renaming it into place, so a reader never observes a half-written
// file and a failed write never corrupts an existing one.
func publishFile(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".digests-publish-")
	if err != nil {
		return fmt.Errorf("creating temp file for %q: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %q: %w", path, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file for %q: %w", path, err)
	}
	if err = os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("setting permissions on %q: %w", path, err)
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publishing %q: %w", path, err)
	}
	return nil
}

// publishLayout moves the staged layout into place at final without destroying the
// previous layout first: an existing final layout is moved aside and only removed
// once the new one is in place, and is restored if the swap fails. staging is a
// sibling of final (same filesystem), so the rename is atomic.
func publishLayout(staging, final string) error {
	var backup string
	if _, err := os.Stat(final); err == nil {
		backup = final + ".previous"
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("clearing previous-layout backup %q: %w", backup, err)
		}
		if err := os.Rename(final, backup); err != nil {
			return fmt.Errorf("moving previous OCI layout aside: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking for existing OCI layout %q: %w", final, err)
	}

	if err := os.Rename(staging, final); err != nil {
		if backup != "" {
			// Best-effort restore of the previous layout.
			_ = os.Rename(backup, final)
		}
		return fmt.Errorf("publishing OCI layout to %q: %w", final, err)
	}

	if backup != "" {
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("removing previous-layout backup %q: %w", backup, err)
		}
	}
	return nil
}

// verifyLayoutEntries confirms that each entry resolves to exactly one descriptor
// in the staged layout index by its (digest, ociTag) pair -- the same pair the
// consumer (localimages) selects on -- so a producer/layout mismatch is caught
// before publication.
func verifyLayoutEntries(layoutDir string, entries []localimages.ImageEntry) error {
	index, err := readLayoutIndex(layoutDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		matches := 0
		for i := range index.Manifests {
			d := index.Manifests[i]
			if d.Digest.String() == e.Digest && d.Annotations[imgspecv1.AnnotationRefName] == e.OCITag {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("image %q (%s, tag %q) resolves to %d layout descriptors, want exactly 1", e.Name, e.Digest, e.OCITag, matches)
		}
	}
	return nil
}

// computeSizeReport returns the OCI-layout subtotal and the full staged-p2 total.
func computeSizeReport(stagingRoot, ociLayoutDir string) (SizeReport, error) {
	ociBytes, err := dirSize(ociLayoutDir)
	if err != nil {
		return SizeReport{}, fmt.Errorf("sizing OCI layout: %w", err)
	}
	stagedBytes, err := dirSize(stagingRoot)
	if err != nil {
		return SizeReport{}, fmt.Errorf("sizing staged media: %w", err)
	}
	return SizeReport{OCILayoutBytes: ociBytes, StagedBytes: stagedBytes}, nil
}

// logSizeReport logs the resulting-size estimate for the USB-size budget.
func logSizeReport(log *logrus.Logger, r SizeReport) {
	if log == nil {
		return
	}
	log.WithFields(logrus.Fields{
		"ociLayoutBytes": r.OCILayoutBytes,
		"stagedBytes":    r.StagedBytes,
	}).Info("create-usb resulting-size estimate (excludes final filesystem/ISO overhead)")
}

// dirSize returns the total size in bytes of the regular files under root.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %q: %w", d.Name(), err)
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("walking %q: %w", root, err)
	}
	return total, nil
}

// nameForRef returns the canonical repository name of an image reference, stripping
// any @digest and :tag (but preserving a registry :port).
func nameForRef(ref string) string {
	name := ref
	if at := strings.Index(name, "@"); at >= 0 {
		name = name[:at]
	}
	lastSlash := strings.LastIndex(name, "/")
	if colon := strings.LastIndex(name, ":"); colon > lastSlash {
		name = name[:colon]
	}
	return name
}

// uniqueOCITag returns a deterministic org.opencontainers.image.ref.name-valid tag
// for ref, disambiguated by index so tags never collide (a collision would make
// the consumer's selection ambiguous).
func uniqueOCITag(ref string, index int, used map[string]struct{}) string {
	tag := ociTagForRef(ref, index)
	// Defensive: index already guarantees uniqueness, but keep disambiguating if a
	// caller ever reuses an index.
	for {
		if _, taken := used[tag]; !taken {
			break
		}
		tag = fmt.Sprintf("%s-%d", tag, index)
	}
	used[tag] = struct{}{}
	return tag
}

// ociTagForRef derives a deterministic, valid ref.name tag from an image reference,
// suffixed with index for uniqueness. The result is always a valid
// org.opencontainers.image.ref.name: alphanumeric groups separated by single
// dashes, ending in the index digits.
func ociTagForRef(ref string, index int) string {
	name := ref
	if at := strings.Index(name, "@"); at >= 0 {
		name = name[:at]
	}
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		name = name[slash+1:]
	}
	if colon := strings.LastIndex(name, ":"); colon >= 0 {
		name = name[:colon]
	}
	sanitized := strings.Trim(ociTagInvalidChars.ReplaceAllString(name, "-"), "-")
	if sanitized == "" {
		sanitized = "image"
	}
	return fmt.Sprintf("%s-%d", sanitized, index)
}

// readLayoutIndex reads and parses the index.json at the root of an OCI layout.
func readLayoutIndex(layoutDir string) (*imgspecv1.Index, error) {
	indexPath := filepath.Join(layoutDir, "index.json")
	data, err := os.ReadFile(indexPath) //nolint:gosec // indexPath is under a build-time staging directory the tool created
	if err != nil {
		return nil, fmt.Errorf("reading OCI layout index %q: %w", indexPath, err)
	}
	var index imgspecv1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("parsing OCI layout index %q: %w", indexPath, err)
	}
	return &index, nil
}

// layoutDigestForTag returns the digest of the single index.json descriptor
// carrying the given ociTag annotation -- the top-level index descriptor the
// consumer selects on.
func layoutDigestForTag(layoutDir, ociTag string) (string, error) {
	index, err := readLayoutIndex(layoutDir)
	if err != nil {
		return "", err
	}
	var found []string
	for i := range index.Manifests {
		d := index.Manifests[i]
		if d.Annotations[imgspecv1.AnnotationRefName] == ociTag {
			found = append(found, d.Digest.String())
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("no layout descriptor carries tag %q after copy", ociTag)
	default:
		return "", fmt.Errorf("%d layout descriptors carry tag %q after copy", len(found), ociTag)
	}
}

// copyIntoLayout copies srcRef into the OCI layout at destLayoutDir under ociTag,
// preserving the full multi-arch index (copy.CopyAllImages), then returns the
// digest of the top-level index.json descriptor written under ociTag. It is shared
// by the production docker-source mirrorer and the integration test (which supplies
// an OCI-layout source), so the real copy + digest-capture path is exercised in CI.
func copyIntoLayout(ctx context.Context, srcRef types.ImageReference, destLayoutDir, ociTag, authFile string) (string, error) {
	if err := os.MkdirAll(destLayoutDir, 0o750); err != nil {
		return "", fmt.Errorf("creating OCI layout directory %q: %w", destLayoutDir, err)
	}

	destRef, err := layout.NewReference(destLayoutDir, ociTag)
	if err != nil {
		return "", fmt.Errorf("building OCI layout destination for tag %q: %w", ociTag, err)
	}

	policyCtx, err := newAcceptAnythingPolicyContext()
	if err != nil {
		return "", err
	}
	defer func() { _ = policyCtx.Destroy() }()

	sysCtx := &types.SystemContext{}
	if authFile != "" {
		sysCtx.AuthFilePath = authFile
	}

	if _, err := copy.Image(ctx, policyCtx, destRef, srcRef, &copy.Options{
		SourceCtx:          sysCtx,
		DestinationCtx:     sysCtx,
		ImageListSelection: copy.CopyAllImages,
	}); err != nil {
		return "", fmt.Errorf("copying image into OCI layout under tag %q: %w", ociTag, err)
	}

	return layoutDigestForTag(destLayoutDir, ociTag)
}

// newAcceptAnythingPolicyContext builds a signature policy context that accepts any
// image. Mirroring from a registry reached by digest provides content-addressing;
// signature/publisher verification is intentionally not performed (matching the
// consumer's localimages.NewImporter policy).
func newAcceptAnythingPolicyContext() (*signature.PolicyContext, error) {
	policy := &signature.Policy{
		Default: []signature.PolicyRequirement{signature.NewPRInsecureAcceptAnything()},
	}
	policyCtx, err := signature.NewPolicyContext(policy)
	if err != nil {
		return nil, fmt.Errorf("creating signature policy context: %w", err)
	}
	return policyCtx, nil
}

// ---- production seam implementations ----

// containersImageMirrorer mirrors from a docker:// registry into an OCI layout
// using in-process containers/image (resolving USB-based IBI §6.6: no oc-mirror or
// external skopeo binary; symmetric with the localimages import side).
type containersImageMirrorer struct{}

// MirrorToLayout parses imageRef as a docker registry reference and copies it into
// the layout.
func (*containersImageMirrorer) MirrorToLayout(ctx context.Context, imageRef, destLayoutDir, ociTag, authFile string) (string, error) {
	srcRef, err := docker.ParseReference("//" + imageRef)
	if err != nil {
		return "", fmt.Errorf("parsing source image reference %q: %w", imageRef, err)
	}
	return copyIntoLayout(ctx, srcRef, destLayoutDir, ociTag, authFile)
}

// opsSeedClosureReader reads the seed image's embedded containers.list by pulling
// and mounting the seed image with the host podman via ops.
type opsSeedClosureReader struct {
	ops ops.Ops
	log *logrus.Logger
}

// ReadContainersList pulls and mounts the seed image, reads its root
// /containers.list, and returns the deduplicated non-empty references.
func (r *opsSeedClosureReader) ReadContainersList(seedImage, authFile string) (list []string, err error) {
	pullArgs := []string{"pull"}
	if authFile != "" {
		pullArgs = append(pullArgs, "--authfile", authFile)
	}
	pullArgs = append(pullArgs, seedImage)
	if _, err := r.ops.RunInHostNamespace("podman", pullArgs...); err != nil {
		return nil, fmt.Errorf("pulling seed image %q: %w", seedImage, err)
	}
	// Clean up the pulled image on every exit path, including a mount failure below.
	// UnmountAndRemoveImage is a no-op unmount when the image was never mounted.
	defer func() {
		if umErr := r.ops.UnmountAndRemoveImage(seedImage); umErr != nil && err == nil {
			err = fmt.Errorf("cleaning up seed image %q: %w", seedImage, umErr)
		}
	}()

	mountpoint, err := r.ops.MountImage(seedImage)
	if err != nil {
		return nil, fmt.Errorf("mounting seed image %q: %w", seedImage, err)
	}

	data, err := r.ops.ReadFile(filepath.Join(mountpoint, common.ContainersListFileName))
	if err != nil {
		return nil, fmt.Errorf("reading %s from seed image: %w", common.ContainersListFileName, err)
	}
	return parseContainersList(data), nil
}

// parseContainersList splits a containers.list file (one reference per line) into a
// deduplicated slice of non-empty references.
func parseContainersList(data []byte) []string {
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = utils.AppendToListIfNotExists(out, line)
		}
	}
	return out
}

// notImplementedP3Scanner is the default self-contained p3 scanner: it fails loudly
// until CNF-27169 supplies the real rendered-p3-manifest scan.
type notImplementedP3Scanner struct{}

// ScanP3Images always returns a not-implemented error so self-contained builds do
// not ship media missing their p3 images.
func (notImplementedP3Scanner) ScanP3Images(_ *Manifest) ([]string, error) {
	return nil, fmt.Errorf("self-contained image scan not implemented (pending CNF-27169)")
}
