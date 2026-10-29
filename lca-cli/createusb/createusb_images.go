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
// Mirroring runs under the build host's configured container trust policy
// (newTrustPolicyContext / signature.DefaultPolicy), so a host that requires signatures
// rejects an unsigned or untrusted source and an unsigned image is mirrored only when the
// host policy permits it. Pinning each reference to a digest additionally guarantees
// content-addressing/immutability. An OCI layout cannot store signatures, so the
// signatures the source serves are stripped from the layout copy and staged beside it
// (createusb_signatures.go), keyed by manifest instance digest, and re-attached to the
// image when the node imports it into containers-storage (lca-cli/localimages).
//
// # Crash-safe publication
//
// The on-media digest manifest (digests.json at the p2 root) is the single commit
// point. Each build mirrors into a fresh, build-unique layout directory (never the
// one the live manifest references) and records that directory's name as the
// manifest's OCILayoutPath. Because the consumer locates the layout *through* the
// manifest, atomically replacing digests.json with a single rename is what switches
// the media from the previous layout to the new one: before the rename the committed
// manifest still points at the intact previous layout, after it the committed manifest
// points at the fully-written new layout. A crash (or power loss) at any point therefore
// leaves the media consistent -- the committed manifest always points at a complete
// layout -- and at worst leaves an orphaned layout directory, which the next build
// garbage-collects. Superseded and orphaned layout directories are removed only *after*
// the manifest commit succeeds, so the only intact layout is never deleted. The
// beside-output copy (<output>.digests.json) is an audit-only, best-effort artifact that
// never participates in the commit (it may sit on a different filesystem).
//
// The guarantee holds across power loss, not merely process termination: the completed
// layout (its files and directories) is fsynced to stable storage before the commit, the
// p2 root directory entry that names the new layout is fsynced before the manifest can
// reference it, and the manifest rename is itself made durable by fsyncing the manifest
// file and the p2 root directory. Only after the commit is durable are superseded/orphaned
// layouts collected, so a power loss can never persist the manifest rename ahead of the
// layout data (or the layout's own directory entry) it references. If the commit rename
// succeeds but its durability fsync then fails, the medium is treated as committed (the
// manifest already points at the complete new layout, so the layout is kept, never
// deleted) and the fsync error is surfaced so the caller knows durability was not
// confirmed.
//
// # Concurrent builds
//
// Builds against the same p2 root are serialized by an advisory file lock, so two builds
// can never interleave their mirror/commit/GC steps and GC a layout the other build just
// made live. As defense in depth, GC re-reads the committed manifest to learn which layout
// is live rather than trusting the caller's just-written name, and deletes nothing if no
// committed manifest can be read.
//
// # Wiring
//
// Build currently produces only the placeholder partition image; the end-to-end assembly
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
	"syscall"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage/pkg/configfile"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/localimages"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

