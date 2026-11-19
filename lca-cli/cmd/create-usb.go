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
	"context"
	"fmt"
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
		manifestFile     string
		authFile         string
		outputPath       string
		writeDevice      string
		rhcosLiveISO     string
		rhcosLiveSHA     string
		recertImage      string
		lcaImage         string
		usbSize          string
		openshiftInstall string
		credentialsDir   string
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
				manifestFile:     manifestFile,
				authFile:         authFile,
				outputPath:       outputPath,
				writeDevice:      writeDevice,
				rhcosLiveISO:     rhcosLiveISO,
				rhcosLiveSHA:     rhcosLiveSHA,
				recertImage:      recertImage,
				lcaImage:         lcaImage,
				usbSize:          usbSize,
				openshiftInstall: openshiftInstall,
				credentialsDir:   credentialsDir,
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
	// --openshift-install enables automatic kubeconfig-identity generation for self-contained
	// media when the manifest supplies neither siteConfig.kubeadminPasswordHash nor
	// kubeconfigCryptoRetention. create-usb shells out to this binary's
	// `image-based create config-image` flow in a scrubbed temp directory, populates the two
	// fields, and emits the matching kubeconfig + kubeadmin password (see --credentials-dir).
	// Source it for disconnected builders with `oc adm release extract --command=openshift-install`.
	cmd.Flags().StringVar(&openshiftInstall, "openshift-install", "openshift-install", "openshift-install binary used to generate self-contained kubeconfig identity when the manifest omits it (path or name on PATH).")
	// --credentials-dir is where the generated kubeconfig and kubeadmin-password artifacts are
	// written (owner-only), defaulting to a per-output <output-file-name>-credentials directory next
	// to --output (or ./credentials when writing to a device).
	cmd.Flags().StringVar(&credentialsDir, "credentials-dir", "", "Directory for the generated kubeconfig and kubeadmin-password when create-usb mints self-contained identity (defaults to a <output-file-name>-credentials directory next to --output, or ./credentials when writing to a device).")

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
	manifestFile     string
	authFile         string
	outputPath       string
	writeDevice      string
	rhcosLiveISO     string
	rhcosLiveSHA     string
	recertImage      string
	lcaImage         string
	usbSize          string
	openshiftInstall string
	credentialsDir   string
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

	// Preflight the (empty) credentials directory before the expensive build: fail fast on an
	// unusable path rather than after assembling an image whose credentials could then never be
	// written. No secret material is written before the build.
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
		if strings.TrimSpace(opts.openshiftInstall) == "" {
			return nil, fmt.Errorf("self-contained manifest omits siteConfig.kubeadminPasswordHash and siteConfig.kubeconfigCryptoRetention; pass --openshift-install to generate them, or supply both in the manifest")
		}
		gen = createusb.NewInstallerConfigImageGenerator(log, opts.openshiftInstall)
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
