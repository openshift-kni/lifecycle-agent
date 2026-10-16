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
	"testing"

	"go.podman.io/image/v5/oci/layout"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/localimages"
)

const (
	// validSHA256 is a well-formed 64-character lowercase hex digest (sha256 of empty input).
	validSHA256     = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	testSeedImage   = "quay.io/example/seed@sha256:" + validSHA256
	testSeedVersion = "4.20.0"
	testRelease     = "4.20.0"
	testInstallDisk = "/dev/disk/by-id/ata-example"
	testISOURL      = "https://example.com/rhcos-live.x86_64.iso"
	validPullSecret = `{"auths":{"registry.example.com":{"auth":"dXNlcjpwYXNz"}}}`
)

// writeManifest writes content to a temp file and returns its path.
func writeManifest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// ptrTo returns a pointer to v (used for the *bool Shutdown field).
func ptrTo[T any](v T) *T { return &v }

// validInstallOnly returns a fully valid install-only manifest.
func validInstallOnly() *Manifest {
	return &Manifest{
		Mode:                ModeInstallOnly,
		SeedImage:           testSeedImage,
		SeedVersion:         testSeedVersion,
		ReleaseVersion:      testRelease,
		InstallationDisk:    testInstallDisk,
		Disconnected:        true,
		Shutdown:            ptrTo(true),
		RHCOSLiveISO:        RHCOSLiveISO{URL: testISOURL, SHA256: validSHA256},
		ExtraPrecacheImages: &[]string{"quay.io/example/extra@sha256:" + validSHA256},
	}
}

// validSelfContained returns a fully valid self-contained manifest.
func validSelfContained() *Manifest {
	return &Manifest{
		Mode:             ModeSelfContained,
		SeedImage:        testSeedImage,
		SeedVersion:      testSeedVersion,
		ReleaseVersion:   testRelease,
		InstallationDisk: testInstallDisk,
		Disconnected:     true,
		Shutdown:         ptrTo(false),
		RHCOSLiveISO:     RHCOSLiveISO{URL: testISOURL, SHA256: validSHA256},
		ClusterManifests: &[]ManifestSource{{Path: "manifests/cluster.yaml"}},
		SiteConfig: &SiteConfig{
			ClusterName: "example",
			BaseDomain:  "example.com",
			Hostname:    "node0",
			PullSecret:  validPullSecret,
		},
	}
}

func TestParseManifestBothProfiles(t *testing.T) {
	installOnly := `mode: install-only
seedImage: ` + testSeedImage + `
seedVersion: ` + testSeedVersion + `
releaseVersion: ` + testRelease + `
installationDisk: ` + testInstallDisk + `
disconnected: true
shutdown: true
rhcosLiveIso:
  url: ` + testISOURL + `
  sha256: ` + validSHA256 + `
extraPrecacheImages:
  - quay.io/example/extra@sha256:` + validSHA256 + `
`
	m, err := ParseManifest(writeManifest(t, installOnly))
	require.NoError(t, err)
	assert.Equal(t, ModeInstallOnly, m.Mode)
	assert.Equal(t, testSeedImage, m.SeedImage)
	assert.Equal(t, testRelease, m.ReleaseVersion)
	assert.True(t, m.Disconnected)
	require.NotNil(t, m.Shutdown)
	assert.True(t, *m.Shutdown)
	assert.Equal(t, testISOURL, m.RHCOSLiveISO.URL)
	assert.Equal(t, validSHA256, m.RHCOSLiveISO.SHA256)
	require.NotNil(t, m.ExtraPrecacheImages)
	assert.Equal(t, []string{"quay.io/example/extra@sha256:" + validSHA256}, *m.ExtraPrecacheImages)
	assert.NoError(t, m.Validate())

	selfContained := `mode: self-contained
seedImage: ` + testSeedImage + `
seedVersion: ` + testSeedVersion + `
releaseVersion: ` + testRelease + `
installationDisk: ` + testInstallDisk + `
disconnected: true
shutdown: false
rhcosLiveIso:
  url: ` + testISOURL + `
  sha256: ` + validSHA256 + `
clusterManifests:
  - path: manifests/cluster.yaml
siteConfig:
  clusterName: example
  baseDomain: example.com
  hostname: node0
  pullSecret: '` + validPullSecret + `'
`
	m, err = ParseManifest(writeManifest(t, selfContained))
	require.NoError(t, err)
	assert.Equal(t, ModeSelfContained, m.Mode)
	require.NotNil(t, m.Shutdown)
	assert.False(t, *m.Shutdown)
	require.NotNil(t, m.SiteConfig)
	assert.Equal(t, "example", m.SiteConfig.ClusterName)
	assert.Equal(t, validPullSecret, m.SiteConfig.PullSecret)
	require.NotNil(t, m.ClusterManifests)
	assert.Len(t, *m.ClusterManifests, 1)
	assert.Equal(t, "manifests/cluster.yaml", (*m.ClusterManifests)[0].Path)
	assert.NoError(t, m.Validate())
}

