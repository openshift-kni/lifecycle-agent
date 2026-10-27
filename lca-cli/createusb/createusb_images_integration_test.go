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

// Integration coverage for the real mirror path (copyIntoLayout): a genuine
// multi-arch OCI layout is copied with copy.CopyAllImages into a destination layout,
// and the result is checked against exactly the properties the faked-seam unit tests
// cannot establish (codex review point 6):
//
//   - copy.CopyAllImages preserves the full multi-arch index in the destination;
//   - the recorded digest is the top-level index.json descriptor digest;
//   - the destination descriptor carries a unique, valid org.opencontainers.image.ref.name;
//   - the emitted digest manifest validates under localimages.ParseDigestManifest; and
//   - the consumer (localimages) selects and imports the correct host-platform instance
//     from the mirrored layout (the import round-trip is guarded by a root check, as in
//     the localimages store-backed tests).
//
// The source layout is synthesized in-process (as the localimages tests do) rather
// than committed as a binary fixture, so no registry and no root are needed for the
// copy/selection assertions; only the final containers-storage import needs root.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage/pkg/reexec"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/localimages"
)

const (
	integrationImageName = "quay.io/openshift-release-dev/ocp-release"
	integrationOCITag    = "ocp-release-0"
	// integrationLayoutDir is a standalone OCI layout directory name for the copy/import
	// round-trip; the production build derives a build-unique name per invocation.
	integrationLayoutDir = "oci"
)

func TestMain(m *testing.M) {
	// containers/storage may re-exec the test binary as a helper process (used by the
	// root-guarded import round-trip below).
	if reexec.Init() {
		return
	}
	os.Exit(m.Run())
}

// TestMirrorAndImportRoundTrip mirrors a real multi-arch OCI layout and verifies the
// copy/selection properties end-to-end.
func TestMirrorAndImportRoundTrip(t *testing.T) {
	otherArch := "arm64"
	if runtime.GOARCH == "arm64" {
		otherArch = "amd64"
	}
	arches := []string{runtime.GOARCH, otherArch}

	// A genuine multi-arch source layout, addressable as oci:<srcDir>:src.
	srcDir := buildMultiArchSourceLayout(t, arches, "src")
	srcRef, err := layout.NewReference(srcDir, "src")
	require.NoError(t, err)

	// Mirror into a destination layout under the media root, exercising the real
	// copy.CopyAllImages + digest-capture path.
	mediaRoot := t.TempDir()
	destLayout := filepath.Join(mediaRoot, integrationLayoutDir)
	res, err := copyIntoLayout(context.Background(), logrus.New(), srcRef, destLayout, integrationOCITag, "")
	require.NoError(t, err)
	recordedDigest := res.digest
	require.NotEmpty(t, recordedDigest)

	// The destination descriptor for the tag carries the recorded digest and is the
	// top-level index.json descriptor.
	destIndex := readDestIndex(t, destLayout)
	var tagged []imgspecv1.Descriptor
	for _, d := range destIndex.Manifests {
		if d.Annotations[imgspecv1.AnnotationRefName] == integrationOCITag {
			tagged = append(tagged, d)
		}
	}
	require.Len(t, tagged, 1, "exactly one descriptor must carry the ociTag")
	assert.Equal(t, recordedDigest, tagged[0].Digest.String(), "recorded digest must be the top-level index descriptor digest")
	assert.Equal(t, imgspecv1.MediaTypeImageIndex, tagged[0].MediaType, "CopyAllImages must preserve the multi-arch index")

	// The full multi-arch index is preserved: the top-level index blob still
	// references one manifest per architecture.
	preserved := readIndexBlob(t, destLayout, tagged[0].Digest)
	assert.Len(t, preserved.Manifests, len(arches), "every platform manifest must be preserved")

	// The emitted digest manifest validates under the consumer's parser.
	entry := localimages.ImageEntry{Name: integrationImageName, Digest: recordedDigest, OCITag: integrationOCITag}
	manifest := localimages.DigestManifest{Version: digestManifestVersion, OCILayoutPath: integrationLayoutDir, Images: []localimages.ImageEntry{entry}}
	manifestPath := filepath.Join(mediaRoot, "digests.json")
	writeJSON(t, manifestPath, manifest)
	parsed, err := localimages.ParseDigestManifest(manifestPath)
	require.NoError(t, err)
	require.Len(t, parsed.Images, 1)
	assert.Equal(t, entry, parsed.Images[0])

	// Consumer round-trip: localimages selects and imports the host-platform instance
	// from the mirrored layout. Importing a layer into containers-storage needs root
	// (or a user namespace) for the layer chown, so guard it like the localimages
	// store-backed tests.
	if os.Geteuid() != 0 {
		t.Skip("importing into containers-storage requires root (or a user namespace); copy/selection assertions above still ran")
	}
	graphRoot := filepath.Join(t.TempDir(), "graph")
	im, err := localimages.NewImporter(logrus.New(), localimages.StoreConfig{
		GraphDriverName: "vfs",
		GraphRoot:       graphRoot,
		RunRoot:         filepath.Join(t.TempDir(), "run"),
	})
	if err != nil {
		t.Skipf("containers-storage unavailable: %v", err)
	}
	defer im.Close()
	require.NoError(t, im.ImportImage(context.Background(), mediaRoot, parsed, parsed.Images[0]))
}

