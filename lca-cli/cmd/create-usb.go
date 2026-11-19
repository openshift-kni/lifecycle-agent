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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/createusb"
)

func init() {
	// Self-register the create-usb command so root.go needs no change.
	rootCmd.AddCommand(newCreateUsbCmd())
}

// newCreateUsbCmd builds the `create-usb` subcommand. It is constructed via a factory
// (rather than package-level vars) so each invocation — including unit tests — gets
// fresh flag state, mirroring the ip-config command's NewIPConfigCmd pattern.
func newCreateUsbCmd() *cobra.Command {
	var (
		manifestFile   string
		authFile       string
		outputPath     string
		writeDevice    string
		rhcosLiveISO   string
		rhcosLiveSHA   string
		recertImage    string
		lcaImage       string
		usbSize        string
		releaseImage   string
		credentialsDir string
		force          bool
	)

	cmd := &cobra.Command{
		Use:   "create-usb",
		Short: "Create a bootable USB-based IBI image from a manifest.",
		Long: `create-usb parses a single USB-based IBI manifest, applies CLI overrides, and runs
every build-time validation rule before assembling the media.

With --output, the command builds the customized RHCOS live ISO (fetched over HTTPS and
verified, with an embedded ignition that auto-runs lca-cli ibi), writes it to the front of the
image (LBA 0), and appends the formatted, labelled data partitions after it (the CNF-27197-proven
DOS/MBR layout). It then populates the ibi-images partition with the OCI image layout, the on-media
digests.json, and the staged lca-cli binary (self-contained media also carries the read-only
durability store). --lca-image is required for assembly. --usb-size, when set, fails the build if
the assembled media exceeds the budget. --write-device is deferred.`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCreateUsb(createUsbOptions{
				manifestFile:   manifestFile,
				authFile:       authFile,
				outputPath:     outputPath,
				writeDevice:    writeDevice,
				rhcosLiveISO:   rhcosLiveISO,
				rhcosLiveSHA:   rhcosLiveSHA,
				recertImage:    recertImage,
				lcaImage:       lcaImage,
				usbSize:        usbSize,
				releaseImage:   releaseImage,
				credentialsDir: credentialsDir,
				force:          force,
			})
		},
	}

	cmd.Flags().StringVarP(&manifestFile, "manifest", "f", "", "Path to the USB-based IBI YAML manifest (required).")
	// --auth-file supplies workstation mirror-registry credentials used at build time only
	// (never written to the media). It is consumed by the media-build pipeline that lands in
	// CNF-27167/CNF-27168/CNF-27169; the flag is defined here so the command contract is stable.
	cmd.Flags().StringVar(&authFile, "auth-file", "", "Path to workstation mirror-registry credentials used at build time only (never written to the media).")
	cmd.Flags().StringVar(&outputPath, "output", "", "Path of the .img file to write (mutually exclusive with --write-device).")
	cmd.Flags().StringVar(&writeDevice, "write-device", "", "Block device to write the media to (mutually exclusive with --output).")
	cmd.Flags().StringVar(&rhcosLiveISO, "rhcos-live-iso", "", "Override the manifest rhcosLiveIso.url (must be set together with --rhcos-live-iso-sha256).")
	cmd.Flags().StringVar(&rhcosLiveSHA, "rhcos-live-iso-sha256", "", "Override the manifest rhcosLiveIso.sha256 (must be set together with --rhcos-live-iso).")
	// --recert-image and --lca-image are the authoritative sources for the recert and
	// lca-cli images added to the install-only image set. They are guaranteed in the
	// resolved set independently of the seed's containers.list contents. The recert
	// default follows the same precedence the controllers use (flag > RELATED_IMAGE_RECERT_IMAGE
	// env > built-in default); lca-cli has no build-workstation default because the
	// runtime resolver reads the running operator pod, so an explicit value is required.
	// They are consumed by the media-build pipeline (image resolution/mirroring landing
	// in CNF-27171); the flags are defined here so the command contract is stable.
	cmd.Flags().StringVar(&recertImage, "recert-image", defaultRecertImage(), "Recert image pull-spec added to the install-only image set (defaults to $RELATED_IMAGE_RECERT_IMAGE or the built-in default).")
	cmd.Flags().StringVar(&lcaImage, "lca-image", "", "lca-cli image pull-spec added to the install-only image set (required for media assembly).")
	// --usb-size is the whole-stick size budget (e.g. "14Gi", "32G"). When set, the build fails if
	// the assembled media's final apparent size exceeds it. Empty disables the check.
	cmd.Flags().StringVar(&usbSize, "usb-size", "", "USB-stick size budget (e.g. 14Gi, 32G). The build fails if the assembled media exceeds it. Empty disables the check.")
	// --release-image is the OpenShift release image create-usb extracts openshift-install from to
	// mint the self-contained kubeconfig identity when the manifest omits it. When unset it defaults
	// to the canonical ocp-release image for the manifest releaseVersion and the seed image's
	// architecture; set it explicitly for mirrored or disconnected builds.
	cmd.Flags().StringVar(&releaseImage, "release-image", "", "OpenShift release image to source openshift-install from when create-usb mints self-contained kubeconfig identity. Defaults to quay.io/openshift-release-dev/ocp-release:<releaseVersion>-<arch> (architecture read from the seed image); set it explicitly for mirrored or disconnected builds.")
	// --credentials-dir is where the generated kubeconfig and kubeadmin-password artifacts are
	// written (owner-only), defaulting to a per-output <output-file-name>-credentials directory next
	// to --output (or ./credentials when writing to a device).
	cmd.Flags().StringVar(&credentialsDir, "credentials-dir", "", "Directory for the generated kubeconfig and kubeadmin-password when create-usb mints self-contained identity (defaults to a <output-file-name>-credentials directory next to --output, or ./credentials when writing to a device).")
	// --force overwrites existing credential artifacts without prompting. Without it, an existing
	// kubeconfig/kubeadmin-password in the credentials directory prompts on an interactive terminal
	// and fails a non-interactive run, so a rebuild cannot silently replace distributed credentials.
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite existing credential artifacts in the credentials directory without prompting.")

	_ = cmd.MarkFlagRequired("manifest")

	return cmd
}

