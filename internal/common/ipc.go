package common

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	igntypes "github.com/coreos/ignition/v2/config/v3_2/types"
	machineconfigv1 "github.com/openshift/api/machineconfiguration/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type IPConfigPrePivotConfig struct {
	NewStaterootName              string `json:"new-stateroot-name,omitempty"`
	IPv4Address                   string `json:"ipv4-address,omitempty"`
	IPv4MachineNetwork            string `json:"ipv4-machine-network,omitempty"`
	IPv6Address                   string `json:"ipv6-address,omitempty"`
	IPv6MachineNetwork            string `json:"ipv6-machine-network,omitempty"`
	DesiredIPv4Gateway            string `json:"ipv4-gateway,omitempty"`
	DesiredIPv6Gateway            string `json:"ipv6-gateway,omitempty"`
	CurrentIPv4Gateway            string `json:"current-ipv4-gateway,omitempty"`
	CurrentIPv6Gateway            string `json:"current-ipv6-gateway,omitempty"`
	IPv4DNSServer                 string `json:"ipv4-dns,omitempty"`
	IPv6DNSServer                 string `json:"ipv6-dns,omitempty"`
	VLANID                        int    `json:"vlan-id,omitempty"`
	DNSIPFamily                   string `json:"dns-ip-family,omitempty"`
	PullSecretRefName             string `json:"pull-secret-ref-name,omitempty"`
	InstallInitMonitor            bool   `json:"install-init-monitor,omitempty"`
	InstallIPConfigurationService bool   `json:"install-ip-configuration-service,omitempty"`
}

type IPConfigPostPivotConfig struct {
	RecertImage string `json:"recert-image,omitempty"`
	DNSIPFamily string `json:"dns-ip-family,omitempty"`
}

// SetDNSMasqFilterInMachineConfig updates the dnsmasq MachineConfig to filter DNS answers
// according to the desired IP family ("ipv4" or "ipv6").
func SetDNSMasqFilterInMachineConfig(
	ctx context.Context,
	k8sClient runtimeclient.Client,
	family string,
) error {
	var filterLine string
	switch family {
	case IPv4FamilyName:
		filterLine = DnsmasqFilterIPv4
	case IPv6FamilyName:
		filterLine = DnsmasqFilterIPv6
	default:
		return fmt.Errorf("unsupported DNS IP family: %s", family)
	}

	encoded := url.PathEscape(filterLine)
	source := fmt.Sprintf(DataURLBase64Template, encoded)

	existingMC := &machineconfigv1.MachineConfig{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: DnsmasqMachineConfigName}, existingMC); err != nil {
		return fmt.Errorf("failed to get existing machine config %s: %w", DnsmasqMachineConfigName, err)
	}

	var cfg igntypes.Config
	if len(existingMC.Spec.Config.Raw) > 0 {
		if err := json.Unmarshal(existingMC.Spec.Config.Raw, &cfg); err != nil {
			return fmt.Errorf("failed to parse ignition config in %s: %w", DnsmasqMachineConfigName, err)
		}
	}
	if cfg.Ignition.Version == "" {
		cfg.Ignition.Version = IgnitionVersion32
	}

	trueVal := true
	modeVal := FileMode0644
	newFile := igntypes.File{
		Node: igntypes.Node{
			Path:      DnsmasqFilterTargetPath,
			Overwrite: &trueVal,
		},
		FileEmbedded1: igntypes.FileEmbedded1{
			Mode: &modeVal,
			Contents: igntypes.Resource{
				Source: &source,
			},
		},
	}

	updated := false
	for idx, f := range cfg.Storage.Files {
		if f.Path == DnsmasqFilterTargetPath {
			if f.Contents.Source != nil && *f.Contents.Source == source {
				// Already configured as desired
				existingMC.Spec.Config = runtime.RawExtension{Raw: existingMC.Spec.Config.Raw}
				return nil
			}
			cfg.Storage.Files[idx] = newFile
			updated = true
			break
		}
	}
	if !updated {
		cfg.Storage.Files = append(cfg.Storage.Files, newFile)
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal updated ignition for %s: %w", DnsmasqMachineConfigName, err)
	}
	existingMC.Spec.Config = runtime.RawExtension{Raw: raw}

	if err := k8sClient.Update(ctx, existingMC); err != nil {
		return fmt.Errorf("failed to update machine config %s: %w", DnsmasqMachineConfigName, err)
	}

	return nil
}

// DetectClusterIPFamilies inspects cluster info to determine whether the
// cluster is configured with IPv4, IPv6, or both (dual-stack).
func DetectClusterIPFamilies(ips []string) (bool, bool) {
	var nodeIPv4, nodeIPv6 string
	for _, ip := range ips {
		if strings.Contains(ip, ":") {
			if nodeIPv6 == "" {
				nodeIPv6 = ip
			}
		} else {
			if nodeIPv4 == "" {
				nodeIPv4 = ip
			}
		}
	}

	clusterHasIPv4 := nodeIPv4 != ""
	clusterHasIPv6 := nodeIPv6 != ""

	return clusterHasIPv4, clusterHasIPv6
}