type pinnedLayoutReference struct {
	types.ImageReference
	canonical reference.Canonical
}

func (r pinnedLayoutReference) DockerReference() reference.Named { return r.canonical }

// The pinned Docker schema 2 bytes must remain addressable after the OCI copy
// converts media types, including when the source reference is digest-pinned.
func TestCopyIntoLayoutPreservesPinnedDockerManifest(t *testing.T) {
	srcDir := buildMultiArchSourceLayout(t, []string{runtime.GOARCH}, "src")
	srcIndex := readDestIndex(t, srcDir)
	topData, err := os.ReadFile(filepath.Join(srcDir, "blobs", "sha256", srcIndex.Manifests[0].Digest.Encoded()))
	require.NoError(t, err)
	var topIndex imgspecv1.Index
	require.NoError(t, json.Unmarshal(topData, &topIndex))
	child := topIndex.Manifests[0]
	data, err := os.ReadFile(filepath.Join(srcDir, "blobs", "sha256", child.Digest.Encoded()))
	require.NoError(t, err)
	var dockerManifest map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &dockerManifest))
	dockerManifest["mediaType"] = manifest.DockerV2Schema2MediaType
	dockerManifest["config"].(map[string]interface{})["mediaType"] = "application/vnd.docker.container.image.v1+json"
	for _, layer := range dockerManifest["layers"].([]interface{}) {
		layer.(map[string]interface{})["mediaType"] = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	}
	data, err = json.Marshal(dockerManifest)
	require.NoError(t, err)
	dockerDigest := digest.FromBytes(data)
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "blobs", "sha256", dockerDigest.Encoded()), data, 0o600))
	srcIndex.Manifests[0].Digest = dockerDigest
	srcIndex.Manifests[0].MediaType = manifest.DockerV2Schema2MediaType
	srcIndex.Manifests[0].Size = int64(len(data))
	writeJSON(t, filepath.Join(srcDir, "index.json"), srcIndex)

	layoutRef, err := layout.NewReference(srcDir, "src")
	require.NoError(t, err)
	named, err := reference.ParseNormalizedNamed("quay.io/example/docker")
	require.NoError(t, err)
	canonical, err := reference.WithDigest(named, dockerDigest)
	require.NoError(t, err)
	ref := pinnedLayoutReference{ImageReference: layoutRef, canonical: canonical}
	destDir := filepath.Join(t.TempDir(), "dest")
	got, err := copyIntoLayout(context.Background(), logrus.New(), ref, destDir, "docker-0", "")
	require.NoError(t, err)
	assert.Equal(t, dockerDigest.String(), got.digest)
	index := readDestIndex(t, destDir)
	require.Len(t, index.Manifests, 1)
	assert.Equal(t, dockerDigest, index.Manifests[0].Digest)
	assert.Equal(t, manifest.DockerV2Schema2MediaType, index.Manifests[0].MediaType)
	assert.FileExists(t, filepath.Join(destDir, "blobs", "sha256", dockerDigest.Encoded()))
	// The offline importer reads the layout through the same transport.
	preservedRef, err := layout.NewReference(destDir, "docker-0")
	require.NoError(t, err)
	_, err = copyIntoLayout(context.Background(), logrus.New(), preservedRef, filepath.Join(t.TempDir(), "offline-copy"), "imported", "")
	require.NoError(t, err)
}

