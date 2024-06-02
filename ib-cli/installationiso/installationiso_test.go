package installationiso

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	igntypes "github.com/coreos/ignition/v2/config/v3_2/types"
	"github.com/openshift-kni/lifecycle-agent/api/ibiconfig"
	"github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/utils"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

const mirrorRegistryTestContent = `
unqualified-search-registries = ["registry.access.redhat.com", "docker.io"]
[[registry]]
prefix = ""
location = "quay.io/openshift-release-dev/ocp-release"
mirror-by-digest-only = true
[[registry.mirror]]
location = "<mirror_host_name>:<mirror_host_port>/ocp4/openshift4"
[[registry]]
prefix = ""
location = "quay.io/openshift-release-dev/ocp-v4.0-art-dev"
mirror-by-digest-only = true
[[registry.mirror]]
location = "<mirror_host_name>:<mirror_host_port>/ocp4/openshift4"
`

func findFileInIgnition(t *testing.T, config igntypes.Config, filePath string) *igntypes.File {
	for _, file := range config.Storage.Files {
		if file.Node.Path == filePath {
			encodedData := strings.Replace(*file.Contents.Source, "data:text/plain;charset=utf-8;base64,", "", -1)
			decoded, err := base64.StdEncoding.DecodeString(encodedData)
			assert.Nil(t, err)
			*file.Contents.Source = string(decoded)
			return &file
		}
	}
	return nil
}

func findServiceInIgnition(config igntypes.Config, serviceName string) *igntypes.Unit {
	for _, unit := range config.Systemd.Units {
		if unit.Name == serviceName {
			return &unit
		}
	}
	return nil
}