const (
	// ociLayoutDirPrefix prefixes the per-build OCI image layout directory created
	// directly under the p2 media root. Every build gets a unique directory
	// (ociLayoutDirPrefix + a random suffix) whose basename is recorded as
	// DigestManifest.OCILayoutPath, so the build never writes into the layout the live
	// manifest still references. The consumer resolves OCILayoutPath with SecureJoin
	// against the media root. Superseded and crash-orphaned layout directories (same
	// prefix) are garbage-collected, but only after the manifest commit succeeds.
	ociLayoutDirPrefix = "oci-"

	// digestManifestFileName is the on-media digest-manifest name written at the p2
	// root; the value later recorded as IBIPrepareConfig.ExpectedDigestsPath. Its atomic
	// replacement is the single commit point that switches the live layout.
	digestManifestFileName = "digests.json"

	// digestManifestSuffix is appended to the --output path for the audit-only
	// build-side copy (<output>.digests.json).
	digestManifestSuffix = ".digests.json"

	// digestManifestVersion includes the source references required by disconnected
	// prepare and the per-image signatures staged beside the layout.
	digestManifestVersion = localimages.SignaturesManifestVersion
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
	// StagedBytes is the total this stage writes to the p2 media root: the OCI image
	// layout plus the on-media digest manifest (digests.json). It is the image-stage
	// subtotal of the p2 content; it does not include content staged by the later
	// media-assembly stage (CNF-27171) or the final filesystem/ISO overhead.
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

// mirrorResult is the outcome of mirroring one image into the OCI layout: the pinned
// top-level index digest and the signatures captured from the source and staged beside
// the layout (empty for an unsigned image). The digest manifest records both.
type mirrorResult struct {
	// digest is the pinned, content-addressed top-level index.json descriptor digest.
	digest string
	// signatures is the per-instance signature metadata staged for this image.
	signatures []localimages.InstanceSignatures
}

// imageMirrorer copies a single image reference into an OCI image layout under
// ociTag, preserving the full multi-arch index, and returns the digest of the
// top-level index.json descriptor that now carries the ociTag annotation together with
// the signatures captured from the source (the OCI layout cannot hold them, so they are
// staged beside it). A reference that cannot be reached/resolved fails the copy (and
// therefore the build); the returned digest is the pinned, content-addressed reference.
type imageMirrorer interface {
	MirrorToLayout(ctx context.Context, imageRef, destLayoutDir, ociTag, authFile string) (mirrorResult, error)
}

// imageBuildDeps bundles the injectable seams.
type imageBuildDeps struct {
	seedReader seedClosureReader
	mirrorer   imageMirrorer
}

// defaultImageBuildDeps returns the production seams backed by containers/image and
// the host ops helpers.
func defaultImageBuildDeps(log *logrus.Logger) imageBuildDeps {
	return imageBuildDeps{
		seedReader: &opsSeedClosureReader{
			ops: ops.NewOps(log, ops.NewRegularExecutor(log, false)),
			log: log,
		},
		mirrorer: &containersImageMirrorer{log: log},
	}
}

// BuildImageLayout resolves the manifest's image set, mirrors it into a fresh,
// build-unique OCI image layout directory under p2Root, and commits the on-media
// digest manifest (digests.json at the p2 root). The on-media manifest is the single
// commit point: it is replaced with one atomic rename, and because the consumer
// locates the layout through the manifest's OCILayoutPath, that rename is what
// switches the media from the previous layout to the new one. A crash at any point
// leaves the committed manifest pointing at a complete layout (never a mismatch); the
// only side effect is a possible orphaned layout directory, which this and later
// builds garbage-collect after the commit. The beside-output copy
// (<output>.digests.json) is written audit-only and best-effort after the commit. It
// is the entry point the media-assembly story (CNF-27171) wires in.
func BuildImageLayout(ctx context.Context, log *logrus.Logger, m *Manifest, opts ImageSetOptions, outputPath, p2Root, p3Root string) (*LayoutBuildResult, error) {
	return buildImageLayout(ctx, log, m, opts, outputPath, p2Root, p3Root, defaultImageBuildDeps(log))
}

// buildImageLayout is the dependency-injected core of BuildImageLayout.
//
// For self-contained media it first renders the p3 config tree under p3Root, fails fast on
// any network-reaching mirror resource in it (ScanMirrorResources), unions the rendered
// manifests' image references into the resolved set, and -- after mirroring -- pins every
// tag-form reference in the rendered manifests to the digest actually shipped. install-only
// renders no p3 tree, so p3Root is unused for that profile.
func buildImageLayout(ctx context.Context, log *logrus.Logger, m *Manifest, opts ImageSetOptions, outputPath, p2Root, p3Root string, deps imageBuildDeps) (result *LayoutBuildResult, err error) {
	var p3Result *P3RenderResult
	if m.Mode == ModeSelfContained {
		// Render the p3 tree first so the image scan and mirror-resource scan operate on the
		// exact files the media ships.
		p3Result, err = RenderP3Tree(m, p3Root)
		if err != nil {
			return nil, fmt.Errorf("rendering p3 config tree: %w", err)
		}
		defer func() {
			if err != nil {
				if rmErr := os.RemoveAll(p3Root); rmErr != nil {
					err = errors.Join(err, fmt.Errorf("cleaning up p3 config tree %q: %w", p3Root, rmErr))
				}
			}
		}()
		// Reject network-reaching mirror resources before the expensive seed pull/mirror so
		// the build fails fast (proposal §6.3).
		if err = ScanMirrorResources(p3Result.ManifestPaths); err != nil {
			return nil, fmt.Errorf("validating p3 mirror resources: %w", err)
		}
	}

	refs, err := resolveImageSet(m, opts, p3ManifestPaths(p3Result), deps)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(p2Root, 0o750); err != nil {
		return nil, fmt.Errorf("creating p2 root %q: %w", p2Root, err)
	}

	// Serialize builds against this p2 root so two concurrent builds cannot interleave
	// their mirror/commit/GC steps -- without this, one build could commit and pause while
	// another commits, and the first build's GC would then delete the second build's
	// now-live layout.
	unlock, err := lockP2Root(p2Root)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Mirror into a fresh, build-unique layout directory directly under p2Root. It is
	// never the directory the live manifest references, so the build cannot corrupt the
	// currently-committed layout. Until the manifest commit below succeeds this new
	// directory is an orphan; if the build fails before committing it is removed here,
	// leaving the previous committed manifest and its layout untouched.
	newLayout, err := os.MkdirTemp(p2Root, ociLayoutDirPrefix)
	if err != nil {
		return nil, fmt.Errorf("creating OCI layout directory under %q: %w", p2Root, err)
	}
	committed := false
	defer func() {
		if !committed {
			if rmErr := os.RemoveAll(newLayout); rmErr != nil && err == nil {
				err = fmt.Errorf("cleaning up OCI layout directory %q: %w", newLayout, rmErr)
			}
		}
	}()

	entries, err := mirrorToOCILayout(ctx, refs, newLayout, opts.AuthFile, deps)
	if err != nil {
		return nil, err
	}

	// Validate every entry against the completed layout before committing, so a
	// producer bug is caught here rather than at prepare time on the node.
	if err := verifyLayoutEntries(newLayout, entries); err != nil {
		return nil, err
	}

	// Self-contained: pin every tag-form image reference in the shipped p3 manifests to the
	// digest we just mirrored, so nothing has to resolve a tag->digest offline (proposal
	// §5.2). A reference that was not mirrored cannot be served offline and fails the build.
	// This runs before the p2 manifest commit, so a failure leaves nothing committed.
	if p3Result != nil {
		if err := rewriteP3ImageReferences(p3Result.ManifestPaths, refs, entries); err != nil {
			return nil, fmt.Errorf("pinning p3 manifest image references: %w", err)
		}
	}

	// Record the new layout's basename as OCILayoutPath so the committed manifest points
	// at it; marshal and validate the manifest bytes before anything is committed, so a
	// bad manifest fails the build while the committed manifest (if any) still points at
	// its previous, intact layout.
	layoutName := filepath.Base(newLayout)
	manifestData, err := marshalDigestManifest(entries, layoutName)
	if err != nil {
		return nil, err
	}
	if err := validateDigestManifestBytes(manifestData, p2Root); err != nil {
		return nil, err
	}

	size, err := computeSizeReport(newLayout, len(manifestData))
	if err != nil {
		return nil, err
	}
	logSizeReport(log, size)

	// Make the completed layout durable before committing. The commit rename below only
	// switches the live layout if the layout's data has already reached stable storage;
	// otherwise a power loss could persist the manifest rename while the new layout's
	// blobs did not, leaving the committed manifest pointing at incomplete data.
	if err = syncTree(newLayout); err != nil {
		return nil, err
	}

	// syncTree flushes the new layout and its contents, but not the p2 root directory entry
	// that names the new layout directory (created by MkdirTemp above). Persist that entry
	// before the manifest can reference it: otherwise a power loss could persist the manifest
	// rename while the p2 root's directory entry for the new layout had not reached stable
	// storage, leaving the committed manifest naming a layout directory that does not exist.
	if err = fsyncPath(p2Root); err != nil {
		return nil, err
	}

	// Commit: atomically replace the on-media digest manifest. This single rename is the
	// commit point -- before it the committed manifest points at the previous layout,
	// after it the committed manifest points at the fully-written new layout. publishFile
	// writes to a temp file and renames, so a failed commit never leaves a corrupt or
	// partially-written manifest and the previous committed manifest/layout stay intact.
	// durable=true fsyncs the manifest file and the p2 root directory so the commit itself
	// survives a power loss, not merely a process exit.
	//
	// Once the rename has succeeded (renamed), the on-media manifest already points at the
	// new layout, so the medium is committed and the new layout must not be deleted -- even
	// if the durability fsync inside publishFile then reports an error. Mark the build
	// committed on renamed, not on a nil error, so the deferred cleanup never removes the
	// layout the live manifest now references; the fsync error is still returned so the
	// caller learns durability was not confirmed.
	p2Path := filepath.Join(p2Root, digestManifestFileName)
	renamed, pubErr := publishFile(p2Path, manifestData, true)
	if renamed {
		committed = true
	}
	if pubErr != nil {
		return nil, pubErr
	}

	// The media is now consistent. Everything below is best-effort cleanup/auditing and
	// must not fail the build: a failure here cannot leave a mismatched medium.

	// Garbage-collect superseded and crash-orphaned layout directories, now that the
	// committed manifest points at the new one. gcSupersededLayouts re-reads the committed
	// manifest to decide which layout is live, so the committed layout is never removed.
	gcSupersededLayouts(log, p2Root)

	// Publish the audit-only beside-output copy. It sits beside --output (possibly on a
	// different filesystem) and is never part of the commit; it is best-effort (durable=false)
	// and a failure is logged, not fatal.
	besideOutput := outputPath + digestManifestSuffix
	if _, wErr := publishFile(besideOutput, manifestData, false); wErr != nil && log != nil {
		log.WithError(wErr).WithField("path", besideOutput).Warn("failed to write audit-only beside-output digest manifest copy")
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
//   - self-contained: the install-only base set (the node still boots the seed image
//     and runs recert/lca-cli) unioned with the images referenced by the rendered p3
//     config tree. p3ManifestPaths are the rendered manifest files (from RenderP3Tree);
//     ScanImageReferences discovers and normalizes their image references.
func resolveImageSet(m *Manifest, opts ImageSetOptions, p3ManifestPaths []string, deps imageBuildDeps) ([]string, error) {
	switch m.Mode {
	case ModeSelfContained:
		// Scan the rendered p3 manifests first so a decode error fails the build before the
		// expensive seed-image pull/mount in the base-set resolution below.
		scanned, err := ScanImageReferences(p3ManifestPaths)
		if err != nil {
			return nil, fmt.Errorf("resolving self-contained image set: %w", err)
		}
		// Self-contained is a superset of install-only: the base set (seed closure +
		// recert + lca-cli + extra precache) is still required, and the p3-scan images
		// are unioned on top of it and deduplicated.
		refs, err := resolveInstallOnlyImageSet(m, opts, deps)
		if err != nil {
			return nil, err
		}
		for _, img := range scanned {
			if img = strings.TrimSpace(img); img != "" {
				refs = utils.AppendToListIfNotExists(refs, img)
			}
		}
		return refs, nil
	case ModeInstallOnly:
		return resolveInstallOnlyImageSet(m, opts, deps)
	default:
		return nil, fmt.Errorf("cannot resolve image set for unsupported mode %q", m.Mode)
	}
}

// p3ManifestPaths returns the rendered manifest paths of a p3 render result, or nil when
// there is none (install-only).
func p3ManifestPaths(r *P3RenderResult) []string {
	if r == nil {
		return nil
	}
	return r.ManifestPaths
}

// rewriteP3ImageReferences pins the tag-form image references in the rendered p3 manifests
// to the digests mirrored for this build. mirrorToOCILayout preserves order, so refs[i] and
// entries[i] correspond; the normalized references the scan unioned into refs are the keys
// the rewriter looks up (createusb_p3_content.go RewriteImageReferences). Because the map is built
// only from mirrored entries, every digest a reference is pinned to is one the media ships,
// which is the offline-pull target the proposal requires.
func rewriteP3ImageReferences(manifestPaths, refs []string, entries []localimages.ImageEntry) error {
	refToDigest := make(map[string]string, len(refs))
	for i, ref := range refs {
		if i < len(entries) {
			refToDigest[ref] = entries[i].Digest
		}
	}
	return RewriteImageReferences(manifestPaths, refToDigest)
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

	// containers.list describes images used by the seed, but does not necessarily
	// include the seed image itself. The offline installer needs both.
	refs := []string{m.SeedImage}
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
		res, err := deps.mirrorer.MirrorToLayout(ctx, ref, stagingLayoutDir, tag, authFile)
		if err != nil {
			return nil, fmt.Errorf("failed to mirror and digest-pin image %q: %w", ref, err)
		}
		entries = append(entries, localimages.ImageEntry{
			Name:       nameForRef(ref),
			Digest:     res.digest,
			OCITag:     tag,
			Sources:    []string{ref},
			Signatures: res.signatures,
		})
	}
	return entries, nil
}

// marshalDigestManifest marshals the digest manifest for the given entries, recording
// layoutName as the OCILayoutPath the consumer resolves to locate the layout. Together
// with validateDigestManifestBytes and publishFile it produces the committed on-media
// manifest (digests.json at the p2 root) and the byte-identical audit-only copy
// (<output>.digests.json) from pre-validated content.
func marshalDigestManifest(entries []localimages.ImageEntry, layoutName string) ([]byte, error) {
	manifest := localimages.DigestManifest{
		Version:       digestManifestVersion,
		OCILayoutPath: layoutName,
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
// file and a failed write never corrupts an existing one. When durable is set, the temp
// file is fsynced before the rename and the parent directory is fsynced after it, so the
// published file survives a power loss (not merely a process exit); the on-media manifest
// commit uses durable=true, while the best-effort audit copy uses durable=false.
//
// The returned renamed flag reports whether the rename actually replaced path. It is true
// once the rename has succeeded -- including when the subsequent durability fsync fails --
// because at that point the target is already published and the caller must treat it as
// live (the committed manifest already points at the new layout) and must not delete it;
// the fsync error is still returned so the caller learns durability was not confirmed.
func publishFile(path string, data []byte, durable bool) (renamed bool, err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".digests-publish-")
	if err != nil {
		return false, fmt.Errorf("creating temp file for %q: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer func() {
		// Only the pre-rename temp file is ours to remove; once the rename succeeds the
		// temp name no longer exists and the published file must be left in place.
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("writing %q: %w", path, err)
	}
	if durable {
		if err = tmp.Sync(); err != nil {
			_ = tmp.Close()
			return false, fmt.Errorf("syncing temp file for %q: %w", path, err)
		}
	}
	if err = tmp.Close(); err != nil {
		return false, fmt.Errorf("closing temp file for %q: %w", path, err)
	}
	if err = os.Chmod(tmpPath, 0o600); err != nil {
		return false, fmt.Errorf("setting permissions on %q: %w", path, err)
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return false, fmt.Errorf("publishing %q: %w", path, err)
	}
	renamed = true
	if durable {
		// fsync the parent directory so the rename (the commit) itself is durable. The
		// rename has already succeeded, so a failure here leaves renamed=true: the target is
		// published and must not be deleted, but the error is surfaced so the caller knows
		// durability was not confirmed.
		if err = fsyncPath(dir); err != nil {
			return true, err
		}
	}
	return true, nil
}

// fsyncPath opens path and fsyncs it, flushing a file's contents or a directory's
// entries to stable storage. It underpins the power-loss guarantee: it is used to make
// the completed layout and the manifest commit (file + parent directory) durable.
func fsyncPath(path string) error {
	f, err := os.Open(path) //nolint:gosec // path is a build-time staging file/dir the tool created
	if err != nil {
		return fmt.Errorf("opening %q to sync: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing %q: %w", path, err)
	}
	return nil
}

// syncTree fsyncs every regular file and directory under root (and root itself) so the
// completed OCI layout is durable on stable storage before the manifest commit. Files are
// synced first, then directories deepest-first, so a directory entry is only flushed once
// the data it references is itself durable. This is what extends the crash-safety
// guarantee from process termination to power loss.
func syncTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		if d.Type().IsRegular() {
			return fsyncPath(path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("syncing layout tree %q: %w", root, err)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := fsyncPath(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

// buildLockFileName is the advisory lock file created under the p2 root to serialize
// concurrent create-usb builds against the same media root.
const buildLockFileName = ".create-usb-build.lock"

// lockP2Root takes an exclusive advisory lock (flock) on a lock file under p2Root so that
// concurrent builds against the same media root are serialized end-to-end (mirror, commit,
// and GC). Without it, build A could commit and pause, build B commit a new layout, and
// build A's GC then delete build B's now-live layout. The returned function releases the
// lock and closes the lock file; call it (deferred) when the build finishes.
func lockP2Root(p2Root string) (func(), error) {
	lockPath := filepath.Join(p2Root, buildLockFileName)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // lockPath is under the build-time p2 root the tool created
	if err != nil {
		return nil, fmt.Errorf("opening build lock %q: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquiring build lock %q: %w", lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// gcSupersededLayouts removes every build-unique OCI layout directory under p2Root
// except the one the committed manifest references. It reclaims the previous build's
// layout and any directories orphaned by an earlier crash between the layout write and
// the manifest commit. It must be called only after the manifest commit has succeeded.
//
// The layout to keep is read from the committed manifest on disk (not passed in by the
// caller) so that, even if build serialization were ever bypassed, GC can only ever
// delete layouts that no committed manifest points at: if the committed manifest cannot
// be read, GC deletes nothing. Removal failures are best-effort: they waste space but
// cannot leave a mismatched medium, so they are logged rather than returned.
func gcSupersededLayouts(log *logrus.Logger, p2Root string) {
	manifestPath := filepath.Join(p2Root, digestManifestFileName)
	m, err := localimages.ParseDigestManifest(manifestPath)
	if err != nil {
		if log != nil {
			log.WithError(err).WithField("path", manifestPath).Warn("failed to read committed manifest for GC; keeping all layout directories")
		}
		return
	}
	keepName := m.OCILayoutPath
	if keepName == "" {
		if log != nil {
			log.WithField("path", manifestPath).Warn("committed manifest has no OCI layout path; keeping all layout directories")
		}
		return
	}

	ents, err := os.ReadDir(p2Root)
	if err != nil {
		if log != nil {
			log.WithError(err).WithField("p2Root", p2Root).Warn("failed to scan for superseded OCI layout directories")
		}
		return
	}
	for _, ent := range ents {
		name := ent.Name()
		if name == keepName || !ent.IsDir() || !strings.HasPrefix(name, ociLayoutDirPrefix) {
			continue
		}
		path := filepath.Join(p2Root, name)
		if rmErr := os.RemoveAll(path); rmErr != nil && log != nil {
			log.WithError(rmErr).WithField("path", path).Warn("failed to remove superseded OCI layout directory")
		}
	}
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
		// Read back every staged signature and confirm it matches its recorded size and
		// sha256, so a producer bug is caught here rather than at prepare time on the node.
		if err := localimages.VerifyStagedSignatures(layoutDir, e); err != nil {
			return fmt.Errorf("verifying staged signatures for image %q (%s): %w", e.Name, e.Digest, err)
		}
	}
	return nil
}

// computeSizeReport returns the OCI-layout subtotal and this stage's p2 image-stage
// subtotal: the layout plus the on-media digest manifest (manifestBytes is the length
// of the digests.json written at the p2 root). Measuring the layout plus the known
// manifest size is exact and avoids counting transient staging/backup artifacts; the
// full-media total including later-staged content is CNF-27171's responsibility.
func computeSizeReport(ociLayoutDir string, manifestBytes int) (SizeReport, error) {
	ociBytes, err := dirSize(ociLayoutDir)
	if err != nil {
		return SizeReport{}, fmt.Errorf("sizing OCI layout: %w", err)
	}
	return SizeReport{OCILayoutBytes: ociBytes, StagedBytes: ociBytes + int64(manifestBytes)}, nil
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
// preserving the full multi-arch index (copy.CopyAllImages), then returns the digest of
// the top-level index.json descriptor written under ociTag together with the signatures
// captured from the source. An OCI layout cannot store signatures, so they are stripped
// from the copy (copy.Options.RemoveSignatures) and captured out-of-band here, keyed by
// manifest instance digest, for re-attachment on the node (lca-cli/localimages). The
// copy itself runs under the build host's configured trust policy (newTrustPolicyContext),
// so an unsigned or untrusted source is accepted only when that policy permits it. It is
// shared by the production docker-source mirrorer and the integration test (which supplies
// an OCI-layout source), so the real copy + digest-capture path is exercised in CI.
func copyIntoLayout(ctx context.Context, log *logrus.Logger, srcRef types.ImageReference, destLayoutDir, ociTag, authFile string) (mirrorResult, error) {
	if err := os.MkdirAll(destLayoutDir, 0o750); err != nil {
		return mirrorResult{}, fmt.Errorf("creating OCI layout directory %q: %w", destLayoutDir, err)
	}

	destRef, err := layout.NewReference(destLayoutDir, ociTag)
	if err != nil {
		return mirrorResult{}, fmt.Errorf("building OCI layout destination for tag %q: %w", ociTag, err)
	}

	sysCtx := &types.SystemContext{}
	if authFile != "" {
		sysCtx.AuthFilePath = authFile
	}

	policyCtx, err := newTrustPolicyContext(log, sysCtx)
	if err != nil {
		return mirrorResult{}, err
	}
	defer func() { _ = policyCtx.Destroy() }()

	// An OCI layout destination cannot store signatures, so strip them from the copy
	// (otherwise copy.Image fails on a signed source). The signatures are captured from
	// the source below and staged beside the layout for re-attachment on the node.
	if _, err := copy.Image(ctx, policyCtx, destRef, srcRef, &copy.Options{
		SourceCtx:          sysCtx,
		DestinationCtx:     sysCtx,
		ImageListSelection: copy.CopyAllImages,
		RemoveSignatures:   true,
	}); err != nil {
		return mirrorResult{}, fmt.Errorf("copying image into OCI layout under tag %q: %w", ociTag, err)
	}

	copiedDigest, err := layoutDigestForTag(destLayoutDir, ociTag)
	if err != nil {
		return mirrorResult{}, err
	}
	if canonical, ok := srcRef.DockerReference().(reference.Canonical); ok && canonical.Digest().String() != copiedDigest {
		if err := preservePinnedDockerManifest(ctx, srcRef, destLayoutDir, ociTag, authFile, canonical.Digest()); err != nil {
			return mirrorResult{}, err
		}
		copiedDigest = canonical.Digest().String()
	}
	if err := verifyPinnedSourceDigest(srcRef, copiedDigest); err != nil {
		return mirrorResult{}, err
	}

	// Capture the signatures the source serves (keyed by the same digest the consumer
	// imports by) and stage them beside the layout.
	signatures, err := captureSourceSignatures(ctx, srcRef, destLayoutDir, copiedDigest, sysCtx)
	if err != nil {
		return mirrorResult{}, fmt.Errorf("capturing signatures under tag %q: %w", ociTag, err)
	}

	return mirrorResult{digest: copiedDigest, signatures: signatures}, nil
}

// captureSourceSignatures opens the source image and stages its signatures beside the
// layout, keyed by copiedDigest (the pinned top-level digest the consumer imports by).
func captureSourceSignatures(ctx context.Context, srcRef types.ImageReference, destLayoutDir, copiedDigest string, sysCtx *types.SystemContext) ([]localimages.InstanceSignatures, error) {
	topDigest, err := digest.Parse(copiedDigest)
	if err != nil {
		return nil, fmt.Errorf("parsing copied digest %q: %w", copiedDigest, err)
	}
	imgSrc, err := srcRef.NewImageSource(ctx, sysCtx)
	if err != nil {
		return nil, fmt.Errorf("opening source image to capture signatures: %w", err)
	}
	defer func() { _ = imgSrc.Close() }()
	return captureAndStageSignatures(ctx, imgSrc, destLayoutDir, topDigest)
}

// verifyPinnedSourceDigest ensures a digest-pinned source remains addressable by
// its original digest after copying into the layout.
func verifyPinnedSourceDigest(srcRef types.ImageReference, copiedDigest string) error {
	canonical, ok := srcRef.DockerReference().(reference.Canonical)
	if ok && canonical.Digest().String() != copiedDigest {
		return fmt.Errorf("copied digest %s differs from pinned source digest %s for %s",
			copiedDigest, canonical.Digest(), srcRef.StringWithinTransport())
	}
	return nil
}

// newTrustPolicyContext builds the signature policy context that governs which source
// images may be mirrored. It uses the build host's configured trust policy
// (signature.DefaultPolicy, honoring sysCtx.SignaturePolicyPath, $CONTAINERS_POLICY_JSON,
// or /etc/containers/policy.json) rather than a hardcoded accept-anything policy, so a
// host configured to require signatures rejects an unsigned or untrusted source, and an
// unsigned image is mirrored only when the host policy permits it (the developer-of-record
// directive's explicit trust check).
//
// If the host has no trust policy configured at all, DefaultPolicy reports the file as
// absent; rather than fail every build on an unconfigured host, the mirror falls back to
// accepting any source but logs the absence loudly, so the operator knows no publisher
// trust is being enforced. A malformed or otherwise unreadable policy is a hard error.
func newTrustPolicyContext(log *logrus.Logger, sysCtx *types.SystemContext) (*signature.PolicyContext, error) {
	policy, err := signature.DefaultPolicy(sysCtx)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, configfile.ErrConfigFileNotFound) {
			return nil, fmt.Errorf("loading signature trust policy: %w", err)
		}
		if log != nil {
			log.Warn("no container signature trust policy is configured on the build host " +
				"(set $CONTAINERS_POLICY_JSON or /etc/containers/policy.json); mirroring will " +
				"accept any source without verifying publisher trust")
		}
		policy = &signature.Policy{
			Default: []signature.PolicyRequirement{signature.NewPRInsecureAcceptAnything()},
		}
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
type containersImageMirrorer struct {
	log *logrus.Logger
}

// MirrorToLayout parses imageRef as a docker registry reference and copies it into
// the layout.
func (m *containersImageMirrorer) MirrorToLayout(ctx context.Context, imageRef, destLayoutDir, ociTag, authFile string) (mirrorResult, error) {
	srcRef, err := docker.ParseReference("//" + imageRef)
	if err != nil {
		return mirrorResult{}, fmt.Errorf("parsing source image reference %q: %w", imageRef, err)
	}
	return copyIntoLayout(ctx, m.log, srcRef, destLayoutDir, ociTag, authFile)
}

// withMountedImage pulls image with the host podman, mounts it, and invokes fn with the
// mountpoint, unmounting it after use. The pulled image may also back a running
// container, so removing it here would make an otherwise successful build fail.
// The cleanup error is surfaced only when fn succeeded.
//
// It factors the shared podman pull → mount → defer unmount lifecycle used by both
// opsSeedClosureReader (here) and opsLCABinaryExtractor (createusb_p1_content.go); each
// caller supplies the image-specific read via fn.
func withMountedImage(o ops.Ops, image, authFile string, fn func(mountpoint string) error) (err error) {
	pullArgs := []string{"pull"}
	if authFile != "" {
		pullArgs = append(pullArgs, "--authfile", authFile)
	}
	pullArgs = append(pullArgs, image)
	if _, err := o.RunInHostNamespace("podman", pullArgs...); err != nil {
		return fmt.Errorf("pulling image %q: %w", image, err)
	}
	mountpoint, err := o.MountImage(image)
	if err != nil {
		return fmt.Errorf("mounting image %q: %w", image, err)
	}
	defer func() {
		if _, umErr := o.RunInHostNamespace("podman", "image", "umount", image); umErr != nil && err == nil {
			err = fmt.Errorf("unmounting image %q: %w", image, umErr)
		}
	}()
	return fn(mountpoint)
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
	err = withMountedImage(r.ops, seedImage, authFile, func(mountpoint string) error {
		data, err := r.ops.ReadFile(filepath.Join(mountpoint, common.ContainersListFileName))
		if err != nil {
			return fmt.Errorf("reading %s from seed image: %w", common.ContainersListFileName, err)
		}
		list = parseContainersList(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return list, nil
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
