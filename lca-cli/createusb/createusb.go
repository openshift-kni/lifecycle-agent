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

// Package createusb implements the manifest schema and build-time validation for
// the `lca-cli create-usb` subcommand (USB-based IBI). It parses the single-manifest
// YAML/JSON schema, enforces every build-time invariant the install-only and
// self-contained flows depend on, and resolves @file pull-secret references on the
// workstation at build time.
//
// The actual media-assembly pipeline (live ISO handling, OCI image layout, config-tree
// render, image resolution/digest-pinning, and ISO/device write) is intentionally out
// of scope here and lands in CNF-27167, CNF-27168, and CNF-27169; Build is a documented
// stub that returns an explicit not-implemented error.
package createusb

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// Mode is the create-usb manifest profile. It is a create-usb concern only and is
// deliberately not part of IBIPrepareConfig (see §8.A of the USB-based IBI proposal).
type Mode string

const (
	// ModeInstallOnly produces media that installs the seed and shuts down.
	ModeInstallOnly Mode = "install-only"
	// ModeSelfContained produces media that installs and reconfigures the node from
	// an on-media site configuration without contacting the network.
	ModeSelfContained Mode = "self-contained"
)

const (
	// pullSecretFilePrefix marks a siteConfig.pullSecret value as a workstation file
	// reference to be resolved at build time (build-time only; never survives into
	// rendered output).
	pullSecretFilePrefix = "@"

	// seedImageDigestMarker is required in seedImage so the seed is digest-pinned.
	seedImageDigestMarker = "@sha256:"

	// httpsScheme is the only scheme accepted for the RHCOS live ISO URL.
	httpsScheme = "https://"
)

