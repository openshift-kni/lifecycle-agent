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
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
)

// ---- fakes for the injectable seams (no network, no podman, no real ISO) ----

type fakeISOFetcher struct {
	content []byte
	err     error
	gotURL  string
	called  bool
}

func (f *fakeISOFetcher) FetchToFile(_ context.Context, url, destPath string) error {
	f.called = true
	f.gotURL = url
	if f.err != nil {
		return f.err
	}
	return os.WriteFile(destPath, f.content, 0o600)
}

type fakeLCAExtractor struct {
	content   []byte
	err       error
	gotImage  string
	gotAuth   string
	gotNoAuth bool
}

func (f *fakeLCAExtractor) ExtractLCABinary(lcaImage, authFile, destPath string) error {
	f.gotImage = lcaImage
	f.gotAuth = authFile
	f.gotNoAuth = authFile == ""
	if f.err != nil {
		return f.err
	}
	return os.WriteFile(destPath, f.content, embeddedLCABinaryMode)
}

type fakeISOCustomizer struct {
	gotIgnition []byte
	gotBase     string
	gotOut      string
	err         error
}

func (f *fakeISOCustomizer) EmbedIgnition(_ logrus.FieldLogger, ignitionBytes []byte, baseISO, outISO string) error {
	f.gotIgnition = append([]byte(nil), ignitionBytes...)
	f.gotBase = baseISO
	f.gotOut = outISO
	if f.err != nil {
		return f.err
	}
	return os.WriteFile(outISO, []byte("customized-iso"), 0o600)
}

// ---- test helpers ----

// decodeIgnitionSource reverses gzipBase64DataURL so a test can assert the embedded file
// contents byte-for-byte.
func decodeIgnitionSource(t *testing.T, source string) []byte {
	t.Helper()
	const prefix = "data:;base64,"
	require.True(t, strings.HasPrefix(source, prefix), "source must be a base64 data URL, got %q", source)
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(source, prefix))
	require.NoError(t, err)
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	require.NoError(t, err)
	defer func() { _ = gr.Close() }()
	out, err := io.ReadAll(gr)
	require.NoError(t, err)
	return out
}

// parsedIgnition is the subset of the embedded ignition the tests assert on.
type parsedIgnition struct {
	Ignition struct {
		Version string `json:"version"`
	} `json:"ignition"`
	Storage struct {
		Files []struct {
			Path     string `json:"path"`
			Mode     int    `json:"mode"`
			Contents struct {
				Source      string `json:"source"`
				Compression string `json:"compression"`
			} `json:"contents"`
		} `json:"files"`
	} `json:"storage"`
	Systemd struct {
		Units []struct {
			Name     string `json:"name"`
			Enabled  bool   `json:"enabled"`
			Contents string `json:"contents"`
		} `json:"units"`
	} `json:"systemd"`
}

func (p parsedIgnition) file(t *testing.T, path string) struct {
	Path     string `json:"path"`
	Mode     int    `json:"mode"`
	Contents struct {
		Source      string `json:"source"`
		Compression string `json:"compression"`
	} `json:"contents"`
} {
	t.Helper()
	for _, f := range p.Storage.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("no storage file at %q in embedded ignition", path)
	return p.Storage.Files[0]
}

