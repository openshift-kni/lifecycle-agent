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

// This file implements the signature half of the offline image-sourcing contract
// (developer-of-record directive on PR #10013). An OCI image layout cannot store an
// image's signatures ("Pushing signatures for OCI images is not supported"), so the
// media builder strips signatures from the layout copy and stages the raw signature
// bytes beside the layout, keyed by the exact manifest instance digest they sign, with
// each blob's size and sha256 recorded in the digest manifest (ImageEntry.Signatures).
//
// On the node the signatures must be re-attached to the image as it is imported into
// containers-storage (which can store them), so the installed store carries the same
// signatures the source registry served. containers-storage cannot have signatures
// grafted onto an already-committed image through the public API (the on-store format
// couples the signature blob with per-signature sizes recorded in the image's internal
// metadata), so re-attachment is done the only robust way: by making the import copy
// itself carry the signatures. signatureInjectingReference wraps the OCI-layout source
// so copy.Image reads the staged bytes (verified first against their recorded sizes and
// hashes) through the public ImageSource.GetSignatures seam and writes them to the
// store exactly as a registry pull would.
//
// Limitation (multi-arch indexes): containers-storage stores single-platform images, and
// ImportImage copies with copy.CopySystemImage, which materialises only the instance matching
// the target architecture. copy.Image then reads that platform instance's signatures (via its
// instance digest) and writes them as the stored image's default/own set. The index
// (manifest-list) digest therefore carries no distinct signature set in the store: resolving
// name@<indexDigest> yields the single stored platform image, whose signatures are the platform
// instance's -- the correct, verifiable representation, since those are the signatures that sign
// the one manifest the store can verify. The staged index-level signatures (served here under
// topLevel for a nil instance) are consumed only when the top-level manifest is itself the
// imported single manifest (a single-arch image); for a multi-arch index they are not routed
// into the store and so are NOT restored. This is a documented limitation rather than full
// index-signature preservation; see docs/create-usb.md.
//
// On-media layout (relative to the OCI layout directory, OCILayoutPath):
//
//	<layout>/signatures/<algo>/<encoded>/<n>   # the n-th signature of instance <algo>:<encoded>
//
// The path is derived from the instance digest, never stored verbatim in the manifest,
// so a manifest cannot point the loader at an arbitrary file; the loader additionally
// resolves it with SecureJoin under the layout directory.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/opencontainers/go-digest"
	"go.podman.io/image/v5/types"
)

// SignaturesDirName is the directory, under the OCI layout directory, that holds the
// staged per-instance signature blobs. It is the producer/consumer contract for the
// on-media signature bundle.
const SignaturesDirName = "signatures"

// SignatureBlob records one staged signature's integrity data so the consumer can
// verify the on-media bytes before restoring them into containers-storage.
type SignatureBlob struct {
	// SHA256 is the lowercase-hex sha256 of the signature blob.
	SHA256 string `json:"sha256"`
	// Size is the signature blob's length in bytes.
	Size int `json:"size"`
}

// InstanceSignatures records the signatures captured for one manifest instance,
// keyed by that instance's digest (the top-level index/image digest, or a
// per-platform instance digest inside a multi-arch index). Only instances that
// carried at least one signature are recorded.
type InstanceSignatures struct {
	// InstanceDigest is the digest of the manifest instance these signatures sign
	// (e.g. "sha256:…"). It is the key the on-media bundle and the import-time
	// re-attachment are organised around.
	InstanceDigest string `json:"instanceDigest"`
	// Blobs is the ordered list of signature blobs captured for the instance.
	Blobs []SignatureBlob `json:"blobs"`
}