func TestParseManifestStrict(t *testing.T) {
	t.Run("unknown field fails", func(t *testing.T) {
		_, err := ParseManifest(writeManifest(t, "mode: install-only\nbogusField: nope\n"))
		assert.Error(t, err)
	})
	t.Run("duplicate key fails", func(t *testing.T) {
		_, err := ParseManifest(writeManifest(t, "mode: install-only\nmode: self-contained\n"))
		assert.Error(t, err)
	})
	t.Run("nested unknown field fails", func(t *testing.T) {
		_, err := ParseManifest(writeManifest(t, "mode: install-only\nrhcosLiveIso:\n  url: https://example.com/x.iso\n  bogus: y\n"))
		assert.Error(t, err)
	})
	t.Run("missing file fails", func(t *testing.T) {
		_, err := ParseManifest(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
		assert.Error(t, err)
	})
	t.Run("well-formed manifest passes", func(t *testing.T) {
		_, err := ParseManifest(writeManifest(t, "mode: install-only\nseedVersion: 4.20.0\n"))
		assert.NoError(t, err)
	})
}

func TestValidateDisconnectedMustBeTrue(t *testing.T) {
	for _, mode := range []Mode{ModeInstallOnly, ModeSelfContained} {
		t.Run(string(mode), func(t *testing.T) {
			m := validInstallOnly()
			if mode == ModeSelfContained {
				m = validSelfContained()
			}
			m.Disconnected = false
			assert.ErrorContains(t, m.Validate(), "disconnected must be true")

			m.Disconnected = true
			assert.NoError(t, m.Validate())
		})
	}
}

func TestValidateShutdownMatchesMode(t *testing.T) {
	t.Run("install-only requires shutdown true", func(t *testing.T) {
		m := validInstallOnly()
		m.Shutdown = ptrTo(false)
		assert.ErrorContains(t, m.Validate(), "shutdown must be true")
	})
	t.Run("self-contained requires shutdown false", func(t *testing.T) {
		m := validSelfContained()
		m.Shutdown = ptrTo(true)
		assert.ErrorContains(t, m.Validate(), "shutdown must be false")
	})
	// Regression for the review's P2: an omitted shutdown decodes to a nil pointer and must
	// fail as a missing required field rather than defaulting to the self-contained-valid
	// false (an explicit false still succeeds for self-contained).
	t.Run("omitted shutdown fails for self-contained", func(t *testing.T) {
		m := validSelfContained()
		m.Shutdown = nil
		assert.ErrorContains(t, m.Validate(), "shutdown is required")
	})
	t.Run("omitted shutdown fails for install-only", func(t *testing.T) {
		m := validInstallOnly()
		m.Shutdown = nil
		assert.ErrorContains(t, m.Validate(), "shutdown is required")
	})
	t.Run("omitted shutdown via strict parse fails", func(t *testing.T) {
		// A self-contained manifest with no shutdown key must fail (missing boolean would
		// otherwise decode to the self-contained-valid false).
		selfContained := `mode: self-contained
seedImage: ` + testSeedImage + `
seedVersion: ` + testSeedVersion + `
releaseVersion: ` + testRelease + `
installationDisk: ` + testInstallDisk + `
disconnected: true
rhcosLiveIso:
  url: ` + testISOURL + `
  sha256: ` + validSHA256 + `
clusterManifests:
  - path: manifests/cluster.yaml
siteConfig:
  clusterName: example
  baseDomain: example.com
  hostname: node0
  pullSecret: '` + validPullSecret + `'
`
		m, err := ParseManifest(writeManifest(t, selfContained))
		require.NoError(t, err)
		assert.Nil(t, m.Shutdown)
		assert.ErrorContains(t, m.Validate(), "shutdown is required")
	})
}

func TestValidateModeFieldPresence(t *testing.T) {
	t.Run("siteConfig under install-only fails", func(t *testing.T) {
		m := validInstallOnly()
		m.SiteConfig = &SiteConfig{PullSecret: validPullSecret}
		assert.ErrorContains(t, m.Validate(), "siteConfig is only valid")
	})
	t.Run("clusterManifests under install-only fails", func(t *testing.T) {
		m := validInstallOnly()
		m.ClusterManifests = &[]ManifestSource{{Path: "x"}}
		assert.ErrorContains(t, m.Validate(), "clusterManifests is only valid")
	})
	t.Run("extraManifests under install-only fails", func(t *testing.T) {
		m := validInstallOnly()
		m.ExtraManifests = &[]ManifestSource{{Path: "x"}}
		assert.ErrorContains(t, m.Validate(), "extraManifests is only valid")
	})
	t.Run("extraPrecacheImages under self-contained fails", func(t *testing.T) {
		m := validSelfContained()
		m.ExtraPrecacheImages = &[]string{"quay.io/example/extra@sha256:" + validSHA256}
		assert.ErrorContains(t, m.Validate(), "extraPrecacheImages is only valid")
	})
	// Regression for the review's P2: presence (not length) disqualifies a field under
	// the wrong mode — an explicit empty list must still be rejected.
	t.Run("empty clusterManifests under install-only fails", func(t *testing.T) {
		m := validInstallOnly()
		m.ClusterManifests = &[]ManifestSource{}
		assert.ErrorContains(t, m.Validate(), "clusterManifests is only valid")
	})
	t.Run("empty extraManifests under install-only fails", func(t *testing.T) {
		m := validInstallOnly()
		m.ExtraManifests = &[]ManifestSource{}
		assert.ErrorContains(t, m.Validate(), "extraManifests is only valid")
	})
	t.Run("empty extraPrecacheImages under self-contained fails", func(t *testing.T) {
		m := validSelfContained()
		m.ExtraPrecacheImages = &[]string{}
		assert.ErrorContains(t, m.Validate(), "extraPrecacheImages is only valid")
	})
	t.Run("empty list via strict parse is rejected under wrong mode", func(t *testing.T) {
		// An install-only manifest that explicitly sets clusterManifests: [] must fail.
		m, err := ParseManifest(writeManifest(t, "mode: install-only\nclusterManifests: []\n"))
		require.NoError(t, err)
		require.NotNil(t, m.ClusterManifests)
		assert.ErrorContains(t, m.Validate(), "clusterManifests is only valid")
	})
	t.Run("valid combinations pass", func(t *testing.T) {
		assert.NoError(t, validInstallOnly().Validate())
		assert.NoError(t, validSelfContained().Validate())
	})
}

func TestValidateSelfContainedRequiresPullSecret(t *testing.T) {
	t.Run("missing siteConfig fails", func(t *testing.T) {
		m := validSelfContained()
		m.SiteConfig = nil
		assert.ErrorContains(t, m.Validate(), "siteConfig is required")
	})
	t.Run("empty pullSecret fails", func(t *testing.T) {
		m := validSelfContained()
		m.SiteConfig.PullSecret = ""
		assert.ErrorContains(t, m.Validate(), "pullSecret is required")
	})
	t.Run("invalid-json pullSecret fails", func(t *testing.T) {
		m := validSelfContained()
		m.SiteConfig.PullSecret = "not-json"
		assert.ErrorContains(t, m.Validate(), "not valid json")
	})
	t.Run("missing identity fields fail", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			mutate   func(*SiteConfig)
			expected string
		}{
			{"clusterName", func(s *SiteConfig) { s.ClusterName = "" }, "clusterName is required"},
			{"baseDomain", func(s *SiteConfig) { s.BaseDomain = "" }, "baseDomain is required"},
			{"hostname", func(s *SiteConfig) { s.Hostname = "" }, "hostname is required"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				m := validSelfContained()
				tc.mutate(m.SiteConfig)
				assert.ErrorContains(t, m.Validate(), tc.expected)
			})
		}
	})
	t.Run("missing clusterManifests fails", func(t *testing.T) {
		m := validSelfContained()
		m.ClusterManifests = nil
		assert.ErrorContains(t, m.Validate(), "clusterManifests is required")
	})
	t.Run("empty clusterManifests fails", func(t *testing.T) {
		m := validSelfContained()
		m.ClusterManifests = &[]ManifestSource{}
		assert.ErrorContains(t, m.Validate(), "clusterManifests is required")
	})
	// Regression for the review's P2: a source with an empty path must be rejected — there
	// is no file for the later media build to render.
	t.Run("clusterManifests entry with empty path fails", func(t *testing.T) {
		m := validSelfContained()
		m.ClusterManifests = &[]ManifestSource{{Path: ""}}
		assert.ErrorContains(t, m.Validate(), "clusterManifests[0].path is required")
	})
	t.Run("clusterManifests entry with blank path fails", func(t *testing.T) {
		m := validSelfContained()
		m.ClusterManifests = &[]ManifestSource{{Path: "manifests/cluster.yaml"}, {Path: "   "}}
		assert.ErrorContains(t, m.Validate(), "clusterManifests[1].path is required")
	})
	t.Run("extraManifests entry with empty path fails", func(t *testing.T) {
		m := validSelfContained()
		m.ExtraManifests = &[]ManifestSource{{Path: ""}}
		assert.ErrorContains(t, m.Validate(), "extraManifests[0].path is required")
	})
	t.Run("extraManifests with valid paths passes", func(t *testing.T) {
		m := validSelfContained()
		m.ExtraManifests = &[]ManifestSource{{Path: "manifests/extra.yaml"}}
		assert.NoError(t, m.Validate())
	})
}

