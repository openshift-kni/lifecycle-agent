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

// This file automates self-contained kubeconfig-identity generation for create-usb (CNF-30028).
// When a self-contained manifest supplies neither siteConfig.kubeadminPasswordHash nor
// kubeconfigCryptoRetention, create-usb sources openshift-install from the release image, runs its
// image-based config-image flow in a scrubbed per-run directory, and reads back the identity
// material. The hash and crypto retention are rendered into m.SiteConfig; the matching kubeconfig
// and kubeadmin password are emitted as owner-only build-side artifacts, never written to the media.
// Supplying exactly one of the two fields is an error.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	// installerSubcmd* are the openshift-install subcommand tokens that generate the config image
	// (and the auth/ identity material) without producing a full install ISO.
	installerSubcmdImageBased  = "image-based"
	installerSubcmdCreate      = "create"
	installerSubcmdConfigImage = "config-image"

	// installerBinaryName is the openshift-install binary extracted from the release image.
	installerBinaryName = "openshift-install"

	// defaultReleaseImageRepo is the canonical public ocp-release repository. When --release-image is
	// unset, create-usb defaults to <repo>:<releaseVersion>-<arch>, so a connected build needs no
	// release-image input.
	defaultReleaseImageRepo = "quay.io/openshift-release-dev/ocp-release"

	// UnsupportedInstallerBinaryOverrideEnv names the development-only escape hatch that bypasses
	// sourcing openshift-install from the release image and uses the named binary as-is. It is
	// unsupported; create-usb logs a loud warning when it is set and never verifies the version.
	UnsupportedInstallerBinaryOverrideEnv = "UNSUPPORTED_DO_NOT_USE_OPENSHIFT_INSTALL_BINARY_OVERRIDE"

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
		return nil, fmt.Errorf("siteConfig.kubeadminPasswordHash and siteConfig.kubeconfigCryptoRetention are absent and no credential generator is configured; supply the credentials in the manifest or pass --release-image to generate them")
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

// WriteCredentialArtifacts publishes the generated kubeconfig and kubeadmin password into dir as
// owner-only files. The pair is written transactionally: both are staged to owner-only temp files
// and then renamed into place, so a failure never leaves a mismatched kubeconfig/password pair. Any
// pre-existing kubeconfig is moved aside first and restored if publication fails, so a failed build
// never destroys credentials that may still authenticate an earlier cluster. Overwriting existing
// artifacts is logged loudly; the secret bytes are never logged.
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

	// Stage both artifacts to owner-only temp files before publishing either, so the only steps left
	// after the first destination is replaced are renames within the same directory — a mid-write
	// failure can no longer leave a mismatched kubeconfig/password pair.
	kubeconfigTmp, err := stageOwnerOnlyFile(dir, KubeconfigArtifactName, creds.Kubeconfig)
	if err != nil {
		return fmt.Errorf("staging kubeconfig artifact: %w", err)
	}
	defer func() { _ = os.Remove(kubeconfigTmp) }()

	passwordTmp, err := stageOwnerOnlyFile(dir, KubeadminPasswordArtifactName, creds.KubeadminPassword)
	if err != nil {
		return fmt.Errorf("staging kubeadmin-password artifact: %w", err)
	}
	defer func() { _ = os.Remove(passwordTmp) }()

	// Move any pre-existing kubeconfig aside so a failed publish can restore it rather than destroy
	// it. The pre-existing password is left in place: os.Rename leaves the destination untouched on
	// failure, so restoring the kubeconfig keeps the prior pair recoverable.
	kubeconfigBackup, err := stashExisting(kubeconfigPath)
	if err != nil {
		return fmt.Errorf("preserving existing kubeconfig artifact %q: %w", kubeconfigPath, err)
	}

	// Publish both staged artifacts with atomic same-directory renames.
	if err := os.Rename(kubeconfigTmp, kubeconfigPath); err != nil {
		// Nothing has replaced the destination yet; restore the previous kubeconfig if one was moved
		// aside so a failed publish leaves the prior artifacts exactly as they were.
		restoreStashedKubeconfig(log, kubeconfigBackup, kubeconfigPath, dir)
		return fmt.Errorf("publishing kubeconfig artifact %q: %w", kubeconfigPath, err)
	}
	if err := os.Rename(passwordTmp, passwordPath); err != nil {
		// The kubeconfig was published but the matching password was not. Discard the freshly
		// published kubeconfig and restore the previous one, so a failed build never leaves a
		// mismatched new-kubeconfig/stale-password pair and never destroys the prior credential pair.
		if rbErr := os.Remove(kubeconfigPath); rbErr != nil {
			log.Warnf("create-usb: failed to roll back the partially published kubeconfig %s after the kubeadmin-password write failed: %v; the credential pair in %s may be inconsistent — rebuild to mint and publish a fresh pair", kubeconfigPath, rbErr, dir)
		} else {
			restoreStashedKubeconfig(log, kubeconfigBackup, kubeconfigPath, dir)
		}
		return fmt.Errorf("publishing kubeadmin-password artifact %q: %w", passwordPath, err)
	}

	// Both artifacts published successfully. A newly minted identity invalidates the superseded
	// kubeconfig, so discard the backup now that the fresh pair is in place.
	if kubeconfigBackup != "" {
		if rmErr := os.Remove(kubeconfigBackup); rmErr != nil {
			log.Warnf("create-usb: failed to remove the superseded kubeconfig backup %s: %v", kubeconfigBackup, rmErr)
		}
	}
	log.Warnf("create-usb: wrote the matching kubeconfig and kubeadmin password to %s (owner-only). Keep them secret and never place them on the media; they are the only copy and a rebuild mints a new identity that replaces them.", dir)
	return nil
}

