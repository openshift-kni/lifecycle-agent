package localimages

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	istorage "go.podman.io/image/v5/storage"
	cstorage "go.podman.io/storage"
	"go.podman.io/storage/pkg/reexec"
)

const (
	testImageName = "quay.io/openshift-release-dev/ocp-release"
	testOCITag    = "ocp-release"
)

func TestMain(m *testing.M) {
	// containers/storage may re-exec the test binary as a helper process.
	if reexec.Init() {
		return
	}
	os.Exit(m.Run())
}

// ----- ParseDigestManifest -----

func TestParseDigestManifest(t *testing.T) {
	const goodDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

	t.Run("valid v1", func(t *testing.T) {
		path := writeJSONFile(t, DigestManifest{
			Version:       1,
			OCILayoutPath: "oci",
			Images: []ImageEntry{
				{Name: testImageName, Digest: goodDigest, OCITag: testOCITag},
			},
		})

		m, err := ParseDigestManifest(path)
		require.NoError(t, err)
		require.NotNil(t, m)
		assert.Equal(t, 1, m.Version)
		assert.Equal(t, "oci", m.OCILayoutPath)
		require.Len(t, m.Images, 1)
		assert.Equal(t, testImageName, m.Images[0].Name)
		assert.Equal(t, goodDigest, m.Images[0].Digest)
		assert.Equal(t, testOCITag, m.Images[0].OCITag)
	})

	t.Run("valid v2 with sources", func(t *testing.T) {
		path := writeJSONFile(t, DigestManifest{
			Version:       SourcesManifestVersion,
			OCILayoutPath: "oci",
			Images: []ImageEntry{
				{Name: testImageName, Digest: goodDigest, OCITag: testOCITag, Sources: []string{testImageName + ":4.15.0"}},
			},
		})

		m, err := ParseDigestManifest(path)
		require.NoError(t, err)
		require.NotNil(t, m)
		assert.Equal(t, SourcesManifestVersion, m.Version)
		require.Len(t, m.Images, 1)
		assert.Equal(t, []string{testImageName + ":4.15.0"}, m.Images[0].Sources)
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := ParseDigestManifest(filepath.Join(t.TempDir(), "does-not-exist.json"))
		assert.Error(t, err)
	})

	t.Run("malformed JSON", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.json")
		require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))
		_, err := ParseDigestManifest(path)
		assert.Error(t, err)
	})

	invalid := map[string]DigestManifest{
		"unsupported version": {Version: 4, OCILayoutPath: "oci", Images: []ImageEntry{{Name: testImageName, Digest: goodDigest, OCITag: testOCITag}}},
		"missing version":     {OCILayoutPath: "oci", Images: []ImageEntry{{Name: testImageName, Digest: goodDigest, OCITag: testOCITag}}},
		"missing layout path": {Version: 1, Images: []ImageEntry{{Name: testImageName, Digest: goodDigest, OCITag: testOCITag}}},
		"empty images":        {Version: 1, OCILayoutPath: "oci"},
		"missing name":        {Version: 1, OCILayoutPath: "oci", Images: []ImageEntry{{Digest: goodDigest, OCITag: testOCITag}}},
		"missing digest":      {Version: 1, OCILayoutPath: "oci", Images: []ImageEntry{{Name: testImageName, OCITag: testOCITag}}},
		"missing ociTag":      {Version: 1, OCILayoutPath: "oci", Images: []ImageEntry{{Name: testImageName, Digest: goodDigest}}},
		"invalid digest":      {Version: 1, OCILayoutPath: "oci", Images: []ImageEntry{{Name: testImageName, Digest: "not-a-digest", OCITag: testOCITag}}},
	}
	for name, m := range invalid {
		t.Run(name, func(t *testing.T) {
			_, err := ParseDigestManifest(writeJSONFile(t, m))
			assert.Error(t, err)
		})
	}
}

// ----- selectManifestIndex -----

