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

package ibi_preparation

// Root-gated integration coverage for the read-only store handoff (CNF-27165). The faked-seam
// unit tests in rostore_test.go exercise the orchestration (fail-closed, atomic seal, drop-in,
// ordering) with a synthetic store and a plain copy; they cannot establish that the *production*
// copier (CopyWithTar) and the *production* verification (opening the copied store as a read-only
// additional image store) work on a real overlay containers/storage store. This test builds a
// real overlay store from a staged OCI layout, copies it with defaultCopyReadOnlyStore, and
// confirms every manifest image resolves through the copy opened as a read-only additional image
// store -- the §6.6 compatibility gate, checked prepare-side.
//
// It is root-gated (the overlay layer chown needs root) and self-skips otherwise, matching the
// localimages and create-usb integration tests.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.podman.io/storage/pkg/reexec"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/localimages"
)

func TestMain(m *testing.M) {
	// reexec.Init handles the storage driver's re-exec helpers; it returns true in a re-exec'd
	// child, which must not run the test suite. A normal run returns false and proceeds.
	if reexec.Init() {
		return
	}
	os.Exit(m.Run())
}

// buildRealOverlayStore stages a single-arch OCI layout for one image and imports it into a real
// overlay containers/storage store, returning the store path (graphroot) and the matching digest
// manifest. It skips the test when overlay/root is unavailable.
func buildRealOverlayStore(t *testing.T) (string, *localimages.DigestManifest) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("building an overlay containers-storage store requires root (or a user namespace); skipping")
	}

	mediaRoot := t.TempDir()
	layoutPath := "oci"
	blobsDir := filepath.Join(mediaRoot, layoutPath, "blobs")
	require.NoError(t, os.MkdirAll(blobsDir, 0o755))

	// A tiny gzipped tar layer + minimal config + manifest, written as OCI blobs.
	uncompressed := makeTestTar(t, "file.txt", "content")
	diffID := digest.FromBytes(uncompressed)
	compressed := gzipTestBytes(t, uncompressed)
	layerDigest := writeTestBlob(t, blobsDir, compressed)

	config := imgspecv1.Image{
		Platform: imgspecv1.Platform{Architecture: runtime.GOARCH, OS: "linux"},
		RootFS:   imgspecv1.RootFS{Type: "layers", DiffIDs: []digest.Digest{diffID}},
	}
	configBytes := mustJSONBytes(t, config)
	configDigest := writeTestBlob(t, blobsDir, configBytes)

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
	manifestBytes := mustJSONBytes(t, manifest)
	manifestDigest := writeTestBlob(t, blobsDir, manifestBytes)

	const imageName = "quay.io/openshift-release-dev/ocp-release"
	const ociTag = "ocp-release-0"
	index := imgspecv1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: imgspecv1.MediaTypeImageIndex,
		Manifests: []imgspecv1.Descriptor{{
			MediaType:   imgspecv1.MediaTypeImageManifest,
			Digest:      manifestDigest,
			Size:        int64(len(manifestBytes)),
			Platform:    &imgspecv1.Platform{Architecture: runtime.GOARCH, OS: "linux"},
			Annotations: map[string]string{imgspecv1.AnnotationRefName: ociTag},
		}},
	}
	writeTestFile(t, filepath.Join(mediaRoot, layoutPath, "oci-layout"), mustJSONBytes(t, imgspecv1.ImageLayout{Version: imgspecv1.ImageLayoutVersion}))
	writeTestFile(t, filepath.Join(mediaRoot, layoutPath, "index.json"), mustJSONBytes(t, index))

	m := &localimages.DigestManifest{
		Version:       localimages.SourcesManifestVersion,
		OCILayoutPath: layoutPath,
		Images: []localimages.ImageEntry{{
			Name:    imageName,
			Digest:  manifestDigest.String(),
			OCITag:  ociTag,
			Sources: []string{imageName + "@" + manifestDigest.String()},
		}},
	}

	storePath := filepath.Join(t.TempDir(), "store")
	importer, err := localimages.NewImporter(logrus.New(), localimages.StoreConfig{
		GraphDriverName: closureStoreDriver,
		GraphRoot:       storePath,
		RunRoot:         filepath.Join(t.TempDir(), "run"),
	})
	if err != nil {
		t.Skipf("overlay containers-storage unavailable in this environment: %v", err)
	}
	if err := importer.ImportAll(context.Background(), mediaRoot, m); err != nil {
		_ = importer.Close()
		t.Skipf("overlay import unavailable in this environment: %v", err)
	}
	// Close the importer so its store locks are released before the store is copied.
	require.NoError(t, importer.Close())
	return storePath, m
}

