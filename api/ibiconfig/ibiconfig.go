package ibiconfig

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
)

// ImageBasedInstallConfigVersion is the version supported by this package.
const ImageBasedInstallConfigVersion = "v1beta1"

const (
	defaultExtraPartitionLabel = "varlibcontainers"
	sshPublicKeyRegex          = "^(ssh-rsa AAAAB3NzaC1yc2|ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNT|ecdsa-sha2-nistp384 AAAAE2VjZHNhLXNoYTItbmlzdHAzODQAAAAIbmlzdHAzOD|ecdsa-sha2-nistp521 AAAAE2VjZHNhLXNoYTItbmlzdHA1MjEAAAAIbmlzdHA1Mj|ssh-ed25519 AAAAC3NzaC1lZDI1NTE5|ssh-dss AAAAB3NzaC1kc3)[0-9A-Za-z+/]+[=]{0,3}( .*)?$"
)

// ImageBasedInstallConfig is the API for specifying configuration for the image-based installation iso.
type ImageBasedInstallConfig struct {
	// Params that are used to configure the installation iso and will be moved to installer
	RHCOSLiveISO string `json:"rhcosLiveIso,omitempty"`

	// RawNMStateConfig contains nmstate configuration YAML file provided as string.
	// String will be written to file and will be applied with "nmstatectl apply" command.
	// Example of nmstate configurations can be found in this link https://nmstate.io/examples.html
	// +optional
	NMStateConfig string `json:"nmStateConfig,omitempty"`

	// PullSecret is the secret to use when pulling images. Equivalent to install-config.yaml's pullSecret.
	PullSecret string `json:"pullSecret,omitempty"`

	// Proxy is the proxy settings for the installation iso. Equivalent to
	// install-config.yaml's proxy. This will be set to the installation service
	// +optional
	Proxy *seedreconfig.Proxy `json:"proxy,omitempty"`

	// ImageDigestSources lists sources/repositories for the release-image content.
	// +optional
	ImageDigestSources []ImageDigestSource `json:"imageDigestSources,omitempty"`

	// PEM-encoded X.509 certificate bundle. Will be copied to /etc/pki/ca-trust/source/anchors/
	// in the installation iso
	// +optional
	AdditionalTrustBundle string `json:"additionalTrustBundle,omitempty"`

	// SSHKey is the public Secure Shell (SSH) key to provide access to
	// instances. Equivalent to install-config.yaml's sshKey. T
	// This key will be added to the host to allow ssh access while running with installation iso
	SSHKey string `json:"sshKey,omitempty"`

	// IBIPrepareConfig parameter that should be provided to lca cli installation service
	IBIPrepareConfig `json:",inline"`
}

// IBIPrepareConfig is the API for specifying configuration for lca cli.
type IBIPrepareConfig struct {

	// SeedImage is the seed image to use for the installation.
	// This image will be used to prepare the installation disk.
	SeedImage string `json:"seedImage"`

	// SeedVersion is the version of the seed image.
	// Version will be validated against the seed image.
	SeedVersion string `json:"seedVersion"`

	// InstallationDisk is the disk to install the seed image.
	// Provide the device id like /dev/by-id/ata-xxxxx
	InstallationDisk string `json:"installationDisk"`

	// PrecacheBestEffort is a flag to enable best effort precaching.
	// +optional
	PrecacheBestEffort bool `json:"precacheBestEffort,omitempty"`

	// PrecacheDisabled is a flag to disable precaching.
	// +optional
	PrecacheDisabled bool `json:"precacheDisabled,omitempty"`

	// Shutdown is a flag to shutdown the host after installation.
	// Default is false
	// +optional
	Shutdown bool `json:"shutdown,omitempty"`

	// UseContainersFolder is a flag to use /var/lib/containers as a folder and not extra partition.
	// Default is false
	UseContainersFolder bool `json:"useContainersFolder,omitempty"`

	// ExtraPartitionStart start of extra partition used for /var/lib/containers.
	// Partition will expand until the end of the disk. Uses sgdisk notation
	// Default is -40G (40GB before the end of the disk)
	// +optional
	ExtraPartitionStart string `json:"extraPartitionStart,omitempty"`

	// ExtraPartitionLabel label of extra partition used for /var/lib/containers.
	// Default is varlibcontainers
	// +optional
	ExtraPartitionLabel string `json:"extraPartitionLabel,omitempty"`

	// ExtraPartitionNumber number of extra partition used for /var/lib/containers.
	// Default is 5
	// +optional
	ExtraPartitionNumber uint `json:"extraPartitionNumber,omitempty"`

	// SkipDiskCleanup is a flag to skip disk cleanup before installation.
	// As part of installation we will try to format the disk this flag will skip that step.
	// Default is false
	SkipDiskCleanup bool `json:"skipDiskCleanup,omitempty"`
}

