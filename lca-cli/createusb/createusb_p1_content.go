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

// This file owns the p1 live-ISO build block (§6.4 step 3, §8.E, §8.J of the USB-based
// IBI proposal): fetch the RHCOS live ISO over HTTPS and verify its sha256 before use,
// embed an ignition config that auto-runs `lca-cli ibi` on boot, and produce a customized
// bootable ISO. The exact shipping lca-cli binary is extracted from the --lca-image and
// staged for delivery on the p2 (ibi-images) partition -- it is NOT embedded in the ISO
// ignition, because at ~90 MB uncompressed it far exceeds the fixed RHCOS ISO ignition
// embed area (~256 KB) and would fail the embed. The embedded ignition carries only the
// small IBIPrepareConfig and the oneshot unit, whose ExecStart points at the staged binary
// on p2 (common.LocalImagesLCABinaryPath) and waits for that mount.
//
// BuildLiveISO is a standalone building block, mirroring BuildImageLayout in
// createusb_images.go: it is unit-tested with fakes (no network, no root, no real
// container image) and driven by the media-assembly pipeline (createusb_assemble.go). The
// customized ISO it returns is written to the front of the media at LBA 0 by
// writeLiveISOToFront (createusb_p1.go); the extracted lca-cli binary it returns is placed
// onto p2 (common.LocalImagesLCABinaryPath) by that same media-assembly pipeline.
//
// # Config contract (self-contained pivot)
//
// The embedded IBIPrepareConfig carries Shutdown verbatim from the manifest: install-only
// ends in shutdown (Shutdown:true), self-contained sets Shutdown:false so the embedded
// shipping lca-cli binary performs the pivot reboot. The pivot / pivot-failure on-node
// logic AND its end-to-end verification are owned by CNF-27164 -- this story only sets the
// flag.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/openshift-kni/lifecycle-agent/api/ibiconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

const (
	// embeddedIBIConfigPath is where the embedded ignition writes the rendered
	// IBIPrepareConfig that `lca-cli ibi -f` consumes on boot.
	embeddedIBIConfigPath = "/etc/lca-cli/ibi-config.json"
	// lcaIBIServiceName is the oneshot systemd unit the ignition enables to auto-run the
	// prepare on boot.
	lcaIBIServiceName = "lca-ibi.service"
	// lcaBinarySourcePath is the path of the lca-cli binary inside the --lca-image
	// container, from which the exact shipping binary is extracted (§8.E).
	lcaBinarySourcePath = "/usr/local/bin/lca-cli"
	// stagedLCABinaryName is the filename of the extracted lca-cli binary under the build
	// work directory; it is handed off for placement on p2 (common.LocalImagesLCABinaryPath).
	stagedLCABinaryName = "lca-cli"

	// stagedLCABinaryMode is the executable mode the extracted lca-cli binary is written
	// with, and embeddedIBIConfigMode is the ignition file mode for the embedded config
	// (read-only data).
	stagedLCABinaryMode   = 0o755
	embeddedIBIConfigMode = common.FileMode0644

	// customizedISOFileName is the name of the ignition-customized ISO produced under the
	// build work directory.
	customizedISOFileName = "rhcos-live-ibi.iso"
	// baseISOFileName is the name of the fetched, unmodified RHCOS live ISO under the build
	// work directory.
	baseISOFileName = "rhcos-live.iso"

	// schemeHTTPS is the only URL scheme the RHCOS live ISO may be fetched over: the fetch
	// must never fall back to plain HTTP.
	schemeHTTPS = "https"
)