func TestValidateUnknownOrEmptyMode(t *testing.T) {
	for _, mode := range []Mode{"", "bogus"} {
		m := validInstallOnly()
		m.Mode = mode
		assert.ErrorContains(t, m.Validate(), "mode must be")
	}
}

func TestValidateRequiredFields(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(*Manifest)
		expected string
	}{
		{"missing seedImage", func(m *Manifest) { m.SeedImage = "" }, "seedImage is required"},
		{"non-digest-pinned seedImage", func(m *Manifest) { m.SeedImage = "quay.io/example/seed:latest" }, "digest-pinned"},
		{"empty-digest seedImage", func(m *Manifest) { m.SeedImage = "quay.io/example/seed@sha256:" }, "digest-pinned"},
		{"short-digest seedImage", func(m *Manifest) { m.SeedImage = "quay.io/example/seed@sha256:abcd" }, "digest-pinned"},
		{"no-name seedImage", func(m *Manifest) { m.SeedImage = "@sha256:" + validSHA256 }, "digest-pinned"},
		{"missing seedVersion", func(m *Manifest) { m.SeedVersion = "" }, "seedVersion is required"},
		{"missing releaseVersion", func(m *Manifest) { m.ReleaseVersion = "" }, "releaseVersion is required"},
		{"missing installationDisk", func(m *Manifest) { m.InstallationDisk = "" }, "installationDisk is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := validInstallOnly()
			tc.mutate(m)
			assert.ErrorContains(t, m.Validate(), tc.expected)
		})
	}
}