// defaultRecertImage resolves the default recert image for the --recert-image flag,
// following the controllers' precedence: the RELATED_IMAGE_RECERT_IMAGE environment
// variable if set, otherwise the built-in default. An explicit --recert-image flag
// still overrides this default.
func defaultRecertImage() string {
	if v := os.Getenv(common.RecertImageEnvKey); v != "" {
		return v
	}
	return common.DefaultRecertImage
}

// createUsbOptions carries the resolved create-usb flag values.
type createUsbOptions struct {
	manifestFile   string
	authFile       string
	outputPath     string
	writeDevice    string
	rhcosLiveISO   string
	rhcosLiveSHA   string
	recertImage    string
	lcaImage       string
	usbSize        string
	releaseImage   string
	credentialsDir string
	force          bool
}

// runCreateUsb orchestrates the create-usb flow: validate the flag combinations, parse the
// --usb-size budget, parse the manifest, apply the --rhcos-live-iso* overrides, validate the
// manifest, then hand off to the media assembly build.
func runCreateUsb(opts createUsbOptions) error {
	if err := validateOutputTarget(opts.outputPath, opts.writeDevice); err != nil {
		return err
	}
	if err := validateISOOverrideFlags(opts.rhcosLiveISO, opts.rhcosLiveSHA); err != nil {
		return err
	}

	usbSizeBytes, err := parseUSBSize(opts.usbSize)
	if err != nil {
		return err
	}

	manifest, err := createusb.ParseManifest(opts.manifestFile)
	if err != nil {
		return fmt.Errorf("create-usb: %w", err)
	}

	// The --rhcos-live-iso* flags override the manifest value (flag wins) and apply as a
	// pair; validateISOOverrideFlags has already rejected a partial override.
	if opts.rhcosLiveISO != "" {
		manifest.RHCOSLiveISO.URL = opts.rhcosLiveISO
		manifest.RHCOSLiveISO.SHA256 = opts.rhcosLiveSHA
	}

	// If create-usb will mint credentials, confirm any overwrite of existing artifacts before the
	// expensive installer extraction and media build, so a rebuild cannot silently replace
	// credentials that may still authenticate an earlier cluster.
	if createusb.NeedsCredentialGeneration(manifest) {
		if err := confirmCredentialOverwrite(resolveCredentialsDir(opts), opts.force); err != nil {
			return fmt.Errorf("create-usb: %w", err)
		}
	}

	// Generate the self-contained kubeconfig identity before validation so Validate checks the
	// generated material like an operator-supplied override. The hash and crypto retention are
	// populated into the manifest here; the matching kubeconfig and password are written only after
	// the build succeeds (below).
	creds, err := generateCredentialsIfNeeded(opts, manifest)
	if err != nil {
		return fmt.Errorf("create-usb: %w", err)
	}

	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("create-usb: manifest validation failed: %w", err)
	}

	log.Infof("create-usb manifest validated (mode=%s)", manifest.Mode)

	// Preflight the (empty) credentials directory before the build: fail fast on an unusable path
	// rather than after assembling an image whose credentials could then never be written. No secret
	// material is written before the build.
	if creds != nil {
		if err := os.MkdirAll(resolveCredentialsDir(opts), 0o700); err != nil {
			return fmt.Errorf("create-usb: creating credentials directory: %w", err)
		}
	}

	if err := createusb.Build(context.Background(), log, manifest, createusb.BuildOptions{
		OutputPath:   opts.outputPath,
		WriteDevice:  opts.writeDevice,
		LCAImage:     opts.lcaImage,
		AuthFile:     opts.authFile,
		RecertImage:  opts.recertImage,
		USBSizeBytes: usbSizeBytes,
	}); err != nil {
		return fmt.Errorf("create-usb: %w", err)
	}

	// Publish the generated credentials only after the build succeeds, so a failed build leaves any
	// previously distributed kubeconfig/password untouched and still matching the surviving image.
	if err := writeGeneratedCredentials(opts, creds); err != nil {
		return fmt.Errorf("create-usb: %w", err)
	}
	return nil
}

