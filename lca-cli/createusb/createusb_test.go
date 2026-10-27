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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/oci/layout"

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

// validSSHPublicKey is a throwaway ed25519 authorized_keys line for sshKey validation tests,
// generated with: ssh-keygen -t ed25519 -C "test@example.com" -N "" (private key discarded).
const validSSHPublicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICRIY/su3X3EirySD0+UmBF5LwDgbgp8TeIZ9oxyrK0a test@example.com"

func TestValidateSiteConfigSSHKey(t *testing.T) {
	t.Run("omitted sshKey passes", func(t *testing.T) {
		m := validSelfContained()
		m.SiteConfig.SSHKey = ""
		assert.NoError(t, m.Validate())
	})
	t.Run("valid public key passes", func(t *testing.T) {
		m := validSelfContained()
		m.SiteConfig.SSHKey = validSSHPublicKey
		assert.NoError(t, m.Validate())
	})
	t.Run("malformed key fails", func(t *testing.T) {
		m := validSelfContained()
		m.SiteConfig.SSHKey = "ssh-ed25519 not-a-key"
		assert.ErrorContains(t, m.Validate(), "not a valid public authorized key")
	})
	t.Run("private-key material fails", func(t *testing.T) {
		m := validSelfContained()
		m.SiteConfig.SSHKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nnot-a-real-key\n-----END OPENSSH PRIVATE KEY-----"
		assert.ErrorContains(t, m.Validate(), "not private-key material")
	})
	t.Run("multiple keys fail", func(t *testing.T) {
		m := validSelfContained()
		m.SiteConfig.SSHKey = validSSHPublicKey + "\n" + validSSHPublicKey
		assert.ErrorContains(t, m.Validate(), "exactly one public authorized key")
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

// TestBuildRejectsWriteDevice covers public Build's behavior for --write-device: it is rejected
// before any ISO build or attach, so a real block device is never destructively overwritten. This
// needs no seam (no privileged op is reached).
func TestBuildRejectsWriteDevice(t *testing.T) {
	err := Build(context.Background(), testLogger(), validInstallOnly(), BuildOptions{WriteDevice: "/dev/sdx"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--write-device is not yet supported")
}

// TestBuildInstallOnly covers the full build through the injected seams: the live ISO is built into
// a scratch work directory, written to the front of the image, and the data partitions are
// appended and formatted; the image is finalized (loop detached) and kept on disk.
func TestBuildInstallOnly(t *testing.T) {
	img := filepath.Join(t.TempDir(), "out.img")
	fp := &fakePartitionOps{attachDev: "/dev/loop3", dataNodes: []string{"/dev/loop3p3", "/dev/loop3p4"}}
	iso := &fakeISOBuilder{isoBytes: 4096}
	images := &fakeImagePopulator{}

	err := build(context.Background(), testLogger(), validInstallOnly(),
		BuildOptions{OutputPath: img, LCAImage: "quay.io/example/lca@sha256:abc"},
		mediaBuildDeps{partition: fp, iso: iso, images: images})
	require.NoError(t, err)

	assert.FileExists(t, img)
	assert.Equal(t, []string{"/dev/loop3"}, fp.detached, "the loop is detached at finalize")
	assert.NotEmpty(t, iso.gotOpts.WorkDir, "the ISO build gets a scratch work directory")
	assert.Equal(t, "quay.io/example/lca@sha256:abc", iso.gotOpts.LCAImage, "the lca-image flows into the ISO build")
	assert.Equal(t, []string{"buildLayout"}, images.calls, "install-only populates ibi-images (OCI layout) but builds no store")
}

// TestBuildLayoutBothModes covers both modes through the injected seams: the data partitions are
// appended once and formatted in creation order (self-contained adds cluster-config), and no
// partition is mounted when no config tree is supplied.
func TestBuildLayoutBothModes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		manifest   func() *Manifest
		mode       Mode
		loopDev    string
		dataNodes  []string
		wantLabels []string
	}{
		{"install-only appends ibi-images + ibi-status", validInstallOnly, ModeInstallOnly, "/dev/loop3",
			[]string{"/dev/loop3p3", "/dev/loop3p4"}, []string{labelIBIImages, labelIBIStatus}},
		{"self-contained appends ibi-images + cluster-config + ibi-status", validSelfContained, ModeSelfContained, "/dev/loop4",
			[]string{"/dev/loop4p3", "/dev/loop4p5", "/dev/loop4p6"}, []string{labelIBIImages, labelClusterConfig, labelIBIStatus}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := filepath.Join(t.TempDir(), "out.img")
			fp := &fakePartitionOps{attachDev: tc.loopDev, dataNodes: tc.dataNodes}
			iso := &fakeISOBuilder{isoBytes: 4096}
			images := &fakeImagePopulator{}

			err := build(context.Background(), testLogger(), tc.manifest(),
				BuildOptions{OutputPath: img, LCAImage: "lca"}, mediaBuildDeps{partition: fp, iso: iso, images: images})
			require.NoError(t, err)

			wantCalls := []string{"attach", "snapshot", "append", "dataNodes"}
			for range tc.wantLabels {
				wantCalls = append(wantCalls, "format")
			}
			// Populate ibi-images (resolve/mount/umount). Self-contained then also populates the
			// rendered cluster-config partition (a second resolve/mount/umount).
			wantCalls = append(wantCalls, "resolve", "mount", "umount")
			if tc.mode == ModeSelfContained {
				wantCalls = append(wantCalls, "resolve", "mount", "umount")
			}
			wantCalls = append(wantCalls, "detach")
			assert.Equal(t, wantCalls, fp.calls)
			assert.Equal(t, tc.wantLabels, fp.formattedLabels)
			assert.FileExists(t, img)
		})
	}
}

// TestBuildISOBuildFailure covers the ISO-build failure path: build surfaces it and never creates
// the output image or reaches a privileged op.
func TestBuildISOBuildFailure(t *testing.T) {
	img := filepath.Join(t.TempDir(), "out.img")
	fp := &fakePartitionOps{attachDev: "/dev/loop3"}
	iso := &fakeISOBuilder{buildErr: fmt.Errorf("fetch boom")}

	err := build(context.Background(), testLogger(), validInstallOnly(),
		BuildOptions{OutputPath: img, LCAImage: "lca"}, mediaBuildDeps{partition: fp, iso: iso, images: &fakeImagePopulator{}})
	require.Error(t, err)
	assert.ErrorContains(t, err, "fetch boom")
	assert.Empty(t, fp.calls, "no privileged op runs when the ISO build fails")
	_, statErr := os.Stat(img)
	assert.True(t, os.IsNotExist(statErr), "no image is created when the ISO build fails")
}

func TestBuildFinalizeFailurePreservesImage(t *testing.T) {
	img := filepath.Join(t.TempDir(), "out.img")
	fp := &fakePartitionOps{attachDev: "/dev/loop5", dataNodes: []string{"/dev/loop5p3", "/dev/loop5p4"}, detachErr: fmt.Errorf("detach boom")}
	iso := &fakeISOBuilder{isoBytes: 4096}
	err := build(context.Background(), testLogger(), validInstallOnly(),
		BuildOptions{OutputPath: img, LCAImage: "lca"}, mediaBuildDeps{partition: fp, iso: iso, images: &fakeImagePopulator{}})
	require.ErrorContains(t, err, "detach boom")
	assert.FileExists(t, img, "the image must remain available while its loop device is attached")
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

func (f fakeMirrorer) MirrorToLayout(_ context.Context, imageRef, destLayoutDir, ociTag, _ string) (mirrorResult, error) {
	if f.failOn[imageRef] {
		return mirrorResult{}, assert.AnError
	}
	dgst := fakeDigest(imageRef)
	appendFakeLayoutEntry(destLayoutDir, ociTag, dgst)
	return mirrorResult{digest: dgst}, nil
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
	}
	opts := ImageSetOptions{RecertImage: testRecertImage, LCAImage: testLCAImage}

	refs, err := resolveImageSet(m, opts, nil, deps)
	require.NoError(t, err)

	// recert and lca-cli are guaranteed present (the checked invariant).
	assert.Contains(t, refs, testRecertImage)
	assert.Contains(t, refs, testLCAImage)
	assert.Contains(t, refs, extra)
	// The seed image itself is required even when containers.list omits it.
	assert.ElementsMatch(t, []string{m.SeedImage, seedA, seedB, testRecertImage, testLCAImage, extra}, refs)
	assert.Len(t, refs, 6)
}

func TestResolveImageSetDedupesRecertFromSeedList(t *testing.T) {
	// When the seed containers.list already carries recert, it must not be duplicated.
	m := validInstallOnly()
	m.ExtraPrecacheImages = nil
	deps := imageBuildDeps{
		seedReader: fakeSeedReader{list: []string{testRecertImage}},
	}
	opts := ImageSetOptions{RecertImage: testRecertImage, LCAImage: testLCAImage}

	refs, err := resolveImageSet(m, opts, nil, deps)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{m.SeedImage, testRecertImage, testLCAImage}, refs)
}

func TestResolveImageSetSelfContainedUnionsBaseAndScan(t *testing.T) {
	// Regression for the review's P1: self-contained is a superset of install-only, not
	// a replacement. With the real rendered-manifest scanner the resolved set must still
	// carry the seed closure, recert and lca-cli, with the scanned p3 images unioned on
	// top and deduplicated.
	seedA := "quay.io/seed/a@sha256:" + validSHA256
	p3Only := "quay.io/p3/only@sha256:" + validSHA256

	// A rendered manifest references a p3-only image and (to prove dedup) recert again.
	manifestPath := filepath.Join(t.TempDir(), "workload.yaml")
	require.NoError(t, os.WriteFile(manifestPath, []byte(
		"apiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n"+
			"      - name: a\n        image: "+p3Only+"\n      - name: b\n        image: "+testRecertImage+"\n"), 0o600))

	m := validSelfContained()
	deps := imageBuildDeps{seedReader: fakeSeedReader{list: []string{seedA}}}
	opts := ImageSetOptions{RecertImage: testRecertImage, LCAImage: testLCAImage}

	refs, err := resolveImageSet(m, opts, []string{manifestPath}, deps)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{m.SeedImage, seedA, testRecertImage, testLCAImage, p3Only}, refs)
}

