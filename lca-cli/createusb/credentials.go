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

// This file automates the self-contained kubeconfig-identity workflow that was previously
// a manual, error-prone six-step procedure (extract openshift-install, hand-write installer
// configs, generate a config ISO, bsdtar the manifest out of it, jq the fields into the
// create-usb manifest, and keep the kubeconfig/password separately). See CNF-30028.
//
// The developer of record chose "Option B" (openshift-install subprocess): create-usb derives
// the installer input files from siteConfig, shells out to
//
//	openshift-install --dir=<workDir> image-based create config-image
//
// in a scrubbed 0700 per-run working directory, and reads back the generated identity material
// (auth/kubeconfig, auth/kubeadmin-password, and the kubeadmin_password_hash +
// KubeconfigCryptoRetention fields from the generated cluster-configuration/manifest.json).
// The hash and crypto retention are populated into m.SiteConfig so the existing p3 render and
// the existing build-time validators consume them exactly as if the operator had supplied them;
// the matching kubeconfig and kubeadmin password are emitted as build-side artifacts for the
// operator and are deliberately never written to the media.
//
// Generation runs only in self-contained mode and only when neither field is supplied. An
// operator-supplied pair remains a validated override (reproducibility); supplying exactly one
// is an error. The generator is behind the CredentialGenerator interface so the orchestration
// and the derived installer inputs are unit-tested without the installer binary.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/sirupsen/logrus"
	"sigs.k8s.io/yaml"

	"github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
)

const (
	// installConfigVersion is install-config.yaml's apiVersion (openshift/installer
	// pkg/types.InstallConfigVersion).
	installConfigVersion = "v1"
	// imageBasedConfigVersion / imageBasedConfigKind identify the image-based config document
	// (openshift/installer pkg/types/imagebased.ImageBasedConfigVersion + its kind).
	imageBasedConfigVersion = "v1beta1"
	imageBasedConfigKind    = "ImageBasedConfig"
	// ovnNetworkType is the only supported SNO network type, matching IBIO's installer input.
	ovnNetworkType = "OVNKubernetes"

	// installerConfigImageCommand is the openshift-install subcommand that generates the
	// config image (and the auth/ identity material) without producing a full install ISO.
	// Passed as separate argv tokens to the executor.
	installerSubcmdImageBased  = "image-based"
	installerSubcmdCreate      = "create"
	installerSubcmdConfigImage = "config-image"

	// installConfigFileName / imageBasedConfigFileName are the installer's expected input file
	// names in the working directory.
	installConfigFileName    = "install-config.yaml"
	imageBasedConfigFileName = "image-based-config.yaml"

	// configImageISOName is the config image the installer writes to the working directory.
	configImageISOName = "imagebasedconfig.iso"

	// KubeconfigArtifactName / KubeadminPasswordArtifactName are the file names of the emitted
	// build-side credential artifacts the operator consumes (never written to the media).
	KubeconfigArtifactName        = "kubeconfig"
	KubeadminPasswordArtifactName = "kubeadmin-password"

	// credentialDirMode / credentialFileMode keep the generated secret material owner-only.
	credentialDirMode  os.FileMode = 0o700
	credentialFileMode os.FileMode = 0o600
)

// GeneratedCredentials is the identity bundle produced by a CredentialGenerator. The hash and
// crypto retention are rendered into the on-media SeedReconfiguration; the kubeconfig and
// kubeadmin password are secret build-side artifacts handed to the operator and never written
// to the media.
type GeneratedCredentials struct {
	KubeadminPasswordHash     string
	KubeconfigCryptoRetention seedreconfig.KubeConfigCryptoRetention
	Kubeconfig                []byte
	KubeadminPassword         []byte
}

// CredentialInputs are the siteConfig-derived values the installer needs to mint a consistent
// identity bundle. PullSecret must already be resolved (no @file reference).
type CredentialInputs struct {
	ClusterName      string
	BaseDomain       string
	Hostname         string
	MachineNetworks  []string
	RawNMStateConfig string
	SSHKey           string
	PullSecret       string
}

// CredentialGenerator mints the kubeconfig identity bundle for self-contained media. It is an
// interface so the generate-if-absent orchestration and the derived installer inputs are
// unit-tested with a fake, and only the real subprocess path depends on the installer binary.
type CredentialGenerator interface {
	Generate(ctx context.Context, log *logrus.Logger, in CredentialInputs) (*GeneratedCredentials, error)
}