// ISOBuildResult is what BuildLiveISO produces: the customized bootable live ISO (written
// into p1) and the extracted shipping lca-cli binary staged under the work directory for the
// media-assembly wiring (CNF-27171) to place onto p2 at common.LocalImagesLCABinaryPath. The
// binary is delivered on p2 rather than embedded in the ISO ignition because it far exceeds
// the fixed ISO ignition embed area.
type ISOBuildResult struct {
	// ISOPath is the path of the ignition-customized bootable live ISO under WorkDir.
	ISOPath string
	// LCABinaryPath is the path of the extracted shipping lca-cli binary under WorkDir, to be
	// copied onto p2 by the wiring story so the embedded unit's ExecStart resolves on boot.
	LCABinaryPath string
}

// ISOBuildOptions carries the build inputs the p1 live-ISO block needs beyond the manifest.
type ISOBuildOptions struct {
	// LCAImage is the authoritative lca-cli pull-spec (from --lca-image). The exact
	// shipping binary is extracted from it so the embedded binary is byte-identical to the
	// one mirrored into p2 (§8.E). Required.
	LCAImage string
	// AuthFile is the workstation --auth-file used to pull the lca-cli image (build-time
	// only; never written to the media).
	AuthFile string
	// WorkDir is the directory under which the fetched base ISO, extracted binary, and
	// customized ISO are written. Required.
	WorkDir string
}

// ---- injectable seams ----
//
// The network fetch, the container-image binary extraction, and the ISO customization are
// hidden behind these interfaces so the pure fetch-verify / ignition-render logic is
// unit-testable with fakes; the real implementations (which need network, podman, and a
// real RHCOS ISO) are exercised by the root/network-gated integration test.

// isoFetcher downloads the RHCOS live ISO at url into destPath. The production
// implementation is HTTPS-only and refuses redirects to non-https URLs.
type isoFetcher interface {
	FetchToFile(ctx context.Context, url, destPath string) error
}

// lcaBinaryExtractor extracts the lca-cli binary from the --lca-image container into
// destPath (mode preserved by the caller).
type lcaBinaryExtractor interface {
	ExtractLCABinary(lcaImage, authFile, destPath string) error
}

// isoCustomizer embeds ignitionBytes into the baseISO and writes the bootable result to
// outISO. The production implementation is ops.CreateIsoWithEmbeddedIgnition.
type isoCustomizer interface {
	EmbedIgnition(log logrus.FieldLogger, ignitionBytes []byte, baseISO, outISO string) error
}

// isoBuildDeps bundles the injectable seams, mirroring imageBuildDeps in
// createusb_images.go.
type isoBuildDeps struct {
	fetcher    isoFetcher
	extractor  lcaBinaryExtractor
	customizer isoCustomizer
}

// defaultISOBuildDeps returns the production seams backed by net/http, the host ops helpers,
// and the in-tree isoeditor primitive.
func defaultISOBuildDeps(log *logrus.Logger) isoBuildDeps {
	return isoBuildDeps{
		fetcher:   &httpISOFetcher{log: log},
		extractor: &opsLCABinaryExtractor{ops: ops.NewOps(log, ops.NewRegularExecutor(log, false)), log: log},
		customizer: &opsISOCustomizer{
			ops: ops.NewOps(log, ops.NewRegularExecutor(log, false)),
		},
	}
}

// BuildLiveISO fetches and verifies the RHCOS live ISO, embeds an ignition that auto-runs
// `lca-cli ibi` with the manifest-derived config, extracts the exact shipping lca-cli binary
// from --lca-image, and returns the customized bootable ISO plus the staged binary (to be
// placed on p2). It is a standalone building block wired into the media-assembly pipeline by
// CNF-27171.
func BuildLiveISO(ctx context.Context, log *logrus.Logger, m *Manifest, opts ISOBuildOptions) (ISOBuildResult, error) {
	return buildLiveISO(ctx, log, m, opts, defaultISOBuildDeps(log))
}