// generateCredentialsIfNeeded mints the self-contained kubeconfig identity when the manifest omits
// it, populating the manifest's hash and crypto-retention fields. It returns the generated bundle
// for the caller to write after validation, or nil when the operator supplied the credentials or the
// mode does not use them.
func generateCredentialsIfNeeded(opts createUsbOptions, manifest *createusb.Manifest) (*createusb.GeneratedCredentials, error) {
	var gen createusb.CredentialGenerator
	if createusb.NeedsCredentialGeneration(manifest) {
		// --release-image is optional: when unset, the generator defaults to the canonical ocp-release
		// image for the manifest release version and the seed image's architecture.
		gen = createusb.NewInstallerConfigImageGenerator(log, createusb.InstallerConfig{
			ReleaseImage:   opts.releaseImage,
			SeedImage:      manifest.SeedImage,
			AuthFile:       opts.authFile,
			ReleaseVersion: manifest.ReleaseVersion,
		})
	}

	creds, err := createusb.GenerateSiteConfigCredentials(context.Background(), log, manifest, gen)
	if err != nil {
		return nil, fmt.Errorf("generating self-contained credentials: %w", err)
	}
	return creds, nil
}

// writeGeneratedCredentials emits the generated kubeconfig and kubeadmin password as owner-only
// build artifacts (never on the media). It runs only after the build succeeds and is a no-op when
// nothing was generated.
func writeGeneratedCredentials(opts createUsbOptions, creds *createusb.GeneratedCredentials) error {
	if creds == nil {
		return nil
	}
	if err := createusb.WriteCredentialArtifacts(log, resolveCredentialsDir(opts), creds); err != nil {
		return fmt.Errorf("writing credential artifacts: %w", err)
	}
	return nil
}

// confirmCredentialOverwrite guards the credentials directory before the build: if credential
// artifacts already exist there, it prompts on an interactive terminal and refuses a non-interactive
// run, so a rebuild cannot silently replace credentials that may still authenticate an earlier
// cluster. --force overwrites without prompting.
func confirmCredentialOverwrite(dir string, force bool) error {
	existing := existingCredentialArtifacts(dir)
	if len(existing) == 0 {
		return nil
	}
	proceed, err := resolveOverwriteConsent(dir, existing, force, stdinIsInteractive(), os.Stdin)
	if err != nil {
		return err
	}
	if !proceed {
		return fmt.Errorf("credential overwrite declined; aborting before build")
	}
	return nil
}