// loadAndVerifyStagedSignatures reads every signature recorded for e from under
// layoutDir, verifies each blob against its recorded size and sha256, and returns the
// bytes keyed by manifest instance digest together with the top-level (e.Digest)
// digest. It fails if a recorded signature file is missing, truncated, or does not
// match its recorded hash -- the "captured signatures cannot be verified" failure the
// directive requires. It is shared by the import path (which injects the returned
// bytes) and the build-time read-back check (VerifyStagedSignatures).
func loadAndVerifyStagedSignatures(layoutDir string, e ImageEntry) (map[digest.Digest][][]byte, digest.Digest, error) {
	topLevel, err := digest.Parse(e.Digest)
	if err != nil {
		return nil, "", fmt.Errorf("parsing top-level digest %q: %w", e.Digest, err)
	}

	out := make(map[digest.Digest][][]byte, len(e.Signatures))
	for _, is := range e.Signatures {
		inst, err := digest.Parse(is.InstanceDigest)
		if err != nil {
			return nil, "", fmt.Errorf("parsing signature instance digest %q: %w", is.InstanceDigest, err)
		}
		blobs := make([][]byte, 0, len(is.Blobs))
		for i, b := range is.Blobs {
			rel := filepath.Join(SignaturesDirName, inst.Algorithm().String(), inst.Encoded(), strconv.Itoa(i))
			path, err := securejoin.SecureJoin(layoutDir, rel)
			if err != nil {
				return nil, "", fmt.Errorf("resolving signature path %q under %q: %w", rel, layoutDir, err)
			}
			// #nosec G304 -- path is SecureJoin'd under the on-media layout directory.
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, "", fmt.Errorf("reading staged signature %s[%d]: %w", is.InstanceDigest, i, err)
			}
			if len(data) != b.Size {
				return nil, "", fmt.Errorf("staged signature %s[%d] is %d bytes, manifest records %d", is.InstanceDigest, i, len(data), b.Size)
			}
			sum := sha256.Sum256(data)
			if got := hex.EncodeToString(sum[:]); got != b.SHA256 {
				return nil, "", fmt.Errorf("staged signature %s[%d] sha256 %s does not match recorded %s", is.InstanceDigest, i, got, b.SHA256)
			}
			blobs = append(blobs, data)
		}
		if len(blobs) > 0 {
			out[inst] = blobs
		}
	}
	return out, topLevel, nil
}

// VerifyStagedSignatures confirms every signature recorded for e is present under
// layoutDir and matches its recorded size and sha256. The media builder calls it as a
// build-time read-back check so a producer bug is caught before publication; the import
// path performs the same verification implicitly when it loads the bytes for
// re-attachment.
func VerifyStagedSignatures(layoutDir string, e ImageEntry) error {
	_, _, err := loadAndVerifyStagedSignatures(layoutDir, e)
	return err
}

// signatureInjectingReference wraps an OCI-layout image reference so the image source
// it opens serves the staged signatures for the matching manifest instance. It is used
// only on the import path: copy.Image opens the source through NewImageSource and reads
// signatures through the public GetSignatures seam, so wrapping the source is enough to
// have the staged bytes written into containers-storage by the copy itself.
type signatureInjectingReference struct {
	types.ImageReference
	sigs     map[digest.Digest][][]byte
	topLevel digest.Digest
}

// NewImageSource returns the wrapped reference's source, decorated to serve the staged
// signatures.
func (r signatureInjectingReference) NewImageSource(ctx context.Context, sys *types.SystemContext) (types.ImageSource, error) {
	src, err := r.ImageReference.NewImageSource(ctx, sys)
	if err != nil {
		return nil, fmt.Errorf("opening OCI layout image source: %w", err)
	}
	return &signatureInjectingSource{ImageSource: src, sigs: r.sigs, topLevel: r.topLevel}, nil
}

// signatureInjectingSource overrides GetSignatures to return the staged signatures for
// the requested instance; all other reads delegate to the wrapped OCI-layout source.
// Because it exposes only the public types.ImageSource method set, copy.Image reaches
// this GetSignatures through the internal public-source adapter.
type signatureInjectingSource struct {
	types.ImageSource
	sigs     map[digest.Digest][][]byte
	topLevel digest.Digest
}

// GetSignatures returns the staged signatures for instanceDigest (or the top-level
// manifest when instanceDigest is nil). An instance with no staged signatures reports
// an empty set, exactly as an unsigned image would.
func (s *signatureInjectingSource) GetSignatures(_ context.Context, instanceDigest *digest.Digest) ([][]byte, error) {
	key := s.topLevel
	if instanceDigest != nil {
		key = *instanceDigest
	}
	return s.sigs[key], nil
}