// buildLiveISO is the dependency-injected core of BuildLiveISO (mirroring BuildImageLayout →
// buildImageLayout), so tests exercise the full build with fakes while production injects
// defaultISOBuildDeps.
func buildLiveISO(ctx context.Context, log *logrus.Logger, m *Manifest, opts ISOBuildOptions, deps isoBuildDeps) (ISOBuildResult, error) {
	if err := ctx.Err(); err != nil {
		return ISOBuildResult{}, fmt.Errorf("live ISO build canceled: %w", err)
	}
	if opts.WorkDir == "" {
		return ISOBuildResult{}, fmt.Errorf("a work directory is required to build the live ISO")
	}
	if opts.LCAImage == "" {
		return ISOBuildResult{}, fmt.Errorf("lca-cli image is required to stage the shipping binary (set --lca-image)")
	}

	baseISO := filepath.Join(opts.WorkDir, baseISOFileName)
	if err := fetchAndVerifyISO(ctx, deps.fetcher, m.RHCOSLiveISO, baseISO); err != nil {
		return ISOBuildResult{}, err
	}

	// Extract the exact shipping binary from --lca-image and stage it under WorkDir. It is
	// NOT embedded in the ignition (it is far larger than the ISO embed area); the wiring
	// story copies it onto p2, where the embedded unit's ExecStart resolves it on boot.
	binaryPath := filepath.Join(opts.WorkDir, stagedLCABinaryName)
	if err := deps.extractor.ExtractLCABinary(opts.LCAImage, opts.AuthFile, binaryPath); err != nil {
		return ISOBuildResult{}, fmt.Errorf("extracting lca-cli binary from %q: %w", opts.LCAImage, err)
	}

	ignitionBytes, err := renderEmbeddedIgnition(m)
	if err != nil {
		return ISOBuildResult{}, fmt.Errorf("rendering embedded ignition: %w", err)
	}

	outISO := filepath.Join(opts.WorkDir, customizedISOFileName)
	if err := deps.customizer.EmbedIgnition(log, ignitionBytes, baseISO, outISO); err != nil {
		return ISOBuildResult{}, fmt.Errorf("embedding ignition into live ISO: %w", err)
	}
	return ISOBuildResult{ISOPath: outISO, LCABinaryPath: binaryPath}, nil
}

// fetchAndVerifyISO downloads iso.URL over HTTPS into destPath and verifies its sha256
// against iso.SHA256 before the ISO is used. Every failure fails the build (§6.1/§7.5/§8.J):
// a non-https URL is rejected before any download, a fetch error (missing/unreadable source)
// is surfaced, and a checksum mismatch is rejected. The caller (Validate) has already
// enforced the URL/digest shape; this re-checks the scheme as defense in depth because the
// download must never fall back to plain HTTP.
func fetchAndVerifyISO(ctx context.Context, fetcher isoFetcher, iso RHCOSLiveISO, destPath string) error {
	if err := validateHTTPSURL(iso.URL); err != nil {
		return fmt.Errorf("rhcos live ISO url %q %w", iso.URL, err)
	}
	if err := fetcher.FetchToFile(ctx, iso.URL, destPath); err != nil {
		return fmt.Errorf("fetching rhcos live ISO from %q: %w", iso.URL, err)
	}
	sum, err := sha256File(destPath)
	if err != nil {
		return fmt.Errorf("computing sha256 of fetched rhcos live ISO: %w", err)
	}
	// Validate already enforces a lowercase-hex digest in-pipeline; EqualFold keeps this
	// standalone building block correct if it is handed a (valid) uppercase digest directly.
	if !strings.EqualFold(sum, iso.SHA256) {
		return fmt.Errorf("rhcos live ISO sha256 mismatch: manifest expected %s, fetched %s", iso.SHA256, sum)
	}
	return nil
}

