package ibiconfig

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	testRHCOSLiveISO    = "https://example.com/rhcos-live.x86_64.iso"
	testSeedImage       = "quay.io/example/seed:latest"
	testSeedVersion     = "4.20.0"
	testInstallDisk     = "/dev/disk/by-id/ata-example"
	testLocalImagesPath = "/mnt/images"
	testDigestsPath     = "/mnt/digests.json"
	validPullSecret     = `{"auths":{"registry.example.com":{"auth":"dXNlcjpwYXNz"}}}`
)

func TestIBIPrepareConfigMarshalRoundTrip(t *testing.T) {
	cfg := IBIPrepareConfig{
		SeedImage:           testSeedImage,
		SeedVersion:         testSeedVersion,
		InstallationDisk:    testInstallDisk,
		LocalImagesPath:     testLocalImagesPath,
		Disconnected:        true,
		ExpectedDigestsPath: testDigestsPath,
	}

	data, err := json.Marshal(cfg)
	assert.NoError(t, err)

	// JSON keys are the expected camelCase tags.
	var raw map[string]interface{}
	assert.NoError(t, json.Unmarshal(data, &raw))
	assert.Equal(t, testLocalImagesPath, raw["localImagesPath"])
	assert.Equal(t, true, raw["disconnected"])
	assert.Equal(t, testDigestsPath, raw["expectedDigestsPath"])

	// Values round-trip.
	var got IBIPrepareConfig
	assert.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, testLocalImagesPath, got.LocalImagesPath)
	assert.True(t, got.Disconnected)
	assert.Equal(t, testDigestsPath, got.ExpectedDigestsPath)
}

func TestIBIPrepareConfigFieldsOmittedWhenZero(t *testing.T) {
	cfg := IBIPrepareConfig{
		SeedImage:        testSeedImage,
		SeedVersion:      testSeedVersion,
		InstallationDisk: testInstallDisk,
	}

	data, err := json.Marshal(cfg)
	assert.NoError(t, err)

	var raw map[string]interface{}
	assert.NoError(t, json.Unmarshal(data, &raw))
	_, hasLocal := raw["localImagesPath"]
	_, hasDisconnected := raw["disconnected"]
	_, hasDigests := raw["expectedDigestsPath"]
	assert.False(t, hasLocal, "localImagesPath should be omitted when empty")
	assert.False(t, hasDisconnected, "disconnected should be omitted when false")
	assert.False(t, hasDigests, "expectedDigestsPath should be omitted when empty")
}

func baseInstallConfig() ImageBasedInstallConfig {
	return ImageBasedInstallConfig{
		RHCOSLiveISO: testRHCOSLiveISO,
		PullSecret:   validPullSecret,
		IBIPrepareConfig: IBIPrepareConfig{
			SeedImage:        testSeedImage,
			SeedVersion:      testSeedVersion,
			InstallationDisk: testInstallDisk,
		},
	}
}

func TestValidateConnectedRequiresPullSecret(t *testing.T) {
	cfg := baseInstallConfig()
	cfg.PullSecret = ""
	cfg.Disconnected = false

	err := cfg.Validate()
	assert.EqualError(t, err, "pullSecret is required")
}

func TestValidateDisconnectedAllowsEmptyPullSecret(t *testing.T) {
	cfg := baseInstallConfig()
	cfg.PullSecret = ""
	cfg.Disconnected = true
	cfg.LocalImagesPath = testLocalImagesPath
	cfg.ExpectedDigestsPath = testDigestsPath
	cfg.Profile = ProfileInstallOnly

	assert.NoError(t, cfg.Validate())
}

func TestValidateDisconnectedWithSyntheticPullSecretIsValid(t *testing.T) {
	cfg := baseInstallConfig()
	cfg.PullSecret = `{"auths":{}}`
	cfg.Disconnected = true
	cfg.LocalImagesPath = testLocalImagesPath
	cfg.ExpectedDigestsPath = testDigestsPath
	cfg.Profile = ProfileSelfContained

	assert.NoError(t, cfg.Validate())

	// A malformed pull secret is still rejected when disconnected.
	cfg.PullSecret = "not-json"
	assert.EqualError(t, cfg.Validate(), "pullSecret is not a valid json")
}

func TestValidateDisconnectedRequiresLocalImagesPath(t *testing.T) {
	cfg := baseInstallConfig()
	cfg.PullSecret = ""
	cfg.Disconnected = true
	cfg.LocalImagesPath = ""

	err := cfg.Validate()
	assert.EqualError(t, err, "localImagesPath is required when disconnected is set")
}

func TestValidateDisconnectedRequiresExpectedDigestsPath(t *testing.T) {
	// A disconnected prepare always parses the digest manifest at ExpectedDigestsPath,
	// so validation must reject a disconnected config that leaves it empty rather than
	// accepting one prepare cannot use.
	cfg := baseInstallConfig()
	cfg.PullSecret = ""
	cfg.Disconnected = true
	cfg.LocalImagesPath = testLocalImagesPath
	cfg.ExpectedDigestsPath = ""

	err := cfg.Validate()
	assert.EqualError(t, err, "expectedDigestsPath is required when disconnected is set")
}

func TestValidateConnectedIgnoresLocalImageFields(t *testing.T) {
	// When connected, the local-image fields are ignored and not required.
	cfg := baseInstallConfig()
	cfg.Disconnected = false
	cfg.LocalImagesPath = ""
	cfg.ExpectedDigestsPath = ""

	assert.NoError(t, cfg.Validate())
}

func TestValidateConnectedIgnoresProfile(t *testing.T) {
	// The profile is a USB (disconnected) concern; a connected config needs none and an
	// empty value must not be rejected.
	cfg := baseInstallConfig()
	cfg.Disconnected = false
	cfg.Profile = ""

	assert.NoError(t, cfg.Validate())
}

func TestValidateDisconnectedRequiresProfile(t *testing.T) {
	// A disconnected prepare records its media profile and selects the ibi-status (p4)
	// re-entry guard behaviour from it, so an empty profile is rejected rather than
	// defaulting silently.
	cfg := baseInstallConfig()
	cfg.PullSecret = ""
	cfg.Disconnected = true
	cfg.LocalImagesPath = testLocalImagesPath
	cfg.ExpectedDigestsPath = testDigestsPath
	cfg.Profile = ""

	assert.EqualError(t, cfg.Validate(), "profile is required when disconnected is set")
}

func TestValidateDisconnectedRejectsUnknownProfile(t *testing.T) {
	cfg := baseInstallConfig()
	cfg.PullSecret = ""
	cfg.Disconnected = true
	cfg.LocalImagesPath = testLocalImagesPath
	cfg.ExpectedDigestsPath = testDigestsPath
	cfg.Profile = "bogus"

	assert.EqualError(t, cfg.Validate(),
		`profile must be "install-only" or "self-contained", got "bogus"`)
}
