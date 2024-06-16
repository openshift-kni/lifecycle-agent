package installationiso

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"io"
	"net/http"
	"os"
	"path"

	"github.com/coreos/ignition/v2/config/merge"
	"github.com/coreos/ignition/v2/config/v3_2"
	igntypes "github.com/coreos/ignition/v2/config/v3_2/types"
	"github.com/sirupsen/logrus"

	"github.com/openshift-kni/lifecycle-agent/api/ibiconfig"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

type InstallationIso struct {
	log     *logrus.Logger
	ops     ops.Ops
	workDir string
}

type installServiceTemplate struct {
	SeedImage            string
	IBIConfigurationPath string
	HTTPProxy            string
	HTTPSProxy           string
	NoProxy              string
	PullSecretPath       string
}

//go:embed data/*
var folder embed.FS

func NewInstallationIso(log *logrus.Logger, ops ops.Ops, workDir string) *InstallationIso {
	return &InstallationIso{
		log:     log,
		ops:     ops,
		workDir: workDir,
	}
}

const (
	seedInstallScriptFilePath = "data/install-rhcos-and-restore-seed.sh"
	seedInstallationService   = "data/install-rhcos-and-restore-seed.service.template"
	networkConfigServicePath  = "data/network-config.service"
	ibiIgnitionFileName       = "ibi-ignition.json"
	rhcosIsoFileName          = "rhcos-live.x86_64.iso"
	ibiIsoFileName            = "rhcos-ibi.iso"
	coreosInstallerImage      = "quay.io/coreos/coreos-installer:latest"
	ibiConfigFileName         = "ibi-configuration.json"
	psIgnitioFilePath         = "/var/tmp/pull-secret.json"
	ibiConfigIgnitionFilePath = "/var/tmp/" + ibiConfigFileName
	trustedBundleFilePath     = "/etc/pki/ca-trust/source/anchors/additional-trust-bundle.pem"
	nmstateConfigFilePath     = "/var/tmp/network-config.yaml"
	installationScriptPath    = "/usr/local/bin/install-rhcos-and-restore-seed.sh"
	mirrorRegistryFilePath    = "/etc/containers/registries.conf"
)

func (r *InstallationIso) Create(ibiConfig *ibiconfig.ImageBasedInstallConfig) error {
	r.log.Info("Creating IBI installation ISO")
	err := r.validate()
	if err != nil {
		return err
	}
	err = r.createIgnitionFile(ibiConfig)
	if err != nil {
		return err
	}
	if err := r.downloadLiveIso(ibiConfig.RHCOSLiveISO); err != nil {
		return err
	}
	if err := r.embedIgnitionToIso(); err != nil {
		return err
	}
	r.log.Infof("installation ISO created at: %s", path.Join(r.workDir, ibiIsoFileName))

	return nil
}

func (r *InstallationIso) validate() error {
	_, err := os.Stat(r.workDir)
	if err != nil && os.IsNotExist(err) {
		return fmt.Errorf("work dir doesn't exists %w", err)
	}
	return nil
}

func (r *InstallationIso) createIgnitionFile(ibiConfig *ibiconfig.ImageBasedInstallConfig) error {
	r.log.Info("Generating Ignition Config")
	ignition, err := r.renderIgnition(ibiConfig)
	if err != nil {
		return err
	}

	ignitionFilePath := path.Join(r.workDir, ibiIgnitionFileName)
	if err := utils.MarshalToFile(ignition, ignitionFilePath); err != nil {
		return fmt.Errorf("failed to write ignition file: %w", err)
	}
	return nil
}

func (r *InstallationIso) embedIgnitionToIso() error {
	r.log.Info("Embedding Ignition to ISO")
	baseIsoPath := path.Join(r.workDir, rhcosIsoFileName)
	ibiIsoPath := path.Join(r.workDir, ibiIsoFileName)
	if _, err := os.Stat(ibiIsoPath); err == nil {
		r.log.Infof("ibi ISO exists (%s), deleting it", ibiIsoPath)
		if err = os.Remove(ibiIsoPath); err != nil {
			return fmt.Errorf("failed to delete existing ibi ISO: %w", err)
		}
	}
	ignitionFilePath := path.Join(r.workDir, ibiIgnitionFileName)
	config, err := os.ReadFile(ignitionFilePath)
	if err != nil {
		return fmt.Errorf("failed to read ignition file: %w", err)
	}

	if err := r.ops.CreateIsoWithEmbeddedIgnition(r.log, config, baseIsoPath, ibiIsoPath); err != nil {
		return fmt.Errorf("failed to create iso with embedded ignition: %w", err)
	}

	return nil
}

func (r *InstallationIso) downloadLiveIso(url string) error {
	r.log.Info("Downloading live ISO")
	rhcosIsoPath := path.Join(r.workDir, rhcosIsoFileName)
	if _, err := os.Stat(rhcosIsoPath); err == nil {
		r.log.Infof("rhcos live ISO (%s) exists, skipping download", rhcosIsoPath)
		return nil
	}

	isoFile, err := os.Create(rhcosIsoPath)
	if err != nil {
		return fmt.Errorf("failed to rhcos iso path in %s: %w", rhcosIsoPath, err)
	}
	defer isoFile.Close()

	resp, err := http.Get(url) //nolint:gosec
	if err != nil {
		return fmt.Errorf("failed to make http get call to %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download ISO from URL, status: %s", resp.Status)
	}

	if _, err := io.Copy(isoFile, resp.Body); err != nil {
		return fmt.Errorf("failed iso file from resp: %w", err)
	}

	return nil
}