// sha256File returns the lowercase hex sha256 of the file at path.
func sha256File(path string) (string, error) {
	f, err := os.Open(filepath.Clean(path)) //nolint:gosec // path is build-time, under the caller's WorkDir
	if err != nil {
		return "", fmt.Errorf("opening file for checksum: %w", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("reading file for checksum: %w", err)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// deriveIBIPrepareConfig builds the embedded IBIPrepareConfig from the manifest. It sets the
// disconnected offline-prepare invariant and the cross-story on-media paths, and carries
// Shutdown verbatim (install-only true ⇒ shutdown; self-contained false ⇒ the binary pivots,
// CNF-27164). m.Shutdown is non-nil here: Validate rejects a nil Shutdown before any build.
// Profile mirrors the manifest mode (the Mode values are identical to the ibiconfig Profile
// constants); a disconnected config must carry it or the prepare phase's Validate rejects it.
func deriveIBIPrepareConfig(m *Manifest) ibiconfig.IBIPrepareConfig {
	return ibiconfig.IBIPrepareConfig{
		SeedImage:           m.SeedImage,
		SeedVersion:         m.SeedVersion,
		InstallationDisk:    m.InstallationDisk,
		Disconnected:        true,
		Shutdown:            m.Shutdown != nil && *m.Shutdown,
		LocalImagesPath:     common.LocalImagesMountPath,
		ExpectedDigestsPath: filepath.Join(common.LocalImagesMountPath, digestManifestFileName),
		Profile:             string(m.Mode),
	}
}

// renderEmbeddedIgnition builds the ignition config (version 3.2.0, hand-built map[string]any
// per the repo convention in postpivot.go) embedded into the live ISO. It writes the rendered
// IBIPrepareConfig at embeddedIBIConfigPath and a oneshot systemd unit that runs
// `lca-cli ibi -f <cfg>` on boot. The ignition deliberately carries NO binary: the multi-MB
// lca-cli binary is staged on p2 (common.LocalImagesLCABinaryPath) because it far exceeds the
// fixed ISO ignition embed area, so the ignition stays tiny regardless of binary size.
func renderEmbeddedIgnition(m *Manifest) ([]byte, error) {
	cfg := deriveIBIPrepareConfig(m)
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshaling embedded IBIPrepareConfig: %w", err)
	}

	configFile, err := ignitionFile(embeddedIBIConfigPath, embeddedIBIConfigMode, cfgJSON)
	if err != nil {
		return nil, fmt.Errorf("encoding embedded ibi config: %w", err)
	}

	ignConfig := map[string]any{
		"ignition": map[string]string{"version": common.IgnitionVersion32},
		"storage": map[string]any{
			"files": []any{configFile},
		},
		"systemd": map[string]any{
			"units": []any{
				ignitionUnit(ibiImagesMountUnitName(), ibiImagesMountUnitContents()),
				ignitionUnit(lcaIBIServiceName, lcaIBIUnitContents()),
			},
		},
	}
	out, err := json.Marshal(ignConfig)
	if err != nil {
		return nil, fmt.Errorf("marshaling embedded ignition: %w", err)
	}
	return out, nil
}

// ignitionUnit builds a single ignition v3.2 systemd.units entry that is enabled with the given
// name and unit contents.
func ignitionUnit(name, contents string) map[string]any {
	return map[string]any{
		"name":     name,
		"enabled":  true,
		"contents": contents,
	}
}

// ignitionFile builds a single ignition v3.2 storage.files entry whose contents are the
// gzip+base64 data URL of data. The decimal ignition file mode is set from mode.
func ignitionFile(path string, mode int, data []byte) (map[string]any, error) {
	source, err := gzipBase64DataURL(data)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"path": path,
		"mode": mode,
		"contents": map[string]any{
			"source":      source,
			"compression": "gzip",
		},
	}, nil
}

// gzipBase64DataURL gzips data and returns it as a base64 ignition data URL
// (data:;base64,<base64-of-gzip>). Paired with contents.compression="gzip", ignition
// base64-decodes then gunzips the content on the node.
func gzipBase64DataURL(data []byte) (string, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		return "", fmt.Errorf("gzipping ignition file contents: %w", err)
	}
	if err := gw.Close(); err != nil {
		return "", fmt.Errorf("finalizing gzip of ignition file contents: %w", err)
	}
	return "data:;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// lcaIBIUnitContents returns the oneshot systemd unit that auto-runs `lca-cli ibi` on boot,
// modeled on installation-configuration.service. The lca-cli binary and the mirrored images both
// live on the p2 (ibi-images) partition mounted at common.LocalImagesMountPath, so the unit is
// ordered explicitly after the label-based .mount unit this ignition also carries
// (ibiImagesMountUnitName), via Requires= + After=. RequiresMountsFor= alone only *orders*
// lca-ibi.service against a covering mount unit that already exists -- it does not define the mount
// -- so without the explicit .mount unit the ExecStart binary path could be absent when it runs.
func lcaIBIUnitContents() string {
	mountUnit := ibiImagesMountUnitName()
	return fmt.Sprintf(`[Unit]
Description=LCA USB-based image-based install preparation
Requires=%[1]s
After=%[1]s

[Service]
Type=oneshot
RemainAfterExit=no
ExecStart=%[2]s ibi -f %[3]s

[Install]
WantedBy=multi-user.target
`, mountUnit, common.LocalImagesLCABinaryPath, embeddedIBIConfigPath)
}

// ibiImagesMountUnitName returns the systemd unit name of the label-based .mount unit for the
// ibi-images partition. systemd derives a mount unit's name from its mount point, so the name must
// be the systemd-escaped form of common.LocalImagesMountPath with a .mount suffix (e.g.
// /var/mnt/ibi-images -> var-mnt-ibi\x2dimages.mount); a name that does not match Where= is
// rejected by systemd.
func ibiImagesMountUnitName() string {
	return systemdEscapePath(common.LocalImagesMountPath) + ".mount"
}

// ibiImagesMountUnitContents returns the label-based systemd .mount unit the embedded ignition
// carries so the ibi-images partition is mounted before lca-ibi.service execs the staged lca-cli
// binary from it. It is addressed by filesystem label (What=/dev/disk/by-label/<label>), never by
// partition number, matching the on-node discovery contract; Where= is the on-node mount point.
//
// It is mounted read-only (the on-media OCI layout, lca-cli binary, and durability store are all
// read-only runtime inputs) but WITHOUT noexec, because the lca-ibi.service domain must exec the
// staged lca-cli binary from it on boot.
//
// SELinux file context: the ibi-images ext4 filesystem is authored on the build host, so its
// on-disk labels are not the node's policy labels and the staged binary would otherwise mount as
// the generic mount default (e.g. unlabeled_t / mnt_t), which the service domain cannot execute --
// an enforcing-mode boot fails lca-ibi.service with status=203/EXEC ("Permission denied") before
// lca-cli can start. LCA already establishes the pattern of giving a staged executable an
// executable type: ops.prepareSELinuxTar relabels a copied tar to install_exec_t via
// `chcon -t install_exec_t` before executing it. For a read-only mounted filesystem the equivalent
// is to assign an executable type to the whole mount with a `context=` mount option; bin_t is the
// standard executable type, so the mount carries Options=ro,context=system_u:object_r:bin_t:s0.
// This is a single change that needs no first-boot relabel and works on a read-only filesystem.
// Enforcing-mode boot verification of the end-to-end IBI flow remains tracked under CNF-27158; the
// mount stays exec-capable (no noexec) so the service domain can exec the staged lca-cli binary.
func ibiImagesMountUnitContents() string {
	return fmt.Sprintf(`[Unit]
Description=LCA USB-based IBI images partition (%[1]s)

[Mount]
What=/dev/disk/by-label/%[1]s
Where=%[2]s
Type=auto
Options=ro,context=system_u:object_r:bin_t:s0

[Install]
WantedBy=multi-user.target
`, common.LocalImagesFSLabel, common.LocalImagesMountPath)
}

// systemdEscapePath returns the systemd path-escaped form of a mount path, matching
// `systemd-escape --path`: leading/trailing slashes are stripped (the root path escapes to "-"),
// remaining '/' become '-', and any byte outside [A-Za-z0-9_.] (plus a leading '.') is written as
// a C-style \xNN escape. This is how systemd derives a .mount unit name from its mount point.
func systemdEscapePath(path string) string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return "-"
	}
	var b strings.Builder
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		switch {
		case c == '/':
			b.WriteByte('-')
		case c == '.' && i == 0:
			fmt.Fprintf(&b, `\x%02x`, c)
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String()
}

