package ops

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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

func newTestOpsForEtcd(t *testing.T, timeout time.Duration) *ops {
	t.Helper()
	log := logrus.New()
	log.SetLevel(logrus.DebugLevel)
	return &ops{log: log, etcdTimeout: timeout}
}

func TestWaitForEtcd_ImmediateSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	o := newTestOpsForEtcd(t, 5*time.Second)
	err := o.waitForEtcd(context.Background(), srv.URL)
	assert.NoError(t, err)
}

func TestWaitForEtcd_SucceedsAfterRetries(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	o := newTestOpsForEtcd(t, 5*time.Second)
	err := o.waitForEtcd(context.Background(), srv.URL)
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, count.Load(), int32(3))
}

func TestWaitForEtcd_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	o := newTestOpsForEtcd(t, 2*time.Second)
	err := o.waitForEtcd(context.Background(), srv.URL)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "timeout waiting for etcd")
}

func TestWaitForEtcd_ConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	url := srv.URL
	srv.Close()

	o := newTestOpsForEtcd(t, 2*time.Second)
	err := o.waitForEtcd(context.Background(), url)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "timeout waiting for etcd")
}

func TestWaitForEtcd_DefaultTimeout(t *testing.T) {
	// etcdTimeout == 0 should fall back to defaultEtcdHealthTimeout.
	// We verify that the function still works (returns on first OK)
	// without explicitly setting etcdTimeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	log := logrus.New()
	log.SetLevel(logrus.DebugLevel)
	o := &ops{log: log} // etcdTimeout is zero-value
	assert.Equal(t, time.Duration(0), o.etcdTimeout, "precondition: etcdTimeout should be zero")

	err := o.waitForEtcd(context.Background(), srv.URL)
	assert.NoError(t, err)
}

