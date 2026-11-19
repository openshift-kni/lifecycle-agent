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

package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/createusb"
)

const (
	testSHA256       = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	testValidPullSec = `{"auths":{"registry.example.com":{"auth":"dXNlcjpwYXNz"}}}`
)

// validManifestYAML returns a fully valid install-only manifest (passes every
// validation rule, so the only remaining failure is the Build stub).
func validManifestYAML() string {
	return `mode: install-only
seedImage: quay.io/example/seed@sha256:` + testSHA256 + `
seedVersion: 4.20.0
releaseVersion: 4.20.0
installationDisk: /dev/disk/by-id/ata-example
disconnected: true
shutdown: true
rhcosLiveIso:
  url: https://example.com/rhcos-live.x86_64.iso
  sha256: ` + testSHA256 + `
`
}

// runCreateUsbCmd executes a fresh create-usb command with args and returns the error.
func runCreateUsbCmd(t *testing.T, args ...string) error {
	t.Helper()
	// The package logger's formatter is initialized in Execute() for the real CLI; set it
	// here so runCreateUsb's info log does not panic when the command is driven directly.
	log.SetFormatter(&logrus.TextFormatter{})
	log.SetOutput(io.Discard)
	cmd := newCreateUsbCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	return cmd.Execute()
}

func TestCreateUsbRequiresManifest(t *testing.T) {
	err := runCreateUsbCmd(t, "--output", "/tmp/out.img")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "manifest")
}