func TestSelectManifestIndex(t *testing.T) {
	const (
		digA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		digB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		digC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	)
	desc := func(d, tag string) imgspecv1.Descriptor {
		out := imgspecv1.Descriptor{Digest: digest.Digest(d)}
		if tag != "" {
			out.Annotations = map[string]string{imgspecv1.AnnotationRefName: tag}
		}
		return out
	}

	t.Run("exact digest and tag match", func(t *testing.T) {
		idx := &imgspecv1.Index{Manifests: []imgspecv1.Descriptor{desc(digA, "other"), desc(digB, testOCITag)}}
		pos, err := selectManifestIndex(idx, ImageEntry{Name: testImageName, Digest: digB, OCITag: testOCITag})
		require.NoError(t, err)
		assert.Equal(t, 1, pos)
	})

	t.Run("digest present but untagged is rejected", func(t *testing.T) {
		// The requested tag is mandatory: a descriptor carrying the digest but
		// no org.opencontainers.image.ref.name annotation must not be selected,
		// so an image is never imported for a tag it does not identify.
		idx := &imgspecv1.Index{Manifests: []imgspecv1.Descriptor{desc(digA, ""), desc(digB, "")}}
		_, err := selectManifestIndex(idx, ImageEntry{Name: testImageName, Digest: digA, OCITag: testOCITag})
		assert.ErrorIs(t, err, ErrAmbiguousSelection)
	})

	t.Run("multi-arch index descriptor selected", func(t *testing.T) {
		d := desc(digC, testOCITag)
		d.MediaType = imgspecv1.MediaTypeImageIndex
		idx := &imgspecv1.Index{Manifests: []imgspecv1.Descriptor{desc(digA, "other"), d}}
		pos, err := selectManifestIndex(idx, ImageEntry{Name: testImageName, Digest: digC, OCITag: testOCITag})
		require.NoError(t, err)
		assert.Equal(t, 1, pos)
	})

	t.Run("digest absent", func(t *testing.T) {
		idx := &imgspecv1.Index{Manifests: []imgspecv1.Descriptor{desc(digA, testOCITag)}}
		_, err := selectManifestIndex(idx, ImageEntry{Name: testImageName, Digest: digB, OCITag: testOCITag})
		assert.ErrorIs(t, err, ErrDigestNotFound)
	})

	t.Run("digest present under a different tag is ambiguous", func(t *testing.T) {
		idx := &imgspecv1.Index{Manifests: []imgspecv1.Descriptor{desc(digA, "a-different-tag")}}
		_, err := selectManifestIndex(idx, ImageEntry{Name: testImageName, Digest: digA, OCITag: testOCITag})
		assert.ErrorIs(t, err, ErrAmbiguousSelection)
	})

	t.Run("more than one match is ambiguous", func(t *testing.T) {
		idx := &imgspecv1.Index{Manifests: []imgspecv1.Descriptor{desc(digA, testOCITag), desc(digA, testOCITag)}}
		_, err := selectManifestIndex(idx, ImageEntry{Name: testImageName, Digest: digA, OCITag: testOCITag})
		assert.ErrorIs(t, err, ErrAmbiguousSelection)
	})
}

// ----- store-backed import tests -----

func TestImportImage_StoresCanonicalByDigest(t *testing.T) {
	requireCopyCapable(t)
	im, graphRoot := newImporter(t)
	mediaRoot, m := buildLayout(t, []layoutImage{{tag: testOCITag, name: testImageName, arches: []string{"amd64"}}})

	require.NoError(t, im.ImportImage(context.Background(), mediaRoot, m, m.Images[0]))

	img := resolveStored(t, graphRoot, m.Images[0])
	assertNameAndDigest(t, img, m.Images[0])
}

