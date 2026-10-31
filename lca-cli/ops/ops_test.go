package ops

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
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