// NeedsCredentialGeneration reports whether create-usb should mint the kubeconfig identity for
// this manifest: self-contained mode with no credential material supplied at all. In every other
// case the operator either supplied the material (validated override, possibly partial — rejected
// by GenerateSiteConfigCredentials) or the mode does not use it.
func NeedsCredentialGeneration(m *Manifest) bool {
	if m.Mode != ModeSelfContained || m.SiteConfig == nil {
		return false
	}
	hashPresent, retentionPresent, _ := credentialMaterialState(m.SiteConfig)
	return !hashPresent && !retentionPresent
}

// credentialMaterialState classifies the operator-supplied credential material:
//   - hashPresent:       the kubeadminPasswordHash is set
//   - retentionPresent:  any kubeconfigCryptoRetention material (a PEM field or the ingress CN) is set
//   - retentionComplete: every required retention field (all PEMs + the ingress CN) is set
//
// It is the single source of truth for the generate / override / partial decision.
func credentialMaterialState(sc *SiteConfig) (hashPresent, retentionPresent, retentionComplete bool) {
	hashPresent = strings.TrimSpace(sc.KubeadminPasswordHash) != ""
	missing := sc.KubeconfigCryptoRetention.MissingPEMFields()
	cnPresent := strings.TrimSpace(sc.KubeconfigCryptoRetention.IngresssCrypto.IngressCertificateCN) != ""
	retentionPresent = len(missing) < 5 || cnPresent
	retentionComplete = len(missing) == 0 && cnPresent
	return hashPresent, retentionPresent, retentionComplete
}

// GenerateSiteConfigCredentials runs the generate-if-absent workflow. When the manifest needs
// generation (see NeedsCredentialGeneration) it resolves the pull secret, invokes gen, populates
// m.SiteConfig with the generated hash and crypto retention, and returns the secret artifacts
// (kubeconfig + kubeadmin password) for the caller to emit. It returns (nil, nil) when no
// generation is required (fully operator-supplied override, or a mode that does not use the
// material).
//
// Supplying only part of the material is rejected here: the hash and the crypto retention are
// generated together, and a mismatched half-supplied set is the exact failure mode this
// automation exists to remove.
func GenerateSiteConfigCredentials(ctx context.Context, log *logrus.Logger, m *Manifest, gen CredentialGenerator) (*GeneratedCredentials, error) {
	if m.Mode != ModeSelfContained || m.SiteConfig == nil {
		return nil, nil
	}
	sc := m.SiteConfig

	hashPresent, retentionPresent, retentionComplete := credentialMaterialState(sc)
	switch {
	case !hashPresent && !retentionPresent:
		// Nothing supplied: generate below.
	case hashPresent && retentionComplete:
		// Fully supplied: a validated override, nothing to generate.
		return nil, nil
	default:
		// A half-supplied set is rejected rather than completed by generation so a mismatched
		// kubeconfig can never be shipped.
		return nil, fmt.Errorf("siteConfig.kubeadminPasswordHash and siteConfig.kubeconfigCryptoRetention must be supplied together: provide both to use operator-managed credentials, or omit both to have create-usb generate them")
	}

	if gen == nil {
		return nil, fmt.Errorf("siteConfig.kubeadminPasswordHash and siteConfig.kubeconfigCryptoRetention are absent and no credential generator is configured; supply the credentials in the manifest or pass --openshift-install to generate them")
	}

	// The generator needs a resolved pull secret; ResolvePullSecret is idempotent (Validate
	// resolves it again later for the @file-reference case) and a no-op for an inline value.
	if err := m.ResolvePullSecret(); err != nil {
		return nil, err
	}

	log.Infof("create-usb: generating kubeconfig identity for self-contained cluster %q via openshift-install", sc.ClusterName)
	creds, err := gen.Generate(ctx, log, CredentialInputs{
		ClusterName:      sc.ClusterName,
		BaseDomain:       sc.BaseDomain,
		Hostname:         sc.Hostname,
		MachineNetworks:  sc.MachineNetworks,
		RawNMStateConfig: sc.RawNMStateConfig,
		SSHKey:           sc.SSHKey,
		PullSecret:       sc.PullSecret,
	})
	if err != nil {
		return nil, fmt.Errorf("generating kubeconfig identity: %w", err)
	}
	if creds == nil {
		return nil, fmt.Errorf("credential generator returned no identity material")
	}

	sc.KubeadminPasswordHash = creds.KubeadminPasswordHash
	sc.KubeconfigCryptoRetention = creds.KubeconfigCryptoRetention
	return creds, nil
}