// existingCredentialArtifacts returns the credential artifact paths that already exist in dir.
func existingCredentialArtifacts(dir string) []string {
	var existing []string
	for _, name := range []string{createusb.KubeconfigArtifactName, createusb.KubeadminPasswordArtifactName} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			existing = append(existing, p)
		}
	}
	return existing
}

// resolveOverwriteConsent decides whether overwriting the existing artifacts may proceed: --force
// always allows it, an interactive terminal is prompted, and a non-interactive run without --force is
// refused with an actionable error.
func resolveOverwriteConsent(dir string, existing []string, force, interactive bool, in io.Reader) (bool, error) {
	if force {
		log.Warnf("create-usb: overwriting existing credential artifacts in %s (--force); previously distributed credentials will no longer authenticate", dir)
		return true, nil
	}
	if !interactive {
		return false, fmt.Errorf("existing credential artifacts found in %s (%s); refusing to overwrite them in a non-interactive run -- re-run with --force to overwrite, or set --credentials-dir to a new location", dir, strings.Join(existing, ", "))
	}
	fmt.Fprintf(os.Stderr, "create-usb: existing credential artifacts found in %s (%s).\nOverwrite them? Previously distributed credentials will no longer authenticate [y/N]: ", dir, strings.Join(existing, ", "))
	answer, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("reading overwrite confirmation: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// stdinIsInteractive reports whether standard input is a terminal (character device), so create-usb
// only prompts when a human can answer.
func stdinIsInteractive() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// resolveCredentialsDir returns where credential artifacts are written: the explicit
// --credentials-dir when set, otherwise the per-output default from credentialsOutputDir. The
// pre-build preflight and the publish step share it so they target the same directory.
func resolveCredentialsDir(opts createUsbOptions) string {
	if opts.credentialsDir != "" {
		return opts.credentialsDir
	}
	return credentialsOutputDir(opts.outputPath)
}

// credentialsOutputDir resolves the default credentials directory, keyed off the full --output file
// name (extension kept, so out/cluster-a.img and out/cluster-a do not collide) so distinct images in
// the same parent directory get separate credential directories. When writing to a device there is
// no output path to key off, so a single "credentials" directory is used.
func credentialsOutputDir(outputPath string) string {
	if outputPath == "" {
		return "credentials"
	}
	base := filepath.Base(outputPath)
	return filepath.Join(filepath.Dir(outputPath), base+"-credentials")
}

// parseUSBSize parses the --usb-size budget (e.g. "14Gi", "32G") into bytes using the standard
// Kubernetes quantity format. An empty value disables the budget (returns 0); a negative or
// unparseable value is an error.
func parseUSBSize(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, fmt.Errorf("invalid --usb-size %q (use a size like 14Gi or 32G): %w", s, err)
	}
	bytes := q.Value()
	if bytes <= 0 {
		return 0, fmt.Errorf("invalid --usb-size %q: must be greater than zero", s)
	}
	return bytes, nil
}

// validateOutputTarget enforces that exactly one of --output or --write-device is set.
func validateOutputTarget(outputPath, writeDevice string) error {
	switch {
	case outputPath == "" && writeDevice == "":
		return fmt.Errorf("exactly one of --output or --write-device is required")
	case outputPath != "" && writeDevice != "":
		return fmt.Errorf("--output and --write-device are mutually exclusive")
	}
	return nil
}

// validateISOOverrideFlags enforces that --rhcos-live-iso and --rhcos-live-iso-sha256 are
// provided together (they override the manifest value as a pair).
func validateISOOverrideFlags(rhcosLiveISO, rhcosLiveSHA string) error {
	if (rhcosLiveISO == "") != (rhcosLiveSHA == "") {
		return fmt.Errorf("--rhcos-live-iso and --rhcos-live-iso-sha256 must be provided together")
	}
	return nil
}