// TestImportImage_RegistersSourceAliases proves a v2 entry's tagged sources are
// registered as local aliases of the imported digest, so a consumer that asks for
// name:tag (the reference the seed actually requests) resolves the image locally.
// Digest-pinned sources need no alias (they already resolve by canonical ref).
func TestImportImage_RegistersSourceAliases(t *testing.T) {
	requireCopyCapable(t)
	im, graphRoot := newImporter(t)
	mediaRoot, m := buildLayout(t, []layoutImage{{tag: testOCITag, name: testImageName, arches: []string{"amd64"}}})

	tagRef := testImageName + ":4.15.0"
	m.Images[0].Sources = []string{tagRef, testImageName + "@" + m.Images[0].Digest}

	require.NoError(t, im.ImportImage(context.Background(), mediaRoot, m, m.Images[0]))

	// Resolves under its canonical digest, and the tagged source is a stored name.
	img := resolveStored(t, graphRoot, m.Images[0])
	assertNameAndDigest(t, img, m.Images[0])
	assert.Contains(t, img.Names, tagRef, "tagged source must be registered as a local alias")

	// The alias resolves through the storage transport -- the same lookup path
	// podman uses -- to the imported image.
	verifier, err := NewImporter(logrus.New(), StoreConfig{
		GraphDriverName: "vfs",
		GraphRoot:       graphRoot,
		RunRoot:         filepath.Join(t.TempDir(), "run-alias"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = verifier.Close() })
	ref, err := istorage.Transport.ParseStoreReference(verifier.store, tagRef)
	require.NoError(t, err)
	_, aliasImg, err := istorage.ResolveReference(ref)
	require.NoError(t, err, "tagged alias %s must resolve locally", tagRef)
	assert.Equal(t, img.ID, aliasImg.ID, "the alias must resolve to the imported image")
}

func TestImportImage_MultiArchIndex(t *testing.T) {
	requireCopyCapable(t)
	im, graphRoot := newImporter(t)

	// A genuine multi-arch index that includes the host platform. containers-storage
	// stores single-platform images, so ImportImage materialises only the instance
	// matching the host architecture; the index (list) digest is still recorded, so
	// the canonical name@<index-digest> reference resolves and round-trips.
	otherArch := "arm64"
	if runtime.GOARCH == "arm64" {
		otherArch = "amd64"
	}
	mediaRoot, m := buildLayout(t, []layoutImage{{tag: testOCITag, name: testImageName, arches: []string{runtime.GOARCH, otherArch}}})

	require.NoError(t, im.ImportImage(context.Background(), mediaRoot, m, m.Images[0]))

	// The multi-arch index digest round-trips: the stored host-platform image
	// resolves under the canonical name@<index-digest>.
	img := resolveStored(t, graphRoot, m.Images[0])
	assertNameAndDigest(t, img, m.Images[0])
}

func TestImportImage_ExplicitStore(t *testing.T) {
	requireCopyCapable(t)
	im, graphRoot := newImporter(t)
	mediaRoot, m := buildLayout(t, []layoutImage{{tag: testOCITag, name: testImageName, arches: []string{"amd64"}}})

	require.NoError(t, im.ImportImage(context.Background(), mediaRoot, m, m.Images[0]))

	// Present in the explicitly-targeted store.
	resolveStored(t, graphRoot, m.Images[0])

	// Absent from a different store: the import targeted the explicit graphroot,
	// not a shared/live one.
	other, err := NewImporter(logrus.New(), StoreConfig{
		GraphDriverName: "vfs",
		GraphRoot:       filepath.Join(t.TempDir(), "other-graph"),
		RunRoot:         filepath.Join(t.TempDir(), "other-run"),
	})
	require.NoError(t, err)
	defer other.Close()
	ref, err := istorage.Transport.ParseStoreReference(other.store, m.Images[0].Name+"@"+m.Images[0].Digest)
	require.NoError(t, err)
	_, _, err = istorage.ResolveReference(ref)
	assert.Error(t, err, "image must not be present in a different store")
}

func TestImportImage_DigestAbsentFromLayout(t *testing.T) {
	im, _ := newImporter(t)
	mediaRoot, m := buildLayout(t, []layoutImage{{tag: testOCITag, name: testImageName, arches: []string{"amd64"}}})

	absent := ImageEntry{
		Name:   testImageName,
		Digest: "sha256:" + strings.Repeat("d", 64),
		OCITag: testOCITag,
	}
	err := im.ImportImage(context.Background(), mediaRoot, m, absent)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDigestNotFound)

	// No copy was attempted: the store is empty.
	imgs, err := im.store.Images()
	require.NoError(t, err)
	assert.Empty(t, imgs, "no image should have been imported for an absent digest")
}

// TestImportImage_LayoutPathStaysInMediaRoot verifies that an ociLayoutPath
// which traverses out of the media root cannot select a layout outside it. A
// complete, valid layout is placed outside the media root; the import must not
// read it. Runs in all environments (the escaping read fails before any copy).
func TestImportImage_LayoutPathStaysInMediaRoot(t *testing.T) {
	im, _ := newImporter(t)

	// A valid layout that lives OUTSIDE the media root. Without containment, a
	// "../" ociLayoutPath could reach it.
	outsideRoot, outside := buildLayout(t, []layoutImage{{tag: testOCITag, name: testImageName, arches: []string{"amd64"}}})

	// Media root with no layout of its own. Point ociLayoutPath at the outside
	// layout via a traversal sequence; containment must prevent the escape.
	mediaRoot := t.TempDir()
	rel, err := filepath.Rel(mediaRoot, filepath.Join(outsideRoot, outside.OCILayoutPath))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(rel, ".."), "test setup: path must traverse out of the media root")

	m := &DigestManifest{Version: 1, OCILayoutPath: rel, Images: outside.Images}
	err = im.ImportImage(context.Background(), mediaRoot, m, m.Images[0])
	require.Error(t, err, "import must not read a layout outside the media root")

	// Nothing was imported.
	imgs, err := im.store.Images()
	require.NoError(t, err)
	assert.Empty(t, imgs, "no image should have been imported from outside the media root")
}

