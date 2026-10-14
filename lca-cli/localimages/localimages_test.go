package localimages

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	istorage "github.com/containers/image/v5/storage"
	cstorage "github.com/containers/storage"
	"github.com/containers/storage/pkg/reexec"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	t.Run("valid", func(t *testing.T) {
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
		"unsupported version": {Version: 2, OCILayoutPath: "oci", Images: []ImageEntry{{Name: testImageName, Digest: goodDigest, OCITag: testOCITag}}},
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

	t.Run("digest match with no tag annotation", func(t *testing.T) {
		idx := &imgspecv1.Index{Manifests: []imgspecv1.Descriptor{desc(digA, ""), desc(digB, "")}}
		pos, err := selectManifestIndex(idx, ImageEntry{Name: testImageName, Digest: digA, OCITag: testOCITag})
		require.NoError(t, err)
		assert.Equal(t, 0, pos)
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

func TestImportImage_MultiArchIndex(t *testing.T) {
	requireCopyCapable(t)
	im, graphRoot := newImporter(t)
	mediaRoot, m := buildLayout(t, []layoutImage{{tag: testOCITag, name: testImageName, arches: []string{"amd64", "arm64"}}})

	require.NoError(t, im.ImportImage(context.Background(), mediaRoot, m, m.Images[0]))

	// The multi-arch index digest round-trips and is stored under name@digest.
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