func (r *InstallationIso) renderIgnition(imageConfig *ibiconfig.ImageBasedInstallConfig) (*igntypes.Config, error) {
	config := &igntypes.Config{
		Ignition: igntypes.Ignition{
			Version: igntypes.MaxVersion.String(),
		},
		Passwd: igntypes.Passwd{
			Users: []igntypes.PasswdUser{
				{
					Name: "core",
					SSHAuthorizedKeys: []igntypes.SSHAuthorizedKey{
						igntypes.SSHAuthorizedKey(imageConfig.SSHKey),
					},
				},
			},
		},
	}

	setFileInIgnition(config, psIgnitioFilePath, imageConfig.PullSecret, 0o600)
	marshaled, err := json.Marshal(imageConfig.IBIPrepareConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to marshall ibi config data: %w", err)
	}

	setFileInIgnition(config, ibiConfigIgnitionFilePath, string(marshaled), 0o600)

	if len(imageConfig.ImageDigestSources) > 0 {
		registryConfContent, err := GenerateRegistryConf(imageConfig.ImageDigestSources)
		if err != nil {
			return nil, fmt.Errorf("failed to generate registry conf: %w", err)
		}
		setFileInIgnition(config, mirrorRegistryFilePath, registryConfContent, 0o600)
	}

	if err := r.setInstallationService(config, imageConfig); err != nil {
		return nil, fmt.Errorf("failed to add installation service into ignition: %w", err)
	}

	if imageConfig.AdditionalTrustBundle != "" {
		setFileInIgnition(config, trustedBundleFilePath, imageConfig.AdditionalTrustBundle, 0o600)
	}

	if imageConfig.NMStateConfig != "" {
		if err := r.nmstateConfig(config, imageConfig.NMStateConfig); err != nil {
			return nil, fmt.Errorf("failed to add nmstate config into ignition: %w", err)
		}
	}

	if imageConfig.IgnitionConfigOverride != "" {
		ignitionConfigOverride, _, err := v3_2.Parse([]byte(imageConfig.IgnitionConfigOverride))
		if err != nil {
			return nil, fmt.Errorf("failed to parse ignition config override: %w", err)
		}

		merged, _ := merge.MergeStructTranscribe(*config, ignitionConfigOverride)
		marshaledMerged, err := json.Marshal(merged)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal merged ignition config: %w", err)
		}
		if err := json.Unmarshal(marshaledMerged, config); err != nil {
			return nil, fmt.Errorf("failed to unmarshal merged ignition config: %w", err)
		}
	}

	return config, nil
}

func (r *InstallationIso) setInstallationService(config *igntypes.Config, imageConfig *ibiconfig.ImageBasedInstallConfig) error {
	installationScript, err := folder.ReadFile(seedInstallScriptFilePath)
	if err != nil {
		return fmt.Errorf("failed to read seed install script: %w", err)
	}
	setFileInIgnition(config, installationScriptPath, string(installationScript), 0o755)
	templateData := installServiceTemplate{
		SeedImage:            imageConfig.SeedImage,
		PullSecretPath:       psIgnitioFilePath,
		IBIConfigurationPath: ibiConfigIgnitionFilePath,
		HTTPProxy:            "",
		HTTPSProxy:           "",
		NoProxy:              "",
	}

	if imageConfig.Proxy != nil {
		templateData.HTTPProxy = imageConfig.Proxy.HTTPProxy
		templateData.HTTPSProxy = imageConfig.Proxy.HTTPSProxy
		templateData.NoProxy = imageConfig.Proxy.NoProxy
	}

	installationScriptService, err := folder.ReadFile(seedInstallationService)
	if err != nil {
		return fmt.Errorf("failed to read seed install script: %w", err)
	}
	installationServiceContent, err := utils.RenderTemplate(string(installationScriptService), templateData)
	if err != nil {
		return fmt.Errorf("failed to render installation service: %w", err)
	}
	setUnitInIgnition(config, "install-rhcos-and-restore-seed.service", string(installationServiceContent))
	return nil
}

func (r *InstallationIso) nmstateConfig(config *igntypes.Config, nmstateConfigContent string) error {
	setFileInIgnition(config, nmstateConfigFilePath, nmstateConfigContent, 0o600)

	networkService, err := folder.ReadFile(networkConfigServicePath)
	if err != nil {
		return fmt.Errorf("failed to read network service file: %w", err)
	}
	setUnitInIgnition(config, path.Base(networkConfigServicePath), string(networkService))
	return nil
}

func setFileInIgnition(config *igntypes.Config, filePath, fileContents string, mode int) {
	override := true
	fileContentsEncoded := "data:text/plain;charset=utf-8;base64," + base64.StdEncoding.EncodeToString([]byte(fileContents))
	file := igntypes.File{
		Node: igntypes.Node{
			Path:      filePath,
			Overwrite: &override,
			Group:     igntypes.NodeGroup{},
		},
		FileEmbedded1: igntypes.FileEmbedded1{
			Append: []igntypes.Resource{},
			Contents: igntypes.Resource{
				Source: &fileContentsEncoded,
			},
			Mode: &mode,
		},
	}
	config.Storage.Files = append(config.Storage.Files, file)
}

func setUnitInIgnition(config *igntypes.Config, name, contents string) {
	enabled := true
	newUnit := igntypes.Unit{
		Contents: &contents,
		Name:     name,
		Enabled:  &enabled,
	}
	config.Systemd.Units = append(config.Systemd.Units, newUnit)
}