func TestResolveImageSetSelfContainedScanErrorFails(t *testing.T) {
	// A manifest-scan failure must fail the build (and short-circuit before the expensive
	// seed-image read), not silently drop the p3 images.
	m := validSelfContained()
	deps := imageBuildDeps{
		seedReader: fakeSeedReader{err: assert.AnError}, // must not be reached
	}
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	_, err := resolveImageSet(m, ImageSetOptions{RecertImage: testRecertImage, LCAImage: testLCAImage}, []string{missing}, deps)
	assert.ErrorContains(t, err, "resolving self-contained image set")
}

func TestResolveImageSetMissingRequiredImageFails(t *testing.T) {
	deps := imageBuildDeps{seedReader: fakeSeedReader{list: []string{"quay.io/seed/a@sha256:" + validSHA256}}}

	t.Run("empty recert fails", func(t *testing.T) {
		_, err := resolveImageSet(validInstallOnly(), ImageSetOptions{RecertImage: "", LCAImage: testLCAImage}, nil, deps)
		assert.ErrorContains(t, err, "recert image is required")
	})
	t.Run("empty lca fails", func(t *testing.T) {
		_, err := resolveImageSet(validInstallOnly(), ImageSetOptions{RecertImage: testRecertImage, LCAImage: "  "}, nil, deps)
		assert.ErrorContains(t, err, "lca-cli image is required")
	})
}