func TestCopyIntoLayoutPreservesPinnedDockerManifestList(t *testing.T) {
	otherArch := "arm64"
	if runtime.GOARCH == "arm64" {
		otherArch = "amd64"
	}
	srcDir := buildMultiArchSourceLayout(t, []string{runtime.GOARCH, otherArch}, "src")
	srcIndex := readDestIndex(t, srcDir)
	topData, err := os.ReadFile(filepath.Join(srcDir, "blobs", "sha256", srcIndex.Manifests[0].Digest.Encoded()))
	require.NoError(t, err)
	var topIndex imgspecv1.Index
	require.NoError(t, json.Unmarshal(topData, &topIndex))
	for i, child := range topIndex.Manifests {
		data, readErr := os.ReadFile(filepath.Join(srcDir, "blobs", "sha256", child.Digest.Encoded()))
		require.NoError(t, readErr)
		var image map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &image))
		image["mediaType"] = manifest.DockerV2Schema2MediaType
		image["config"].(map[string]interface{})["mediaType"] = "application/vnd.docker.container.image.v1+json"
		for _, layer := range image["layers"].([]interface{}) {
			layer.(map[string]interface{})["mediaType"] = "application/vnd.docker.image.rootfs.diff.tar.gzip"
		}
		data, err = json.Marshal(image)
		require.NoError(t, err)
		dgst := digest.FromBytes(data)
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, "blobs", "sha256", dgst.Encoded()), data, 0o600))
		topIndex.Manifests[i].Digest = dgst
		topIndex.Manifests[i].MediaType = manifest.DockerV2Schema2MediaType
		topIndex.Manifests[i].Size = int64(len(data))
	}
	topIndex.MediaType = manifest.DockerV2ListMediaType
	data, err := json.Marshal(topIndex)
	require.NoError(t, err)
	listDigest := digest.FromBytes(data)
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "blobs", "sha256", listDigest.Encoded()), data, 0o600))
	srcIndex.Manifests[0].Digest = listDigest
	srcIndex.Manifests[0].MediaType = manifest.DockerV2ListMediaType
	srcIndex.Manifests[0].Size = int64(len(data))
	writeJSON(t, filepath.Join(srcDir, "index.json"), srcIndex)

	layoutRef, err := layout.NewReference(srcDir, "src")
	require.NoError(t, err)
	named, err := reference.ParseNormalizedNamed("quay.io/example/docker-list")
	require.NoError(t, err)
	canonical, err := reference.WithDigest(named, listDigest)
	require.NoError(t, err)
	destDir := filepath.Join(t.TempDir(), "dest")
	got, err := copyIntoLayout(context.Background(), logrus.New(), pinnedLayoutReference{ImageReference: layoutRef, canonical: canonical}, destDir, "docker-list-0", "")
	require.NoError(t, err)
	assert.Equal(t, listDigest.String(), got.digest)
	index := readDestIndex(t, destDir)
	require.Len(t, index.Manifests, 1)
	assert.Equal(t, listDigest, index.Manifests[0].Digest)
	assert.Equal(t, manifest.DockerV2ListMediaType, index.Manifests[0].MediaType)
	for _, child := range topIndex.Manifests {
		assert.FileExists(t, filepath.Join(destDir, "blobs", "sha256", child.Digest.Encoded()))
	}
}