// stageOwnerOnlyFile writes data to a fresh owner-only (0600) temp file in dir and returns its path
// without publishing it; the caller renames it over the destination. Writing to a new file (rather
// than os.WriteFile over the destination, which leaves an existing file's mode untouched) guarantees
// the result is owner-only even when a world-readable file was already there.
func stageOwnerOnlyFile(dir, name string, data []byte) (string, error) {
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("creating temporary file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	// os.CreateTemp already creates the file 0600, but set it explicitly so the owner-only
	// guarantee does not depend on that implementation detail.
	if err := tmp.Chmod(credentialFileMode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("securing temporary file %q: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("writing temporary file %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("closing temporary file %q: %w", tmpName, err)
	}
	return tmpName, nil
}

// stashExisting moves an existing file at path aside to a sibling backup file and returns the backup
// path, so a later publish failure can restore it. It returns ("", nil) when path does not exist.
// The backup is created in the same directory as path (an atomic rename, same filesystem) and keeps
// path's contents and mode untouched.
func stashExisting(path string) (string, error) {
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("inspecting %q: %w", path, err)
	}
	// Reserve a unique backup name alongside path; os.Rename then replaces that placeholder with the
	// existing file's contents and mode.
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".bak-*")
	if err != nil {
		return "", fmt.Errorf("creating backup file for %q: %w", path, err)
	}
	backup := f.Name()
	_ = f.Close()
	if err := os.Rename(path, backup); err != nil {
		_ = os.Remove(backup)
		return "", fmt.Errorf("moving %q aside: %w", path, err)
	}
	return backup, nil
}

// restoreStashedKubeconfig moves a kubeconfig previously stashed by stashExisting back to dest. It is
// best-effort cleanup on a rollback path: a no-op when nothing was stashed, and a failed restore is
// logged (leaving the original recoverable under the backup name) rather than returned, because the
// caller is already returning the publication error that triggered the rollback.
func restoreStashedKubeconfig(log *logrus.Logger, backup, dest, dir string) {
	if backup == "" {
		return
	}
	if err := os.Rename(backup, dest); err != nil {
		log.Warnf("create-usb: failed to restore the previous kubeconfig to %s after a failed publish: %v; the prior kubeconfig is preserved at %s — move it back manually or rebuild to mint a fresh pair in %s", dest, err, backup, dir)
	}
}