func TestResolveImageSetSeedReaderErrorFails(t *testing.T) {
	deps := imageBuildDeps{seedReader: fakeSeedReader{err: assert.AnError}}
	_, err := resolveImageSet(validInstallOnly(), ImageSetOptions{RecertImage: testRecertImage, LCAImage: testLCAImage}, nil, deps)
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
	assert.Equal(t, []string{refs[0]}, entries[0].Sources)
	assert.Equal(t, []string{refs[1]}, entries[1].Sources)
}

func TestVerifyPinnedSourceDigest(t *testing.T) {
	pinned := "//quay.io/example/image@sha256:" + validSHA256
	srcRef, err := docker.ParseReference(pinned)
	require.NoError(t, err)

	assert.NoError(t, verifyPinnedSourceDigest(srcRef, "sha256:"+validSHA256))
	assert.ErrorContains(t, verifyPinnedSourceDigest(srcRef, fakeDigest("converted")), "differs from pinned source digest")

	taggedRef, err := docker.ParseReference("//quay.io/example/image:latest")
	require.NoError(t, err)
	assert.NoError(t, verifyPinnedSourceDigest(taggedRef, fakeDigest("converted")))
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
		{Name: "quay.io/openshift-release-dev/ocp-release", Digest: fakeDigest("a"), OCITag: "ocp-release-0", Sources: []string{"quay.io/openshift-release-dev/ocp-release:latest"}},
		{Name: "quay.io/edge-infrastructure/recert", Digest: fakeDigest("b"), OCITag: "recert-1", Sources: []string{"quay.io/edge-infrastructure/recert:latest"}},
	}
	p2Root := t.TempDir()

	data, err := marshalDigestManifest(entries, "oci-test")
	require.NoError(t, err)
	require.NoError(t, validateDigestManifestBytes(data, p2Root))
	manifestPath := filepath.Join(p2Root, "digests.json")
	_, err = publishFile(manifestPath, data, true)
	require.NoError(t, err)

	// Cross-check: the emitted file parses and validates under the consumer's parser,
	// guaranteeing producer/consumer schema parity.
	parsed, err := localimages.ParseDigestManifest(manifestPath)
	require.NoError(t, err)
	assert.Equal(t, localimages.SignaturesManifestVersion, parsed.Version)
	assert.NotEmpty(t, parsed.OCILayoutPath)
	require.Len(t, parsed.Images, len(entries))
	for i, e := range entries {
		assert.Equal(t, e.Name, parsed.Images[i].Name)
		assert.Equal(t, e.Digest, parsed.Images[i].Digest)
		assert.Equal(t, e.OCITag, parsed.Images[i].OCITag)
		assert.Equal(t, e.Sources, parsed.Images[i].Sources)
	}
}