func TestImportAll_FailFast(t *testing.T) {
	requireCopyCapable(t)
	im, graphRoot := newImporter(t)
	mediaRoot, m := buildLayout(t, []layoutImage{{tag: testOCITag, name: testImageName, arches: []string{"amd64"}}})

	// Second entry references a digest that is absent from the layout.
	bad := ImageEntry{
		Name:   "quay.io/openshift-release-dev/ocp-v4.0-art-dev",
		Digest: "sha256:" + strings.Repeat("e", 64),
		OCITag: "art-dev",
	}
	good := m.Images[0]
	m.Images = []ImageEntry{good, bad}

	err := im.ImportAll(context.Background(), mediaRoot, m)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDigestNotFound)
	// The error names the offending image.
	assert.Contains(t, err.Error(), bad.Name)
	assert.Contains(t, err.Error(), bad.Digest)

	// Fail-fast: the first (good) image was imported before the failure.
	resolveStored(t, graphRoot, good)
}

// TestImportAll_FailFastNoCopy verifies fail-fast and the named error without a
// real copy: the first entry references an absent digest, so ImportAll fails
// immediately and the second entry is never processed. Runs in all environments.
func TestImportAll_FailFastNoCopy(t *testing.T) {
	im, _ := newImporter(t)
	mediaRoot, m := buildLayout(t, []layoutImage{{tag: testOCITag, name: testImageName, arches: []string{"amd64"}}})

	bad := ImageEntry{
		Name:   testImageName,
		Digest: "sha256:" + strings.Repeat("f", 64),
		OCITag: testOCITag,
	}
	never := ImageEntry{Name: "quay.io/never/processed", Digest: m.Images[0].Digest, OCITag: testOCITag}
	m.Images = []ImageEntry{bad, never}

	err := im.ImportAll(context.Background(), mediaRoot, m)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDigestNotFound)
	// The error names the offending (first) image.
	assert.Contains(t, err.Error(), bad.Name)
	assert.Contains(t, err.Error(), bad.Digest)

	// Fail-fast: nothing was imported (neither entry reached a copy).
	imgs, err := im.store.Images()
	require.NoError(t, err)
	assert.Empty(t, imgs, "no image should have been imported")
}

// ----- VerifyAll (store-residency gate) -----

// TestVerifyAllEmptyStore proves VerifyAll against a real (but empty) store reports
// every manifest image as missing, naming each missing name@digest and the verified
// count. It exercises the real ParseStoreReference + ResolveReference residency path
// and needs no privileged layer copy (nothing is imported).
func TestVerifyAllEmptyStore(t *testing.T) {
	im, _ := newImporter(t)
	_, m := buildLayout(t, []layoutImage{
		{tag: testOCITag, name: testImageName, arches: []string{"amd64"}},
	})

	err := im.VerifyAll(context.Background(), m)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verified 0/1")
	assert.Contains(t, err.Error(), m.Images[0].Name+"@"+m.Images[0].Digest)
}

// TestVerifyAllAllPresent imports every manifest image into a real store, then
// VerifyAll resolves them all and returns nil -- exercising the real residency path
// over a populated store, not a fake. The release entry is a multi-arch index (as
// OpenShift release content normally is), so this also covers VerifyAll resolving
// the stored host instance under the canonical name@<index-digest>.
func TestVerifyAllAllPresent(t *testing.T) {
	requireCopyCapable(t)
	im, _ := newImporter(t)

	otherArch := "arm64"
	if runtime.GOARCH == "arm64" {
		otherArch = "amd64"
	}
	mediaRoot, m := buildLayout(t, []layoutImage{
		{tag: testOCITag, name: testImageName, arches: []string{runtime.GOARCH, otherArch}},
		{tag: "workload", name: "quay.io/openshift-release-dev/ocp-v4.0-art-dev", arches: []string{"amd64"}},
	})

	require.NoError(t, im.ImportAll(context.Background(), mediaRoot, m))
	assert.NoError(t, im.VerifyAll(context.Background(), m))
}