func TestWaitForEtcd_ResponseBodyDrained(t *testing.T) {
	// Verify the function works correctly when the server returns a
	// response body. This exercises the io.Copy(io.Discard) + Body.Close()
	// path that is the core fix for the resource leak.
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"health":"false","reason":"ALARM NOSPACE"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"health":"true"}`)
	}))
	defer srv.Close()

	o := newTestOpsForEtcd(t, 5*time.Second)
	err := o.waitForEtcd(context.Background(), srv.URL)
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, count.Load(), int32(3))
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
		{
			name: "path with whitespace is trimmed",
			lsblkOutput: `{
				"blockdevices": [{
					"name": "sda",
					"children": [
						{"name": "sda1", "path": "  /dev/sda1  "}
					]
				}]
			}`,
			partitionNumber: 1,
			expectedPath:    "/dev/sda1",
		},
		{
			name: "device with empty children array",
			lsblkOutput: `{
				"blockdevices": [{
					"name": "sda",
					"children": []
				}]
			}`,
			partitionNumber: 1,
			expectErr:       true,
		},
		{
			name: "device with no children field",
			lsblkOutput: `{
				"blockdevices": [{
					"name": "sda"
				}]
			}`,
			partitionNumber: 1,
			expectErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, mockExec := newTestOps(t)

			mockExec.EXPECT().
				Execute(gomock.Any(), "lsblk", "/dev/sda", "--json", "--output", "NAME,PATH").
				Return(tt.lsblkOutput, nil)

			path, err := o.getExtraPartitionPath(context.Background(), "/dev/sda", tt.partitionNumber)
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
		Execute(gomock.Any(), "lsblk", "/dev/sda", "--json", "--output", "NAME,PATH").
		Return("", fmt.Errorf("command not found"))

	_, err := o.getExtraPartitionPath(context.Background(), "/dev/sda", 1)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to run lsblk")
}

func expectKubeletStop(mockExec *MockExecute) {
	mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "stop", "kubelet.service").Return("", nil)
	mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "disable", "kubelet.service").Return("", nil)
}

func expectPodSandboxRemoval(mockExec *MockExecute) {
	mockExec.EXPECT().Execute(gomock.Any(), "bash", "-c", gomock.Any()).Return("", nil).AnyTimes()
}

func TestStopClusterServices_CrictlLoop(t *testing.T) {
	o, mockExec := newTestOps(t)

	expectKubeletStop(mockExec)
	mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "is-active", "crio").Return("active", nil)
	mockExec.EXPECT().Execute(gomock.Any(), "crictl", "ps", "-q").Return("abc123\ndef456\nghi789", nil)
	mockExec.EXPECT().Execute(gomock.Any(), "crictl", "stop", "--timeout", "5", gomock.Any()).Return("", nil).Times(3)
	expectPodSandboxRemoval(mockExec)
	mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "stop", "crio.service").Return("", nil)

	assert.NoError(t, o.StopClusterServices(context.Background()))
}

func TestStopClusterServices_NoContainers(t *testing.T) {
	o, mockExec := newTestOps(t)

	expectKubeletStop(mockExec)
	mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "is-active", "crio").Return("active", nil)
	mockExec.EXPECT().Execute(gomock.Any(), "crictl", "ps", "-q").Return("", nil)
	expectPodSandboxRemoval(mockExec)
	mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "stop", "crio.service").Return("", nil)

	assert.NoError(t, o.StopClusterServices(context.Background()))
}

func TestStopClusterServices_CrioInactive(t *testing.T) {
	o, mockExec := newTestOps(t)

	expectKubeletStop(mockExec)
	mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "is-active", "crio").Return("inactive", nil)

	assert.NoError(t, o.StopClusterServices(context.Background()))
}

func TestStopClusterServices_ContainerStopFails(t *testing.T) {
	o, mockExec := newTestOps(t)

	expectKubeletStop(mockExec)
	mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "is-active", "crio").Return("active", nil)
	mockExec.EXPECT().Execute(gomock.Any(), "crictl", "ps", "-q").Return("abc123\ndef456", nil)
	mockExec.EXPECT().Execute(gomock.Any(), "crictl", "stop", "--timeout", "5", gomock.Any()).
		Return("", fmt.Errorf("container not responding")).AnyTimes()
	expectPodSandboxRemoval(mockExec)
	mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "stop", "crio.service").Return("", nil)

	assert.NoError(t, o.StopClusterServices(context.Background()))
}

func TestStopClusterServices_CrictlListFails(t *testing.T) {
	o, mockExec := newTestOps(t)

	expectKubeletStop(mockExec)
	mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "is-active", "crio").Return("active", nil)
	mockExec.EXPECT().Execute(gomock.Any(), "crictl", "ps", "-q").Return("", fmt.Errorf("crictl unavailable")).AnyTimes()
	expectPodSandboxRemoval(mockExec)
	mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "stop", "crio.service").Return("", nil)

	assert.NoError(t, o.StopClusterServices(context.Background()))
}

func TestStopClusterServices_SingleContainer(t *testing.T) {
	o, mockExec := newTestOps(t)

	expectKubeletStop(mockExec)
	mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "is-active", "crio").Return("active", nil)
	mockExec.EXPECT().Execute(gomock.Any(), "crictl", "ps", "-q").Return("only1", nil)
	mockExec.EXPECT().Execute(gomock.Any(), "crictl", "stop", "--timeout", "5", "only1").Return("", nil)
	expectPodSandboxRemoval(mockExec)
	mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "stop", "crio.service").Return("", nil)

	assert.NoError(t, o.StopClusterServices(context.Background()))
}

func TestStopClusterServices_WhitespaceOnlyOutput(t *testing.T) {
	o, mockExec := newTestOps(t)

	expectKubeletStop(mockExec)
	mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "is-active", "crio").Return("active", nil)
	mockExec.EXPECT().Execute(gomock.Any(), "crictl", "ps", "-q").Return("\n\n\n", nil)
	expectPodSandboxRemoval(mockExec)
	mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "stop", "crio.service").Return("", nil)

	assert.NoError(t, o.StopClusterServices(context.Background()))
}

func TestStopClusterServices_MixedContainerStopResults(t *testing.T) {
	o, mockExec := newTestOps(t)

	expectKubeletStop(mockExec)
	mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "is-active", "crio").Return("active", nil)
	mockExec.EXPECT().Execute(gomock.Any(), "crictl", "ps", "-q").Return("aaa\nbbb\nccc", nil)
	// Use DoAndReturn to simulate mixed results: some succeed, some fail
	mockExec.EXPECT().Execute(gomock.Any(), "crictl", "stop", "--timeout", "5", gomock.Any()).
		DoAndReturn(func(_ context.Context, cmd string, args ...string) (string, error) {
			containerID := args[len(args)-1]
			if containerID == "bbb" {
				return "", fmt.Errorf("timeout stopping container bbb")
			}
			return "", nil
		}).Times(3)
	expectPodSandboxRemoval(mockExec)
	mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "stop", "crio.service").Return("", nil)

	// StopClusterServices should succeed overall (PollUntilContextCancel error is ignored)
	assert.NoError(t, o.StopClusterServices(context.Background()))
}
