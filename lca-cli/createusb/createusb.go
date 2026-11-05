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

// Package createusb implements the manifest schema and build-time validation for
// the `lca-cli create-usb` subcommand (USB-based IBI). It parses the single-manifest
// YAML/JSON schema, enforces every build-time invariant the install-only and
// self-contained flows depend on, and resolves @file pull-secret references on the
// workstation at build time.
//
// The media-assembly pipeline is coordinated by createusb_assemble.go: it builds the customized
// RHCOS live ISO (createusb_p1_content.go), writes it to the front of the media at LBA 0
// (createusb_p1.go), and appends the formatted, labelled data partitions after it following the
// CNF-27197-proven DOS/MBR "append after the ISO" layout (the ibi-images, cluster-config, and
// ibi-status filesystem policies live in createusb_p2.go / createusb_p3.go / createusb_p4.go).
// OCI image layout and the on-media digest manifest land in createusb_images.go. Content
// population of ibi-images and ibi-status is owned by the sibling runtime/populate stories.
package createusb

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
	"sigs.k8s.io/yaml"

	"github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
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
)

// sha256HexRegex matches a 64-character lowercase hex SHA-256 digest.
var sha256HexRegex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// seedImageDigestRegex matches a digest-pinned image reference: a non-empty name
// component followed by a full 64-character lowercase hex sha256 digest. It rejects a
// bare tag (no digest) and a malformed digest such as "repo@sha256:" with no hex.
var seedImageDigestRegex = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

