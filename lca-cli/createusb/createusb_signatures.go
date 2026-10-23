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

// This file implements the signature-capture half of the offline image-sourcing
// contract (developer-of-record directive on PR #10013). An OCI image layout cannot
// store an image's signatures, so copyIntoLayout strips them from the layout copy
// (copy.Options.RemoveSignatures) and this file captures the raw signature bytes from
// the source registry -- which can serve them -- and stages them beside the layout,
// keyed by the exact manifest instance digest they sign. The consumer
// (lca-cli/localimages) verifies and re-attaches them on the node at import time.
//
// The source registry is the only place the signatures exist; they are read through the
// public types.ImageSource.GetSignatures seam for the top-level manifest and, for a
// multi-arch index, every platform instance. An unsigned image yields no staged
// signatures -- whether an unsigned source is allowed at all is decided by the
// build-host trust policy enforced by the copy in copyIntoLayout.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/opencontainers/go-digest"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/types"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/localimages"
)

// captureAndStageSignatures reads the signatures the source registry holds for the
// image's top-level manifest (topDigest) and, for a multi-arch index, every platform
// instance, writes each signature blob into the layout's signatures/ subtree keyed by
// manifest instance digest, and returns the per-instance integrity metadata recorded in
// the digest manifest (localimages.InstanceSignatures). Only instances that carry at
// least one signature are staged and recorded; an entirely unsigned image returns nil.
func captureAndStageSignatures(ctx context.Context, imgSrc types.ImageSource, layoutDir string, topDigest digest.Digest) ([]localimages.InstanceSignatures, error) {
	// The instances whose signatures must be captured: the top-level manifest plus,
	// for a multi-arch index, every platform instance (OpenShift release content is
	// typically signed per platform instance, not only at the index).
	instances := []digest.Digest{topDigest}
	manifestBlob, manifestType, err := imgSrc.GetManifest(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("reading source manifest: %w", err)
	}
	if manifest.MIMETypeIsMultiImage(manifestType) {
		list, err := manifest.ListFromBlob(manifestBlob, manifestType)
		if err != nil {
			return nil, fmt.Errorf("parsing source manifest list: %w", err)
		}
		instances = append(instances, list.Instances()...)
	}

	var out []localimages.InstanceSignatures
	for _, inst := range instances {
		sigs, err := signaturesForInstance(ctx, imgSrc, inst, topDigest)
		if err != nil {
			return nil, err
		}
		if len(sigs) == 0 {
			continue
		}
		meta, err := stageInstanceSignatures(layoutDir, inst, sigs)
		if err != nil {
			return nil, err
		}
		out = append(out, meta)
	}
	return out, nil
}

// signaturesForInstance reads the signatures for one manifest instance. The top-level
// manifest is read with a nil instance digest (the source resolves it directly); a
// per-platform instance is read by its own digest.
func signaturesForInstance(ctx context.Context, imgSrc types.ImageSource, inst, topDigest digest.Digest) ([][]byte, error) {
	var (
		sigs [][]byte
		err  error
	)
	if inst == topDigest {
		sigs, err = imgSrc.GetSignatures(ctx, nil)
	} else {
		instance := inst
		sigs, err = imgSrc.GetSignatures(ctx, &instance)
	}
	if err != nil {
		return nil, fmt.Errorf("reading signatures for instance %s: %w", inst, err)
	}
	return sigs, nil
}

// stageInstanceSignatures writes each signature blob for inst into
// <layoutDir>/signatures/<algo>/<encoded>/<n> and returns its integrity metadata. The
// on-media path is derived from the instance digest, so the manifest records only the
// digest and per-blob size/sha256, never a verbatim path.
func stageInstanceSignatures(layoutDir string, inst digest.Digest, sigs [][]byte) (localimages.InstanceSignatures, error) {
	dir := filepath.Join(layoutDir, localimages.SignaturesDirName, inst.Algorithm().String(), inst.Encoded())
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return localimages.InstanceSignatures{}, fmt.Errorf("creating signature directory %q: %w", dir, err)
	}
	blobs := make([]localimages.SignatureBlob, 0, len(sigs))
	for i, sig := range sigs {
		path := filepath.Join(dir, strconv.Itoa(i))
		if err := os.WriteFile(path, sig, 0o600); err != nil {
			return localimages.InstanceSignatures{}, fmt.Errorf("writing signature %s[%d]: %w", inst, i, err)
		}
		sum := sha256.Sum256(sig)
		blobs = append(blobs, localimages.SignatureBlob{
			SHA256: hex.EncodeToString(sum[:]),
			Size:   len(sig),
		})
	}
	return localimages.InstanceSignatures{InstanceDigest: inst.String(), Blobs: blobs}, nil
}
