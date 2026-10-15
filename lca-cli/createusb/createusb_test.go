/*
Copyright 2023.

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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// validInstallOnly returns a fully valid install-only manifest.
func validInstallOnly() *Manifest {
	return &Manifest{
		Mode:                ModeInstallOnly,
		SeedImage:           testSeedImage,
		SeedVersion:         testSeedVersion,
		ReleaseVersion:      testRelease,
		InstallationDisk:    testInstallDisk,
		Disconnected:        true,
		Shutdown:            true,
		RHCOSLiveISO:        RHCOSLiveISO{URL: testISOURL, SHA256: validSHA256},
		ExtraPrecacheImages: []string{"quay.io/example/extra@sha256:" + validSHA256},
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
		Shutdown:         false,
		RHCOSLiveISO:     RHCOSLiveISO{URL: testISOURL, SHA256: validSHA256},
		ClusterManifests: []ManifestSource{{Path: "manifests/cluster.yaml"}},
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
	assert.True(t, m.Shutdown)
	assert.Equal(t, testISOURL, m.RHCOSLiveISO.URL)
	assert.Equal(t, validSHA256, m.RHCOSLiveISO.SHA256)
	assert.Equal(t, []string{"quay.io/example/extra@sha256:" + validSHA256}, m.ExtraPrecacheImages)
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
	assert.False(t, m.Shutdown)
	require.NotNil(t, m.SiteConfig)
	assert.Equal(t, "example", m.SiteConfig.ClusterName)
	assert.Equal(t, validPullSecret, m.SiteConfig.PullSecret)
	assert.Len(t, m.ClusterManifests, 1)
	assert.Equal(t, "manifests/cluster.yaml", m.ClusterManifests[0].Path)
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
		m.Shutdown = false
		assert.ErrorContains(t, m.Validate(), "shutdown must be true")
	})
	t.Run("self-contained requires shutdown false", func(t *testing.T) {
		m := validSelfContained()
		m.Shutdown = true
		assert.ErrorContains(t, m.Validate(), "shutdown must be false")
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
		m.ClusterManifests = []ManifestSource{{Path: "x"}}
		assert.ErrorContains(t, m.Validate(), "clusterManifests is only valid")
	})
	t.Run("extraManifests under install-only fails", func(t *testing.T) {
		m := validInstallOnly()
		m.ExtraManifests = []ManifestSource{{Path: "x"}}
		assert.ErrorContains(t, m.Validate(), "extraManifests is only valid")
	})
	t.Run("extraPrecacheImages under self-contained fails", func(t *testing.T) {
		m := validSelfContained()
		m.ExtraPrecacheImages = []string{"quay.io/example/extra@sha256:" + validSHA256}
		assert.ErrorContains(t, m.Validate(), "extraPrecacheImages is only valid")
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
			name   string
			mutate func(*SiteConfig)
			want   string
		}{
			{"clusterName", func(s *SiteConfig) { s.ClusterName = "" }, "clusterName is required"},
			{"baseDomain", func(s *SiteConfig) { s.BaseDomain = "" }, "baseDomain is required"},
			{"hostname", func(s *SiteConfig) { s.Hostname = "" }, "hostname is required"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				m := validSelfContained()
				tc.mutate(m.SiteConfig)
				assert.ErrorContains(t, m.Validate(), tc.want)
			})
		}
	})
	t.Run("missing clusterManifests fails", func(t *testing.T) {
		m := validSelfContained()
		m.ClusterManifests = nil
		assert.ErrorContains(t, m.Validate(), "clusterManifests is required")
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
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{"missing seedImage", func(m *Manifest) { m.SeedImage = "" }, "seedImage is required"},
		{"non-digest-pinned seedImage", func(m *Manifest) { m.SeedImage = "quay.io/example/seed:latest" }, "digest-pinned"},
		{"missing seedVersion", func(m *Manifest) { m.SeedVersion = "" }, "seedVersion is required"},
		{"missing releaseVersion", func(m *Manifest) { m.ReleaseVersion = "" }, "releaseVersion is required"},
		{"missing installationDisk", func(m *Manifest) { m.InstallationDisk = "" }, "installationDisk is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := validInstallOnly()
			tc.mutate(m)
			assert.ErrorContains(t, m.Validate(), tc.want)
		})
	}
}

func TestValidateRHCOSLiveISO(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{"missing url", func(m *Manifest) { m.RHCOSLiveISO.URL = "" }, "url is required"},
		{"non-https url", func(m *Manifest) { m.RHCOSLiveISO.URL = "http://example.com/x.iso" }, "https url"},
		{"missing sha256", func(m *Manifest) { m.RHCOSLiveISO.SHA256 = "" }, "sha256 is required"},
		{"short sha256", func(m *Manifest) { m.RHCOSLiveISO.SHA256 = "abcd" }, "64-character"},
		{"non-hex sha256", func(m *Manifest) { m.RHCOSLiveISO.SHA256 = "g" + validSHA256[1:] }, "64-character"},
		{"uppercase sha256", func(m *Manifest) { m.RHCOSLiveISO.SHA256 = "E" + validSHA256[1:] }, "64-character"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := validInstallOnly()
			tc.mutate(m)
			assert.ErrorContains(t, m.Validate(), tc.want)
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