// ---- production seam implementations ----

// httpISOFetcher is the production isoFetcher. It downloads over HTTPS only and refuses any
// redirect to a non-https URL, so the fetch can never silently fall back to plain HTTP.
type httpISOFetcher struct {
	log *logrus.Logger
	// transport, when set, overrides the default round-tripper. It exists so the HTTPS and
	// redirect-refusal behavior can be exercised against an httptest TLS server in unit
	// tests; production leaves it nil (http.DefaultTransport).
	transport http.RoundTripper
}

func (f *httpISOFetcher) FetchToFile(ctx context.Context, rawURL, destPath string) (err error) {
	client := &http.Client{
		Transport: f.transport,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if req.URL.Scheme != schemeHTTPS {
				return fmt.Errorf("refusing redirect to non-https url %q", req.URL.Redacted())
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected http status %s", resp.Status)
	}

	out, err := os.Create(filepath.Clean(destPath)) //nolint:gosec // destPath is build-time, under the caller's WorkDir
	if err != nil {
		return fmt.Errorf("creating destination file: %w", err)
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("closing destination file: %w", cerr)
		}
	}()
	if _, cerr := io.Copy(out, resp.Body); cerr != nil {
		// Remove the partial download so a truncated ISO is never left behind for a later
		// stage to stumble on; the checksum verify would fail the build regardless.
		if rmErr := os.Remove(destPath); rmErr != nil && !os.IsNotExist(rmErr) {
			f.log.Warnf("create-usb: removing partial ISO download %s failed: %v", destPath, rmErr)
		}
		return fmt.Errorf("writing destination file: %w", cerr)
	}
	return nil
}