func TestDigestManifestWrittenToBothPaths(t *testing.T) {
	entries := []localimages.ImageEntry{{Name: "quay.io/x/y", Digest: fakeDigest("x"), OCITag: "y-0"}}
	dir := t.TempDir()
	p2Root := filepath.Join(dir, "p2")
	require.NoError(t, os.MkdirAll(p2Root, 0o755))
	outputPath := filepath.Join(dir, "usb.img")

	data, err := marshalDigestManifest(entries, "oci-test")
	require.NoError(t, err)
	besidePath := outputPath + ".digests.json"
	p2Path := filepath.Join(p2Root, "digests.json")
	_, err = publishFile(besidePath, data, false)
	require.NoError(t, err)
	_, err = publishFile(p2Path, data, true)
	require.NoError(t, err)

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

// committedLayoutName returns the OCILayoutPath recorded in the committed on-media
// manifest and asserts the referenced layout directory exists, is build-unique
// (prefixed), and matches the manifest entries exactly the way the consumer selects.
func committedLayoutName(t *testing.T, p2Root string) string {
	t.Helper()
	parsed, err := localimages.ParseDigestManifest(filepath.Join(p2Root, "digests.json"))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(parsed.OCILayoutPath, ociLayoutDirPrefix),
		"OCILayoutPath %q must be a build-unique layout directory", parsed.OCILayoutPath)
	layoutDir := filepath.Join(p2Root, parsed.OCILayoutPath)
	assert.DirExists(t, layoutDir)
	// The committed manifest must point at a complete, matching layout.
	require.NoError(t, verifyLayoutEntries(layoutDir, parsed.Images),
		"the committed manifest must point at a layout that matches every entry")
	return parsed.OCILayoutPath
}

// layoutDirsUnder returns the names of the build-unique OCI layout directories present
// under p2Root.
func layoutDirsUnder(t *testing.T, p2Root string) []string {
	t.Helper()
	ents, err := os.ReadDir(p2Root)
	require.NoError(t, err)
	var dirs []string
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), ociLayoutDirPrefix) {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs
}

// assertNoPublishTempArtifacts asserts that no temp files from validation/publication
// were left behind in p2Root.
func assertNoPublishTempArtifacts(t *testing.T, p2Root string) {
	t.Helper()
	for _, pattern := range []string{".digests-publish-*", ".digests-validate-*"} {
		leftover, _ := filepath.Glob(filepath.Join(p2Root, pattern))
		assert.Empty(t, leftover, "no %s temp artifact must remain", pattern)
	}
}