func TestValidateRHCOSLiveISO(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(*Manifest)
		expected string
	}{
		{"missing url", func(m *Manifest) { m.RHCOSLiveISO.URL = "" }, "url is required"},
		{"non-https url", func(m *Manifest) { m.RHCOSLiveISO.URL = "http://example.com/x.iso" }, "https url"},
		{"https url without host", func(m *Manifest) { m.RHCOSLiveISO.URL = "https://" }, "host"},
		{"scheme-only no host", func(m *Manifest) { m.RHCOSLiveISO.URL = "https:///path/x.iso" }, "host"},
		{"missing sha256", func(m *Manifest) { m.RHCOSLiveISO.SHA256 = "" }, "sha256 is required"},
		{"short sha256", func(m *Manifest) { m.RHCOSLiveISO.SHA256 = "abcd" }, "64-character"},
		{"non-hex sha256", func(m *Manifest) { m.RHCOSLiveISO.SHA256 = "g" + validSHA256[1:] }, "64-character"},
		{"uppercase sha256", func(m *Manifest) { m.RHCOSLiveISO.SHA256 = "E" + validSHA256[1:] }, "64-character"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := validInstallOnly()
			tc.mutate(m)
			assert.ErrorContains(t, m.Validate(), tc.expected)
		})
	}
	t.Run("valid pair passes", func(t *testing.T) {
		assert.NoError(t, validInstallOnly().Validate())
	})
}

func TestResolvePullSecretAtFile(t *testing.T) {
	t.Run("literal json passes through unchanged", func(t *testing.T) {
		m := validSelfContained()
		require.NoError(t, m.ResolvePullSecret())
		assert.Equal(t, validPullSecret, m.SiteConfig.PullSecret)
	})
	t.Run("@file resolves to file contents", func(t *testing.T) {
		secretPath := filepath.Join(t.TempDir(), "pull-secret.json")
		require.NoError(t, os.WriteFile(secretPath, []byte(validPullSecret), 0o600))
		m := validSelfContained()
		m.SiteConfig.PullSecret = "@" + secretPath
		require.NoError(t, m.ResolvePullSecret())
		assert.Equal(t, validPullSecret, m.SiteConfig.PullSecret)
		assert.NotContains(t, m.SiteConfig.PullSecret, "@", "the @ prefix must not survive resolution")
	})
	t.Run("missing @file fails", func(t *testing.T) {
		m := validSelfContained()
		m.SiteConfig.PullSecret = "@" + filepath.Join(t.TempDir(), "missing.json")
		assert.ErrorContains(t, m.ResolvePullSecret(), "failed to resolve pull-secret")
	})
	t.Run("unparseable @file fails validation", func(t *testing.T) {
		secretPath := filepath.Join(t.TempDir(), "bad.json")
		require.NoError(t, os.WriteFile(secretPath, []byte("not-json"), 0o600))
		m := validSelfContained()
		m.SiteConfig.PullSecret = "@" + secretPath
		assert.ErrorContains(t, m.Validate(), "not valid json")
	})
	t.Run("resolution runs before json check in Validate", func(t *testing.T) {
		// Regression test for the review's point 1: a valid @file reference must pass
		// Validate end-to-end (resolution happens before the pull-secret JSON check).
		secretPath := filepath.Join(t.TempDir(), "pull-secret.json")
		require.NoError(t, os.WriteFile(secretPath, []byte(validPullSecret), 0o600))
		m := validSelfContained()
		m.SiteConfig.PullSecret = "@" + secretPath
		assert.NoError(t, m.Validate())
		assert.Equal(t, validPullSecret, m.SiteConfig.PullSecret)
	})
	t.Run("nil siteConfig is a no-op", func(t *testing.T) {
		m := validInstallOnly()
		assert.NoError(t, m.ResolvePullSecret())
	})
}

func TestBuildStubReturnsNotImplemented(t *testing.T) {
	err := Build(validInstallOnly())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not yet implemented")
	assert.Contains(t, err.Error(), "CNF-27167")
}

// ----- image-build stage (createusb_images.go) -----

const (
	testRecertImage = "quay.io/edge-infrastructure/recert@sha256:" + validSHA256
	testLCAImage    = "quay.io/openshift-kni/lifecycle-agent@sha256:" + validSHA256
)

// fakeDigest returns a deterministic, well-formed sha256 digest for s.
func fakeDigest(s string) string {
	h := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(h[:])
}

// fakeSeedReader is a seedClosureReader stub returning a fixed containers.list.
type fakeSeedReader struct {
	list []string
	err  error
}

func (f fakeSeedReader) ReadContainersList(_, _ string) ([]string, error) { return f.list, f.err }

// fakeMirrorer is an imageMirrorer stub. It fails on any reference in failOn and,
// for a successful mirror, appends a matching descriptor to the destination
// layout's index.json so verifyLayoutEntries and the digest capture behave as the
// real layout would.
type fakeMirrorer struct {
	failOn map[string]bool
}

func (f fakeMirrorer) MirrorToLayout(_ context.Context, imageRef, destLayoutDir, ociTag, _ string) (string, error) {
	if f.failOn[imageRef] {
		return "", assert.AnError
	}
	dgst := fakeDigest(imageRef)
	appendFakeLayoutEntry(destLayoutDir, ociTag, dgst)
	return dgst, nil
}