// TestVerifyAllAfterReopen proves the production two-open pattern that
// ibi-preparation uses (importClosureToStateroot then verifyImportedClosure):
// import the closure through one importer, close it, then open a SECOND importer at
// the same graphRoot with a DIFFERENT runRoot and VerifyAll. containers-storage's
// store cache keys on (graphRoot, runRoot), so the distinct runRoot yields a fresh,
// working store rather than reusing the first (already shut-down) one.
func TestVerifyAllAfterReopen(t *testing.T) {
	requireCopyCapable(t)
	graphRoot := filepath.Join(t.TempDir(), "graph")

	importer, err := NewImporter(logrus.New(), StoreConfig{
		GraphDriverName: "vfs",
		GraphRoot:       graphRoot,
		RunRoot:         filepath.Join(t.TempDir(), "run-import"),
	})
	require.NoError(t, err)

	mediaRoot, m := buildLayout(t, []layoutImage{
		{tag: testOCITag, name: testImageName, arches: []string{"amd64"}},
	})
	require.NoError(t, importer.ImportAll(context.Background(), mediaRoot, m))
	require.NoError(t, importer.Close())

	// Second open at the same graphRoot with a distinct runRoot, as the self-check does.
	verifier, err := NewImporter(logrus.New(), StoreConfig{
		GraphDriverName: "vfs",
		GraphRoot:       graphRoot,
		RunRoot:         filepath.Join(t.TempDir(), "run-verify"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = verifier.Close() })

	assert.NoError(t, verifier.VerifyAll(context.Background(), m))
}

// TestVerifyAllMissingImage proves VerifyAll fails against the real store when a
// manifest entry was never imported: the imported entries resolve, the absent one
// does not, and the error names the missing name@digest and the verified count.
func TestVerifyAllMissingImage(t *testing.T) {
	requireCopyCapable(t)
	im, _ := newImporter(t)
	mediaRoot, m := buildLayout(t, []layoutImage{
		{tag: testOCITag, name: testImageName, arches: []string{"amd64"}},
	})

	require.NoError(t, im.ImportAll(context.Background(), mediaRoot, m))

	// Append a manifest entry that was never imported into the store.
	missing := ImageEntry{
		Name:   "quay.io/openshift-release-dev/absent",
		Digest: "sha256:" + strings.Repeat("a", 64),
		OCITag: "absent",
	}
	m.Images = append(m.Images, missing)

	err := im.VerifyAll(context.Background(), m)
	require.Error(t, err)
	assert.Contains(t, err.Error(), missing.Name+"@"+missing.Digest)
	assert.Contains(t, err.Error(), "verified 1/2")
}

// TestVerifyAllSourceReferenceUnresolved proves VerifyAll fails when a manifest
// entry's canonical name@digest is resident but a source reference the seed
// requests for it does not resolve. registerSourceAliases skips digest-pinned
// sources on the assumption they resolve by their canonical reference, which does
// not hold for a source in a repository different from Name: such a source is
// never aliased, so the reference the node requests offline is unavailable even
// though the canonical image would pass a name@digest-only check. VerifyAll must
// catch this before any terminal action.
func TestVerifyAllSourceReferenceUnresolved(t *testing.T) {
	requireCopyCapable(t)
	im, _ := newImporter(t)
	mediaRoot, m := buildLayout(t, []layoutImage{
		{tag: testOCITag, name: testImageName, arches: []string{"amd64"}},
	})

	// A digest-pinned source in a DIFFERENT repository than Name. It shares the
	// imported digest (so it would pass a metadata-only coverage check), but
	// registerSourceAliases skips it (digest-pinned) and the image is stored only
	// under testImageName -- so this reference never resolves.
	crossRepoSource := "quay.io/openshift-release-dev/other-repo@" + m.Images[0].Digest
	m.Images[0].Sources = []string{crossRepoSource}

	require.NoError(t, im.ImportAll(context.Background(), mediaRoot, m))

	err := im.VerifyAll(context.Background(), m)
	require.Error(t, err)
	assert.Contains(t, err.Error(), crossRepoSource, "the unresolved source reference must be named")
	assert.Contains(t, err.Error(), "verified 0/1")
}

// TestVerifyAllTaggedSourceResolves proves VerifyAll passes when an entry's source
// references resolve: the tagged source through the local alias ImportImage
// registered for it, and the same-repo digest-pinned source through its canonical
// reference -- the references the seed's containers.list actually requests,
// confirmed resolvable in the installed store.
func TestVerifyAllTaggedSourceResolves(t *testing.T) {
	requireCopyCapable(t)
	im, _ := newImporter(t)
	mediaRoot, m := buildLayout(t, []layoutImage{
		{tag: testOCITag, name: testImageName, arches: []string{"amd64"}},
	})

	m.Images[0].Sources = []string{
		testImageName + ":4.15.0",                // tagged: registered as a local alias
		testImageName + "@" + m.Images[0].Digest, // digest-pinned, same repo: resolves canonically
	}

	require.NoError(t, im.ImportAll(context.Background(), mediaRoot, m))
	assert.NoError(t, im.VerifyAll(context.Background(), m))
}

// ----- helpers -----

func writeJSONFile(t *testing.T, v any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.json")
	writeFile(t, path, mustJSON(t, v))
	return path
}

// requireCopyCapable skips tests that perform a real layer copy into
// containers-storage, which needs root (or a user namespace) for the layer
// chown. Opening the store itself works unprivileged, so tests that only
// exercise the pre-copy path (selection/error handling) do not call this.
func requireCopyCapable(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("importing a layer into containers-storage requires root (or a user namespace); skipping")
	}
}