// InstallerConfig configures the production CredentialGenerator: the release image openshift-install
// is sourced from (defaulted from ReleaseVersion and the SeedImage architecture when empty), the
// seed image whose architecture that default is keyed off, the build-time auth file used to pull
// them, and the release version the extracted installer must match.
type InstallerConfig struct {
	ReleaseImage   string
	SeedImage      string
	AuthFile       string
	ReleaseVersion string
}

// installerExec abstracts the external installer operations so the generator is unit-tested without
// oc or the installer binary.
type installerExec interface {
	// ImageArchitecture returns the GOARCH (e.g. "amd64") of the given single-arch image.
	ImageArchitecture(ctx context.Context, log *logrus.Logger, image, authFile string) (string, error)
	// ExtractInstaller extracts openshift-install from releaseImage into destDir, returning its path.
	ExtractInstaller(ctx context.Context, log *logrus.Logger, releaseImage, authFile, destDir string) (string, error)
	// InstallerVersion returns the raw output of the installer's `version` subcommand.
	InstallerVersion(ctx context.Context, binaryPath string) (string, error)
	// RunConfigImage runs the image-based config-image flow in workDir using binaryPath.
	RunConfigImage(ctx context.Context, log *logrus.Logger, binaryPath, workDir string) error
}

// installerConfigImageGenerator is the production CredentialGenerator: it sources openshift-install
// from the release image, runs its image-based config-image flow in a scrubbed per-run working
// directory, and reads back the generated identity material.
type installerConfigImageGenerator struct {
	cfg  InstallerConfig
	exec installerExec
}

// NewInstallerConfigImageGenerator returns the production CredentialGenerator that sources
// openshift-install from cfg.ReleaseImage.
func NewInstallerConfigImageGenerator(_ *logrus.Logger, cfg InstallerConfig) CredentialGenerator {
	return &installerConfigImageGenerator{cfg: cfg, exec: &ocInstallerExec{}}
}

// Generate sources the installer, derives the installer input files from in, runs the config-image
// flow, and reads back the identity bundle. The working directory is created 0700 and removed on
// return so no secret material (installer binary, pull secret, kubeconfig) outlives the build.
func (g *installerConfigImageGenerator) Generate(ctx context.Context, log *logrus.Logger, in CredentialInputs) (*GeneratedCredentials, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("credential generation canceled: %w", err)
	}

	workDir, err := os.MkdirTemp("", "create-usb-identity-")
	if err != nil {
		return nil, fmt.Errorf("creating credential working directory: %w", err)
	}
	defer func() {
		if rerr := os.RemoveAll(workDir); rerr != nil {
			log.Warnf("create-usb: removing credential working directory %s: %v", workDir, rerr)
		}
	}()
	if err := os.Chmod(workDir, credentialDirMode); err != nil {
		return nil, fmt.Errorf("securing credential working directory %q: %w", workDir, err)
	}

	installer, err := g.resolveInstaller(ctx, log, workDir)
	if err != nil {
		return nil, err
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

	if err := g.exec.RunConfigImage(ctx, log, installer, workDir); err != nil {
		return nil, fmt.Errorf("generating config image: %w", err)
	}

	return readGeneratedCredentials(workDir)
}