// appendFakeLayoutEntry records a descriptor (digest + ref.name annotation) into
// the layout index.json at destLayoutDir, creating the layout if needed.
func appendFakeLayoutEntry(destLayoutDir, ociTag, dgst string) {
	_ = os.MkdirAll(destLayoutDir, 0o755)
	indexPath := filepath.Join(destLayoutDir, "index.json")
	var index imgspecv1.Index
	if data, err := os.ReadFile(indexPath); err == nil {
		_ = json.Unmarshal(data, &index)
	}
	index.SchemaVersion = 2
	index.Manifests = append(index.Manifests, imgspecv1.Descriptor{
		Digest:      digest.Digest(dgst),
		Annotations: map[string]string{imgspecv1.AnnotationRefName: ociTag},
	})
	data, _ := json.Marshal(index)
	_ = os.WriteFile(indexPath, data, 0o644)
	_ = os.WriteFile(filepath.Join(destLayoutDir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644)
}

func TestResolveImageSetInstallOnly(t *testing.T) {
	seedA := "quay.io/seed/a@sha256:" + validSHA256
	seedB := "quay.io/seed/b@sha256:" + validSHA256
	extra := "quay.io/example/extra@sha256:" + validSHA256

	m := validInstallOnly()
	// The seed closure deliberately omits recert and lca-cli (they must be added from
	// the authoritative inputs), and includes a duplicate of an extra image to prove
	// deduplication.
	m.ExtraPrecacheImages = &[]string{extra, seedA}
	deps := imageBuildDeps{
		seedReader: fakeSeedReader{list: []string{seedA, seedB, seedA}},
		p3Scanner:  notImplementedP3Scanner{},
	}
	opts := ImageSetOptions{RecertImage: testRecertImage, LCAImage: testLCAImage}

	refs, err := resolveImageSet(m, opts, deps)
	require.NoError(t, err)

	// recert and lca-cli are guaranteed present (the checked invariant).
	assert.Contains(t, refs, testRecertImage)
	assert.Contains(t, refs, testLCAImage)
	assert.Contains(t, refs, extra)
	// Deduplicated union: seedA, seedB, recert, lca, extra (no duplicates).
	assert.ElementsMatch(t, []string{seedA, seedB, testRecertImage, testLCAImage, extra}, refs)
	assert.Len(t, refs, 5)
}

func TestResolveImageSetDedupesRecertFromSeedList(t *testing.T) {
	// When the seed containers.list already carries recert, it must not be duplicated.
	m := validInstallOnly()
	m.ExtraPrecacheImages = nil
	deps := imageBuildDeps{
		seedReader: fakeSeedReader{list: []string{testRecertImage}},
		p3Scanner:  notImplementedP3Scanner{},
	}
	opts := ImageSetOptions{RecertImage: testRecertImage, LCAImage: testLCAImage}

	refs, err := resolveImageSet(m, opts, deps)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{testRecertImage, testLCAImage}, refs)
}

func TestResolveImageSetSelfContainedNotImplemented(t *testing.T) {
	m := validSelfContained()
	deps := imageBuildDeps{seedReader: fakeSeedReader{}, p3Scanner: notImplementedP3Scanner{}}

	_, err := resolveImageSet(m, ImageSetOptions{RecertImage: testRecertImage, LCAImage: testLCAImage}, deps)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not implemented")
	assert.Contains(t, err.Error(), "CNF-27169")
}

// fakeP3Scanner is a p3ImageScanner stub returning a fixed image set (or error).
type fakeP3Scanner struct {
	images []string
	err    error
}

func (f fakeP3Scanner) ScanP3Images(_ *Manifest) ([]string, error) { return f.images, f.err }

func TestResolveImageSetSelfContainedUnionsBaseAndScan(t *testing.T) {
	// Regression for the review's P1: self-contained is a superset of install-only, not
	// a replacement. Once the p3 scanner is supplied, the resolved set must still carry
	// the seed closure, recert and lca-cli, with the scan images unioned on top and
	// deduplicated.
	seedA := "quay.io/seed/a@sha256:" + validSHA256
	p3Only := "quay.io/p3/only@sha256:" + validSHA256

	m := validSelfContained()
	deps := imageBuildDeps{
		seedReader: fakeSeedReader{list: []string{seedA}},
		// The scan returns a p3-only image plus a duplicate of recert, proving the scan
		// result is unioned and deduplicated against the base set rather than appended blindly.
		p3Scanner: fakeP3Scanner{images: []string{p3Only, testRecertImage}},
	}
	opts := ImageSetOptions{RecertImage: testRecertImage, LCAImage: testLCAImage}

	refs, err := resolveImageSet(m, opts, deps)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{seedA, testRecertImage, testLCAImage, p3Only}, refs)
}

func TestResolveImageSetSelfContainedScannerErrorFails(t *testing.T) {
	// A scanner failure must fail the build (and short-circuit before the expensive
	// seed-image read), not silently drop the p3 images.
	m := validSelfContained()
	deps := imageBuildDeps{
		seedReader: fakeSeedReader{err: assert.AnError}, // must not be reached
		p3Scanner:  fakeP3Scanner{err: assert.AnError},
	}
	_, err := resolveImageSet(m, ImageSetOptions{RecertImage: testRecertImage, LCAImage: testLCAImage}, deps)
	assert.ErrorContains(t, err, "resolving self-contained image set")
}

