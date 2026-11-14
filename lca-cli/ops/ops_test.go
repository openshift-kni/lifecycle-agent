package ops

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/recert"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func newTestOps(t *testing.T, timeout time.Duration) *ops {
	t.Helper()
	log := logrus.New()
	log.SetLevel(logrus.DebugLevel)
	return &ops{log: log, etcdTimeout: timeout}
}

func TestFormatISOKernelArgs(t *testing.T) {
	assert.Nil(t, formatISOKernelArgs(nil))
	assert.Equal(t, " coreos.liveiso.fromram\n", string(formatISOKernelArgs([]string{"coreos.liveiso.fromram"})))
}

func TestMountReadOnly(t *testing.T) {
	const deviceName = "sdb1"

	t.Run("mounts the discovered device read-only", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		exec := NewMockExecute(ctrl)
		log := logrus.New()
		o := &ops{log: log, hostCommandsExecutor: exec}

		mountFolder := filepath.Join(t.TempDir(), "p2")
		// The device path returned by discovery is mounted read-only (-o ro).
		exec.EXPECT().
			Execute("mount", "-o", "ro", fmt.Sprintf("/dev/%s", deviceName), mountFolder).
			Return("", nil).
			Times(1)

		err := o.MountReadOnly(deviceName, mountFolder)
		assert.NoError(t, err)
		// The mountpoint is created.
		_, statErr := os.Stat(mountFolder)
		assert.NoError(t, statErr)
	})

	t.Run("cleans up the mountpoint it created on a failed mount", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		exec := NewMockExecute(ctrl)
		log := logrus.New()
		o := &ops{log: log, hostCommandsExecutor: exec}

		mountFolder := filepath.Join(t.TempDir(), "p2")
		exec.EXPECT().
			Execute("mount", "-o", "ro", fmt.Sprintf("/dev/%s", deviceName), mountFolder).
			Return("mount: wrong fs type", fmt.Errorf("exit status 32")).
			Times(1)

		err := o.MountReadOnly(deviceName, mountFolder)
		assert.Error(t, err)
		// A failed attempt must leave no partial state: the mountpoint is removed.
		_, statErr := os.Stat(mountFolder)
		assert.True(t, os.IsNotExist(statErr), "mountpoint must be removed after a failed mount")
	})

	t.Run("preserves a pre-existing mountpoint on a failed mount", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		exec := NewMockExecute(ctrl)
		log := logrus.New()
		o := &ops{log: log, hostCommandsExecutor: exec}

		// A mountpoint that already exists (e.g. operator-configured) must not be removed.
		mountFolder := filepath.Join(t.TempDir(), "p2")
		assert.NoError(t, os.MkdirAll(mountFolder, 0o700))
		exec.EXPECT().
			Execute("mount", "-o", "ro", fmt.Sprintf("/dev/%s", deviceName), mountFolder).
			Return("", fmt.Errorf("exit status 32")).
			Times(1)

		err := o.MountReadOnly(deviceName, mountFolder)
		assert.Error(t, err)
		_, statErr := os.Stat(mountFolder)
		assert.NoError(t, statErr, "a pre-existing mountpoint must not be removed")
	})
}

func TestPodmanArgsPrependLocalImageStore(t *testing.T) {
	o := &ops{localImageStorePath: common.RODurabilityStorePath}
	got := o.podmanArgs([]string{"run", "--rm", "image"})
	assert.Equal(t,
		[]string{"--storage-opt", "imagestore=" + common.RODurabilityStorePath, "run", "--rm", "image"},
		got,
		"the storage option is a global Podman option and must precede the subcommand")
}

func TestPodmanArgsNoLocalImageStore(t *testing.T) {
	o := &ops{}
	got := o.podmanArgs([]string{"run", "--rm", "image"})
	assert.Equal(t, []string{"run", "--rm", "image"}, got)
}

