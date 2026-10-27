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

package localimages

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stageTestSignature writes a signature blob to its derived on-media path under
// layoutDir and returns the recorded SignatureBlob metadata.
func stageTestSignature(t *testing.T, layoutDir string, inst digest.Digest, index int, data []byte) SignatureBlob {
	t.Helper()
	dir := filepath.Join(layoutDir, SignaturesDirName, inst.Algorithm().String(), inst.Encoded())
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, strconv.Itoa(index)), data, 0o600))
	sum := sha256.Sum256(data)
	return SignatureBlob{SHA256: hex.EncodeToString(sum[:]), Size: len(data)}
}

func TestLoadAndVerifyStagedSignatures(t *testing.T) {
	layoutDir := t.TempDir()
	index := digest.FromBytes([]byte("index-manifest"))
	instance := digest.FromBytes([]byte("amd64-instance"))

	indexSig := []byte("index-signature-bytes")
	instSig0 := []byte("instance-signature-0")
	instSig1 := []byte("instance-signature-1")

	e := ImageEntry{
		Name:   testImageName,
		OCITag: testOCITag,
		Digest: index.String(),
		Signatures: []InstanceSignatures{
			{InstanceDigest: index.String(), Blobs: []SignatureBlob{
				stageTestSignature(t, layoutDir, index, 0, indexSig),
			}},
			{InstanceDigest: instance.String(), Blobs: []SignatureBlob{
				stageTestSignature(t, layoutDir, instance, 0, instSig0),
				stageTestSignature(t, layoutDir, instance, 1, instSig1),
			}},
		},
	}

	sigs, topLevel, err := loadAndVerifyStagedSignatures(layoutDir, e)
	require.NoError(t, err)
	assert.Equal(t, index, topLevel)
	require.Len(t, sigs, 2)
	assert.Equal(t, [][]byte{indexSig}, sigs[index])
	assert.Equal(t, [][]byte{instSig0, instSig1}, sigs[instance])
}

func TestLoadAndVerifyStagedSignaturesRejectsTampering(t *testing.T) {
	index := digest.FromBytes([]byte("index-manifest"))
	good := []byte("good-signature")

	newEntry := func(layoutDir string, blob SignatureBlob) ImageEntry {
		return ImageEntry{
			Name: testImageName, OCITag: testOCITag, Digest: index.String(),
			Signatures: []InstanceSignatures{{InstanceDigest: index.String(), Blobs: []SignatureBlob{blob}}},
		}
	}

	t.Run("missing file", func(t *testing.T) {
		layoutDir := t.TempDir()
		// Record a blob but never stage its file.
		sum := sha256.Sum256(good)
		e := newEntry(layoutDir, SignatureBlob{SHA256: hex.EncodeToString(sum[:]), Size: len(good)})
		_, _, err := loadAndVerifyStagedSignatures(layoutDir, e)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reading staged signature")
	})

	t.Run("size mismatch", func(t *testing.T) {
		layoutDir := t.TempDir()
		blob := stageTestSignature(t, layoutDir, index, 0, good)
		blob.Size++ // claim one more byte than staged
		_, _, err := loadAndVerifyStagedSignatures(layoutDir, newEntry(layoutDir, blob))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bytes, manifest records")
	})

	t.Run("hash mismatch", func(t *testing.T) {
		layoutDir := t.TempDir()
		blob := stageTestSignature(t, layoutDir, index, 0, good)
		blob.SHA256 = hex.EncodeToString(make([]byte, sha256.Size)) // wrong hash, right length
		_, _, err := loadAndVerifyStagedSignatures(layoutDir, newEntry(layoutDir, blob))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not match recorded")
	})

	t.Run("invalid top-level digest", func(t *testing.T) {
		layoutDir := t.TempDir()
		e := newEntry(layoutDir, stageTestSignature(t, layoutDir, index, 0, good))
		e.Digest = "not-a-digest"
		_, _, err := loadAndVerifyStagedSignatures(layoutDir, e)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parsing top-level digest")
	})
}

func TestVerifyStagedSignaturesNoSignatures(t *testing.T) {
	// An entry with no recorded signatures (v1/v2 manifests, or an unsigned image)
	// verifies trivially.
	e := ImageEntry{Name: testImageName, OCITag: testOCITag, Digest: digest.FromBytes([]byte("x")).String()}
	require.NoError(t, VerifyStagedSignatures(t.TempDir(), e))
}

func TestSignatureInjectingSourceGetSignatures(t *testing.T) {
	index := digest.FromBytes([]byte("index"))
	instance := digest.FromBytes([]byte("instance"))
	other := digest.FromBytes([]byte("other"))

	src := &signatureInjectingSource{
		topLevel: index,
		sigs: map[digest.Digest][][]byte{
			index:    {[]byte("index-sig")},
			instance: {[]byte("inst-sig-0"), []byte("inst-sig-1")},
		},
	}

	// nil instance digest resolves to the top-level manifest's signatures.
	got, err := src.GetSignatures(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("index-sig")}, got)

	// A specific instance digest resolves to that instance's signatures.
	got, err = src.GetSignatures(t.Context(), &instance)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("inst-sig-0"), []byte("inst-sig-1")}, got)

	// An instance with no staged signatures reports an empty set (unsigned instance).
	got, err = src.GetSignatures(t.Context(), &other)
	require.NoError(t, err)
	assert.Empty(t, got)
}
