package createusb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type transientMirrorer struct {
	attempts int
	failFor  int
	err      error
}

func (m *transientMirrorer) MirrorToLayout(_ context.Context, imageRef, layoutDir, tag, _ string) (mirrorResult, error) {
	m.attempts++
	if m.attempts <= m.failFor {
		return mirrorResult{}, fmt.Errorf("copying blob: %w", m.err)
	}
	dgst := fakeDigest(imageRef)
	appendFakeLayoutEntry(layoutDir, tag, dgst)
	return mirrorResult{digest: dgst}, nil
}

func TestMirrorRetriesUnexpectedEOF(t *testing.T) {
	ref := "quay.io/example/image@sha256:" + validSHA256
	m := &transientMirrorer{failFor: 1, err: io.ErrUnexpectedEOF}
	entries, err := mirrorToOCILayout(context.Background(), []string{ref}, filepath.Join(t.TempDir(), "oci"), "", imageBuildDeps{mirrorer: m, mirrorRetryBaseDelay: time.Millisecond})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, 2, m.attempts)
	require.Equal(t, fakeDigest(ref), entries[0].Digest)
}

func TestRetryableMirrorErrorRecognizesUnwrappedEOF(t *testing.T) {
	require.True(t, retryableMirrorError(errors.New("copying blob: unexpected EOF")))
}

func TestMirrorRetriesFallbackTimeout(t *testing.T) {
	ref := "quay.io/example/image@sha256:" + validSHA256
	fallbackErr := errors.New("(Fallback sources also failed: [quay.io/example/image: dial tcp: i/o timeout]): BLOB_UNKNOWN")
	m := &transientMirrorer{failFor: 1, err: fallbackErr}
	_, err := mirrorToOCILayout(context.Background(), []string{ref}, filepath.Join(t.TempDir(), "oci"), "", imageBuildDeps{mirrorer: m, mirrorRetryBaseDelay: time.Millisecond})
	require.NoError(t, err)
	require.Equal(t, 2, m.attempts)
	require.False(t, retryableMirrorError(errors.New("BLOB_UNKNOWN")))
	require.False(t, retryableMirrorError(errors.New("(Fallback sources also failed: [quay.io/example/image: unauthorized]): BLOB_UNKNOWN")))
}

func TestMirrorDoesNotRetryPermanentError(t *testing.T) {
	ref := "quay.io/example/image@sha256:" + validSHA256
	m := &transientMirrorer{failFor: 3, err: errors.New("unauthorized")}
	_, err := mirrorToOCILayout(context.Background(), []string{ref}, filepath.Join(t.TempDir(), "oci"), "", imageBuildDeps{mirrorer: m})
	require.ErrorContains(t, err, "unauthorized")
	require.ErrorIs(t, err, m.err)
	require.Equal(t, 1, m.attempts)
}

func TestMirrorStopsAfterThreeTransportFailures(t *testing.T) {
	ref := "quay.io/example/image@sha256:" + validSHA256
	m := &transientMirrorer{failFor: 3, err: io.ErrUnexpectedEOF}
	_, err := mirrorToOCILayout(context.Background(), []string{ref}, filepath.Join(t.TempDir(), "oci"), "", imageBuildDeps{mirrorer: m, mirrorRetryBaseDelay: time.Millisecond})
	require.ErrorContains(t, err, "after 3 attempts")
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Equal(t, 3, m.attempts)
}
