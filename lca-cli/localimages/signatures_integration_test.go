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

// End-to-end coverage for signature restoration by digest (developer-of-record directive on
// PR #10013). It stages index-level and per-platform signatures for a multi-arch index, imports
// the single host-platform instance into a real containers/storage store (what the node does),
// then reads the signatures back through the storage transport under each digest the store can
// represent, asserting exact bytes.
//
// It encodes the representation a single-platform store can actually hold -- and its documented
// limitation. containers/storage stores single-platform images; copy.Image with CopySystemImage
// materialises only the selected platform instance and writes that instance's signatures as the
// image's default/own set. The index (manifest-list) digest therefore carries no distinct
// signature set: the staged index-level signatures are not routed into the store, so they are not
// preserved -- only the selected platform instance's signatures survive. See the package doc in
// signatures.go.
//
// Importing a layer needs root (or a user namespace) for the layer chown, so the test is gated
// with requireCopyCapable. TestMain (reexec.Init) is defined in localimages_test.go.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	istorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
)

// gpgSimpleSigningFixture returns bytes that go.podman.io/image's signature.FromBlob accepts as a
// "simple signing" signature. The leading 0x88 is an old-format OpenPGP signature-packet tag, which
// is how a real detached GPG signature (the simple-signing format) begins; signature.FromBlob keys
// off that first byte. The trailing bytes are opaque: ImportImage copies under an
// insecureAcceptAnything policy, so no GPG verification runs, and containers-storage round-trips the
// blob verbatim -- letting the test assert exact signature bytes. A plain-text payload would be
// rejected on read-back with "unrecognized signature format".
func gpgSimpleSigningFixture(payload string) []byte {
	return append([]byte{0x88}, []byte(payload)...)
}

// platformInstanceDigest reads the multi-arch index blob at indexDigest from the OCI layout and
// returns the manifest instance digest for arch.
func platformInstanceDigest(t *testing.T, layoutDir string, indexDigest digest.Digest, arch string) digest.Digest {
	t.Helper()
	blobPath := filepath.Join(layoutDir, "blobs", indexDigest.Algorithm().String(), indexDigest.Encoded())
	data, err := os.ReadFile(blobPath) //nolint:gosec // test-controlled layout path
	require.NoError(t, err)
	var index imgspecv1.Index
	require.NoError(t, json.Unmarshal(data, &index))
	for _, man := range index.Manifests {
		if man.Platform != nil && man.Platform.Architecture == arch {
			return man.Digest
		}
	}
	t.Fatalf("no manifest instance for arch %q in index %s", arch, indexDigest)
	return ""
}

// TestSignatureRestorationByDigestIntegration imports one platform instance of a multi-arch index
// with staged index + platform signatures and verifies the exact signature bytes the store serves
// under each addressable digest, encoding the single-platform representation and its limitation.
func TestSignatureRestorationByDigestIntegration(t *testing.T) {
	requireCopyCapable(t)
	im, graphRoot := newImporter(t)

	otherArch := "arm64"
	if runtime.GOARCH == "arm64" {
		otherArch = "amd64"
	}
	mediaRoot, m := buildLayout(t, []layoutImage{{tag: testOCITag, name: testImageName, arches: []string{runtime.GOARCH, otherArch}}})
	entry := m.Images[0]

	indexDigest, err := digest.Parse(entry.Digest)
	require.NoError(t, err)
	layoutDir := filepath.Join(mediaRoot, m.OCILayoutPath)
	platformDigest := platformInstanceDigest(t, layoutDir, indexDigest, runtime.GOARCH)

	// Stage index-level and per-platform signatures on the media, as the capture side does.
	// The fixtures must be well-formed "simple signing" blobs: containers/storage parses every
	// stored signature with signature.FromBlob on read-back (storage_src.go), which rejects a blob
	// whose first byte is neither an OpenPGP packet tag nor the self-describing 0x00 prefix with
	// "unrecognized signature format". A plain-text payload is therefore rejected before the
	// restoration assertions run. gpgSimpleSigningFixture gives the bytes a leading OpenPGP
	// signature-packet tag so they round-trip verbatim under the insecureAcceptAnything policy.
	indexSig := gpgSimpleSigningFixture("index-level-signature-bytes")
	platformSig := gpgSimpleSigningFixture("platform-instance-signature-bytes")
	entry.Signatures = []InstanceSignatures{
		{InstanceDigest: indexDigest.String(), Blobs: []SignatureBlob{stageTestSignature(t, layoutDir, indexDigest, 0, indexSig)}},
		{InstanceDigest: platformDigest.String(), Blobs: []SignatureBlob{stageTestSignature(t, layoutDir, platformDigest, 0, platformSig)}},
	}
	m.Images[0] = entry

	require.NoError(t, im.ImportImage(context.Background(), mediaRoot, m, entry))

	// Re-open the store and read signatures back through the storage transport -- the path a node's
	// signature verification uses.
	verifier, err := NewImporter(logrus.New(), StoreConfig{
		GraphDriverName: "vfs",
		GraphRoot:       graphRoot,
		RunRoot:         filepath.Join(t.TempDir(), "run-sig"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = verifier.Close() })

	ref, err := istorage.Transport.ParseStoreReference(verifier.store, entry.Name+"@"+entry.Digest)
	require.NoError(t, err)
	src, err := ref.NewImageSource(context.Background(), &types.SystemContext{})
	require.NoError(t, err)
	defer src.Close()

	// The default (nil) signatures are the materialised platform instance's -- the signatures that
	// sign the single manifest the store can verify, exactly as a registry pull of one platform.
	defaultSigs, err := src.GetSignatures(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{platformSig}, defaultSigs, "resolved image must carry the platform instance's signatures")

	// Querying the platform instance digest returns the same platform signatures.
	pDig := platformDigest
	platSigs, err := src.GetSignatures(context.Background(), &pDig)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{platformSig}, platSigs, "platform-instance digest must carry the platform signatures")

	// Documented limitation: the index (manifest-list) digest carries NO distinct signature set in a
	// single-platform store. CopySystemImage imports only the selected platform instance and never
	// routes the staged index-level signatures into the store, so they are not preserved. We assert
	// the index set is empty (not the captured index bytes, and not the platform bytes misfiled).
	iDig := indexDigest
	indexSigs, err := src.GetSignatures(context.Background(), &iDig)
	require.NoError(t, err)
	assert.Empty(t, indexSigs, "index-level signatures are not restored into a single-platform store")
	assert.NotEqual(t, [][]byte{indexSig}, defaultSigs, "the captured index signature must not be reported as the resolved image's signatures")
}