func TestCreateUsbOutputVsWriteDevice(t *testing.T) {
	manifest := filepath.Join(t.TempDir(), "m.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(validManifestYAML()), 0o600))

	t.Run("neither fails", func(t *testing.T) {
		err := runCreateUsbCmd(t, "--manifest", manifest)
		assert.ErrorContains(t, err, "exactly one of --output or --write-device")
	})
	t.Run("both fails", func(t *testing.T) {
		err := runCreateUsbCmd(t, "--manifest", manifest, "--output", "/tmp/out.img", "--write-device", "/dev/sdx")
		assert.ErrorContains(t, err, "mutually exclusive")
	})
	t.Run("exactly one is accepted (reaches build)", func(t *testing.T) {
		// --write-device is exactly one accepted target and is rejected by Build before any
		// privileged op, so this proves validation passed and reached the build without the
		// test touching losetup or a real device.
		err := runCreateUsbCmd(t, "--manifest", manifest, "--write-device", "/dev/sdx")
		assert.ErrorContains(t, err, "--write-device is not yet supported")
	})
}

func TestCreateUsbPartialISOFlags(t *testing.T) {
	manifest := filepath.Join(t.TempDir(), "m.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(validManifestYAML()), 0o600))

	t.Run("url without sha256 fails", func(t *testing.T) {
		err := runCreateUsbCmd(t, "--manifest", manifest, "--output", "/tmp/out.img",
			"--rhcos-live-iso", "https://example.com/other.iso")
		assert.ErrorContains(t, err, "must be provided together")
	})
	t.Run("sha256 without url fails", func(t *testing.T) {
		err := runCreateUsbCmd(t, "--manifest", manifest, "--output", "/tmp/out.img",
			"--rhcos-live-iso-sha256", testSHA256)
		assert.ErrorContains(t, err, "must be provided together")
	})
	t.Run("both together override and reach build", func(t *testing.T) {
		// --write-device reaches Build (rejected early) without a privileged op, proving the
		// paired override applied and validation passed.
		err := runCreateUsbCmd(t, "--manifest", manifest, "--write-device", "/dev/sdx",
			"--rhcos-live-iso", "https://example.com/other.iso", "--rhcos-live-iso-sha256", testSHA256)
		assert.ErrorContains(t, err, "--write-device is not yet supported")
	})
}

// invalidISOManifestYAML returns an otherwise-valid install-only manifest whose
// rhcosLiveIso is invalid (non-https url + malformed sha256), so it fails Validate
// unless the --rhcos-live-iso* flags override it.
func invalidISOManifestYAML() string {
	return `mode: install-only
seedImage: quay.io/example/seed@sha256:` + testSHA256 + `
seedVersion: 4.20.0
releaseVersion: 4.20.0
installationDisk: /dev/disk/by-id/ata-example
disconnected: true
shutdown: true
rhcosLiveIso:
  url: http://insecure.example.com/rhcos-live.x86_64.iso
  sha256: deadbeef
`
}

func TestCreateUsbISOFlagWins(t *testing.T) {
	t.Run("valid flags rescue an invalid manifest ISO (flag applied and wins)", func(t *testing.T) {
		manifest := filepath.Join(t.TempDir(), "m.yaml")
		require.NoError(t, os.WriteFile(manifest, []byte(invalidISOManifestYAML()), 0o600))
		// The manifest ISO is invalid; valid overrides must make Validate pass and reach the
		// build (--write-device is rejected there without a privileged op).
		err := runCreateUsbCmd(t, "--manifest", manifest, "--write-device", "/dev/sdx",
			"--rhcos-live-iso", "https://example.com/override.iso", "--rhcos-live-iso-sha256", testSHA256)
		assert.ErrorContains(t, err, "--write-device is not yet supported")
	})
	t.Run("invalid flags override a valid manifest ISO (overridden value is validated)", func(t *testing.T) {
		manifest := filepath.Join(t.TempDir(), "m.yaml")
		require.NoError(t, os.WriteFile(manifest, []byte(validManifestYAML()), 0o600))
		// The manifest ISO is valid; invalid overrides must be the value that gets validated and fails.
		err := runCreateUsbCmd(t, "--manifest", manifest, "--output", "/tmp/out.img",
			"--rhcos-live-iso", "http://insecure.example.com/override.iso", "--rhcos-live-iso-sha256", testSHA256)
		assert.ErrorContains(t, err, "manifest validation failed")
		assert.ErrorContains(t, err, "https")
	})
}

// TestCreateUsbAcceptsImageFlags verifies the image-mirroring flags are now accepted (CNF-27171
// wired image mirroring into the command): --auth-file, --lca-image, and --recert-image feed the
// media build. With --write-device set the command reaches Build and surfaces its deferral error
// rather than rejecting the flags.
func TestCreateUsbAcceptsImageFlags(t *testing.T) {
	manifest := filepath.Join(t.TempDir(), "m.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(validManifestYAML()), 0o600))
	for _, flag := range []string{"--auth-file", "--lca-image", "--recert-image"} {
		t.Run(flag, func(t *testing.T) {
			err := runCreateUsbCmd(t, "--manifest", manifest, "--write-device", "/dev/sdx", flag, "example")
			assert.ErrorContains(t, err, "--write-device is not yet supported",
				"the flag is accepted; the command reaches Build and surfaces the write-device deferral")
		})
	}
}

// TestCreateUsbRejectsInvalidUSBSize verifies --usb-size is parsed and an unparseable value is
// rejected before the build runs.
func TestCreateUsbRejectsInvalidUSBSize(t *testing.T) {
	manifest := filepath.Join(t.TempDir(), "m.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(validManifestYAML()), 0o600))
	err := runCreateUsbCmd(t, "--manifest", manifest, "--write-device", "/dev/sdx", "--usb-size", "not-a-size")
	assert.ErrorContains(t, err, "invalid --usb-size")
}

// TestCreateUsbWriteDeviceDeferred verifies the command wires createusb.Build and surfaces
// its --write-device deferral error (the destructive device path is rejected until media
// assembly is complete). The --output foundation path runs real privileged ops and is
// covered by the createusb package unit tests (via dependency injection) and the
// root-gated integration test.
func TestCreateUsbWriteDeviceDeferred(t *testing.T) {
	manifest := filepath.Join(t.TempDir(), "m.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(validManifestYAML()), 0o600))

	err := runCreateUsbCmd(t, "--manifest", manifest, "--write-device", "/dev/sdx")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--write-device is not yet supported")
}

func TestCreateUsbInvalidManifestFailsValidation(t *testing.T) {
	// A manifest that parses but violates a rule must fail before the build stub.
	manifest := filepath.Join(t.TempDir(), "m.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte("mode: install-only\ndisconnected: false\n"), 0o600))

	err := runCreateUsbCmd(t, "--manifest", manifest, "--output", "/tmp/out.img")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "manifest validation failed")
}

func TestCredentialsOutputDir(t *testing.T) {
	// The default credentials directory must key off the full output image file name so distinct
	// images built into the same parent directory do not overwrite each other's credentials. The
	// extension is kept, not stripped.
	assert.Equal(t, filepath.Join("out", "cluster-a.img-credentials"), credentialsOutputDir(filepath.Join("out", "cluster-a.img")))
	assert.Equal(t, filepath.Join("out", "cluster-b.img-credentials"), credentialsOutputDir(filepath.Join("out", "cluster-b.img")))

	// Two distinct images in the same parent directory resolve to distinct credential directories.
	a := credentialsOutputDir(filepath.Join("out", "cluster-a.img"))
	b := credentialsOutputDir(filepath.Join("out", "cluster-b.img"))
	assert.NotEqual(t, a, b)

	// An .img extension is not enforced, so an output with the extension and one without it must
	// still resolve to distinct credential directories (stripping the extension would collide them).
	withExt := credentialsOutputDir(filepath.Join("out", "cluster-a.img"))
	withoutExt := credentialsOutputDir(filepath.Join("out", "cluster-a"))
	assert.NotEqual(t, withExt, withoutExt)
	assert.Equal(t, filepath.Join("out", "cluster-a-credentials"), withoutExt)

	// No output file path (device target) falls back to a fixed local directory.
	assert.Equal(t, "credentials", credentialsOutputDir(""))
}

// selfContainedManifestYAML returns a self-contained manifest with the credential fields omitted,
// so it needs automatic generation (which requires --release-image). It need not pass full
// validation: the credential-sourcing check runs before manifest validation.
func selfContainedManifestYAML() string {
	return `mode: self-contained
seedImage: quay.io/example/seed@sha256:` + testSHA256 + `
seedVersion: 4.20.0
releaseVersion: 4.20.0
installationDisk: /dev/disk/by-id/ata-example
disconnected: true
shutdown: false
siteConfig:
  clusterName: example
`
}

// TestCreateUsbRequiresReleaseImage verifies that a self-contained manifest needing credential
// generation fails fast (before the build) when neither --release-image nor the unsupported override
// is set to source openshift-install.
func TestCreateUsbRequiresReleaseImage(t *testing.T) {
	manifest := filepath.Join(t.TempDir(), "m.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(selfContainedManifestYAML()), 0o600))

	err := runCreateUsbCmd(t, "--manifest", manifest, "--output", filepath.Join(t.TempDir(), "out.img"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--release-image")
}

func TestExistingCredentialArtifacts(t *testing.T) {
	dir := t.TempDir()
	assert.Empty(t, existingCredentialArtifacts(dir))

	require.NoError(t, os.WriteFile(filepath.Join(dir, createusb.KubeconfigArtifactName), []byte("x"), 0o600))
	got := existingCredentialArtifacts(dir)
	require.Len(t, got, 1)
	assert.Equal(t, filepath.Join(dir, createusb.KubeconfigArtifactName), got[0])

	require.NoError(t, os.WriteFile(filepath.Join(dir, createusb.KubeadminPasswordArtifactName), []byte("y"), 0o600))
	assert.Len(t, existingCredentialArtifacts(dir), 2)
}

func TestResolveOverwriteConsent(t *testing.T) {
	// The force branch logs via the package logger; initialize it as Execute() would.
	log.SetFormatter(&logrus.TextFormatter{})
	log.SetOutput(io.Discard)

	existing := []string{"/creds/kubeconfig"}

	t.Run("force overwrites without prompting", func(t *testing.T) {
		proceed, err := resolveOverwriteConsent("/creds", existing, true, false, strings.NewReader(""))
		require.NoError(t, err)
		assert.True(t, proceed)
	})
	t.Run("non-interactive without force is refused", func(t *testing.T) {
		proceed, err := resolveOverwriteConsent("/creds", existing, false, false, strings.NewReader(""))
		require.Error(t, err)
		assert.False(t, proceed)
		assert.Contains(t, err.Error(), "--force")
	})
	t.Run("interactive yes proceeds", func(t *testing.T) {
		for _, answer := range []string{"y\n", "yes\n", "Y\n"} {
			proceed, err := resolveOverwriteConsent("/creds", existing, false, true, strings.NewReader(answer))
			require.NoError(t, err, answer)
			assert.True(t, proceed, answer)
		}
	})
	t.Run("interactive no or empty declines", func(t *testing.T) {
		for _, answer := range []string{"n\n", "\n", ""} {
			proceed, err := resolveOverwriteConsent("/creds", existing, false, true, strings.NewReader(answer))
			require.NoError(t, err, answer)
			assert.False(t, proceed, answer)
		}
	})
}

func TestResolveCredentialsDir(t *testing.T) {
	// An explicit --credentials-dir always wins over the per-output default.
	opts := createUsbOptions{outputPath: filepath.Join("out", "cluster-a.img"), credentialsDir: filepath.Join("custom", "creds")}
	assert.Equal(t, filepath.Join("custom", "creds"), resolveCredentialsDir(opts))

	// With no override, it falls back to the per-output default (credentialsOutputDir). The pre-build
	// preflight and the post-build publish must resolve to the same directory.
	opts = createUsbOptions{outputPath: filepath.Join("out", "cluster-a.img")}
	assert.Equal(t, credentialsOutputDir(filepath.Join("out", "cluster-a.img")), resolveCredentialsDir(opts))
	assert.Equal(t, filepath.Join("out", "cluster-a.img-credentials"), resolveCredentialsDir(opts))
}