func TestFetchAndVerifyISO(t *testing.T) {
	content := []byte("rhcos-live-iso-bytes")
	matchSum := fmt.Sprintf("%x", sha256.Sum256(content))
	// A syntactically valid but wrong digest (64 lowercase hex) to drive the mismatch case.
	otherSum := strings.Repeat("a", 64)

	t.Run("accepts a matching sha256", func(t *testing.T) {
		f := &fakeISOFetcher{content: content}
		dest := filepath.Join(t.TempDir(), "iso")
		err := fetchAndVerifyISO(context.Background(), f,
			RHCOSLiveISO{URL: "https://example.com/rhcos.iso", SHA256: matchSum}, dest)
		require.NoError(t, err)
		assert.Equal(t, "https://example.com/rhcos.iso", f.gotURL)
		got, rerr := os.ReadFile(dest)
		require.NoError(t, rerr)
		assert.Equal(t, content, got)
	})

	t.Run("rejects a sha256 mismatch", func(t *testing.T) {
		f := &fakeISOFetcher{content: content}
		dest := filepath.Join(t.TempDir(), "iso")
		err := fetchAndVerifyISO(context.Background(), f,
			RHCOSLiveISO{URL: "https://example.com/rhcos.iso", SHA256: otherSum}, dest)
		require.ErrorContains(t, err, "sha256 mismatch")
	})

	t.Run("rejects a non-https url before fetching", func(t *testing.T) {
		f := &fakeISOFetcher{content: content}
		dest := filepath.Join(t.TempDir(), "iso")
		err := fetchAndVerifyISO(context.Background(), f,
			RHCOSLiveISO{URL: "http://example.com/rhcos.iso", SHA256: matchSum}, dest)
		require.ErrorContains(t, err, "must be an https url")
		assert.False(t, f.called, "fetch must not run for a non-https url")
	})

	t.Run("rejects a missing or unreadable source", func(t *testing.T) {
		f := &fakeISOFetcher{err: fmt.Errorf("404 not found")}
		dest := filepath.Join(t.TempDir(), "iso")
		err := fetchAndVerifyISO(context.Background(), f,
			RHCOSLiveISO{URL: "https://example.com/rhcos.iso", SHA256: matchSum}, dest)
		require.ErrorContains(t, err, "fetching rhcos live ISO")
		require.ErrorContains(t, err, "404 not found")
	})
}

// TestHTTPISOFetcher exercises the production fetcher's transport-level behavior (HTTPS
// download, non-200 rejection, and refusal to follow a redirect to a non-https URL) against
// an httptest TLS server -- the part fetchAndVerifyISO's fake cannot cover.
func TestHTTPISOFetcher(t *testing.T) {
	t.Run("downloads over https", func(t *testing.T) {
		want := []byte("rhcos-iso-payload")
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(want)
		}))
		defer srv.Close()

		f := &httpISOFetcher{log: testLogger(), transport: srv.Client().Transport}
		dest := filepath.Join(t.TempDir(), "iso")
		require.NoError(t, f.FetchToFile(context.Background(), srv.URL, dest))
		got, err := os.ReadFile(dest)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	})

	t.Run("rejects a non-200 status", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		f := &httpISOFetcher{log: testLogger(), transport: srv.Client().Transport}
		err := f.FetchToFile(context.Background(), srv.URL, filepath.Join(t.TempDir(), "iso"))
		require.ErrorContains(t, err, "unexpected http status")
	})

	t.Run("refuses a redirect to a non-https url", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Redirect(w, &http.Request{}, "http://insecure.example.com/rhcos.iso", http.StatusFound)
		}))
		defer srv.Close()

		f := &httpISOFetcher{log: testLogger(), transport: srv.Client().Transport}
		err := f.FetchToFile(context.Background(), srv.URL, filepath.Join(t.TempDir(), "iso"))
		require.ErrorContains(t, err, "refusing redirect to non-https")
	})
}

func TestRenderEmbeddedIgnition(t *testing.T) {
	binary := []byte("exact-shipping-lca-cli-binary\x00\x01\x02")

	tests := []struct {
		name         string
		manifest     func() *Manifest
		wantShutdown bool
	}{
		{"install-only ends in shutdown", validInstallOnly, true},
		{"self-contained pivots (no shutdown)", validSelfContained, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := renderEmbeddedIgnition(tc.manifest(), binary)
			require.NoError(t, err)

			var ign parsedIgnition
			require.NoError(t, json.Unmarshal(out, &ign))
			assert.Equal(t, common.IgnitionVersion32, ign.Ignition.Version)

			// The lca-cli binary is embedded executable and byte-for-byte.
			bin := ign.file(t, embeddedLCABinaryPath)
			assert.Equal(t, embeddedLCABinaryMode, bin.Mode)
			assert.Equal(t, "gzip", bin.Contents.Compression)
			assert.Equal(t, binary, decodeIgnitionSource(t, bin.Contents.Source))

			// The embedded IBIPrepareConfig is present and correctly derived from the manifest.
			cfgFile := ign.file(t, embeddedIBIConfigPath)
			assert.Equal(t, embeddedIBIConfigMode, cfgFile.Mode)
			var cfg map[string]any
			require.NoError(t, json.Unmarshal(decodeIgnitionSource(t, cfgFile.Contents.Source), &cfg))
			assert.Equal(t, true, cfg["disconnected"], "disconnected must be true")
			assert.Equal(t, common.LocalImagesMountPath, cfg["localImagesPath"])
			assert.Equal(t, filepath.Join(common.LocalImagesMountPath, digestManifestFileName), cfg["expectedDigestsPath"])
			assert.Equal(t, testSeedImage, cfg["seedImage"])
			assert.Equal(t, testInstallDisk, cfg["installationDisk"])
			if tc.wantShutdown {
				assert.Equal(t, true, cfg["shutdown"], "install-only must shut down")
			} else {
				// shutdown:false is the zero value and omitempty drops it from the JSON.
				_, present := cfg["shutdown"]
				assert.False(t, present, "self-contained must not shut down (pivot)")
			}

			// The oneshot unit auto-runs the embedded binary against the embedded config.
			require.Len(t, ign.Systemd.Units, 1)
			unit := ign.Systemd.Units[0]
			assert.Equal(t, lcaIBIServiceName, unit.Name)
			assert.True(t, unit.Enabled)
			assert.Contains(t, unit.Contents, "Type=oneshot")
			assert.Contains(t, unit.Contents,
				fmt.Sprintf("ExecStart=%s ibi -f %s", embeddedLCABinaryPath, embeddedIBIConfigPath))
			assert.Contains(t, unit.Contents, "WantedBy=multi-user.target")
		})
	}
}

