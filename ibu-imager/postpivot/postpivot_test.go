package postpivot

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	clusterconfig_api "github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
)

func TestSetNodeIPIfNotProvided(t *testing.T) {
	createNodeIpFile := func(t *testing.T, ipFile string, ipToSet string) {
		f, err := os.Create(ipFile)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		defer f.Close()
		if _, err = f.WriteString(ipToSet); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}

	var (
		mockController = gomock.NewController(t)
		mockOps        = ops.NewMockOps(mockController)
	)

	defer func() {
		mockController.Finish()
	}()

	testcases := []struct {
		name             string
		expectedError    bool
		nodeipFileExists bool
		ipProvided       bool
		ipToSet          string
	}{
		{
			name:             "Ip provided nothing to do",
			expectedError:    false,
			nodeipFileExists: true,
			ipProvided:       true,
			ipToSet:          "192.167.1.2",
		},
		{
			name:             "Ip is not provided, bad ip in file",
			expectedError:    true,
			nodeipFileExists: true,
			ipProvided:       false,
			ipToSet:          "bad ip",
		},
		{
			name:             "Ip is not provided, happy flow",
			expectedError:    false,
			nodeipFileExists: true,
			ipProvided:       false,
			ipToSet:          "192.167.1.2",
		},
	}

	for _, tc := range testcases {
		tmpDir := t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			log := &logrus.Logger{}
			pp := NewPostPivot(nil, log, mockOps, "", "", "")
			seedReconfig := &clusterconfig_api.SeedReconfiguration{}
			if tc.ipProvided {
				seedReconfig.NodeIP = tc.ipToSet
			}
			ipFile := path.Join(tmpDir, "primary")
			if tc.nodeipFileExists {
				createNodeIpFile(t, ipFile, tc.ipToSet)
			} else {
				mockOps.EXPECT().SystemctlAction("start", "nodeip-configuration").Return("", nil).Do(func(any) {
					createNodeIpFile(t, ipFile, tc.ipToSet)
				})
			}

			err := pp.setNodeIPIfNotProvided(context.TODO(), seedReconfig, ipFile)
			if !tc.expectedError && err != nil {
				t.Errorf("unexpected error: %v", err)
			} else {
				assert.Equal(t, err != nil, tc.expectedError)
			}
		})
	}
}

func TestSetDnsMasqConfiguration(t *testing.T) {
	var (
		mockController = gomock.NewController(t)
		mockOps        = ops.NewMockOps(mockController)
	)

	defer func() {
		mockController.Finish()
	}()

	testcases := []struct {
		name                string
		expectedError       bool
		seedReconfiguration *clusterconfig_api.SeedReconfiguration
	}{
		{
			name: "Happy flow",
			seedReconfiguration: &clusterconfig_api.SeedReconfiguration{
				BaseDomain:  "new.com",
				ClusterName: "new_name",
				NodeIP:      "192.167.127.10",
			},
			expectedError: false,
		},
	}

	for _, tc := range testcases {
		tmpDir := t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			log := &logrus.Logger{}
			pp := NewPostPivot(nil, log, mockOps, "", "", "")
			mockOps.EXPECT().SystemctlAction("restart", "NetworkManager.service").Return("", nil)
			mockOps.EXPECT().SystemctlAction("restart", "dnsmasq.service").Return("", nil)
			confFile := path.Join(tmpDir, "override")
			err := pp.setDnsMasqConfiguration(tc.seedReconfiguration, confFile)
			if !tc.expectedError && err != nil {
				t.Errorf("unexpected error: %v", err)
			} else {
				assert.Equal(t, err != nil, tc.expectedError)
			}
			if !tc.expectedError {
				data, errF := os.ReadFile(confFile)
				if errF != nil {
					t.Errorf("unexpected error: %v", err)
				}
				lines := strings.Split(string(data), "\n")
				assert.Equal(t, len(lines), 3)
				assert.Equal(t, lines[0], fmt.Sprintf("SNO_CLUSTER_NAME_OVERRIDE=%s", tc.seedReconfiguration.ClusterName))
				assert.Equal(t, lines[1], fmt.Sprintf("SNO_BASE_DOMAIN_OVERRIDE=%s", tc.seedReconfiguration.BaseDomain))
				assert.Equal(t, lines[2], fmt.Sprintf("SNO_DNSMASQ_IP_OVERRIDE=%s", tc.seedReconfiguration.NodeIP))
			}
		})
	}
}