func TestInstallationIso(t *testing.T) {
	var ()

	testcases := []struct {
		name               string
		workDirExists      bool
		sshPublicKeyExists bool
		liveIsoUrlSuccess  bool
		precacheBestEffort bool
		precacheDisabled   bool
		shutdown           bool
		skipDiskCleanup    bool
		addTrustedBundle   bool
		nmstateConfig      bool
		proxy              *seedreconfig.Proxy
		ids                []ibiconfig.ImageDigestSource
		embedCommandReturn error
		expectedError      string
	}{
		{
			name:               "Happy flow",
			workDirExists:      true,
			sshPublicKeyExists: true,
			liveIsoUrlSuccess:  true,
			precacheBestEffort: false,
			precacheDisabled:   false,
			shutdown:           false,
			skipDiskCleanup:    false,
			expectedError:      "",
		},
		{
			name:               "Happy flow with proxy",
			workDirExists:      true,
			sshPublicKeyExists: true,
			liveIsoUrlSuccess:  true,
			precacheBestEffort: false,
			precacheDisabled:   false,
			shutdown:           false,
			skipDiskCleanup:    false,
			expectedError:      "",
			proxy:              &seedreconfig.Proxy{NoProxy: "noProxy", HTTPSProxy: "httpsProxy", HTTPProxy: "httpProxy"},
		},
		{
			name:               "Happy flow - precache best-effort set",
			workDirExists:      true,
			sshPublicKeyExists: true,
			liveIsoUrlSuccess:  true,
			precacheBestEffort: true,
			precacheDisabled:   false,
			shutdown:           false,
			skipDiskCleanup:    false,
			expectedError:      "",
		},
		{
			name:               "Happy flow - precache disabled set",
			workDirExists:      true,
			sshPublicKeyExists: true,
			liveIsoUrlSuccess:  true,
			precacheBestEffort: false,
			precacheDisabled:   true,
			shutdown:           false,
			skipDiskCleanup:    false,
			expectedError:      "",
		},
		{
			name:               "Happy flow - shutdown set",
			workDirExists:      true,
			sshPublicKeyExists: true,
			liveIsoUrlSuccess:  true,
			precacheBestEffort: false,
			precacheDisabled:   false,
			shutdown:           true,
			skipDiskCleanup:    false,
			expectedError:      "",
		},
		{
			name:               "Happy flow - skipDiskCleanup set",
			workDirExists:      true,
			sshPublicKeyExists: true,
			liveIsoUrlSuccess:  true,
			precacheBestEffort: false,
			precacheDisabled:   false,
			shutdown:           false,
			skipDiskCleanup:    true,
			expectedError:      "",
		},
		{
			name:               "Happy flow with additional trusted bundle",
			workDirExists:      true,
			sshPublicKeyExists: true,
			liveIsoUrlSuccess:  true,
			precacheBestEffort: false,
			precacheDisabled:   false,
			shutdown:           false,
			skipDiskCleanup:    true,
			addTrustedBundle:   true,
			expectedError:      "",
		},
		{
			name:               "Happy flow with nmstate config",
			workDirExists:      true,
			sshPublicKeyExists: true,
			liveIsoUrlSuccess:  true,
			precacheBestEffort: false,
			precacheDisabled:   false,
			shutdown:           false,
			skipDiskCleanup:    true,
			nmstateConfig:      true,
			expectedError:      "",
		},
		{
			name:               "Happy flow with ids",
			workDirExists:      true,
			sshPublicKeyExists: true,
			liveIsoUrlSuccess:  true,
			precacheBestEffort: false,
			precacheDisabled:   false,
			shutdown:           false,
			skipDiskCleanup:    true,
			nmstateConfig:      false,
			ids: []ibiconfig.ImageDigestSource{{Source: "quay.io/openshift-release-dev/ocp-release", Mirrors: []string{"f04-h09-000-r640.rdu2.scalelab.redhat.com:5000/localimages/local-release-image"}},
				{Source: "quay.io/openshift-release-dev/ocp-v4.0-art-dev", Mirrors: []string{"f04-h09-000-r640.rdu2.scalelab.redhat.com:5000/localimages/local-release-image"}}},
			expectedError: "",
		},
		{
			name:               "missing workdir",
			workDirExists:      false,
			sshPublicKeyExists: false,
			liveIsoUrlSuccess:  false,
			precacheBestEffort: false,
			precacheDisabled:   false,
			shutdown:           false,
			skipDiskCleanup:    false,
			expectedError:      "work dir doesn't exists",
		},
		{
			name:               "Failed to download rhcos",
			workDirExists:      true,
			sshPublicKeyExists: true,
			liveIsoUrlSuccess:  false,
			precacheBestEffort: false,
			precacheDisabled:   false,
			shutdown:           false,
			skipDiskCleanup:    false,
			expectedError:      "notfound",
		},
		{
			name:               "embed failure",
			workDirExists:      true,
			sshPublicKeyExists: true,
			liveIsoUrlSuccess:  true,
			precacheBestEffort: false,
			precacheDisabled:   false,
			shutdown:           false,
			skipDiskCleanup:    false,
			embedCommandReturn: fmt.Errorf("failed to embed ignition config to ISO"),
			expectedError:      "failed to embed ignition config to ISO",
		},
	}
	var (
		mockController      = gomock.NewController(t)
		mockOps             = ops.NewMockOps(mockController)
		seedImage           = "seedImage"
		seedVersion         = "seedVersion"
		installationDisk    = "/dev/sda"
		extraPartitionStart = "-40G"
	)

	for _, tc := range testcases {
		tmpDir := "noSuchDir"
		if tc.workDirExists {
			tmpDir = t.TempDir()
		}
		t.Run(tc.name, func(t *testing.T) {
			log := &logrus.Logger{}

			if tc.liveIsoUrlSuccess {
				mockOps.EXPECT().RunInHostNamespace("podman", "run",
					"-v", fmt.Sprintf("%s:/data:rw,Z", tmpDir),
					coreosInstallerImage,
					"iso", "ignition", "embed",
					"-i", path.Join("/data", ibiIgnitionFileName),
					"-o", path.Join("/data", ibiIsoFileName),
					path.Join("/data", rhcosIsoFileName)).Return("", tc.embedCommandReturn).Times(1)
			}
			rhcosLiveIsoUrl := "notfound"
			if tc.liveIsoUrlSuccess {
				server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
					rw.Write([]byte(`rhcos-live-iso`))
				}))
				rhcosLiveIsoUrl = server.URL
				defer server.Close()
			}
			isoConfig := &ibiconfig.ImageBasedInstallConfig{
				PullSecret:   common.PullSecretEmptyData,
				SSHKey:       "sshKey",
				RHCOSLiveISO: rhcosLiveIsoUrl,
				Proxy:        tc.proxy,
				IBIPrepareConfig: ibiconfig.IBIPrepareConfig{
					SeedImage:           seedImage,
					PrecacheDisabled:    tc.precacheDisabled,
					PrecacheBestEffort:  tc.precacheBestEffort,
					Shutdown:            tc.shutdown,
					SkipDiskCleanup:     tc.skipDiskCleanup,
					SeedVersion:         seedVersion,
					InstallationDisk:    installationDisk,
					ExtraPartitionStart: extraPartitionStart,
				},
			}
			if tc.addTrustedBundle {
				isoConfig.AdditionalTrustBundle = "trustedBundle"
			}
			if tc.nmstateConfig {
				isoConfig.NMStateConfig = "nmstateConfig"
			}
			if len(tc.ids) > 0 {
				isoConfig.ImageDigestSources = tc.ids
			}

			installationIso := NewInstallationIso(log, mockOps, tmpDir)
			err := installationIso.Create(isoConfig)
			if tc.expectedError == "" {
				assert.Equal(t, err, nil)
				var config igntypes.Config
				path.Join(tmpDir, ibiIgnitionFileName)
				errReading := utils.ReadYamlOrJSONFile(path.Join(tmpDir, ibiIgnitionFileName), &config)
				ibiConfigFile := findFileInIgnition(t, config, ibiConfigIgnitionFilePath)
				assert.NotNil(t, ibiConfigFile)
				var ibiConfig ibiconfig.ImageBasedInstallConfig
				assert.NotNil(t, ibiConfigFile.Contents)
				assert.NotNil(t, ibiConfigFile.Contents.Source)
				err = json.Unmarshal([]byte(*ibiConfigFile.Contents.Source), &ibiConfig)
				assert.Nil(t, err)

				assert.Equal(t, errReading, nil)
				assert.Equal(t, ibiConfig.PrecacheBestEffort, tc.precacheBestEffort)
				assert.Equal(t, ibiConfig.Shutdown, tc.shutdown)
				assert.Equal(t, ibiConfig.SkipDiskCleanup, tc.skipDiskCleanup)
				assert.Equal(t, ibiConfig.SeedImage, seedImage)
				assert.Equal(t, ibiConfig.SeedVersion, seedVersion)
				assert.Equal(t, ibiConfig.InstallationDisk, installationDisk)
				assert.Equal(t, ibiConfig.ExtraPartitionStart, extraPartitionStart)

				installationScriptFIle := findFileInIgnition(t, config, installationScriptPath)
				assert.NotNil(t, installationScriptFIle)
				assert.NotNil(t, installationScriptFIle.Contents)
				assert.NotNil(t, installationScriptFIle.Contents.Source)

				installationService := findServiceInIgnition(config, "install-rhcos-and-restore-seed.service")
				assert.NotNil(t, installationService)
				assert.NotNil(t, installationService.Contents)
				assert.Contains(t, *installationService.Contents, path.Base(seedInstallScriptFilePath))
				if tc.proxy == nil {
					tc.proxy = &seedreconfig.Proxy{
						HTTPProxy:  "",
						HTTPSProxy: "",
						NoProxy:    "",
					}
				}
				assert.Contains(t, *installationService.Contents, fmt.Sprintf("PULL_SECRET_FILE=%s", psIgnitioFilePath))
				assert.Contains(t, *installationService.Contents, fmt.Sprintf("HTTP_PROXY=%s", tc.proxy.HTTPProxy))
				assert.Contains(t, *installationService.Contents, fmt.Sprintf("HTTPS_PROXY=%s", tc.proxy.HTTPSProxy))
				assert.Contains(t, *installationService.Contents, fmt.Sprintf("NO_PROXY=%s", tc.proxy.NoProxy))

				if tc.addTrustedBundle {
					trustedBundleFile := findFileInIgnition(t, config, trustedBundleFilePath)
					assert.NotNil(t, trustedBundleFile)
					assert.NotNil(t, trustedBundleFile.Contents)
					assert.NotNil(t, trustedBundleFile.Contents.Source)
					assert.Equal(t, *trustedBundleFile.Contents.Source, isoConfig.AdditionalTrustBundle, true)
				} else {
					trustedBundleFile := findFileInIgnition(t, config, trustedBundleFilePath)
					assert.Nil(t, trustedBundleFile)
				}

				if tc.nmstateConfig {
					nmstateConfig := findFileInIgnition(t, config, nmstateConfigFilePath)
					assert.NotNil(t, nmstateConfig)
					assert.NotNil(t, nmstateConfig.Contents)
					assert.NotNil(t, nmstateConfig.Contents.Source)
					assert.Equal(t, *nmstateConfig.Contents.Source, isoConfig.NMStateConfig, true)

					networkService := findServiceInIgnition(config, path.Base(networkConfigServicePath))
					assert.NotNil(t, networkService)
					assert.NotNil(t, networkService.Contents)
					assert.Contains(t, *networkService.Contents, "nmstatectl")
					assert.Contains(t, *networkService.Contents, nmstateConfigFilePath)

				} else {
					nmstateConfig := findFileInIgnition(t, config, nmstateConfigFilePath)
					assert.Nil(t, nmstateConfig)

					networkService := findServiceInIgnition(config, path.Base(networkConfigServicePath))
					assert.Nil(t, networkService)
				}
				if len(tc.ids) > 0 {
					registries := findFileInIgnition(t, config, mirrorRegistryFilePath)
					assert.NotNil(t, registries)
					assert.NotNil(t, registries.Contents)
					assert.NotNil(t, registries.Contents.Source)
					for _, mirror := range tc.ids {
						assert.Contains(t, *registries.Contents.Source, mirror.Source)
						assert.Contains(t, *registries.Contents.Source, mirror.Mirrors[0])
					}
				} else {
					registries := findFileInIgnition(t, config, mirrorRegistryFilePath)
					assert.Nil(t, registries)
				}

			} else {
				assert.Contains(t, err.Error(), tc.expectedError)
			}

		})
	}
}
