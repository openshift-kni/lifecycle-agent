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

package cmd

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	t.Run("exactly one is accepted (reaches build stub)", func(t *testing.T) {
		err := runCreateUsbCmd(t, "--manifest", manifest, "--output", "/tmp/out.img")
		assert.ErrorContains(t, err, "not yet implemented")
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
	t.Run("both together override and reach build stub", func(t *testing.T) {
		err := runCreateUsbCmd(t, "--manifest", manifest, "--output", "/tmp/out.img",
			"--rhcos-live-iso", "https://example.com/other.iso", "--rhcos-live-iso-sha256", testSHA256)
		assert.ErrorContains(t, err, "not yet implemented")
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
		// The manifest ISO is invalid; valid overrides must make Validate pass and reach the stub.
		err := runCreateUsbCmd(t, "--manifest", manifest, "--output", "/tmp/out.img",
			"--rhcos-live-iso", "https://example.com/override.iso", "--rhcos-live-iso-sha256", testSHA256)
		assert.ErrorContains(t, err, "not yet implemented")
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

func TestCreateUsbAuthFileAccepted(t *testing.T) {
	manifest := filepath.Join(t.TempDir(), "m.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(validManifestYAML()), 0o600))
	// --auth-file is accepted (reaches the build stub), not rejected by flag parsing.
	err := runCreateUsbCmd(t, "--manifest", manifest, "--output", "/tmp/out.img",
		"--auth-file", filepath.Join(t.TempDir(), "creds.json"))
	assert.ErrorContains(t, err, "not yet implemented")
}

func TestCreateUsbBuildStubFails(t *testing.T) {
	manifest := filepath.Join(t.TempDir(), "m.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(validManifestYAML()), 0o600))

	err := runCreateUsbCmd(t, "--manifest", manifest, "--output", "/tmp/out.img")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not yet implemented")
	assert.Contains(t, err.Error(), "CNF-27167")
}

func TestCreateUsbInvalidManifestFailsValidation(t *testing.T) {
	// A manifest that parses but violates a rule must fail before the build stub.
	manifest := filepath.Join(t.TempDir(), "m.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte("mode: install-only\ndisconnected: false\n"), 0o600))

	err := runCreateUsbCmd(t, "--manifest", manifest, "--output", "/tmp/out.img")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "manifest validation failed")
}