// newImporter returns an Importer backed by a temporary vfs store, or skips the
// test if containers-storage cannot even be opened in this environment. Returns
// the graphRoot so the store can be re-opened to verify imported images.
func newImporter(t *testing.T) (*Importer, string) {
	t.Helper()
	graphRoot := filepath.Join(t.TempDir(), "graph")
	im, err := NewImporter(logrus.New(), StoreConfig{
		GraphDriverName: "vfs",
		GraphRoot:       graphRoot,
		RunRoot:         filepath.Join(t.TempDir(), "run"),
	})
	if err != nil {
		t.Skipf("containers-storage unavailable in this environment: %v", err)
	}
	t.Cleanup(func() { _ = im.Close() })
	return im, graphRoot
}

// resolveStored re-opens the store at graphRoot and requires the image to be
// present, returning the resolved storage image.
func resolveStored(t *testing.T, graphRoot string, e ImageEntry) *cstorage.Image {
	t.Helper()
	verifier, err := NewImporter(logrus.New(), StoreConfig{
		GraphDriverName: "vfs",
		GraphRoot:       graphRoot,
		RunRoot:         filepath.Join(t.TempDir(), "run-verify"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = verifier.Close() })

	ref, err := istorage.Transport.ParseStoreReference(verifier.store, e.Name+"@"+e.Digest)
	require.NoError(t, err)
	_, img, err := istorage.ResolveReference(ref)
	require.NoError(t, err, "image %s@%s must be present in the store", e.Name, e.Digest)
	require.NotNil(t, img)
	return img
}

func assertNameAndDigest(t *testing.T, img *cstorage.Image, e ImageEntry) {
	t.Helper()
	// Stored under the canonical digest.
	matchedDigest := img.Digest.String() == e.Digest
	for _, d := range img.Digests {
		if d.String() == e.Digest {
			matchedDigest = true
		}
	}
	assert.True(t, matchedDigest, "stored image should carry digest %s (got %s / %v)", e.Digest, img.Digest, img.Digests)

	// No hostname rewriting: stored under the original canonical name.
	matchedName := false
	for _, n := range img.Names {
		if strings.HasPrefix(n, e.Name) {
			matchedName = true
		}
	}
	assert.True(t, matchedName, "stored image names %v should include canonical name %s", img.Names, e.Name)
}

// layoutImage describes one image to synthesize into a test OCI layout.
type layoutImage struct {
	tag    string
	name   string
	arches []string // one arch => single manifest; multiple => multi-arch index
}

// buildLayout synthesizes an OCI image layout (oci-layout, blobs, index.json)
// under a temp media root and returns the matching digest manifest. Each image's
// top-level descriptor digest is recorded as the manifest entry's digest.
func buildLayout(t *testing.T, images []layoutImage) (string, *DigestManifest) {
	t.Helper()
	mediaRoot := t.TempDir()
	layoutPath := "oci"
	srcDir := filepath.Join(mediaRoot, layoutPath)
	blobsDir := filepath.Join(srcDir, "blobs")
	require.NoError(t, os.MkdirAll(blobsDir, 0o755))

	index := imgspecv1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: imgspecv1.MediaTypeImageIndex,
	}
	m := &DigestManifest{Version: 1, OCILayoutPath: layoutPath}

	for _, img := range images {
		var top imgspecv1.Descriptor
		if len(img.arches) == 1 {
			top = buildImageManifest(t, blobsDir, img.arches[0])
		} else {
			top = buildImageIndex(t, blobsDir, img.arches)
		}
		top.Annotations = map[string]string{imgspecv1.AnnotationRefName: img.tag}
		index.Manifests = append(index.Manifests, top)
		m.Images = append(m.Images, ImageEntry{Name: img.name, Digest: top.Digest.String(), OCITag: img.tag})
	}

	writeFile(t, filepath.Join(srcDir, "oci-layout"), mustJSON(t, imgspecv1.ImageLayout{Version: imgspecv1.ImageLayoutVersion}))
	writeFile(t, filepath.Join(srcDir, ociLayoutIndexFile), mustJSON(t, index))
	return mediaRoot, m
}