func TestResolveImageSetMissingRequiredImageFails(t *testing.T) {
	deps := imageBuildDeps{seedReader: fakeSeedReader{list: []string{"quay.io/seed/a@sha256:" + validSHA256}}, p3Scanner: notImplementedP3Scanner{}}

	t.Run("empty recert fails", func(t *testing.T) {
		_, err := resolveImageSet(validInstallOnly(), ImageSetOptions{RecertImage: "", LCAImage: testLCAImage}, deps)
		assert.ErrorContains(t, err, "recert image is required")
	})
	t.Run("empty lca fails", func(t *testing.T) {
		_, err := resolveImageSet(validInstallOnly(), ImageSetOptions{RecertImage: testRecertImage, LCAImage: "  "}, deps)
		assert.ErrorContains(t, err, "lca-cli image is required")
	})
}

func TestResolveImageSetSeedReaderErrorFails(t *testing.T) {
	deps := imageBuildDeps{seedReader: fakeSeedReader{err: assert.AnError}, p3Scanner: notImplementedP3Scanner{}}
	_, err := resolveImageSet(validInstallOnly(), ImageSetOptions{RecertImage: testRecertImage, LCAImage: testLCAImage}, deps)
	assert.ErrorContains(t, err, "reading seed image closure")
}

func TestMirrorToOCILayoutSuccess(t *testing.T) {
	refs := []string{
		"quay.io/openshift-release-dev/ocp-release@sha256:" + validSHA256,
		"quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:" + validSHA256,
	}
	deps := imageBuildDeps{mirrorer: fakeMirrorer{}}
	layoutDir := filepath.Join(t.TempDir(), "oci")

	entries, err := mirrorToOCILayout(context.Background(), refs, layoutDir, "", deps)
	require.NoError(t, err)
	require.Len(t, entries, 2)

	// Canonical names (digest stripped), pinned digests, and unique valid ociTags.
	assert.Equal(t, "quay.io/openshift-release-dev/ocp-release", entries[0].Name)
	assert.Equal(t, fakeDigest(refs[0]), entries[0].Digest)
	assert.NotEqual(t, entries[0].OCITag, entries[1].OCITag)
}

func TestMirrorFailsOnUnpinnableReference(t *testing.T) {
	bad := "quay.io/example/unpinnable@sha256:" + validSHA256
	refs := []string{"quay.io/example/ok@sha256:" + validSHA256, bad}
	deps := imageBuildDeps{mirrorer: fakeMirrorer{failOn: map[string]bool{bad: true}}}

	entries, err := mirrorToOCILayout(context.Background(), refs, filepath.Join(t.TempDir(), "oci"), "", deps)
	require.Error(t, err)
	assert.Nil(t, entries, "no partial manifest is produced when a reference cannot be pinned")
	assert.Contains(t, err.Error(), bad, "the error must name the offending image")
	assert.Contains(t, err.Error(), "digest-pin")
}

func TestMirrorToOCILayoutEmptySetFails(t *testing.T) {
	_, err := mirrorToOCILayout(context.Background(), nil, filepath.Join(t.TempDir(), "oci"), "", imageBuildDeps{mirrorer: fakeMirrorer{}})
	assert.ErrorContains(t, err, "no images to mirror")
}

func TestEmitDigestManifestSchema(t *testing.T) {
	entries := []localimages.ImageEntry{
		{Name: "quay.io/openshift-release-dev/ocp-release", Digest: fakeDigest("a"), OCITag: "ocp-release-0"},
		{Name: "quay.io/edge-infrastructure/recert", Digest: fakeDigest("b"), OCITag: "recert-1"},
	}
	p2Root := t.TempDir()

	data, err := marshalDigestManifest(entries)
	require.NoError(t, err)
	require.NoError(t, validateDigestManifestBytes(data, p2Root))
	manifestPath := filepath.Join(p2Root, "digests.json")
	require.NoError(t, publishFile(manifestPath, data))

	// Cross-check: the emitted file parses and validates under the consumer's parser,
	// guaranteeing producer/consumer schema parity.
	parsed, err := localimages.ParseDigestManifest(manifestPath)
	require.NoError(t, err)
	assert.Equal(t, 1, parsed.Version)
	assert.NotEmpty(t, parsed.OCILayoutPath)
	require.Len(t, parsed.Images, len(entries))
	for i, e := range entries {
		assert.Equal(t, e.Name, parsed.Images[i].Name)
		assert.Equal(t, e.Digest, parsed.Images[i].Digest)
		assert.Equal(t, e.OCITag, parsed.Images[i].OCITag)
	}
}

