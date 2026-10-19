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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/types"
)

// preservePinnedDockerManifest restores the source's exact Docker manifest bytes
// after containers/image has converted them for the OCI destination. The source
// digest then names a real layout descriptor and remains usable by the offline
// importer; merely recording a source-to-OCI mapping would leave digest-pinned
// requests in the node's image store unresolved.
func preservePinnedDockerManifest(ctx context.Context, ref types.ImageReference, layoutDir, tag, authFile string, pinned digest.Digest) error {
	source, err := ref.NewImageSource(ctx, &types.SystemContext{AuthFilePath: authFile})
	if err != nil {
		return fmt.Errorf("opening pinned image source: %w", err)
	}
	defer source.Close()

	mediaType, size, err := preserveSourceManifest(ctx, source, layoutDir, nil, pinned)
	if err != nil {
		return fmt.Errorf("preserving pinned Docker manifest %s: %w", pinned, err)
	}
	if mediaType != manifest.DockerV2Schema2MediaType && mediaType != manifest.DockerV2ListMediaType {
		return fmt.Errorf("copied digest differs from pinned source digest %s and source media type %q is not Docker schema 2", pinned, mediaType)
	}

	index, err := readLayoutIndex(layoutDir)
	if err != nil {
		return err
	}
	matches := 0
	for i := range index.Manifests {
		if index.Manifests[i].Annotations[imgspecv1.AnnotationRefName] == tag {
			index.Manifests[i].Digest = pinned
			index.Manifests[i].MediaType = mediaType
			index.Manifests[i].Size = size
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("layout tag %q has %d descriptors, want one", tag, matches)
	}
	data, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("marshaling layout index: %w", err)
	}
	if err := os.WriteFile(filepath.Join(layoutDir, "index.json"), data, 0o600); err != nil {
		return fmt.Errorf("writing layout index: %w", err)
	}
	return nil
}

// preserveSourceManifest copies the original manifest graph and any blobs that
// conversion omitted. Newly fetched bytes are checked against their source
// digests; blobs already written by copy.Image are reused.
func preserveSourceManifest(ctx context.Context, source types.ImageSource, layoutDir string, instance *digest.Digest, expected digest.Digest) (string, int64, error) {
	data, mediaType, err := source.GetManifest(ctx, instance)
	if err != nil {
		return "", 0, err
	}
	if err := expected.Validate(); err != nil {
		return "", 0, err
	}
	if got := expected.Algorithm().FromBytes(data); got != expected {
		return "", 0, fmt.Errorf("source manifest digest %s differs from expected %s", got, expected)
	}
	if mediaType == "" {
		mediaType = manifest.GuessMIMEType(data)
	}

	var graph struct {
		Manifests []imgspecv1.Descriptor `json:"manifests"`
		Config    imgspecv1.Descriptor   `json:"config"`
		Layers    []imgspecv1.Descriptor `json:"layers"`
	}
	if err := json.Unmarshal(data, &graph); err != nil {
		return "", 0, err
	}
	switch mediaType {
	case manifest.DockerV2ListMediaType, imgspecv1.MediaTypeImageIndex:
		if len(graph.Manifests) == 0 {
			return "", 0, fmt.Errorf("Docker manifest list %s has no instances", expected)
		}
		for _, child := range graph.Manifests {
			if _, _, err := preserveSourceManifest(ctx, source, layoutDir, &child.Digest, child.Digest); err != nil {
				return "", 0, err
			}
		}
	case manifest.DockerV2Schema2MediaType, imgspecv1.MediaTypeImageManifest:
		if err := preserveSourceBlob(ctx, source, layoutDir, graph.Config); err != nil {
			return "", 0, err
		}
		for _, layer := range graph.Layers {
			if err := preserveSourceBlob(ctx, source, layoutDir, layer); err != nil {
				return "", 0, err
			}
		}
	default:
		return "", 0, fmt.Errorf("unsupported source manifest media type %q", mediaType)
	}
	if err := os.MkdirAll(filepath.Dir(layoutBlobPath(layoutDir, expected)), 0o750); err != nil {
		return "", 0, err
	}
	if err := os.WriteFile(layoutBlobPath(layoutDir, expected), data, 0o600); err != nil {
		return "", 0, err
	}
	return mediaType, int64(len(data)), nil
}

func layoutBlobPath(layoutDir string, dgst digest.Digest) string {
	return filepath.Join(layoutDir, "blobs", dgst.Algorithm().String(), dgst.Encoded())
}

func preserveSourceBlob(ctx context.Context, source types.ImageSource, layoutDir string, desc imgspecv1.Descriptor) error {
	if err := desc.Digest.Validate(); err != nil {
		return err
	}
	path := layoutBlobPath(layoutDir, desc.Digest)
	if _, err := os.Stat(path); err == nil {
		return nil // copy.Image already verified and wrote this blob.
	} else if !os.IsNotExist(err) {
		return err
	}
	reader, _, err := source.GetBlob(ctx, types.BlobInfo{Digest: desc.Digest, Size: desc.Size, MediaType: desc.MediaType, URLs: desc.URLs}, nil)
	if err != nil {
		return fmt.Errorf("reading source blob %s: %w", desc.Digest, err)
	}
	defer reader.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".blob-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	digester := desc.Digest.Algorithm().Digester()
	size, copyErr := io.Copy(io.MultiWriter(file, digester.Hash()), reader)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if digester.Digest() != desc.Digest || (desc.Size >= 0 && size != desc.Size) {
		return fmt.Errorf("source blob %s failed digest or size verification", desc.Digest)
	}
	return os.Rename(file.Name(), path)
}