// buildImageManifest writes a minimal single-platform image (layer, config,
// manifest) to blobsDir and returns its manifest descriptor.
func buildImageManifest(t *testing.T, blobsDir, arch string) imgspecv1.Descriptor {
	t.Helper()

	// A tiny, per-arch-unique gzipped tar layer (uniqueness keeps digests distinct).
	uncompressed := makeTar(t, "file-"+arch+".txt", "content-"+arch)
	diffID := digest.FromBytes(uncompressed)
	compressed := gzipBytes(t, uncompressed)
	layerDigest := writeBlob(t, blobsDir, compressed)

	config := imgspecv1.Image{
		Platform: imgspecv1.Platform{Architecture: arch, OS: "linux"},
		RootFS:   imgspecv1.RootFS{Type: "layers", DiffIDs: []digest.Digest{diffID}},
	}
	configBytes := mustJSON(t, config)
	configDigest := writeBlob(t, blobsDir, configBytes)

	manifest := imgspecv1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: imgspecv1.MediaTypeImageManifest,
		Config: imgspecv1.Descriptor{
			MediaType: imgspecv1.MediaTypeImageConfig,
			Digest:    configDigest,
			Size:      int64(len(configBytes)),
		},
		Layers: []imgspecv1.Descriptor{{
			MediaType: imgspecv1.MediaTypeImageLayerGzip,
			Digest:    layerDigest,
			Size:      int64(len(compressed)),
		}},
	}
	manifestBytes := mustJSON(t, manifest)
	manifestDigest := writeBlob(t, blobsDir, manifestBytes)

	return imgspecv1.Descriptor{
		MediaType: imgspecv1.MediaTypeImageManifest,
		Digest:    manifestDigest,
		Size:      int64(len(manifestBytes)),
		Platform:  &imgspecv1.Platform{Architecture: arch, OS: "linux"},
	}
}

// buildImageIndex writes a multi-arch image index (one manifest per arch) to
// blobsDir and returns its index descriptor.
func buildImageIndex(t *testing.T, blobsDir string, arches []string) imgspecv1.Descriptor {
	t.Helper()
	index := imgspecv1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: imgspecv1.MediaTypeImageIndex,
	}
	for _, arch := range arches {
		index.Manifests = append(index.Manifests, buildImageManifest(t, blobsDir, arch))
	}
	indexBytes := mustJSON(t, index)
	indexDigest := writeBlob(t, blobsDir, indexBytes)
	return imgspecv1.Descriptor{
		MediaType: imgspecv1.MediaTypeImageIndex,
		Digest:    indexDigest,
		Size:      int64(len(indexBytes)),
	}
}

func writeBlob(t *testing.T, blobsDir string, data []byte) digest.Digest {
	t.Helper()
	d := digest.FromBytes(data)
	writeFile(t, filepath.Join(blobsDir, d.Algorithm().String(), d.Encoded()), data)
	return d
}

func makeTar(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}))
	_, err := tw.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(data)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return data
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, data, 0o644))
}