type ImageDigestSource struct {
	// Source is the repository that users refer to, e.g. in image pull specifications.
	Source string `json:"source"`

	// Mirrors is one or more repositories that may also contain the same images.
	Mirrors []string `json:"mirrors,omitempty"`
}

func (i *ImageBasedInstallConfig) SetDefaultValues() {
	if i.RHCOSLiveISO == "" {
		i.RHCOSLiveISO = "https://mirror.openshift.com/pub/openshift-v4/amd64/dependencies/rhcos/latest/rhcos-live.x86_64.iso"
	}
	i.IBIPrepareConfig.SetDefaultValues()
}

func (i *ImageBasedInstallConfig) Validate() error {
	if i.RHCOSLiveISO == "" {
		return fmt.Errorf("rhcosLiveIso is required")
	}
	if i.PullSecret == "" {
		return fmt.Errorf("pullSecret is required")
	}

	var ps interface{}
	if err := json.Unmarshal([]byte(i.PullSecret), &ps); err != nil {
		return fmt.Errorf("pullSecret is not a valid json")
	}

	if i.SSHKey != "" {
		if err := i.validateSSHPublicKey(i.SSHKey); err != nil {
			return err
		}
	}

	if i.AdditionalTrustBundle != "" {
		if err := i.validatePEMCertificateBundle(i.AdditionalTrustBundle); err != nil {
			return err
		}
	}

	if err := i.IBIPrepareConfig.Validate(); err != nil {
		return err
	}

	return nil

}

func (i *ImageBasedInstallConfig) validateSSHPublicKey(sshPublicKeys string) error {
	regexpSshPublicKey := regexp.MustCompile(sshPublicKeyRegex)
	sshPublicKeys = strings.TrimSpace(sshPublicKeys)
	for _, sshPublicKey := range strings.Split(sshPublicKeys, "\n") {
		sshPublicKey = strings.TrimSpace(sshPublicKey)
		keyBytes := []byte(sshPublicKey)
		isMatched := regexpSshPublicKey.Match(keyBytes)
		if !isMatched {
			return fmt.Errorf(
				"ssh key: `%s` does not match any supported type: ssh-rsa, ssh-ed25519, ecdsa-[VARIANT]",
				sshPublicKey)
		} else if _, _, _, _, err := ssh.ParseAuthorizedKey(keyBytes); err != nil {
			return fmt.Errorf("malformed SSH key: %s, err: %w ", sshPublicKey, err)
		}
	}

	return nil
}

func (i *ImageBasedInstallConfig) validatePEMCertificateBundle(bundle string) error {
	// From https://github.com/openshift/installer/blob/56e61f1df5aa51ff244465d4bebcd1649003b0c9/pkg/validate/validate.go#L29-L47
	rest := []byte(bundle)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return fmt.Errorf("invalid PEM block in provided trusted bundle")
		}
		_, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("parse failed: %w", err)
		}
		if len(rest) == 0 {
			break
		}
	}
	return nil
}

func (c *IBIPrepareConfig) Validate() error {
	if c.SeedImage == "" {
		return fmt.Errorf("seedImage is required")
	}
	if c.SeedVersion == "" {
		return fmt.Errorf("seedVersion is required")
	}
	if c.InstallationDisk == "" {
		return fmt.Errorf("installationDisk is required")
	}

	return nil
}

func (c *IBIPrepareConfig) SetDefaultValues() {
	if c.ExtraPartitionStart == "" {
		c.ExtraPartitionStart = "-40G"
	}
	if c.ExtraPartitionNumber == 0 {
		c.ExtraPartitionNumber = 5
	}
	if c.ExtraPartitionLabel == "" {
		c.ExtraPartitionLabel = defaultExtraPartitionLabel
	}
}
