package prep

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
)

func TestSetupStaterootRejectsCorruptExtractedRepo(t *testing.T) {
	originalMkdirTemp := osMkdirTemp
	osMkdirTemp = func(_ string, pattern string) (string, error) {
		return os.MkdirTemp(t.TempDir(), pattern)
	}
	t.Cleanup(func() { osMkdirTemp = originalMkdirTemp })

	ctrl := gomock.NewController(t)
	operations := ops.NewMockOps(ctrl)
	client := ostreeclient.NewMockIClient(ctrl)
	seedImage := "example.com/seed:latest"

	operations.EXPECT().MountImage(seedImage).Return("/seed", nil)
	operations.EXPECT().ExtractTarWithSELinux("/seed/ostree.tgz", gomock.Any()).Return(nil)
	client.EXPECT().VerifyRepo(gomock.Any()).Return(errors.New("object checksum mismatch"))
	operations.EXPECT().UnmountAndRemoveImage(seedImage).Return(nil)

	err := SetupStateroot(logr.Discard(), operations, client, nil, seedImage, "4.22.16", true)
	assert.ErrorContains(t, err, "seed OSTree repository is corrupt after extraction: object checksum mismatch")
}

func TestVerifySeedOSTreeRejectsCorruptionBeforeDiskPreparation(t *testing.T) {
	originalMkdirTemp := osMkdirTemp
	osMkdirTemp = func(_ string, pattern string) (string, error) {
		return os.MkdirTemp(t.TempDir(), pattern)
	}
	t.Cleanup(func() { osMkdirTemp = originalMkdirTemp })

	ctrl := gomock.NewController(t)
	operations := ops.NewMockOps(ctrl)
	client := ostreeclient.NewMockIClient(ctrl)
	seedImage := "example.com/seed:latest"
	operations.EXPECT().MountImage(seedImage).Return("/seed", nil)
	operations.EXPECT().ExtractTarWithSELinux("/seed/ostree.tgz", gomock.Any()).Return(nil)
	client.EXPECT().VerifyRepo(gomock.Any()).Return(errors.New("object checksum mismatch"))
	operations.EXPECT().UnmountAndRemoveImage(seedImage).Return(nil)

	err := VerifySeedOSTree(logr.Discard(), operations, client, seedImage)
	assert.ErrorContains(t, err, "seed OSTree repository is corrupt before disk preparation: object checksum mismatch")
}

func TestGetDeploymentFromDeploymentID(t *testing.T) {
	testcases := []struct {
		name         string
		deploymentID string
		expect       string
		err          error
	}{
		{
			name:         "normal version",
			deploymentID: "rhcos-ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1",
			expect:       "ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1",
			err:          nil,
		},
		{
			name:         "multiple dashes",
			deploymentID: "4.15.0-0.nightly-2023-12-04-223539-ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1",
			expect:       "ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1",
			err:          nil,
		},
		{
			name:         "no dashes",
			deploymentID: "ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1",
			expect:       "",
			err: fmt.Errorf(
				"failed to get deployment from deploymentID, there should be a '-' in deployment"),
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := getDeploymentFromDeploymentID(tc.deploymentID)
			assert.Equal(t, tc.expect, res)
			if tc.err != nil {
				assert.ErrorContains(t, err, tc.err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