func TestBuildImageLayoutCommitAndGC(t *testing.T) {
	seedImg := "quay.io/seed/a@sha256:" + validSHA256
	m := validInstallOnly()
	m.ExtraPrecacheImages = nil
	opts := ImageSetOptions{RecertImage: testRecertImage, LCAImage: testLCAImage}

	cleanDeps := func() imageBuildDeps {
		return imageBuildDeps{
			seedReader: fakeSeedReader{list: []string{seedImg}},
			mirrorer:   fakeMirrorer{},
		}
	}

	t.Run("success commits the manifest, layout, and audit copy", func(t *testing.T) {
		dir := t.TempDir()
		p2Root := filepath.Join(dir, "p2")
		outputPath := filepath.Join(dir, "usb.img")

		result, err := buildImageLayout(context.Background(), nil, m, opts, outputPath, p2Root, "", cleanDeps())
		require.NoError(t, err)
		require.NotNil(t, result)
		// Seed image + its listed image + recert + lca.
		assert.Len(t, result.Images, 4)

		// The committed manifest points at a build-unique layout that matches every entry,
		// and that layout is the only one present.
		layoutName := committedLayoutName(t, p2Root)
		assert.Equal(t, []string{layoutName}, layoutDirsUnder(t, p2Root))

		parsed, err := localimages.ParseDigestManifest(filepath.Join(p2Root, "digests.json"))
		require.NoError(t, err)
		assert.Len(t, parsed.Images, 4)

		// The audit-only beside-output copy is byte-identical to the committed manifest.
		beside, err := os.ReadFile(outputPath + ".digests.json")
		require.NoError(t, err)
		onMedia, err := os.ReadFile(filepath.Join(p2Root, "digests.json"))
		require.NoError(t, err)
		assert.Equal(t, onMedia, beside, "the audit-only copy must match the committed on-media manifest")

		assertNoPublishTempArtifacts(t, p2Root)
	})

	// Regression for the review's crash-safety P1: a rebuild commits a new build-unique
	// layout and garbage-collects the superseded one, so exactly one layout (the one the
	// committed manifest references) remains -- no fixed-name swap, no ".previous" backup.
	t.Run("rebuild commits a new layout and GCs the superseded one", func(t *testing.T) {
		dir := t.TempDir()
		p2Root := filepath.Join(dir, "p2")
		outputPath := filepath.Join(dir, "usb.img")

		_, err := buildImageLayout(context.Background(), nil, m, opts, outputPath, p2Root, "", cleanDeps())
		require.NoError(t, err)
		firstLayout := committedLayoutName(t, p2Root)

		// A second build over the same p2 root commits a new layout under a new unique name.
		result, err := buildImageLayout(context.Background(), nil, m, opts, outputPath, p2Root, "", cleanDeps())
		require.NoError(t, err)
		require.NotNil(t, result)

		secondLayout := committedLayoutName(t, p2Root)
		assert.NotEqual(t, firstLayout, secondLayout, "each build must mirror into a fresh, unique layout directory")
		assert.NoDirExists(t, filepath.Join(p2Root, firstLayout), "the superseded layout must be garbage-collected")
		assert.Equal(t, []string{secondLayout}, layoutDirsUnder(t, p2Root), "only the committed layout must remain")

		// No legacy fixed-name or backup artifacts.
		for _, pattern := range []string{"oci", "oci.previous", "digests.json.previous"} {
			leftover, _ := filepath.Glob(filepath.Join(p2Root, pattern))
			assert.Empty(t, leftover, "no %s artifact must remain after a rebuild", pattern)
		}
		assertNoPublishTempArtifacts(t, p2Root)
	})
}