// ----- source-layout synthesis helpers -----

// buildMultiArchSourceLayout writes a multi-arch OCI layout (one image manifest per
// arch under a single image index) addressable as oci:<dir>:<srcTag>, and returns
// the layout directory.
func buildMultiArchSourceLayout(t *testing.T, arches []string, srcTag string) string {
	t.Helper()
	srcDir := t.TempDir()
	blobsDir := filepath.Join(srcDir, "blobs")
	require.NoError(t, os.MkdirAll(blobsDir, 0o755))

	imageIndex := imgspecv1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: imgspecv1.MediaTypeImageIndex,
	}
	for _, arch := range arches {
		imageIndex.Manifests = append(imageIndex.Manifests, buildArchManifest(t, blobsDir, arch))
	}
	indexBytes := marshalJSON(t, imageIndex)
	indexDigest := writeBlobFile(t, blobsDir, indexBytes)

	topIndex := imgspecv1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: imgspecv1.MediaTypeImageIndex,
		Manifests: []imgspecv1.Descriptor{{
			MediaType:   imgspecv1.MediaTypeImageIndex,
			Digest:      indexDigest,
			Size:        int64(len(indexBytes)),
			Annotations: map[string]string{imgspecv1.AnnotationRefName: srcTag},
		}},
	}
	writeJSON(t, filepath.Join(srcDir, "oci-layout"), imgspecv1.ImageLayout{Version: imgspecv1.ImageLayoutVersion})
	writeJSON(t, filepath.Join(srcDir, "index.json"), topIndex)
	return srcDir
}

// buildArchManifest writes a minimal single-platform image (layer, config, manifest)
// and returns its manifest descriptor.
func buildArchManifest(t *testing.T, blobsDir, arch string) imgspecv1.Descriptor {
	t.Helper()
	uncompressed := makeTarball(t, "file-"+arch+".txt", "content-"+arch)
	diffID := digest.FromBytes(uncompressed)
	compressed := gzipBytes(t, uncompressed)
	layerDigest := writeBlobFile(t, blobsDir, compressed)

	config := imgspecv1.Image{
		Platform: imgspecv1.Platform{Architecture: arch, OS: "linux"},
		RootFS:   imgspecv1.RootFS{Type: "layers", DiffIDs: []digest.Digest{diffID}},
	}
	configBytes := marshalJSON(t, config)
	configDigest := writeBlobFile(t, blobsDir, configBytes)

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
	manifestBytes := marshalJSON(t, manifest)
	manifestDigest := writeBlobFile(t, blobsDir, manifestBytes)
	return imgspecv1.Descriptor{
		MediaType: imgspecv1.MediaTypeImageManifest,
		Digest:    manifestDigest,
		Size:      int64(len(manifestBytes)),
		Platform:  &imgspecv1.Platform{Architecture: arch, OS: "linux"},
	}
}

// readDestIndex reads the destination layout's index.json.
func readDestIndex(t *testing.T, layoutDir string) imgspecv1.Index {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(layoutDir, "index.json"))
	require.NoError(t, err)
	var index imgspecv1.Index
	require.NoError(t, json.Unmarshal(data, &index))
	return index
}

// readIndexBlob reads and parses the image index blob at the given digest.
func readIndexBlob(t *testing.T, layoutDir string, d digest.Digest) imgspecv1.Index {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(layoutDir, "blobs", d.Algorithm().String(), d.Encoded()))
	require.NoError(t, err)
	var index imgspecv1.Index
	require.NoError(t, json.Unmarshal(data, &index))
	return index
}

func writeBlobFile(t *testing.T, blobsDir string, data []byte) digest.Digest {
	t.Helper()
	d := digest.FromBytes(data)
	path := filepath.Join(blobsDir, d.Algorithm().String(), d.Encoded())
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, data, 0o644))
	return d
}

func makeTarball(t *testing.T, name, content string) []byte {
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

func marshalJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return data
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, marshalJSON(t, v), 0o644))
}
