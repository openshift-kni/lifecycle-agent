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

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.podman.io/image/v5/types"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/localimages"
)

// fakeSigSource is a types.ImageSource that serves a fixed manifest and a fixed set of
// signatures keyed by instance digest (the empty string keys the top-level manifest,
// read with a nil instance digest). Only GetManifest and GetSignatures are exercised by
// captureAndStageSignatures; the other methods are never called.
type fakeSigSource struct {
	types.ImageSource
	manifestBlob []byte
	manifestType string
	sigs         map[string][][]byte
}

func (f *fakeSigSource) GetManifest(_ context.Context, _ *digest.Digest) ([]byte, string, error) {
	return f.manifestBlob, f.manifestType, nil
}

func (f *fakeSigSource) GetSignatures(_ context.Context, instanceDigest *digest.Digest) ([][]byte, error) {
	key := ""
	if instanceDigest != nil {
		key = instanceDigest.String()
	}
	return f.sigs[key], nil
}

// assertStagedBlob confirms the staged on-media file for inst[index] exists and matches
// the recorded metadata.
func assertStagedBlob(t *testing.T, layoutDir string, inst digest.Digest, index int, want []byte, blob localimages.SignatureBlob) {
	t.Helper()
	path := filepath.Join(layoutDir, localimages.SignaturesDirName, inst.Algorithm().String(), inst.Encoded(), strconv.Itoa(index))
	data, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.Equal(t, want, data)
	assert.Equal(t, len(want), blob.Size)
	sum := sha256.Sum256(want)
	assert.Equal(t, hex.EncodeToString(sum[:]), blob.SHA256)
}

func TestCaptureAndStageSignaturesSingleArch(t *testing.T) {
	layoutDir := t.TempDir()
	manifestBlob := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	topDigest := digest.FromBytes(manifestBlob)

	src := &fakeSigSource{
		manifestBlob: manifestBlob,
		manifestType: imgspecv1.MediaTypeImageManifest,
		sigs:         map[string][][]byte{"": {[]byte("the-signature")}},
	}

	out, err := captureAndStageSignatures(context.Background(), src, layoutDir, topDigest)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, topDigest.String(), out[0].InstanceDigest)
	require.Len(t, out[0].Blobs, 1)
	assertStagedBlob(t, layoutDir, topDigest, 0, []byte("the-signature"), out[0].Blobs[0])

	// The staged bundle round-trips through the consumer's verifying loader.
	require.NoError(t, localimages.VerifyStagedSignatures(layoutDir, localimages.ImageEntry{
		Name: "img", OCITag: "img-0", Digest: topDigest.String(), Signatures: out,
	}))
}

func TestCaptureAndStageSignaturesMultiArch(t *testing.T) {
	layoutDir := t.TempDir()
	instA := digest.FromBytes([]byte("amd64"))
	instB := digest.FromBytes([]byte("arm64"))

	index := imgspecv1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: imgspecv1.MediaTypeImageIndex,
		Manifests: []imgspecv1.Descriptor{
			{Digest: instA, Size: 1, MediaType: imgspecv1.MediaTypeImageManifest},
			{Digest: instB, Size: 1, MediaType: imgspecv1.MediaTypeImageManifest},
		},
	}
	blob, err := json.Marshal(index)
	require.NoError(t, err)
	topDigest := digest.FromBytes(blob)

	src := &fakeSigSource{
		manifestBlob: blob,
		manifestType: imgspecv1.MediaTypeImageIndex,
		sigs: map[string][][]byte{
			// The index itself is unsigned; only the amd64 instance carries a signature.
			instA.String(): {[]byte("amd64-sig")},
		},
	}

	out, err := captureAndStageSignatures(context.Background(), src, layoutDir, topDigest)
	require.NoError(t, err)

	// Only instances that carried a signature are recorded (index and arm64 had none).
	require.Len(t, out, 1)
	assert.Equal(t, instA.String(), out[0].InstanceDigest)
	assertStagedBlob(t, layoutDir, instA, 0, []byte("amd64-sig"), out[0].Blobs[0])

	// No files were staged for the unsigned instances.
	assert.NoDirExists(t, filepath.Join(layoutDir, localimages.SignaturesDirName, instB.Algorithm().String(), instB.Encoded()))

	require.NoError(t, localimages.VerifyStagedSignatures(layoutDir, localimages.ImageEntry{
		Name: "img", OCITag: "img-0", Digest: topDigest.String(), Signatures: out,
	}))
}