func TestRunRecertUsesLocalImageStoreAndNoPull(t *testing.T) {
	ctrl := gomock.NewController(t)
	exec := NewMockExecute(ctrl)
	log := logrus.New()
	o := &ops{log: log, hostCommandsExecutor: exec, localImageStorePath: common.RODurabilityStorePath}

	exec.EXPECT().
		Execute("podman",
			"--storage-opt", "imagestore="+common.RODurabilityStorePath,
			"run", "--rm", "--network=host", "--privileged", "--replace",
			"--name", "recert",
			"-v", "/etc:"+recert.EtcMount,
			"-v", "/etc/ssh:"+recert.EtcSSHMount,
			"-v", "/etc/kubernetes:"+recert.EtcKubernetesMount,
			"-v", "/var/lib/kubelet:"+recert.VarLibKubeletMount,
			"-v", "/var/tmp:/var/tmp",
			"-v", "/etc/machine-config-daemon:"+recert.EtcMachineConfigDaemonMount,
			"-v", "/etc/pki:"+recert.EtcPKIMount,
			"-e", "RECERT_CONFIG=/tmp/recert.yaml",
			"--pull=never",
			"quay.io/example/recert:latest",
		).
		Return("", nil).
		Times(1)

	err := o.RunRecert("quay.io/example/recert:latest", "", "/tmp/recert.yaml")
	assert.NoError(t, err)
}

func TestNewOpsWithLocalImageStoreIfConfigured(t *testing.T) {
	t.Run("valid drop-in and store enable local image store mode", func(t *testing.T) {
		dir := t.TempDir()
		dropIn := filepath.Join(dir, "99-lca-ro-store.conf")
		storeCheck := filepath.Join(dir, "store")
		require.NoError(t, os.MkdirAll(storeCheck, 0o755))
		require.NoError(t, os.WriteFile(dropIn, []byte(common.RODurabilityCrioDropInContent()), 0o644))

		exec := NewMockExecute(gomock.NewController(t))
		got, err := newOpsWithLocalImageStoreIfConfigured(logrus.New(), exec, dropIn, common.RODurabilityStorePath, storeCheck)
		require.NoError(t, err)
		assert.True(t, got.LocalImageStoreEnabled())
	})

	t.Run("absent drop-in leaves connected behavior unchanged", func(t *testing.T) {
		dir := t.TempDir()
		dropIn := filepath.Join(dir, "99-lca-ro-store.conf")
		storeCheck := filepath.Join(dir, "store")

		exec := NewMockExecute(gomock.NewController(t))
		got, err := newOpsWithLocalImageStoreIfConfigured(logrus.New(), exec, dropIn, common.RODurabilityStorePath, storeCheck)
		require.NoError(t, err)
		assert.False(t, got.LocalImageStoreEnabled())
	})

	t.Run("unexpected drop-in content fails closed", func(t *testing.T) {
		dir := t.TempDir()
		dropIn := filepath.Join(dir, "99-lca-ro-store.conf")
		storeCheck := filepath.Join(dir, "store")
		require.NoError(t, os.MkdirAll(storeCheck, 0o755))
		require.NoError(t, os.WriteFile(dropIn, []byte("unexpected"), 0o644))

		exec := NewMockExecute(gomock.NewController(t))
		_, err := newOpsWithLocalImageStoreIfConfigured(logrus.New(), exec, dropIn, common.RODurabilityStorePath, storeCheck)
		assert.ErrorContains(t, err, "unexpected content")
	})

	t.Run("missing durable store fails closed", func(t *testing.T) {
		dir := t.TempDir()
		dropIn := filepath.Join(dir, "99-lca-ro-store.conf")
		storeCheck := filepath.Join(dir, "store")
		require.NoError(t, os.WriteFile(dropIn, []byte(common.RODurabilityCrioDropInContent()), 0o644))

		exec := NewMockExecute(gomock.NewController(t))
		_, err := newOpsWithLocalImageStoreIfConfigured(logrus.New(), exec, dropIn, common.RODurabilityStorePath, storeCheck)
		assert.ErrorContains(t, err, "validating local image store")
	})
}

func TestImageExistsUsesLocalImageStore(t *testing.T) {
	ctrl := gomock.NewController(t)
	exec := NewMockExecute(ctrl)
	log := logrus.New()
	o := &ops{log: log, hostCommandsExecutor: exec, localImageStorePath: common.RODurabilityStorePath}

	exec.EXPECT().
		Execute("podman",
			"--storage-opt", "imagestore="+common.RODurabilityStorePath,
			"image", "exists", "quay.io/example/recert:latest",
		).
		Return("", nil).
		Times(1)

	exists, err := o.ImageExists("quay.io/example/recert:latest")
	assert.NoError(t, err)
	assert.True(t, exists)
	assert.True(t, o.LocalImageStoreEnabled())
}