// TestReadOnlyStoreCopyAndVerifyIntegration copies a real overlay store with the production
// copier and confirms the copy validates and resolves every manifest image as a read-only
// additional image store -- the node's offline consumption path.
func TestReadOnlyStoreCopyAndVerifyIntegration(t *testing.T) {
	srcStore, manifest := buildRealOverlayStore(t)

	dst := filepath.Join(t.TempDir(), common.RODurabilityStoreDirName)
	require.NoError(t, defaultCopyReadOnlyStore(srcStore, dst))

	// The production copier preserves original file modes and numeric ownership (no blanket mode
	// rewrite), so every entry in the real store round-trips bit-for-bit.
	assertTreeModesAndOwnershipMatch(t, srcStore, dst)

	// The copied store is in the overlay layer format and validates.
	require.NoError(t, validateOverlayStoreFormat(dst))

	// Every manifest image resolves through the copied store opened as a read-only additional
	// image store.
	require.NoError(t, defaultVerifyPersistedStore(context.Background(), logrus.New(), dst, manifest))

	// A manifest requiring an image the store does not contain fails closed.
	bogus := &localimages.DigestManifest{
		Version:       manifest.Version,
		OCILayoutPath: manifest.OCILayoutPath,
		Images: append(append([]localimages.ImageEntry{}, manifest.Images...), localimages.ImageEntry{
			Name:   "quay.io/does-not/exist",
			Digest: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		}),
	}
	assert.Error(t, defaultVerifyPersistedStore(context.Background(), logrus.New(), dst, bogus),
		"a store missing a required image must fail digest-manifest validation")
}

// assertTreeModesAndOwnershipMatch walks src and asserts every entry's permission bits and
// numeric uid/gid are identical at the corresponding path under dst, proving the copier preserved
// modes/ownership rather than rewriting them.
func assertTreeModesAndOwnershipMatch(t *testing.T, src, dst string) {
	t.Helper()
	require.NoError(t, filepath.Walk(src, func(p string, srcInfo os.FileInfo, err error) error {
		require.NoError(t, err)
		rel, err := filepath.Rel(src, p)
		require.NoError(t, err)
		dstInfo, err := os.Lstat(filepath.Join(dst, rel))
		require.NoError(t, err, "entry %q must exist after the copy", rel)
		assert.Equal(t, srcInfo.Mode(), dstInfo.Mode(), "mode must be preserved for %q", rel)
		srcStat, ok := srcInfo.Sys().(*syscall.Stat_t)
		require.True(t, ok)
		dstStat, ok := dstInfo.Sys().(*syscall.Stat_t)
		require.True(t, ok)
		assert.Equal(t, srcStat.Uid, dstStat.Uid, "uid must be preserved for %q", rel)
		assert.Equal(t, srcStat.Gid, dstStat.Gid, "gid must be preserved for %q", rel)
		return nil
	}))
}

// --- compact OCI-layout fixture helpers (self-contained to this package) ---

func makeTestTar(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}))
	_, err := tw.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

func gzipTestBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(data)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func writeTestBlob(t *testing.T, blobsDir string, data []byte) digest.Digest {
	t.Helper()
	d := digest.FromBytes(data)
	writeTestFile(t, filepath.Join(blobsDir, d.Algorithm().String(), d.Encoded()), data)
	return d
}

func mustJSONBytes(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return data
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, data, 0o644))
}