// WriteCredentialArtifacts writes the generated kubeconfig and kubeadmin password into dir as
// owner-only files, creating dir 0700 if needed. A newly minted identity invalidates any
// previously distributed kubeconfig, so an overwrite of pre-existing artifacts is surfaced
// loudly rather than done silently. The secret bytes themselves are never logged.
func WriteCredentialArtifacts(log *logrus.Logger, dir string, creds *GeneratedCredentials) error {
	if creds == nil {
		return nil
	}
	if err := os.MkdirAll(dir, credentialDirMode); err != nil {
		return fmt.Errorf("creating credentials output directory %q: %w", dir, err)
	}

	kubeconfigPath := filepath.Join(dir, KubeconfigArtifactName)
	passwordPath := filepath.Join(dir, KubeadminPasswordArtifactName)
	for _, p := range []string{kubeconfigPath, passwordPath} {
		if _, err := os.Stat(p); err == nil {
			log.Warnf("create-usb: overwriting existing credential artifact %s; a new identity has been minted and any previously distributed kubeconfig/password for this media no longer authenticates", p)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("checking credential artifact %q: %w", p, err)
		}
	}

	if err := os.WriteFile(kubeconfigPath, creds.Kubeconfig, credentialFileMode); err != nil {
		return fmt.Errorf("writing kubeconfig artifact %q: %w", kubeconfigPath, err)
	}
	if err := os.WriteFile(passwordPath, creds.KubeadminPassword, credentialFileMode); err != nil {
		return fmt.Errorf("writing kubeadmin-password artifact %q: %w", passwordPath, err)
	}
	log.Warnf("create-usb: wrote the matching kubeconfig and kubeadmin password to %s (owner-only). Keep them secret and never place them on the media; they are the only copy and a rebuild mints a new identity that replaces them.", dir)
	return nil
}

// installerConfigImageGenerator is the production CredentialGenerator: it shells out to
// openshift-install's image-based config-image flow in a scrubbed per-run working directory and
// reads back the generated identity material. The installer binary is sourced by the operator
// (for disconnected builders, via `oc adm release extract --command=openshift-install`).
type installerConfigImageGenerator struct {
	// installerBinary is the openshift-install path (or a name resolved on PATH).
	installerBinary string
}

// NewInstallerConfigImageGenerator returns the production CredentialGenerator backed by the
// openshift-install binary at installerBinary (a path, or a name resolved on PATH).
func NewInstallerConfigImageGenerator(_ *logrus.Logger, installerBinary string) CredentialGenerator {
	return &installerConfigImageGenerator{installerBinary: installerBinary}
}

// Generate derives the installer input files from in, runs the config-image flow, and reads back
// the identity bundle. The working directory is created 0700 and removed on return so no secret
// material is left on disk after the build.
func (g *installerConfigImageGenerator) Generate(ctx context.Context, log *logrus.Logger, in CredentialInputs) (*GeneratedCredentials, error) {
	if strings.TrimSpace(g.installerBinary) == "" {
		return nil, fmt.Errorf("no openshift-install binary configured")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("credential generation canceled: %w", err)
	}

	workDir, err := os.MkdirTemp("", "create-usb-identity-")
	if err != nil {
		return nil, fmt.Errorf("creating credential working directory: %w", err)
	}
	// The working directory holds the pull secret, the generated kubeconfig, and the kubeadmin
	// password; scrub it on every return path so secret material never outlives the build.
	defer func() {
		if rerr := os.RemoveAll(workDir); rerr != nil {
			log.Warnf("create-usb: removing credential working directory %s: %v", workDir, rerr)
		}
	}()
	if err := os.Chmod(workDir, credentialDirMode); err != nil {
		return nil, fmt.Errorf("securing credential working directory %q: %w", workDir, err)
	}

	installConfig, err := renderInstallConfig(in)
	if err != nil {
		return nil, err
	}
	imageBasedConfig, err := renderImageBasedConfig(in)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(workDir, installConfigFileName), installConfig, credentialFileMode); err != nil {
		return nil, fmt.Errorf("writing %s: %w", installConfigFileName, err)
	}
	if err := os.WriteFile(filepath.Join(workDir, imageBasedConfigFileName), imageBasedConfig, credentialFileMode); err != nil {
		return nil, fmt.Errorf("writing %s: %w", imageBasedConfigFileName, err)
	}

	if err := g.runConfigImage(ctx, log, workDir); err != nil {
		return nil, err
	}

	return readGeneratedCredentials(workDir)
}