func TestWaitForEtcd_ImmediateSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	o := newTestOps(t, 5*time.Second)
	err := o.waitForEtcd(srv.URL)
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

	o := newTestOps(t, 5*time.Second)
	err := o.waitForEtcd(srv.URL)
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, count.Load(), int32(3))
}

func TestWaitForEtcd_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	o := newTestOps(t, 2*time.Second)
	err := o.waitForEtcd(srv.URL)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "timeout waiting for etcd")
}

func TestWaitForEtcd_ConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	url := srv.URL
	srv.Close()

	o := newTestOps(t, 2*time.Second)
	err := o.waitForEtcd(url)
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

	err := o.waitForEtcd(srv.URL)
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

	o := newTestOps(t, 5*time.Second)
	err := o.waitForEtcd(srv.URL)
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, count.Load(), int32(3))
}

const (
	crictlStopCmd = "crictl ps -q | xargs --no-run-if-empty --max-args 1 --max-procs 10 crictl stop --timeout 5"
	crictlRmpCmd  = "crictl pods -q | xargs --no-run-if-empty crictl rmp -f"
)

// exitError returns a real *exec.ExitError with the given exit code.
func exitError(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *exec.ExitError, got %v", err)
	}
	return exitErr
}

func TestStopClusterServices(t *testing.T) {
	errCrictl := errors.New("crictl stop failed")

	testCases := []struct {
		name           string
		crioInactive   bool
		setupCrictl    func(m *MockExecute)
		crioStopErr    error
		expectCrioStop bool
		expectErr      string
	}{
		{
			name: "containers stop on first attempt",
			setupCrictl: func(m *MockExecute) {
				m.EXPECT().Execute("bash", "-c", crictlStopCmd).Return("", nil).Times(1)
			},
			expectCrioStop: true,
		},
		{
			name: "containers stop after a failed attempt",
			setupCrictl: func(m *MockExecute) {
				gomock.InOrder(
					m.EXPECT().Execute("bash", "-c", crictlStopCmd).Return("", errCrictl),
					m.EXPECT().Execute("bash", "-c", crictlStopCmd).Return("", nil),
				)
			},
			expectCrioStop: true,
		},
		{
			name: "containers never stop, falls back to sandbox removal",
			setupCrictl: func(m *MockExecute) {
				m.EXPECT().Execute("bash", "-c", crictlStopCmd).Return("", errCrictl).MinTimes(2)
			},
			expectCrioStop: true,
		},
		{
			name:         "crio already inactive",
			crioInactive: true,
			setupCrictl: func(m *MockExecute) {
				m.EXPECT().Execute("bash", "-c", crictlStopCmd).Times(0)
			},
		},
		{
			name: "crio stop fails",
			setupCrictl: func(m *MockExecute) {
				m.EXPECT().Execute("bash", "-c", crictlStopCmd).Return("", nil)
			},
			crioStopErr:    errors.New("stop failed"),
			expectCrioStop: true,
			expectErr:      "failed to stop crio engine",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			mockExec := NewMockExecute(ctrl)

			mockExec.EXPECT().Execute("systemctl", "stop", "kubelet.service").Return("", nil)
			mockExec.EXPECT().Execute("systemctl", "disable", "kubelet.service").Return("", nil)
			if tc.crioInactive {
				mockExec.EXPECT().Execute("systemctl", "is-active", "crio").Return("inactive", exitError(t, 3))
			} else {
				mockExec.EXPECT().Execute("systemctl", "is-active", "crio").Return("active", nil)
			}
			tc.setupCrictl(mockExec)
			if tc.expectCrioStop {
				mockExec.EXPECT().Execute("bash", "-c", crictlRmpCmd).Return("", nil)
				mockExec.EXPECT().Execute("systemctl", "stop", "crio.service").Return("", tc.crioStopErr)
			}

			o := &ops{log: logrus.New(), hostCommandsExecutor: mockExec, containerStopTimeout: 2 * time.Second}
			err := o.StopClusterServices()
			if tc.expectErr != "" {
				assert.ErrorContains(t, err, tc.expectErr)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
