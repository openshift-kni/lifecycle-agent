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
)

func newTestOps(t *testing.T, timeout time.Duration) *ops {
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

	o := newTestOps(t, 5*time.Second)
	err := o.waitForEtcd(context.Background(),srv.URL)
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
	err := o.waitForEtcd(context.Background(),srv.URL)
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, count.Load(), int32(3))
}

func TestWaitForEtcd_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	o := newTestOps(t, 2*time.Second)
	err := o.waitForEtcd(context.Background(),srv.URL)
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
	err := o.waitForEtcd(context.Background(),url)
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

	err := o.waitForEtcd(context.Background(),srv.URL)
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
	err := o.waitForEtcd(context.Background(),srv.URL)
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, count.Load(), int32(3))
}