// runConfigImage executes `openshift-install --dir=<workDir> image-based create config-image`. The
// combined stdout/stderr is captured and deliberately discarded (not logged, not folded into the
// returned error): the installer can echo its input, and the input here is install-config.yaml,
// which carries the pull secret and SSH key. On failure the error reports the exit status only and
// points the operator at the manual flow to reproduce it, so no credential material leaks through a
// log or error string. Context cancellation terminates the subprocess.
func (g *installerConfigImageGenerator) runConfigImage(ctx context.Context, log *logrus.Logger, workDir string) error {
	log.Infof("create-usb: running %s %s %s %s", g.installerBinary,
		installerSubcmdImageBased, installerSubcmdCreate, installerSubcmdConfigImage)
	cmd := exec.CommandContext(ctx, g.installerBinary, "--dir="+workDir, //nolint:gosec // installerBinary is the operator-supplied --openshift-install value
		installerSubcmdImageBased, installerSubcmdCreate, installerSubcmdConfigImage)
	var sink bytes.Buffer
	cmd.Stdout = &sink
	cmd.Stderr = &sink
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s image-based create config-image failed: %w; its output was suppressed because it can contain credential material from install-config.yaml -- rerun the documented manual openshift-install command to see the full output", g.installerBinary, err)
	}
	return nil
}

// renderInstallConfig builds the install-config.yaml the installer expects for a platform:none
// SNO cluster, mirroring IBIO's WriteInstallConfig (one control-plane replica, zero compute,
// OVNKubernetes, platform none).
func renderInstallConfig(in CredentialInputs) ([]byte, error) {
	type nameOnly struct {
		Name string `json:"name"`
	}
	type machineNetworkEntry struct {
		CIDR string `json:"cidr"`
	}
	type networking struct {
		NetworkType    string                `json:"networkType"`
		MachineNetwork []machineNetworkEntry `json:"machineNetwork,omitempty"`
	}
	type machinePool struct {
		Name     string `json:"name"`
		Replicas int    `json:"replicas"`
	}
	type platform struct {
		None struct{} `json:"none"`
	}
	type installConfig struct {
		APIVersion   string        `json:"apiVersion"`
		BaseDomain   string        `json:"baseDomain"`
		Metadata     nameOnly      `json:"metadata"`
		Networking   networking    `json:"networking"`
		ControlPlane machinePool   `json:"controlPlane"`
		Compute      []machinePool `json:"compute"`
		Platform     platform      `json:"platform"`
		PullSecret   string        `json:"pullSecret"`
		SSHKey       string        `json:"sshKey,omitempty"`
	}

	cfg := installConfig{
		APIVersion:   installConfigVersion,
		BaseDomain:   in.BaseDomain,
		Metadata:     nameOnly{Name: in.ClusterName},
		Networking:   networking{NetworkType: ovnNetworkType},
		ControlPlane: machinePool{Name: "master", Replicas: 1},
		Compute:      []machinePool{{Name: "worker", Replicas: 0}},
		PullSecret:   in.PullSecret,
		SSHKey:       in.SSHKey,
	}
	for _, network := range in.MachineNetworks {
		cfg.Networking.MachineNetwork = append(cfg.Networking.MachineNetwork, machineNetworkEntry{CIDR: strings.TrimSpace(network)})
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshaling %s: %w", installConfigFileName, err)
	}
	return out, nil
}

// renderImageBasedConfig builds the image-based-config.yaml the installer expects, mirroring
// IBIO's WriteImageBaseConfig (hostname + inline nmstate networkConfig). clusterID/infraID are
// left unset so the installer generates them.
func renderImageBasedConfig(in CredentialInputs) ([]byte, error) {
	type nameOnly struct {
		Name string `json:"name"`
	}
	type imageBasedConfig struct {
		APIVersion    string      `json:"apiVersion"`
		Kind          string      `json:"kind"`
		Metadata      nameOnly    `json:"metadata"`
		Hostname      string      `json:"hostname,omitempty"`
		NetworkConfig interface{} `json:"networkConfig,omitempty"`
	}

	cfg := imageBasedConfig{
		APIVersion: imageBasedConfigVersion,
		Kind:       imageBasedConfigKind,
		Metadata:   nameOnly{Name: in.ClusterName},
		Hostname:   in.Hostname,
	}
	// networkConfig is inline nmstate YAML; embed it as a nested mapping (not a string) so the
	// installer's nmstate parser consumes it the same way it consumes a hand-written config.
	if strings.TrimSpace(in.RawNMStateConfig) != "" {
		var nmstate interface{}
		if err := yaml.Unmarshal([]byte(in.RawNMStateConfig), &nmstate); err != nil {
			return nil, fmt.Errorf("parsing rawNMStateConfig for %s: %w", imageBasedConfigFileName, err)
		}
		cfg.NetworkConfig = nmstate
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshaling %s: %w", imageBasedConfigFileName, err)
	}
	return out, nil
}