func TestExtractLCABinaryEmbedded(t *testing.T) {
	binary := []byte("the-only-lca-cli-that-ships\x7fELF\x02")

	// The extractor writes the exact bytes it was given...
	ext := &fakeLCAExtractor{content: binary}
	dest := filepath.Join(t.TempDir(), "lca-cli")
	require.NoError(t, ext.ExtractLCABinary("quay.io/example/lca@sha256:"+validSHA256, "", dest))
	extracted, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, binary, extracted)

	// ...and that is exactly what is embedded on p1 (byte-for-byte through the ignition).
	ign, err := renderEmbeddedIgnition(validInstallOnly(), extracted)
	require.NoError(t, err)
	var parsed parsedIgnition
	require.NoError(t, json.Unmarshal(ign, &parsed))
	embedded := decodeIgnitionSource(t, parsed.file(t, embeddedLCABinaryPath).Contents.Source)
	assert.Equal(t, binary, embedded)
}

func TestBuildLiveISO(t *testing.T) {
	binary := []byte("lca-cli-binary")
	isoContent := []byte("base-rhcos-live-iso")
	sum := fmt.Sprintf("%x", sha256.Sum256(isoContent))

	m := validInstallOnly()
	m.RHCOSLiveISO = RHCOSLiveISO{URL: "https://example.com/rhcos.iso", SHA256: sum}

	fetcher := &fakeISOFetcher{content: isoContent}
	extractor := &fakeLCAExtractor{content: binary}
	customizer := &fakeISOCustomizer{}
	deps := isoBuildDeps{fetcher: fetcher, extractor: extractor, customizer: customizer}

	work := t.TempDir()
	out, err := buildLiveISO(context.Background(), testLogger(), m,
		ISOBuildOptions{LCAImage: "quay.io/example/lca@sha256:" + validSHA256, WorkDir: work}, deps)
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(work, customizedISOFileName), out)
	assert.Equal(t, "quay.io/example/lca@sha256:"+validSHA256, extractor.gotImage)
	assert.Equal(t, filepath.Join(work, baseISOFileName), customizer.gotBase)
	assert.Equal(t, out, customizer.gotOut)

	// The customizer received the embedded ignition with the exact extracted binary.
	var parsed parsedIgnition
	require.NoError(t, json.Unmarshal(customizer.gotIgnition, &parsed))
	assert.Equal(t, binary, decodeIgnitionSource(t, parsed.file(t, embeddedLCABinaryPath).Contents.Source))
	assert.FileExists(t, out)
}

func TestBuildLiveISORequiresInputs(t *testing.T) {
	deps := isoBuildDeps{fetcher: &fakeISOFetcher{}, extractor: &fakeLCAExtractor{}, customizer: &fakeISOCustomizer{}}

	_, err := buildLiveISO(context.Background(), testLogger(), validInstallOnly(),
		ISOBuildOptions{LCAImage: "quay.io/example/lca@sha256:" + validSHA256}, deps)
	require.ErrorContains(t, err, "work directory is required")

	_, err = buildLiveISO(context.Background(), testLogger(), validInstallOnly(),
		ISOBuildOptions{WorkDir: t.TempDir()}, deps)
	require.ErrorContains(t, err, "lca-cli image is required")
}
