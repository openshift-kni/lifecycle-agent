package installationiso

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

func TestInstallationIso(t *testing.T) {
	var ()

	testcases := []struct {
		name                string
		workDirExists       bool
		authFileExists      bool
		pullSecretExists    bool
		sshPublicKeyExists  bool
		liveIsoUrlSuccess   bool
		renderCommandReturn error
		embedCommandReturn  error
		expectedError       string
	}{
		{
			name:               "Happy flow",
			workDirExists:      true,
			authFileExists:     true,
			pullSecretExists:   true,
			sshPublicKeyExists: true,
			liveIsoUrlSuccess:  true,
			expectedError:      "",
		},
		{
			name:               "missing workdir",
			workDirExists:      false,
			authFileExists:     false,
			pullSecretExists:   false,
			sshPublicKeyExists: false,
			liveIsoUrlSuccess:  false,
			expectedError:      "work dir doesn't exists",
		},
		{
			name:               "missing authFile",
			workDirExists:      true,
			authFileExists:     false,
			pullSecretExists:   true,
			sshPublicKeyExists: true,
			liveIsoUrlSuccess:  true,
			expectedError:      "authFile: no such file or directory",
		},
		{
			name:               "missing psFile",
			workDirExists:      true,
			authFileExists:     true,
			pullSecretExists:   false,
			sshPublicKeyExists: true,
			liveIsoUrlSuccess:  true,
			expectedError:      "psFile: no such file or directory",
		},
		{
			name:               "missing ssh key",
			workDirExists:      true,
			authFileExists:     true,
			pullSecretExists:   true,
			sshPublicKeyExists: false,
			liveIsoUrlSuccess:  true,
			expectedError:      "sshKey: no such file or directory",
		},
		{
			name:               "Failed to download rhcos",
			workDirExists:      true,
			authFileExists:     true,
			pullSecretExists:   true,
			sshPublicKeyExists: true,
			liveIsoUrlSuccess:  false,
			expectedError:      "notfound",
		},
		{
			name:                "Render failure",
			workDirExists:       true,
			authFileExists:      true,
			pullSecretExists:    true,
			sshPublicKeyExists:  true,
			liveIsoUrlSuccess:   false,
			renderCommandReturn: errors.New("failed to render ignition config"),
			expectedError:       "failed to render ignition config",
		},
		{
			name:                "embed failure",
			workDirExists:       true,
			authFileExists:      true,
			pullSecretExists:    true,
			sshPublicKeyExists:  true,
			liveIsoUrlSuccess:   false,
			renderCommandReturn: errors.New("failed to embed ignition config to ISO"),
			expectedError:       "failed to embed ignition config to ISO",
		},
	}
	var (
		mockController   = gomock.NewController(t)
		mockOps          = ops.NewMockOps(mockController)
		seedImage        = "seedImage"
		seedVersion      = "seedVersion"
		lcaImage         = "lcaImage"
		installationDisk = "/dev/sda"
	)

	for _, tc := range testcases {
		tmpDir := "noSuchDir"
		if tc.workDirExists {
			tmpDir = t.TempDir()
		}
		t.Run(tc.name, func(t *testing.T) {
			log := &logrus.Logger{}
			sshPublicKeyPath := "sshKey"
			if tc.sshPublicKeyExists {
				sshPublicKey, err := os.Create(path.Join(tmpDir, sshPublicKeyPath))
				assert.Equal(t, err, nil)
				sshPublicKeyPath = sshPublicKey.Name()
			}
			authFilePath := "authFile"
			if tc.authFileExists {
				authFile, err := os.Create(path.Join(tmpDir, authFilePath))
				assert.Equal(t, err, nil)
				authFilePath = authFile.Name()
			}
			psFilePath := "psFile"
			if tc.pullSecretExists {
				psFile, err := os.Create(path.Join(tmpDir, psFilePath))
				assert.Equal(t, err, nil)
				psFilePath = psFile.Name()
			}
			if tc.pullSecretExists && tc.authFileExists && tc.sshPublicKeyExists {
				mockOps.EXPECT().RunInHostNamespace("podman", "run",
					"-v", fmt.Sprintf("%s:/data:rw,Z", tmpDir),
					"--rm",
					"quay.io/coreos/butane:release",
					"--pretty", "--strict",
					"-d", "/data",
					path.Join("/data", butaneConfigFile)).Return("", tc.renderCommandReturn).Times(1)
				if tc.liveIsoUrlSuccess {
					mockOps.EXPECT().RunInHostNamespace("podman", "run",
						"-v", fmt.Sprintf("%s:/data:rw,Z", tmpDir),
						coreosInstallerImage,
						"iso", "ignition", "embed",
						"-i", path.Join("/data", ibiIgnitionFileName),
						"-o", path.Join("/data", ibiIsoFileName),
						path.Join("/data", rhcosIsoFileName)).Return("", tc.embedCommandReturn).Times(1)
				}
			}
			rhcosLiveIsoUrl := "notfound"
			if tc.liveIsoUrlSuccess {
				server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
					rw.Write([]byte(`rhcos-live-iso`))
				}))
				rhcosLiveIsoUrl = server.URL
				defer server.Close()
			}
			installationIso := NewInstallationIso(log, mockOps, tmpDir)
			err := installationIso.Create(seedImage, seedVersion, authFilePath, psFilePath, sshPublicKeyPath, lcaImage, rhcosLiveIsoUrl, installationDisk)
			if tc.expectedError == "" {
				assert.Equal(t, err, nil)
			} else {
				assert.Contains(t, err.Error(), tc.expectedError)
			}
		})
	}
}