// kubeadminPasswordHashRegex matches the modular-crypt format emitted by bcrypt and
// restricts the cost to the 04-31 range supported by bcrypt.
var kubeadminPasswordHashRegex = regexp.MustCompile(`^\$2[aby]\$(?:0[4-9]|[12][0-9]|3[01])\$[./A-Za-z0-9]{53}$`)

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
	// SSHKey is one or more OpenSSH public authorized keys (newline-separated) for the
	// live ISO core user and the installed node. Never provide a private key.
	SSHKey     string `json:"sshKey,omitempty"`
	PullSecret string `json:"pullSecret,omitempty"`

	// ChronyConfig is the chrony configuration (chrony.conf contents) placed on the node. On
	// the self-contained offline profile it is the only way to make the pre-recert clock
	// gate's trusted-time path reachable: the gate confirms synchronization against the
	// source(s) declared here before recert. It must be supplied explicitly here -- the node's
	// inherited host chrony configuration is never auto-trusted. See
	// docs/post-pivot-configuration.md. When set it must declare at least one genuine time
	// source (a server/pool/peer at a real address, or a non-RTC hardware refclock);
	// RTC-backed/local sources alone are rejected at build time because the gate cannot trust
	// the very clock it exists to check.
	ChronyConfig string `json:"chronyConfig,omitempty"`

	// MaxSeedAge optionally widens the pre-recert clock gate's plausibility window for
	// long-shelf deployments. It is a Go duration string (for example "4320h" for 180 days);
	// the gate rejects a system clock later than the seed's creation time plus this bound. An
	// empty value uses the default (180 days). It is validated at build time: a non-parseable,
	// non-positive, or absurdly large value fails the build.
	MaxSeedAge string `json:"maxSeedAge,omitempty"`
	// KubeadminPasswordHash and KubeconfigCryptoRetention are generated in advance
	// together with the kubeconfig handed to the user. The hash keeps kubeadmin usable
	// after recert; the retention set makes the target cluster trust that kubeconfig.
	KubeadminPasswordHash     string                                 `json:"kubeadminPasswordHash,omitempty"`
	KubeconfigCryptoRetention seedreconfig.KubeConfigCryptoRetention `json:"kubeconfigCryptoRetention,omitempty"`
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
	// Shutdown must match the mode (install-only ⇒ true, self-contained ⇒ false). It is a
	// pointer so an omitted field is distinguishable from an explicit false: self-contained
	// requires false, which is also the zero value, so omission would otherwise pass
	// silently. A nil Shutdown is rejected as a missing required field.
	Shutdown *bool `json:"shutdown,omitempty"`

	// RHCOSLiveISO is optional; when omitted, the matching ISO is resolved from the seed.
	RHCOSLiveISO RHCOSLiveISO `json:"rhcosLiveIso,omitempty"`

	// ExtraPrecacheImages is install-only only. It is a pointer so a present-but-empty
	// list (extraPrecacheImages: []) is distinguishable from an absent field and is
	// still rejected under self-contained.
	ExtraPrecacheImages *[]string `json:"extraPrecacheImages,omitempty"`

	// ClusterManifests/ExtraManifests/SiteConfig are self-contained only. The slice
	// fields are pointers so a present-but-empty list is distinguishable from an absent
	// field (and rejected under install-only).
	ClusterManifests *[]ManifestSource `json:"clusterManifests,omitempty"`
	ExtraManifests   *[]ManifestSource `json:"extraManifests,omitempty"`
	SiteConfig       *SiteConfig       `json:"siteConfig,omitempty"`
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

	// 4. shutdown is required and must match the mode. It is required for both profiles, so
	// an omitted value fails rather than defaulting to the self-contained-valid false.
	if m.Shutdown == nil {
		return fmt.Errorf("shutdown is required")
	}
	switch m.Mode {
	case ModeInstallOnly:
		if !*m.Shutdown {
			return fmt.Errorf("shutdown must be true for mode %q", ModeInstallOnly)
		}
	case ModeSelfContained:
		if *m.Shutdown {
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
	// Presence (not length) is what disqualifies a field under the wrong mode: an
	// explicit empty list (e.g. clusterManifests: []) is still a use of the field and
	// must be rejected, so these check for a non-nil pointer rather than len() > 0.
	switch m.Mode {
	case ModeInstallOnly:
		if m.SiteConfig != nil {
			return fmt.Errorf("siteConfig is only valid for mode %q", ModeSelfContained)
		}
		if m.ClusterManifests != nil {
			return fmt.Errorf("clusterManifests is only valid for mode %q", ModeSelfContained)
		}
		if m.ExtraManifests != nil {
			return fmt.Errorf("extraManifests is only valid for mode %q", ModeSelfContained)
		}
	case ModeSelfContained:
		if m.ExtraPrecacheImages != nil {
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
	if !seedImageDigestRegex.MatchString(m.SeedImage) {
		return fmt.Errorf("seedImage must be digest-pinned with a full sha256 digest (e.g. repo@sha256:<64-char lowercase hex>)")
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

// validateHTTPSURL returns an error unless rawURL parses to an https url with a non-empty
// host, so an unusable source such as "https://" (no host) is rejected. It is the single
// source of truth for the RHCOS live ISO scheme/host rule, shared by
// Manifest.validateRHCOSLiveISO (in-pipeline validation) and fetchAndVerifyISO
// (defense-in-depth before download) so the rule and the "https url with a host" wording
// cannot drift between the two call sites.
func validateHTTPSURL(rawURL string) error {
	if u, err := url.Parse(rawURL); err != nil || u.Scheme != schemeHTTPS || u.Host == "" {
		return fmt.Errorf("must be an https url with a host")
	}
	return nil
}

// validateRHCOSLiveISO enforces an HTTPS url and a well-formed SHA-256 digest.
func (m *Manifest) validateRHCOSLiveISO() error {
	if m.RHCOSLiveISO.URL == "" && m.RHCOSLiveISO.SHA256 == "" {
		return nil
	}
	if m.RHCOSLiveISO.URL == "" {
		return fmt.Errorf("rhcosLiveIso.url is required when sha256 is set")
	}
	if err := validateHTTPSURL(m.RHCOSLiveISO.URL); err != nil {
		return fmt.Errorf("rhcosLiveIso.url %w", err)
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

	// Required kubeconfig identity data. These values are generated with the kubeconfig
	// that will be handed to the user; omitting them would leave the installed cluster
	// without a working pre-generated kubeconfig or kubeadmin credential.
	if err := m.validateSiteConfigKubeadminPasswordHash(); err != nil {
		return err
	}
	if err := m.validateSiteConfigKubeconfigCryptoRetention(); err != nil {
		return err
	}

	// Required identity fields the reconfigure path needs.
	if m.SiteConfig.ClusterName == "" {
		return fmt.Errorf("siteConfig.clusterName is required for mode %q", ModeSelfContained)
	}
	if m.SiteConfig.BaseDomain == "" {
		return fmt.Errorf("siteConfig.baseDomain is required for mode %q", ModeSelfContained)
	}
	if m.SiteConfig.Hostname == "" {
		return fmt.Errorf("siteConfig.hostname is required for mode %q", ModeSelfContained)
	}

	// Per-field SeedReconfiguration validation (deferred from the schema story): the optional
	// nodeIPs/machineNetworks are rendered verbatim into manifest.json and consumed by
	// post-pivot, so a malformed value must fail the build here rather than on the node.
	if err := m.validateSiteConfigNetworkFields(); err != nil {
		return err
	}
	// Optional sshKey is embedded in the live ISO ignition and the on-media seed
	// reconfiguration, so reject malformed values and private-key material at build time.
	if err := m.validateSiteConfigSSHKey(); err != nil {
		return err
	}
	// Optional clock-gate knobs (maxSeedAge, chronyConfig) are rendered into manifest.json and
	// consumed by post-pivot's pre-recert clock gate, so a value that the gate would reject at
	// runtime must fail the build here instead of halting the node offline.
	if err := m.validateSiteConfigClockGate(); err != nil {
		return err
	}

	// The self-contained media carries the cluster's configuration on-media.
	if m.ClusterManifests == nil || len(*m.ClusterManifests) == 0 {
		return fmt.Errorf("clusterManifests is required for mode %q", ModeSelfContained)
	}
	if err := validateManifestSources("clusterManifests", *m.ClusterManifests); err != nil {
		return err
	}
	// extraManifests is optional, but any entry present must reference a real file.
	if m.ExtraManifests != nil {
		if err := validateManifestSources("extraManifests", *m.ExtraManifests); err != nil {
			return err
		}
	}

	return nil
}

// validateSiteConfigNetworkFields validates the optional network identity fields that are
// rendered into the SeedReconfiguration manifest.json: every nodeIPs entry must parse as an
// IP address and every machineNetworks entry as a CIDR. Empty lists are allowed (the fields
// are optional).
func (m *Manifest) validateSiteConfigNetworkFields() error {
	for i, ip := range m.SiteConfig.NodeIPs {
		if net.ParseIP(strings.TrimSpace(ip)) == nil {
			return fmt.Errorf("siteConfig.nodeIPs[%d] %q is not a valid IP address", i, ip)
		}
	}
	for i, cidr := range m.SiteConfig.MachineNetworks {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err != nil {
			return fmt.Errorf("siteConfig.machineNetworks[%d] %q is not a valid CIDR: %w", i, cidr, err)
		}
	}
	return nil
}

// validateSiteConfigSSHKey validates the optional siteConfig.sshKey: when set it must
// contain one or more parseable OpenSSH public authorized keys. Private-key PEM material
// and malformed lines are rejected so they cannot be embedded in the image.
func (m *Manifest) validateSiteConfigSSHKey() error {
	_, err := parseSSHAuthorizedKeys(m.SiteConfig.SSHKey)
	return err
}

// validateSiteConfigClockGate validates the optional clock-gate tuning knobs so a value the
// runtime clock gate would reject fails the build rather than halting the node offline. Both
// fields are optional and share the same parsing/trust rules the gate applies, via the shared
// api/seedreconfig helpers, so build-time and runtime acceptance cannot drift:
//
//   - maxSeedAge, when set, must be a positive Go duration string within the allowed bound.
//   - chronyConfig, when set, must declare at least one genuine trusted time source. A config
//     that carries only RTC-backed/local sources (or none) would silently fall through to the
//     seed-age window at runtime, so rejecting it here keeps the trusted-time mitigation this
//     field exists for actually reachable, without the node ever auto-trusting inherited host
//     chrony configuration.
func (m *Manifest) validateSiteConfigClockGate() error {
	if _, err := seedreconfig.ParseMaxSeedAge(m.SiteConfig.MaxSeedAge); err != nil {
		return fmt.Errorf("siteConfig.maxSeedAge is invalid: %w", err)
	}
	if strings.TrimSpace(m.SiteConfig.ChronyConfig) != "" && !seedreconfig.ChronyConfigHasTrustedSource(m.SiteConfig.ChronyConfig) {
		return fmt.Errorf("siteConfig.chronyConfig declares no trusted time source for the pre-recert clock gate: provide a server/pool/peer pointing at a real address or a non-RTC hardware refclock (RTC-backed or local-only sources are not trusted), or omit chronyConfig to rely on the seed-age plausibility window")
	}
	return nil
}

// validateSiteConfigKubeadminPasswordHash requires a syntactically valid bcrypt
// hash so a truncated or mistyped value cannot ship unusable kubeadmin media.
func (m *Manifest) validateSiteConfigKubeadminPasswordHash() error {
	m.SiteConfig.KubeadminPasswordHash = strings.TrimSpace(m.SiteConfig.KubeadminPasswordHash)
	hash := m.SiteConfig.KubeadminPasswordHash
	if hash == "" {
		return fmt.Errorf("siteConfig.kubeadminPasswordHash is required for mode %q", ModeSelfContained)
	}
	if !kubeadminPasswordHashRegex.MatchString(hash) {
		return fmt.Errorf("siteConfig.kubeadminPasswordHash must be a bcrypt hash")
	}
	return nil
}

// validateSiteConfigKubeconfigCryptoRetention rejects partial retention before the
// build can ship media that cannot authenticate the pre-generated kubeconfig.
func (m *Manifest) validateSiteConfigKubeconfigCryptoRetention() error {
	r := m.SiteConfig.KubeconfigCryptoRetention
	if missing := r.MissingPEMFields(); len(missing) > 0 {
		return fmt.Errorf("siteConfig.kubeconfigCryptoRetention.%s is required for mode %q", missing[0], ModeSelfContained)
	}
	m.SiteConfig.KubeconfigCryptoRetention.IngresssCrypto.IngressCertificateCN = strings.TrimSpace(r.IngresssCrypto.IngressCertificateCN)
	if m.SiteConfig.KubeconfigCryptoRetention.IngresssCrypto.IngressCertificateCN == "" {
		return fmt.Errorf("siteConfig.kubeconfigCryptoRetention.ingress_certificate_cn is required for mode %q", ModeSelfContained)
	}
	return validateKubeconfigCryptoRetentionPEMs(r)
}

// validateKubeconfigCryptoRetentionPEMs rejects non-PEM or unparseable material
// before media is built. The values are written verbatim for recert, so invalid
// input must fail at build time rather than after installation.
func validateKubeconfigCryptoRetentionPEMs(r seedreconfig.KubeConfigCryptoRetention) error {
	if err := validateCertificateBundlePEM(string(r.KubeAPICrypto.ClientAuthCrypto.AdminCACertificate), "admin_ca_certificate"); err != nil {
		return err
	}
	privateKeys := []struct {
		name  string
		value seedreconfig.PEM
	}{
		{"loadbalancer_external_signer_private_key", r.KubeAPICrypto.ServingCrypto.LoadbalancerSignerPrivateKey},
		{"localhost_signer_private_key", r.KubeAPICrypto.ServingCrypto.LocalhostSignerPrivateKey},
		{"service_network_signer_private_key", r.KubeAPICrypto.ServingCrypto.ServiceNetworkSignerPrivateKey},
		{"ingress_ca", r.IngresssCrypto.IngressCAPrivateKey},
	}
	for _, key := range privateKeys {
		if err := validatePrivateKeyPEM(string(key.value), key.name); err != nil {
			return err
		}
	}
	return nil
}

// validateCertificateBundlePEM requires at least one parseable CERTIFICATE block
// and rejects trailing non-PEM data.
func validateCertificateBundlePEM(certPEM, field string) error {
	rest := []byte(certPEM)
	found := false
	for {
		trimmed := bytes.TrimLeft(rest, " \t\r\n")
		if !bytes.HasPrefix(trimmed, []byte("-----BEGIN ")) {
			break
		}
		block, remaining := pem.Decode(trimmed)
		if block == nil {
			break
		}
		rest = remaining
		if block.Type != "CERTIFICATE" {
			return fmt.Errorf("siteConfig.kubeconfigCryptoRetention.%s must contain a CERTIFICATE PEM block", field)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return fmt.Errorf("siteConfig.kubeconfigCryptoRetention.%s contains an invalid certificate: %w", field, err)
		}
		found = true
	}
	if !found {
		return fmt.Errorf("siteConfig.kubeconfigCryptoRetention.%s must contain a CERTIFICATE PEM block", field)
	}
	if strings.TrimSpace(string(rest)) != "" {
		return fmt.Errorf("siteConfig.kubeconfigCryptoRetention.%s contains non-PEM data", field)
	}
	return nil
}

// validatePrivateKeyPEM accepts the unencrypted private-key formats used by the
// kubeconfig-retention secrets: PKCS#1, PKCS#8, and SEC 1 EC.
func validatePrivateKeyPEM(keyPEM, field string) error {
	trimmed := bytes.TrimLeft([]byte(keyPEM), " \t\r\n")
	if !bytes.HasPrefix(trimmed, []byte("-----BEGIN ")) {
		return fmt.Errorf("siteConfig.kubeconfigCryptoRetention.%s must contain a private-key PEM block", field)
	}
	block, rest := pem.Decode(trimmed)
	if block == nil || !strings.Contains(block.Type, "PRIVATE KEY") {
		return fmt.Errorf("siteConfig.kubeconfigCryptoRetention.%s must contain a private-key PEM block", field)
	}
	if strings.TrimSpace(string(rest)) != "" {
		return fmt.Errorf("siteConfig.kubeconfigCryptoRetention.%s contains non-PEM data", field)
	}
	if _, err := parsePrivateKey(block.Bytes); err != nil {
		return fmt.Errorf("siteConfig.kubeconfigCryptoRetention.%s contains an invalid private key: %w", field, err)
	}
	return nil
}

func parsePrivateKey(der []byte) (any, error) {
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(der); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("unsupported private-key format")
}

// parseSSHAuthorizedKeys returns the non-empty, non-comment public authorized key lines
// from sshKey. An empty/whitespace value yields a nil slice (the field is optional).
// Private-key PEM material and any non-empty line that is not a valid authorized key are
// rejected.
func parseSSHAuthorizedKeys(sshKey string) ([]string, error) {
	raw := strings.TrimSpace(sshKey)
	if raw == "" {
		return nil, nil
	}
	if strings.Contains(raw, "PRIVATE KEY") {
		return nil, fmt.Errorf("siteConfig.sshKey must be public authorized key(s), not private-key material")
	}
	var keys []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err != nil {
			return nil, fmt.Errorf("siteConfig.sshKey contains a malformed public authorized key: %w", err)
		}
		keys = append(keys, line)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("siteConfig.sshKey must contain at least one public authorized key")
	}
	return keys, nil
}

// validateManifestSources rejects a manifest source with an empty path: each entry must
// name a file for the later media build to render, so clusterManifests: [{path: ""}] fails
// the build rather than being carried through as an unusable source.
func validateManifestSources(field string, sources []ManifestSource) error {
	for i, s := range sources {
		if strings.TrimSpace(s.Path) == "" {
			return fmt.Errorf("%s[%d].path is required", field, i)
		}
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

// validateMirrorResources is the self-contained enforcement point for the mirror-resource
// scan that rejects network-reaching mirror configuration (proposal §6.3, CWE-16).
//
// The real scan operates on the rendered p3 manifest tree, which exists only at media-build
// time: buildImageLayout renders the tree and runs ScanMirrorResources
// (createusb_p3_content.go) over it before mirroring, failing the build on any IDMS/ITMS/ICSP that
// is not strictly local + NeverContactSource. Validate cannot run that scan because the
// tree is not rendered yet (and Validate intentionally does not read manifest-source
// contents), so this hook stays a no-op; it is retained so Validate's self-contained
// enforcement structure remains visible alongside the build-time scan it points at.
//
//nolint:unparam // enforcement is the build-time rendered-tree scan (ScanMirrorResources)
func (m *Manifest) validateMirrorResources() error {
	return nil
}