// opsLCABinaryExtractor is the production lcaBinaryExtractor. It pulls and mounts the
// --lca-image container and copies the lca-cli binary out, sharing the pull/mount/cleanup
// lifecycle (withMountedImage) with opsSeedClosureReader in createusb_images.go.
type opsLCABinaryExtractor struct {
	ops ops.Ops
	log *logrus.Logger
}

func (e *opsLCABinaryExtractor) ExtractLCABinary(lcaImage, authFile, destPath string) error {
	return withMountedImage(e.ops, lcaImage, authFile, func(mountpoint string) error {
		data, err := e.ops.ReadFile(filepath.Join(mountpoint, lcaBinarySourcePath))
		if err != nil {
			return fmt.Errorf("reading %s from lca-cli image: %w", lcaBinarySourcePath, err)
		}
		if err := os.WriteFile(destPath, data, stagedLCABinaryMode); err != nil {
			return fmt.Errorf("writing extracted lca-cli binary: %w", err)
		}
		return nil
	})
}

// opsISOCustomizer is the production isoCustomizer backed by the in-tree isoeditor primitive
// (ops.CreateIsoWithEmbeddedIgnition), avoiding any external coreos-installer binary (§6.6).
type opsISOCustomizer struct {
	ops ops.Ops
}

func (c *opsISOCustomizer) EmbedIgnition(log logrus.FieldLogger, ignitionBytes []byte, baseISO, outISO string) error {
	if err := c.ops.CreateIsoWithEmbeddedIgnition(log, ignitionBytes, baseISO, outISO); err != nil {
		return fmt.Errorf("embedding ignition into %s: %w", baseISO, err)
	}
	return nil
}