func TestDigestManifestWrittenToBothPaths(t *testing.T) {
	entries := []localimages.ImageEntry{{Name: "quay.io/x/y", Digest: fakeDigest("x"), OCITag: "y-0"}}
	dir := t.TempDir()
	p2Root := filepath.Join(dir, "p2")
	require.NoError(t, os.MkdirAll(p2Root, 0o755))
	outputPath := filepath.Join(dir, "usb.img")

	data, err := marshalDigestManifest(entries)
	require.NoError(t, err)
	besidePath := outputPath + ".digests.json"
	p2Path := filepath.Join(p2Root, "digests.json")
	require.NoError(t, publishFile(besidePath, data))
	require.NoError(t, publishFile(p2Path, data))

	beside, err := os.ReadFile(besidePath)
	require.NoError(t, err)
	onMedia, err := os.ReadFile(p2Path)
	require.NoError(t, err)
	assert.Equal(t, beside, onMedia, "the build-side and on-media digest manifests must be byte-identical")
}

func TestComputeSizeReportEstimate(t *testing.T) {
	ociLayoutDir := filepath.Join(t.TempDir(), "oci")
	require.NoError(t, os.MkdirAll(filepath.Join(ociLayoutDir, "blobs"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(ociLayoutDir, "index.json"), make([]byte, 100), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ociLayoutDir, "blobs", "layer"), make([]byte, 400), 0o644))

	// StagedBytes is the image-stage subtotal: the OCI layout plus the on-media digest
	// manifest (digests.json). It is derived from the layout size and the known
	// manifest length, not a raw walk of the staging root, so a transient staging or
	// backup file sitting beside the layout never inflates it.
	const manifestBytes = 25
	report, err := computeSizeReport(ociLayoutDir, manifestBytes)
	require.NoError(t, err)
	assert.Equal(t, int64(500), report.OCILayoutBytes, "OCI-layout subtotal")
	assert.Equal(t, int64(525), report.StagedBytes, "OCI layout plus the on-media digest manifest")
}

func TestOCITagIsValidAndUnique(t *testing.T) {
	refs := []string{
		"quay.io/openshift-release-dev/ocp-release@sha256:" + validSHA256,
		"quay.io/openshift-release-dev/ocp-release:4.20", // same base name, different ref
		"registry.example.com:5000/team/app:latest",
		"@sha256:" + validSHA256, // degenerate
	}
	used := make(map[string]struct{})
	layoutDir := t.TempDir()
	seen := make(map[string]bool)
	for i, ref := range refs {
		tag := uniqueOCITag(ref, i, used)
		assert.False(t, seen[tag], "ociTag %q must be unique", tag)
		seen[tag] = true
		// A valid org.opencontainers.image.ref.name is accepted by the OCI layout.
		_, err := layout.NewReference(layoutDir, tag)
		assert.NoError(t, err, "ociTag %q must be a valid org.opencontainers.image.ref.name", tag)
	}
}