// resolveInstaller returns the openshift-install binary to use. By default it extracts the binary
// from the release image into workDir and verifies it matches the manifest release version. The
// unsupported override env var bypasses both and uses the named binary as-is.
func (g *installerConfigImageGenerator) resolveInstaller(ctx context.Context, log *logrus.Logger, workDir string) (string, error) {
	if override := strings.TrimSpace(os.Getenv(UnsupportedInstallerBinaryOverrideEnv)); override != "" {
		log.Warnf("create-usb: %s is set; using the unsupported openshift-install override %q instead of extracting it from the release image. This is for development only and must not be used for production media.", UnsupportedInstallerBinaryOverrideEnv, override)
		return override, nil
	}
	releaseImage, err := g.resolveReleaseImage(ctx, log)
	if err != nil {
		return "", err
	}
	installer, err := g.exec.ExtractInstaller(ctx, log, releaseImage, g.cfg.AuthFile, workDir)
	if err != nil {
		return "", fmt.Errorf("extracting openshift-install from release image %q: %w", releaseImage, err)
	}
	if err := g.verifyInstallerVersion(ctx, installer); err != nil {
		return "", err
	}
	return installer, nil
}

// resolveReleaseImage returns the release image to source openshift-install from: the explicit
// cfg.ReleaseImage override when set, otherwise the canonical ocp-release image for cfg.ReleaseVersion
// and the architecture read from the seed image.
func (g *installerConfigImageGenerator) resolveReleaseImage(ctx context.Context, log *logrus.Logger) (string, error) {
	if img := strings.TrimSpace(g.cfg.ReleaseImage); img != "" {
		return img, nil
	}
	version := strings.TrimSpace(g.cfg.ReleaseVersion)
	if version == "" {
		return "", fmt.Errorf("cannot default the release image: manifest releaseVersion is empty; pass --release-image")
	}
	if strings.TrimSpace(g.cfg.SeedImage) == "" {
		return "", fmt.Errorf("cannot default the release image: no seed image to read the architecture from; pass --release-image")
	}
	goarch, err := g.exec.ImageArchitecture(ctx, log, g.cfg.SeedImage, g.cfg.AuthFile)
	if err != nil {
		return "", fmt.Errorf("reading architecture from seed image %q (pass --release-image to skip this): %w", g.cfg.SeedImage, err)
	}
	arch, err := ocReleaseArch(goarch)
	if err != nil {
		return "", err
	}
	image := fmt.Sprintf("%s:%s-%s", defaultReleaseImageRepo, version, arch)
	log.Infof("create-usb: defaulting release image to %s (override with --release-image)", image)
	return image, nil
}

// ocReleaseArch maps a GOARCH (as reported by oc image info) to the architecture suffix used in
// ocp-release tags.
func ocReleaseArch(goarch string) (string, error) {
	switch strings.TrimSpace(goarch) {
	case "amd64":
		return "x86_64", nil
	case "arm64":
		return "aarch64", nil
	case "ppc64le":
		return "ppc64le", nil
	case "s390x":
		return "s390x", nil
	default:
		return "", fmt.Errorf("unsupported seed image architecture %q; pass --release-image", goarch)
	}
}

// verifyInstallerVersion fails unless the extracted installer reports the manifest release version.
func (g *installerConfigImageGenerator) verifyInstallerVersion(ctx context.Context, binaryPath string) error {
	want := strings.TrimSpace(g.cfg.ReleaseVersion)
	if want == "" {
		return nil
	}
	out, err := g.exec.InstallerVersion(ctx, binaryPath)
	if err != nil {
		return fmt.Errorf("determining extracted openshift-install version: %w", err)
	}
	got := parseInstallerVersion(out)
	if got == "" {
		return fmt.Errorf("could not parse openshift-install version from %q", strings.TrimSpace(out))
	}
	if got != want {
		return fmt.Errorf("extracted openshift-install version %q does not match manifest releaseVersion %q", got, want)
	}
	return nil
}

// parseInstallerVersion extracts the version token from `openshift-install version` output, whose
// first line is "openshift-install <version>".
func parseInstallerVersion(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == installerBinaryName {
			return fields[1]
		}
	}
	return ""
}