// readGeneratedCredentials reads the installer's output from workDir: the user credentials from
// auth/, and the hash + crypto retention from the generated cluster-configuration/manifest.json
// (read from the working directory when the installer left it there, otherwise extracted from the
// generated config-image ISO).
func readGeneratedCredentials(workDir string) (*GeneratedCredentials, error) {
	//nolint:gosec // workDir is the per-run working directory this package created and scrubs
	kubeconfig, err := os.ReadFile(filepath.Join(workDir, "auth", KubeconfigArtifactName))
	if err != nil {
		return nil, fmt.Errorf("reading generated kubeconfig: %w", err)
	}
	//nolint:gosec // workDir is the per-run working directory this package created and scrubs
	password, err := os.ReadFile(filepath.Join(workDir, "auth", KubeadminPasswordArtifactName))
	if err != nil {
		return nil, fmt.Errorf("reading generated kubeadmin password: %w", err)
	}

	manifestJSON, err := readClusterConfigManifest(workDir)
	if err != nil {
		return nil, err
	}
	var sr seedreconfig.SeedReconfiguration
	if err := json.Unmarshal(manifestJSON, &sr); err != nil {
		return nil, fmt.Errorf("parsing generated cluster-configuration manifest: %w", err)
	}
	if strings.TrimSpace(sr.KubeadminPasswordHash) == "" {
		return nil, fmt.Errorf("generated cluster-configuration manifest has no kubeadmin_password_hash")
	}

	return &GeneratedCredentials{
		KubeadminPasswordHash:     strings.TrimSpace(sr.KubeadminPasswordHash),
		KubeconfigCryptoRetention: sr.KubeconfigCryptoRetention,
		Kubeconfig:                kubeconfig,
		KubeadminPassword:         password,
	}, nil
}

// clusterConfigManifestRelPath is the manifest path inside both the working directory and the
// generated config-image ISO.
var clusterConfigManifestRelPath = []string{"cluster-configuration", "manifest.json"}

// readClusterConfigManifest returns the generated cluster-configuration/manifest.json bytes. The
// installer persists the ClusterConfiguration asset to the working directory, so that copy is
// preferred; when it is absent the manifest is extracted from the generated config-image ISO so
// the reader does not depend on an external tool (no bsdtar).
func readClusterConfigManifest(workDir string) ([]byte, error) {
	onDisk := filepath.Join(append([]string{workDir}, clusterConfigManifestRelPath...)...)
	//nolint:gosec // workDir is the per-run working directory this package created and scrubs
	if data, err := os.ReadFile(onDisk); err == nil {
		return data, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading %s: %w", onDisk, err)
	}

	isoPath := filepath.Join(workDir, configImageISOName)
	data, err := readFileFromISO(isoPath, clusterConfigManifestRelPath)
	if err != nil {
		return nil, fmt.Errorf("reading cluster-configuration manifest from %s: %w", configImageISOName, err)
	}
	return data, nil
}

// readFileFromISO reads the file at the given path components from an ISO9660 image. Lookup is
// case-insensitive at each level so an ISO that upper-cased or mangled the names (ISO9660 vs Rock
// Ridge) still resolves.
func readFileFromISO(isoPath string, pathComponents []string) ([]byte, error) {
	disk, err := diskfs.Open(isoPath)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", isoPath, err)
	}
	defer func() {
		if disk.File != nil {
			_ = disk.File.Close()
		}
	}()

	fs, err := disk.GetFilesystem(0)
	if err != nil {
		return nil, fmt.Errorf("reading filesystem from %s: %w", isoPath, err)
	}

	dir := "/"
	for i, want := range pathComponents {
		entries, err := fs.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("listing %q in %s: %w", dir, isoPath, err)
		}
		match := ""
		for _, e := range entries {
			// ISO9660 names may carry a ";1" version suffix; compare the base name.
			name := strings.TrimSuffix(e.Name(), ";1")
			if strings.EqualFold(name, want) {
				match = name
				break
			}
		}
		if match == "" {
			return nil, fmt.Errorf("%q not found under %q", want, dir)
		}
		if i == len(pathComponents)-1 {
			f, err := fs.OpenFile(filepath.Join(dir, match), os.O_RDONLY)
			if err != nil {
				return nil, fmt.Errorf("opening %q: %w", match, err)
			}
			data, err := io.ReadAll(f)
			if err != nil {
				return nil, fmt.Errorf("reading %q: %w", match, err)
			}
			return data, nil
		}
		dir = filepath.Join(dir, match)
	}
	return nil, fmt.Errorf("empty path")
}