func TestBuildImageLayoutAtomicAndSuccess(t *testing.T) {
	seedImg := "quay.io/seed/a@sha256:" + validSHA256
	m := validInstallOnly()
	m.ExtraPrecacheImages = nil
	opts := ImageSetOptions{RecertImage: testRecertImage, LCAImage: testLCAImage}

	t.Run("failure leaves no stale manifest and cleans staging", func(t *testing.T) {
		dir := t.TempDir()
		p2Root := filepath.Join(dir, "p2")
		require.NoError(t, os.MkdirAll(p2Root, 0o755))
		outputPath := filepath.Join(dir, "usb.img")

		// Pre-seed stale manifests at both targets.
		stalePath := filepath.Join(p2Root, "digests.json")
		besidePath := outputPath + ".digests.json"
		require.NoError(t, os.WriteFile(stalePath, []byte("STALE"), 0o600))
		require.NoError(t, os.WriteFile(besidePath, []byte("STALE"), 0o600))

		deps := imageBuildDeps{
			seedReader: fakeSeedReader{list: []string{seedImg}},
			mirrorer:   fakeMirrorer{failOn: map[string]bool{testLCAImage: true}},
			p3Scanner:  notImplementedP3Scanner{},
		}

		_, err := buildImageLayout(context.Background(), nil, m, opts, outputPath, p2Root, deps)
		require.Error(t, err)

		// Stale manifests are untouched (not overwritten with a partial build).
		for _, p := range []string{stalePath, besidePath} {
			data, readErr := os.ReadFile(p)
			require.NoError(t, readErr)
			assert.Equal(t, "STALE", string(data), "pre-existing manifest %q must not be modified on failure", p)
		}

		// Staging directory was cleaned up.
		leftover, err := filepath.Glob(filepath.Join(p2Root, ".oci-staging-*"))
		require.NoError(t, err)
		assert.Empty(t, leftover, "staging directory must be removed on failure")
	})

	t.Run("success publishes layout and both manifests", func(t *testing.T) {
		dir := t.TempDir()
		p2Root := filepath.Join(dir, "p2")
		outputPath := filepath.Join(dir, "usb.img")

		deps := imageBuildDeps{
			seedReader: fakeSeedReader{list: []string{seedImg}},
			mirrorer:   fakeMirrorer{},
			p3Scanner:  notImplementedP3Scanner{},
		}

		result, err := buildImageLayout(context.Background(), nil, m, opts, outputPath, p2Root, deps)
		require.NoError(t, err)
		require.NotNil(t, result)
		// seed + recert + lca.
		assert.Len(t, result.Images, 3)

		// Published layout and both identical manifests.
		assert.DirExists(t, filepath.Join(p2Root, "oci"))
		parsed, err := localimages.ParseDigestManifest(filepath.Join(p2Root, "digests.json"))
		require.NoError(t, err)
		assert.Len(t, parsed.Images, 3)
		beside, err := os.ReadFile(outputPath + ".digests.json")
		require.NoError(t, err)
		onMedia, err := os.ReadFile(filepath.Join(p2Root, "digests.json"))
		require.NoError(t, err)
		assert.Equal(t, beside, onMedia)

		// No staging directory left behind.
		leftover, _ := filepath.Glob(filepath.Join(p2Root, ".oci-staging-*"))
		assert.Empty(t, leftover)
	})

	t.Run("rebuild over existing output replaces it cleanly", func(t *testing.T) {
		dir := t.TempDir()
		p2Root := filepath.Join(dir, "p2")
		outputPath := filepath.Join(dir, "usb.img")
		deps := imageBuildDeps{
			seedReader: fakeSeedReader{list: []string{seedImg}},
			mirrorer:   fakeMirrorer{},
			p3Scanner:  notImplementedP3Scanner{},
		}

		// First build publishes the layout and manifests.
		_, err := buildImageLayout(context.Background(), nil, m, opts, outputPath, p2Root, deps)
		require.NoError(t, err)

		// A second build over the same p2 root must swap in the new layout without
		// leaving a backup or staging directory behind.
		result, err := buildImageLayout(context.Background(), nil, m, opts, outputPath, p2Root, deps)
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.DirExists(t, filepath.Join(p2Root, "oci"))

		parsed, err := localimages.ParseDigestManifest(filepath.Join(p2Root, "digests.json"))
		require.NoError(t, err)
		assert.Len(t, parsed.Images, 3)

		for _, pattern := range []string{".oci-staging-*", "oci.previous"} {
			leftover, _ := filepath.Glob(filepath.Join(p2Root, pattern))
			assert.Empty(t, leftover, "no %s artifact must remain after a clean rebuild", pattern)
		}
	})

	// Regression for the review's P1: a failure while publishing the second manifest
	// copy must roll the layout and the first manifest back to the previous consistent
	// build, never leaving a new layout beside the previous manifest.
	t.Run("failure on the second manifest write rolls layout and manifest back", func(t *testing.T) {
		seedImg2 := "quay.io/seed/b@sha256:" + validSHA256
		dir := t.TempDir()
		p2Root := filepath.Join(dir, "p2")
		outDir := filepath.Join(dir, "out")
		require.NoError(t, os.MkdirAll(outDir, 0o755))
		outputPath := filepath.Join(outDir, "usb.img")
		besidePath := outputPath + ".digests.json"
		p2Path := filepath.Join(p2Root, "digests.json")

		// First build publishes a consistent 3-image layout + both manifests.
		deps1 := imageBuildDeps{
			seedReader: fakeSeedReader{list: []string{seedImg}},
			mirrorer:   fakeMirrorer{},
			p3Scanner:  notImplementedP3Scanner{},
		}
		_, err := buildImageLayout(context.Background(), nil, m, opts, outputPath, p2Root, deps1)
		require.NoError(t, err)

		firstManifest, err := os.ReadFile(p2Path)
		require.NoError(t, err)
		firstBeside, err := os.ReadFile(besidePath)
		require.NoError(t, err)
		firstIndex, err := os.ReadFile(filepath.Join(p2Root, "oci", "index.json"))
		require.NoError(t, err)

		// Make the beside-output directory unwritable so publishing the SECOND manifest
		// copy (<output>.digests.json) fails after the layout and the p2-root manifest
		// have already been swapped in. The second build would otherwise produce a
		// different, 4-image layout + manifest.
		require.NoError(t, os.Chmod(outDir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(outDir, 0o700) })

		deps2 := imageBuildDeps{
			seedReader: fakeSeedReader{list: []string{seedImg, seedImg2}},
			mirrorer:   fakeMirrorer{},
			p3Scanner:  notImplementedP3Scanner{},
		}
		_, err = buildImageLayout(context.Background(), nil, m, opts, outputPath, p2Root, deps2)
		require.Error(t, err)

		// The on-media layout and its manifest must be rolled back to the previous
		// consistent build -- never a new 4-image layout beside the old 3-image manifest.
		gotIndex, err := os.ReadFile(filepath.Join(p2Root, "oci", "index.json"))
		require.NoError(t, err)
		assert.Equal(t, firstIndex, gotIndex, "the OCI layout must be rolled back to the last consistent build")

		gotManifest, err := os.ReadFile(p2Path)
		require.NoError(t, err)
		assert.Equal(t, firstManifest, gotManifest, "the p2 digest manifest must be rolled back to match the layout")

		rolled, err := localimages.ParseDigestManifest(p2Path)
		require.NoError(t, err)
		assert.Len(t, rolled.Images, 3, "the rolled-back manifest must still describe the previous 3-image layout")

		gotBeside, err := os.ReadFile(besidePath)
		require.NoError(t, err)
		assert.Equal(t, firstBeside, gotBeside, "the untouched beside-output manifest must still match the on-media copy")

		// No backup or staging artifacts remain in the p2 root.
		for _, pattern := range []string{".oci-staging-*", "oci.previous", "digests.json.previous"} {
			leftover, _ := filepath.Glob(filepath.Join(p2Root, pattern))
			assert.Empty(t, leftover, "no %s artifact must remain after a rolled-back rebuild", pattern)
		}
	})
}
