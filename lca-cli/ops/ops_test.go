package ops

import (
	"fmt"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
)

func newTestOps(t *testing.T) (*ops, *MockExecute) {
	ctrl := gomock.NewController(t)
	mockExec := NewMockExecute(ctrl)
	log := logrus.New()
	log.SetLevel(logrus.DebugLevel)
	o := &ops{log: log, hostCommandsExecutor: mockExec}
	return o, mockExec
}

func TestGetExtraPartitionPath(t *testing.T) {
	tests := []struct {
		name            string
		lsblkOutput     string
		partitionNumber uint
		expectedPath    string
		expectErr       bool
	}{
		{
			name: "valid partition path",
			lsblkOutput: `{
				"blockdevices": [{
					"name": "sda",
					"children": [
						{"name": "sda1", "path": "/dev/sda1"},
						{"name": "sda2", "path": "/dev/sda2"},
						{"name": "sda3", "path": "/dev/sda3"},
						{"name": "sda4", "path": "/dev/sda4"},
						{"name": "sda5", "path": "/dev/sda5"}
					]
				}]
			}`,
			partitionNumber: 5,
			expectedPath:    "/dev/sda5",
		},
		{
			name: "first partition",
			lsblkOutput: `{
				"blockdevices": [{
					"name": "vda",
					"children": [
						{"name": "vda1", "path": "/dev/vda1"}
					]
				}]
			}`,
			partitionNumber: 1,
			expectedPath:    "/dev/vda1",
		},
		{
			name: "partition number out of range",
			lsblkOutput: `{
				"blockdevices": [{
					"name": "sda",
					"children": [
						{"name": "sda1", "path": "/dev/sda1"}
					]
				}]
			}`,
			partitionNumber: 3,
			expectErr:       true,
		},
		{
			name: "partition number zero",
			lsblkOutput: `{
				"blockdevices": [{
					"name": "sda",
					"children": [
						{"name": "sda1", "path": "/dev/sda1"}
					]
				}]
			}`,
			partitionNumber: 0,
			expectErr:       true,
		},
		{
			name:            "no block devices",
			lsblkOutput:     `{"blockdevices": []}`,
			partitionNumber: 1,
			expectErr:       true,
		},
		{
			name:            "invalid json",
			lsblkOutput:     `not json`,
			partitionNumber: 1,
			expectErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, mockExec := newTestOps(t)

			mockExec.EXPECT().
				Execute("lsblk", "/dev/sda", "--json", "--output", "NAME,PATH").
				Return(tt.lsblkOutput, nil)

			path, err := o.getExtraPartitionPath("/dev/sda", tt.partitionNumber)
			if tt.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectedPath, path)
			}
		})
	}
}

func TestGetExtraPartitionPath_LsblkFails(t *testing.T) {
	o, mockExec := newTestOps(t)

	mockExec.EXPECT().
		Execute("lsblk", "/dev/sda", "--json", "--output", "NAME,PATH").
		Return("", fmt.Errorf("command not found"))

	_, err := o.getExtraPartitionPath("/dev/sda", 1)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to run lsblk")
}

func expectKubeletStop(mockExec *MockExecute) {
	mockExec.EXPECT().Execute("systemctl", "stop", "kubelet.service").Return("", nil)
	mockExec.EXPECT().Execute("systemctl", "disable", "kubelet.service").Return("", nil)
}

func TestStopClusterServices_CrictlLoop(t *testing.T) {
	o, mockExec := newTestOps(t)

	expectKubeletStop(mockExec)
	mockExec.EXPECT().Execute("systemctl", "is-active", "crio").Return("active", nil)
	mockExec.EXPECT().Execute("crictl", "ps", "-q").Return("abc123\ndef456\nghi789", nil)
	mockExec.EXPECT().Execute("crictl", "stop", "--timeout", "5", gomock.Any()).Return("", nil).Times(3)
	mockExec.EXPECT().Execute("systemctl", "stop", "crio.service").Return("", nil)

	assert.NoError(t, o.StopClusterServices())
}

func TestStopClusterServices_NoContainers(t *testing.T) {
	o, mockExec := newTestOps(t)

	expectKubeletStop(mockExec)
	mockExec.EXPECT().Execute("systemctl", "is-active", "crio").Return("active", nil)
	mockExec.EXPECT().Execute("crictl", "ps", "-q").Return("", nil)
	mockExec.EXPECT().Execute("systemctl", "stop", "crio.service").Return("", nil)

	assert.NoError(t, o.StopClusterServices())
}

func TestStopClusterServices_CrioInactive(t *testing.T) {
	o, mockExec := newTestOps(t)

	expectKubeletStop(mockExec)
	mockExec.EXPECT().Execute("systemctl", "is-active", "crio").Return("inactive", nil)

	assert.NoError(t, o.StopClusterServices())
}