func TestBuildImageLayoutCrashRecovery(t *testing.T) {
	seedImg := "quay.io/seed/a@sha256:" + validSHA256
	m := validInstallOnly()
	m.ExtraPrecacheImages = nil
	opts := ImageSetOptions{RecertImage: testRecertImage, LCAImage: testLCAImage}

	cleanDeps := func() imageBuildDeps {
		return imageBuildDeps{
			seedReader: fakeSeedReader{list: []string{seedImg}},
			mirrorer:   fakeMirrorer{},
		}
	}

	// A crash between the layout write and the manifest commit leaves an orphaned layout
	// directory beside the previous committed manifest. The committed manifest still
	// points at its intact previous layout (no mismatch), and the next build commits a
	// new layout and garbage-collects both the superseded layout and the crash orphan.
	t.Run("orphan layout never breaks the committed state and is GCed by the next build", func(t *testing.T) {
		dir := t.TempDir()
		p2Root := filepath.Join(dir, "p2")
		outputPath := filepath.Join(dir, "usb.img")

		_, err := buildImageLayout(context.Background(), nil, m, opts, outputPath, p2Root, "", cleanDeps())
		require.NoError(t, err)
		firstLayout := committedLayoutName(t, p2Root) // asserts the committed state is consistent

		// Simulate a crash that wrote a new layout but never committed its manifest.
		orphan := filepath.Join(p2Root, ociLayoutDirPrefix+"crashorphan")
		require.NoError(t, os.MkdirAll(orphan, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(orphan, "index.json"), []byte(`{"schemaVersion":2}`), 0o600))

		// The committed manifest is unaffected by the orphan: it still points at the intact
		// previous layout.
		assert.Equal(t, firstLayout, committedLayoutName(t, p2Root))
		assert.DirExists(t, orphan)

		// The next build commits a new layout and GCs both the superseded layout and the orphan.
		_, err = buildImageLayout(context.Background(), nil, m, opts, outputPath, p2Root, "", cleanDeps())
		require.NoError(t, err)
		secondLayout := committedLayoutName(t, p2Root)
		assert.NoDirExists(t, orphan, "the crash-orphaned layout must be garbage-collected")
		assert.NoDirExists(t, filepath.Join(p2Root, firstLayout), "the superseded layout must be garbage-collected")
		assert.Equal(t, []string{secondLayout}, layoutDirsUnder(t, p2Root))
	})

	// A failure before the commit (here, a mirror failure) must leave the previous
	// committed manifest and its layout untouched and must not leave the new, half-built
	// layout directory behind.
	t.Run("pre-commit failure leaves prior committed state intact and cleans the new layout", func(t *testing.T) {
		dir := t.TempDir()
		p2Root := filepath.Join(dir, "p2")
		outputPath := filepath.Join(dir, "usb.img")
		p2Path := filepath.Join(p2Root, "digests.json")
		besidePath := outputPath + ".digests.json"

		_, err := buildImageLayout(context.Background(), nil, m, opts, outputPath, p2Root, "", cleanDeps())
		require.NoError(t, err)
		firstLayout := committedLayoutName(t, p2Root)
		firstManifest, err := os.ReadFile(p2Path)
		require.NoError(t, err)
		firstBeside, err := os.ReadFile(besidePath)
		require.NoError(t, err)

		// A second build whose mirror fails must not touch the committed state.
		failDeps := imageBuildDeps{
			seedReader: fakeSeedReader{list: []string{seedImg, "quay.io/seed/b@sha256:" + validSHA256}},
			mirrorer:   fakeMirrorer{failOn: map[string]bool{testLCAImage: true}},
		}
		_, err = buildImageLayout(context.Background(), nil, m, opts, outputPath, p2Root, "", failDeps)
		require.Error(t, err)

		// Committed manifest, its layout, and the audit copy are all untouched.
		assert.Equal(t, firstLayout, committedLayoutName(t, p2Root))
		gotManifest, err := os.ReadFile(p2Path)
		require.NoError(t, err)
		assert.Equal(t, firstManifest, gotManifest, "the committed manifest must be untouched by a failed build")
		gotBeside, err := os.ReadFile(besidePath)
		require.NoError(t, err)
		assert.Equal(t, firstBeside, gotBeside, "the audit copy must be untouched by a failed build")

		// The half-built new layout directory is cleaned up; only the committed one remains.
		assert.Equal(t, []string{firstLayout}, layoutDirsUnder(t, p2Root), "the failed build's layout directory must be removed")
		assertNoPublishTempArtifacts(t, p2Root)
	})

	// A failure at the commit itself (the on-media digests.json rename) must leave no
	// partial manifest and must clean up the new layout directory.
	t.Run("commit failure leaves no partial manifest and cleans the new layout", func(t *testing.T) {
		dir := t.TempDir()
		p2Root := filepath.Join(dir, "p2")
		require.NoError(t, os.MkdirAll(p2Root, 0o755))
		outputPath := filepath.Join(dir, "usb.img")

		// Pre-create digests.json as a non-empty directory so the commit rename fails. On a
		// fresh media there is no valid prior manifest to protect; the point is that the
		// commit failure leaves nothing partial.
		manifestDir := filepath.Join(p2Root, "digests.json")
		require.NoError(t, os.MkdirAll(filepath.Join(manifestDir, "keep"), 0o755))

		_, err := buildImageLayout(context.Background(), nil, m, opts, outputPath, p2Root, "", cleanDeps())
		require.Error(t, err)

		// The pre-existing path is untouched (still a directory -- no partial file over it),
		// the new layout directory is cleaned up, and no publish temp files remain.
		info, statErr := os.Stat(manifestDir)
		require.NoError(t, statErr)
		assert.True(t, info.IsDir(), "a failed commit must not replace the path with a partial file")
		assert.Empty(t, layoutDirsUnder(t, p2Root), "the uncommitted layout directory must be removed on commit failure")
		assertNoPublishTempArtifacts(t, p2Root)
	})
}

