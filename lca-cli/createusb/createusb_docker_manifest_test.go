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
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.podman.io/image/v5/pkg/blobinfocache/memory"
	"go.podman.io/image/v5/types"
)

// fakeBlobSource is a types.ImageSource that serves one fixed blob and records the
// BlobInfoCache it was handed. A real (registry/Docker) source calls
// cache.RecordKnownLocation inside GetBlob and panics on a nil cache, so the cache
// passed by preserveSourceBlob must never be nil.
type fakeBlobSource struct {
	types.ImageSource
	blob      []byte
	sawNil    bool
	callCount int
}

func (f *fakeBlobSource) GetBlob(_ context.Context, _ types.BlobInfo, cache types.BlobInfoCache) (io.ReadCloser, int64, error) {
	f.callCount++
	if cache == nil {
		f.sawNil = true
	}
	return io.NopCloser(bytes.NewReader(f.blob)), int64(len(f.blob)), nil
}

// TestPreserveSourceBlobPassesNonNilCache is the regression guard for the install-only
// build panic: preserveSourceBlob must hand GetBlob a non-nil BlobInfoCache, otherwise
// a real Docker source panics in cache.RecordKnownLocation. It also confirms the fetched
// blob is verified and published into the layout.
func TestPreserveSourceBlobPassesNonNilCache(t *testing.T) {
	layoutDir := t.TempDir()
	blob := []byte("layer-bytes")
	dgst := digest.FromBytes(blob)
	src := &fakeBlobSource{blob: blob}

	err := preserveSourceBlob(context.Background(), src, layoutDir, memory.New(), imgspecv1.Descriptor{
		Digest: dgst,
		Size:   int64(len(blob)),
	})
	require.NoError(t, err)
	assert.False(t, src.sawNil, "preserveSourceBlob must pass a non-nil BlobInfoCache to GetBlob")
	assert.Equal(t, 1, src.callCount)

	// The verified blob is published at its content-addressed layout path.
	got, err := os.ReadFile(layoutBlobPath(layoutDir, dgst)) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.Equal(t, blob, got)
}