func TestCaptureAndStageSignaturesUnsigned(t *testing.T) {
	layoutDir := t.TempDir()
	manifestBlob := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	topDigest := digest.FromBytes(manifestBlob)

	src := &fakeSigSource{manifestBlob: manifestBlob, manifestType: imgspecv1.MediaTypeImageManifest}

	out, err := captureAndStageSignatures(context.Background(), src, layoutDir, topDigest)
	require.NoError(t, err)
	assert.Empty(t, out)
	assert.NoDirExists(t, filepath.Join(layoutDir, localimages.SignaturesDirName))
}

// TestCaptureAndStageSignaturesConvertedTopLevelRejected covers the case where copy.Image
// converted the source manifest (so the layout/top digest no longer matches the source digest
// the registry signed) and the top-level manifest carries a signature: staging must fail closed
// rather than key the signature to a digest it does not sign.
func TestCaptureAndStageSignaturesConvertedTopLevelRejected(t *testing.T) {
	layoutDir := t.TempDir()
	manifestBlob := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`)
	// topDigest is the post-conversion layout digest, deliberately different from the source digest.
	topDigest := digest.FromBytes([]byte("converted-oci-manifest"))

	src := &fakeSigSource{
		manifestBlob: manifestBlob,
		manifestType: imgspecv1.MediaTypeImageManifest,
		sigs:         map[string][][]byte{"": {[]byte("the-signature")}},
	}

	out, err := captureAndStageSignatures(context.Background(), src, layoutDir, topDigest)
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Contains(t, err.Error(), "converted")
	// Nothing is staged once staging is rejected.
	assert.NoDirExists(t, filepath.Join(layoutDir, localimages.SignaturesDirName))
}

// TestCaptureAndStageSignaturesConvertedPlatformRejected covers a converted multi-arch index
// whose top-level index is unsigned but a platform instance is signed: the guard must keep
// checking past the unsigned index and still reject the signed platform instance.
func TestCaptureAndStageSignaturesConvertedPlatformRejected(t *testing.T) {
	layoutDir := t.TempDir()
	instA := digest.FromBytes([]byte("amd64"))

	index := imgspecv1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: imgspecv1.MediaTypeImageIndex,
		Manifests: []imgspecv1.Descriptor{
			{Digest: instA, Size: 1, MediaType: imgspecv1.MediaTypeImageManifest},
		},
	}
	blob, err := json.Marshal(index)
	require.NoError(t, err)
	// Converted: the layout/top digest differs from the source index digest.
	topDigest := digest.FromBytes([]byte("converted-index"))

	src := &fakeSigSource{
		manifestBlob: blob,
		manifestType: imgspecv1.MediaTypeImageIndex,
		// The index itself is unsigned; only the amd64 instance carries a signature.
		sigs: map[string][][]byte{instA.String(): {[]byte("amd64-sig")}},
	}

	out, err := captureAndStageSignatures(context.Background(), src, layoutDir, topDigest)
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Contains(t, err.Error(), "converted")
}

// TestCaptureAndStageSignaturesConvertedUnsignedAllowed confirms the conversion guard fires only
// when signatures actually exist: converting an unsigned image is harmless, so staging succeeds.
func TestCaptureAndStageSignaturesConvertedUnsignedAllowed(t *testing.T) {
	layoutDir := t.TempDir()
	manifestBlob := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	// Converted (top digest differs from the source digest), but the source has no signatures.
	topDigest := digest.FromBytes([]byte("converted-but-unsigned"))

	src := &fakeSigSource{manifestBlob: manifestBlob, manifestType: imgspecv1.MediaTypeImageManifest}

	out, err := captureAndStageSignatures(context.Background(), src, layoutDir, topDigest)
	require.NoError(t, err)
	assert.Empty(t, out)
	assert.NoDirExists(t, filepath.Join(layoutDir, localimages.SignaturesDirName))
}