// The beside-output copy is audit-only: a failure writing it must not fail the build,
// because the on-media manifest has already committed and the medium is consistent.
func TestBuildImageLayoutBesideOutputIsBestEffort(t *testing.T) {
	seedImg := "quay.io/seed/a@sha256:" + validSHA256
	m := validInstallOnly()
	m.ExtraPrecacheImages = nil
	opts := ImageSetOptions{RecertImage: testRecertImage, LCAImage: testLCAImage}

	if os.Geteuid() == 0 {
		t.Skip("the unwritable-directory guard does not apply to root")
	}

	dir := t.TempDir()
	p2Root := filepath.Join(dir, "p2")
	outDir := filepath.Join(dir, "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))
	outputPath := filepath.Join(outDir, "usb.img")

	// Make the beside-output directory unwritable so writing <output>.digests.json fails.
	require.NoError(t, os.Chmod(outDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(outDir, 0o700) })

	deps := imageBuildDeps{
		seedReader: fakeSeedReader{list: []string{seedImg}},
		mirrorer:   fakeMirrorer{},
	}

	result, err := buildImageLayout(context.Background(), logrus.New(), m, opts, outputPath, p2Root, "", deps)
	require.NoError(t, err, "a beside-output (audit-only) write failure must not fail the build")
	require.NotNil(t, result)

	// The on-media manifest committed and points at a consistent layout.
	committedLayoutName(t, p2Root)
	assert.NoFileExists(t, outputPath+".digests.json", "the beside-output copy failed to write (expected)")
}

// Regression for the review's durability P1: the completed layout is fsynced before the
// commit, so syncTree must flush an entire nested layout tree without error. (It cannot
// assert on-disk durability from a unit test, but it exercises the file+directory walk and
// the fsync of every regular file and directory that backs the power-loss guarantee.)
func TestSyncTreeFsyncsLayoutTree(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "blobs", "sha256"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "index.json"), []byte(`{"schemaVersion":2}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "blobs", "sha256", "abc"), make([]byte, 128), 0o600))

	require.NoError(t, syncTree(root), "syncing a completed layout tree must succeed")
}

// Regression for the review's P1 "a failed sync after rename deletes the layout named by
// the live manifest": once publishFile has renamed the target into place it must report
// renamed=true, so buildImageLayout marks the build committed and never deletes the new
// layout even if the subsequent durability fsync reports an error. A rename that never
// happens must report renamed=false and leave no temp artifact behind.
func TestPublishFileReportsRenamed(t *testing.T) {
	p2Root := t.TempDir()

	// A successful durable publish renames the target into place and reports renamed=true.
	path := filepath.Join(p2Root, "digests.json")
	renamed, err := publishFile(path, []byte("payload"), true)
	require.NoError(t, err)
	assert.True(t, renamed, "a successful durable publish must report renamed=true")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []byte("payload"), got)

	// A rename that cannot happen (the destination is a non-empty directory) must report
	// renamed=false -- so the caller does not treat the target as committed -- and must not
	// leave its temp file behind.
	blocked := filepath.Join(p2Root, "blocked.json")
	require.NoError(t, os.MkdirAll(filepath.Join(blocked, "keep"), 0o750))
	renamed, err = publishFile(blocked, []byte("x"), true)
	require.Error(t, err)
	assert.False(t, renamed, "a failed rename must report renamed=false")
	assertNoPublishTempArtifacts(t, p2Root)
}

// Regression for the review's P1 "persist the new layout's parent entry before renaming the
// manifest": buildImageLayout fsyncs the p2 root directory (via fsyncPath) after syncing the
// new layout and before committing the manifest, so the directory entry naming the new layout
// is durable before the committed manifest can reference it. This asserts fsyncPath succeeds on
// the p2 root directory the build calls it on (it cannot assert on-disk durability from a unit
// test, but it exercises the directory open+fsync that backs the parent-entry guarantee).
func TestFsyncPathSyncsDirectory(t *testing.T) {
	p2Root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(p2Root, ociLayoutDirPrefix+"abc"), 0o750))

	require.NoError(t, fsyncPath(p2Root), "fsyncing the p2 root directory must succeed")
}

// Regression for the review's concurrency P1: builds against the same p2 root must
// serialize. A second lockP2Root must block while the first lock is held and succeed only
// once it is released.
func TestLockP2RootSerializesBuilds(t *testing.T) {
	p2Root := t.TempDir()

	unlock, err := lockP2Root(p2Root)
	require.NoError(t, err)

	acquired := make(chan struct{})
	var once sync.Once
	go func() {
		u2, err := lockP2Root(p2Root)
		if err != nil {
			return
		}
		once.Do(func() { close(acquired) })
		u2()
	}()

	// While the first lock is held, the second acquisition must block.
	select {
	case <-acquired:
		t.Fatal("second build acquired the p2-root lock while the first was still held")
	case <-time.After(150 * time.Millisecond):
	}

	// Once released, the blocked acquisition must succeed.
	unlock()
	select {
	case <-acquired:
	case <-time.After(3 * time.Second):
		t.Fatal("second build never acquired the p2-root lock after the first was released")
	}
}

// Regression for the review's concurrency P1: GC must decide which layout is live by
// re-reading the committed manifest, not by trusting a caller-supplied name. It keeps the
// layout the committed manifest references and removes the rest.
func TestGCSupersededLayoutsKeepsManifestReferencedLayout(t *testing.T) {
	p2Root := t.TempDir()

	live := ociLayoutDirPrefix + "live"
	require.NoError(t, os.MkdirAll(filepath.Join(p2Root, live), 0o750))
	stale := ociLayoutDirPrefix + "stale"
	require.NoError(t, os.MkdirAll(filepath.Join(p2Root, stale), 0o750))

	data, err := marshalDigestManifest([]localimages.ImageEntry{{Name: "quay.io/x/y", Digest: fakeDigest("x"), OCITag: "y-0"}}, live)
	require.NoError(t, err)
	_, err = publishFile(filepath.Join(p2Root, "digests.json"), data, true)
	require.NoError(t, err)

	gcSupersededLayouts(nil, p2Root)

	assert.DirExists(t, filepath.Join(p2Root, live), "the layout the committed manifest references must be kept")
	assert.NoDirExists(t, filepath.Join(p2Root, stale), "a layout no committed manifest references must be collected")
}

// Regression for the review's concurrency P1: if no committed manifest can be read, GC
// must delete nothing (it cannot know which layout is live), so a crash that left layouts
// but no manifest never loses the only intact copy.
func TestGCSupersededLayoutsNoManifestKeepsAll(t *testing.T) {
	p2Root := t.TempDir()

	a := filepath.Join(p2Root, ociLayoutDirPrefix+"a")
	b := filepath.Join(p2Root, ociLayoutDirPrefix+"b")
	require.NoError(t, os.MkdirAll(a, 0o750))
	require.NoError(t, os.MkdirAll(b, 0o750))

	gcSupersededLayouts(nil, p2Root)

	assert.DirExists(t, a, "GC must not delete layouts when no committed manifest can be read")
	assert.DirExists(t, b, "GC must not delete layouts when no committed manifest can be read")
}