// ocInstallerExec is the production installerExec: it shells out to `oc adm release extract` to pull
// openshift-install from the release image, runs the binary to read its version, and runs the
// image-based config-image flow.
type ocInstallerExec struct{}

// ImageArchitecture runs `oc image info <image> -o json` and returns its config.architecture (a
// GOARCH value such as "amd64"). The seed image is single-arch, so no --filter-by-os is needed.
func (ocInstallerExec) ImageArchitecture(ctx context.Context, log *logrus.Logger, image, authFile string) (string, error) {
	log.Infof("create-usb: reading architecture from seed image %s", image)
	args := []string{"image", "info", "-o", "json"}
	if strings.TrimSpace(authFile) != "" {
		args = append(args, "--registry-config="+authFile)
	}
	args = append(args, image)
	cmd := exec.CommandContext(ctx, "oc", args...) //nolint:gosec // oc is a fixed command; args are the operator-supplied seed image and auth file
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("oc image info failed: %w: %s", err, stderr)
	}
	var info struct {
		Config struct {
			Architecture string `json:"architecture"`
		} `json:"config"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("parsing oc image info output: %w", err)
	}
	arch := strings.TrimSpace(info.Config.Architecture)
	if arch == "" {
		return "", fmt.Errorf("oc image info reported no architecture for %q", image)
	}
	return arch, nil
}

// ExtractInstaller runs `oc adm release extract --command=openshift-install --to=<destDir>` so the
// installer always matches the release payload (it lives in a release-referenced image, not at a
// fixed path in the release image itself).
func (ocInstallerExec) ExtractInstaller(ctx context.Context, log *logrus.Logger, releaseImage, authFile, destDir string) (string, error) {
	log.Infof("create-usb: extracting %s from release image %s", installerBinaryName, releaseImage)
	args := []string{"adm", "release", "extract", "--command=" + installerBinaryName, "--to=" + destDir}
	if strings.TrimSpace(authFile) != "" {
		args = append(args, "--registry-config="+authFile)
	}
	args = append(args, releaseImage)
	cmd := exec.CommandContext(ctx, "oc", args...) //nolint:gosec // oc is a fixed command; args are the operator-supplied release image and auth file
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("oc adm release extract failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	binary := filepath.Join(destDir, installerBinaryName)
	if _, err := os.Stat(binary); err != nil {
		return "", fmt.Errorf("extracted %s not found in %s: %w", installerBinaryName, destDir, err)
	}
	return binary, nil
}

// InstallerVersion runs `<binaryPath> version` and returns its stdout.
func (ocInstallerExec) InstallerVersion(ctx context.Context, binaryPath string) (string, error) {
	cmd := exec.CommandContext(ctx, binaryPath, "version") //nolint:gosec // binaryPath is the installer this package extracted into its per-run temp dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("running %s version: %w", binaryPath, err)
	}
	return string(out), nil
}

// RunConfigImage executes `<binaryPath> --dir=<workDir> image-based create config-image`. Its output
// is captured and discarded rather than logged because the installer can echo its input, which
// carries the pull secret and SSH key; the error reports the exit status only.
func (ocInstallerExec) RunConfigImage(ctx context.Context, log *logrus.Logger, binaryPath, workDir string) error {
	log.Infof("create-usb: running %s %s %s %s", binaryPath,
		installerSubcmdImageBased, installerSubcmdCreate, installerSubcmdConfigImage)
	cmd := exec.CommandContext(ctx, binaryPath, "--dir="+workDir, //nolint:gosec // binaryPath is the installer this package extracted into its per-run temp dir
		installerSubcmdImageBased, installerSubcmdCreate, installerSubcmdConfigImage)
	var sink bytes.Buffer
	cmd.Stdout = &sink
	cmd.Stderr = &sink
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s image-based create config-image failed: %w; its output was suppressed because it can contain credential material from install-config.yaml -- rerun the documented manual openshift-install command to see the full output", binaryPath, err)
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