// sha256HexRegex matches a 64-character lowercase hex SHA-256 digest.
var sha256HexRegex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// RHCOSLiveISO is the structured reference to the RHCOS live ISO used to build the media.
type RHCOSLiveISO struct {
	URL    string `json:"url,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// ManifestSource is a path reference to a manifest file on the workstation (resolved at
// build time by the self-contained flow).
type ManifestSource struct {
	Path string `json:"path,omitempty"`
}

// SiteConfig is the self-contained site configuration unioned into the manifest. It
// mirrors the field semantics of api/seedreconfig.SeedReconfiguration; it is mapped to
// SeedReconfiguration when the p3 config tree is rendered in CNF-27169.
type SiteConfig struct {
	ClusterName      string   `json:"clusterName,omitempty"`
	BaseDomain       string   `json:"baseDomain,omitempty"`
	Hostname         string   `json:"hostname,omitempty"`
	NodeIPs          []string `json:"nodeIPs,omitempty"`
	MachineNetworks  []string `json:"machineNetworks,omitempty"`
	RawNMStateConfig string   `json:"rawNMStateConfig,omitempty"`
	SSHKey           string   `json:"sshKey,omitempty"`
	PullSecret       string   `json:"pullSecret,omitempty"`
}

// Manifest is the §6.3 single-manifest schema for `lca-cli create-usb`.
//
// Fields are either common to both profiles, install-only only, or self-contained only;
// the field-presence rule in Validate rejects a field used under the wrong mode.
type Manifest struct {
	// Mode selects the profile (install-only or self-contained).
	Mode Mode `json:"mode,omitempty"`

	// Common (both profiles), all required.
	SeedImage        string `json:"seedImage,omitempty"`
	SeedVersion      string `json:"seedVersion,omitempty"`
	ReleaseVersion   string `json:"releaseVersion,omitempty"`
	InstallationDisk string `json:"installationDisk,omitempty"`

	// Disconnected must be true for both profiles (it is not a knob).
	Disconnected bool `json:"disconnected,omitempty"`
	// Shutdown must match the mode (install-only ⇒ true, self-contained ⇒ false).
	Shutdown bool `json:"shutdown,omitempty"`

	// RHCOSLiveISO is required; the --rhcos-live-iso* flags override it (flag wins).
	RHCOSLiveISO RHCOSLiveISO `json:"rhcosLiveIso,omitempty"`

	// ExtraPrecacheImages is install-only only.
	ExtraPrecacheImages []string `json:"extraPrecacheImages,omitempty"`

	// ClusterManifests/ExtraManifests/SiteConfig are self-contained only.
	ClusterManifests []ManifestSource `json:"clusterManifests,omitempty"`
	ExtraManifests   []ManifestSource `json:"extraManifests,omitempty"`
	SiteConfig       *SiteConfig      `json:"siteConfig,omitempty"`
}

// ParseManifest reads and strictly decodes the manifest at path.
//
// Decoding is strict: unknown fields and duplicate YAML keys are rejected so a mistyped
// or mode-wrong key fails the build instead of being silently ignored. This deliberately
// deviates from the non-strict utils.ReadYamlOrJSONFile used by ReadIBIConfigFile,
// because §6.3 is a "fail the build on any violation" contract. JSON input is still
// accepted (YAML is a superset decoded through the same path).
func ParseManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Clean(path)) //nolint:gosec // path is the build-time manifest supplied by the operator
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest %s: %w", path, err)
	}

	var m Manifest
	if err := yaml.UnmarshalStrict(data, &m); err != nil {
		return nil, fmt.Errorf("failed to parse manifest %s: %w", path, err)
	}
	return &m, nil
}

// Validate enforces every in-scope build-time invariant. Each violation returns a
// non-nil error (fails the build). Checks run structural-first, then pull-secret
// resolution, then content checks.
func (m *Manifest) Validate() error {
	// 1. mode must be a known value.
	switch m.Mode {
	case ModeInstallOnly, ModeSelfContained:
	default:
		return fmt.Errorf("mode must be %q or %q, got %q", ModeInstallOnly, ModeSelfContained, m.Mode)
	}

	// 2. field-presence by mode.
	if err := m.validateFieldPresence(); err != nil {
		return err
	}

	// 3. disconnected must be true for both profiles (it is not a knob).
	if !m.Disconnected {
		return fmt.Errorf("disconnected must be true")
	}

	// 4. shutdown must match the mode.
	switch m.Mode {
	case ModeInstallOnly:
		if !m.Shutdown {
			return fmt.Errorf("shutdown must be true for mode %q", ModeInstallOnly)
		}
	case ModeSelfContained:
		if m.Shutdown {
			return fmt.Errorf("shutdown must be false for mode %q", ModeSelfContained)
		}
	}

	// 5. required fields (both profiles).
	if err := m.validateRequiredFields(); err != nil {
		return err
	}

	// 6. self-contained specifics (site config, pull secret, mirror hook).
	if m.Mode == ModeSelfContained {
		if err := m.validateSelfContained(); err != nil {
			return err
		}
		if err := m.validateMirrorResources(); err != nil {
			return err
		}
	}

	return nil
}

// validateFieldPresence rejects a mode-specific field that appears under the wrong mode.
func (m *Manifest) validateFieldPresence() error {
	switch m.Mode {
	case ModeInstallOnly:
		if m.SiteConfig != nil {
			return fmt.Errorf("siteConfig is only valid for mode %q", ModeSelfContained)
		}
		if len(m.ClusterManifests) > 0 {
			return fmt.Errorf("clusterManifests is only valid for mode %q", ModeSelfContained)
		}
		if len(m.ExtraManifests) > 0 {
			return fmt.Errorf("extraManifests is only valid for mode %q", ModeSelfContained)
		}
	case ModeSelfContained:
		if len(m.ExtraPrecacheImages) > 0 {
			return fmt.Errorf("extraPrecacheImages is only valid for mode %q", ModeInstallOnly)
		}
	}
	return nil
}

// validateRequiredFields checks the fields required for both profiles.
func (m *Manifest) validateRequiredFields() error {
	if m.SeedImage == "" {
		return fmt.Errorf("seedImage is required")
	}
	if !strings.Contains(m.SeedImage, seedImageDigestMarker) {
		return fmt.Errorf("seedImage must be digest-pinned (contain %q)", seedImageDigestMarker)
	}
	if m.SeedVersion == "" {
		return fmt.Errorf("seedVersion is required")
	}
	if m.ReleaseVersion == "" {
		return fmt.Errorf("releaseVersion is required")
	}
	if m.InstallationDisk == "" {
		return fmt.Errorf("installationDisk is required")
	}
	return m.validateRHCOSLiveISO()
}

// validateRHCOSLiveISO enforces an HTTPS url and a well-formed SHA-256 digest.
func (m *Manifest) validateRHCOSLiveISO() error {
	if m.RHCOSLiveISO.URL == "" {
		return fmt.Errorf("rhcosLiveIso.url is required")
	}
	if !strings.HasPrefix(m.RHCOSLiveISO.URL, httpsScheme) {
		return fmt.Errorf("rhcosLiveIso.url must be an https url")
	}
	if m.RHCOSLiveISO.SHA256 == "" {
		return fmt.Errorf("rhcosLiveIso.sha256 is required")
	}
	if !sha256HexRegex.MatchString(m.RHCOSLiveISO.SHA256) {
		return fmt.Errorf("rhcosLiveIso.sha256 must be a 64-character lowercase hex string")
	}
	return nil
}

// validateSelfContained enforces the self-contained site configuration, resolving the
// @file pull-secret reference before validating its JSON contents.
func (m *Manifest) validateSelfContained() error {
	if m.SiteConfig == nil {
		return fmt.Errorf("siteConfig is required for mode %q", ModeSelfContained)
	}

	// Resolve @file pull-secret references before the JSON check so a valid @file
	// reference is not rejected by that check.
	if err := m.ResolvePullSecret(); err != nil {
		return err
	}
	if m.SiteConfig.PullSecret == "" {
		return fmt.Errorf("siteConfig.pullSecret is required for mode %q", ModeSelfContained)
	}
	var ps interface{}
	if err := json.Unmarshal([]byte(m.SiteConfig.PullSecret), &ps); err != nil {
		return fmt.Errorf("siteConfig.pullSecret is not valid json: %w", err)
	}

	// Required identity fields the reconfigure path needs. Deeper per-field
	// SeedReconfiguration validation lands with the p3 render in CNF-27169.
	if m.SiteConfig.ClusterName == "" {
		return fmt.Errorf("siteConfig.clusterName is required for mode %q", ModeSelfContained)
	}
	if m.SiteConfig.BaseDomain == "" {
		return fmt.Errorf("siteConfig.baseDomain is required for mode %q", ModeSelfContained)
	}
	if m.SiteConfig.Hostname == "" {
		return fmt.Errorf("siteConfig.hostname is required for mode %q", ModeSelfContained)
	}

	// The self-contained media carries the cluster's configuration on-media.
	if len(m.ClusterManifests) == 0 {
		return fmt.Errorf("clusterManifests is required for mode %q", ModeSelfContained)
	}

	return nil
}

// ResolvePullSecret resolves an @file siteConfig.pullSecret reference to the file's
// literal contents on the workstation at build time. A literal (non-@) value is left
// unchanged, so the method is idempotent and safe to call standalone or from Validate.
// The @ prefix is build-time only and must never survive into rendered output.
func (m *Manifest) ResolvePullSecret() error {
	if m.SiteConfig == nil {
		return nil
	}
	value := m.SiteConfig.PullSecret
	if !strings.HasPrefix(value, pullSecretFilePrefix) {
		return nil
	}

	path := strings.TrimPrefix(value, pullSecretFilePrefix)
	contents, err := os.ReadFile(filepath.Clean(path)) //nolint:gosec // path is the build-time pull-secret file referenced by the operator's manifest
	if err != nil {
		return fmt.Errorf("failed to resolve pull-secret @file reference %q: %w", path, err)
	}
	m.SiteConfig.PullSecret = string(contents)
	return nil
}

// validateMirrorResources is the hook for the p3 mirror-resource scan that rejects
// network-reaching mirror configuration (IDMS/ICSP entries whose mirrorSourcePolicy is
// not NeverContactSource, or that point at external registries).
//
// It is wired into Validate for self-contained manifests so the enforcement point
// already exists, but in this story it is a documented no-op: the real scan operates on
// the rendered p3 manifest tree, which does not exist until the config-tree render
// pipeline lands in CNF-27169 (STORY-012). This story therefore does NOT enforce the
// external-mirror rule; CNF-27169 fills in this function.
//
//nolint:unparam // deferred stub: the real scan (and its error returns) lands in CNF-27169
func (m *Manifest) validateMirrorResources() error {
	return nil
}

// Build assembles the USB media from a validated manifest.
//
// The media-build pipeline (live ISO handling, OCI image layout, config-tree render,
// image resolution/digest-pinning, and ISO/device write) is out of scope for this story
// and lands in CNF-27167, CNF-27168, and CNF-27169. It returns an explicit
// not-implemented error so the scaffold never silently claims to have produced media.
func Build(m *Manifest) error {
	return fmt.Errorf("media build not yet implemented (CNF-27167/CNF-27168/CNF-27169)")
}
