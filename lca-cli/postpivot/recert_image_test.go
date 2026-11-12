package postpivot

import (
	"fmt"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	clusterconfig_api "github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedclusterinfo"
)

const testRecertImage = "quay.io/openshift/recert@sha256:abc"

// expectedRecertPullArgs returns the argument vector ensureRecertImageAvailable builds for a
// no-proxy pull. Using `env` with discrete arguments avoids shell interpolation of operator
// supplied proxy and image values.
func expectedRecertPullArgs() []any {
	return []any{"timeout", fmt.Sprintf("%d", int(recertImagePullAttemptTimeout.Seconds())), "podman", "pull", "--authfile", common.ImageRegistryAuthFile}
}

// TestEnsureRecertImageAvailable_PresentNoPull proves that when the recert image is precached
// no pull (and therefore no network access) is attempted. This is the offline no-network audit
// for the recert image resolution.
func TestEnsureRecertImageAvailable_PresentNoPull(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ImageExists(testRecertImage).Return(true, nil).Times(1)
	// No pull must be issued.
	mockOps.EXPECT().RunInHostNamespace(gomock.Any(), gomock.Any()).Times(0)

	pp := NewPostPivot(nil, logrus.New(), mockOps, "", "", "")
	err := pp.ensureRecertImageAvailable(
		&clusterconfig_api.SeedReconfiguration{},
		&seedclusterinfo.SeedClusterInfo{RecertImagePullSpec: testRecertImage})
	assert.NoError(t, err)
}

// TestEnsureRecertImageAvailable_MissingOffline_FailsFast proves that a missing recert image
// fails fast after a small bounded number of pull attempts (no 10-minute poll loop) with a clear
// precache hint.
func TestEnsureRecertImageAvailable_MissingOffline_FailsFast(t *testing.T) {
	orig := recertImagePullRetryDelay
	recertImagePullRetryDelay = 0 // do not sleep between attempts in the test
	t.Cleanup(func() { recertImagePullRetryDelay = orig })

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ImageExists(testRecertImage).Return(false, nil).Times(1)
	// Connected node (no local image store): the missing image falls through to the bounded pull.
	mockOps.EXPECT().LocalImageStoreEnabled().Return(false).Times(1)
	// A bounded number of pull attempts, then fail fast.
	mockOps.EXPECT().RunInHostNamespace("env", append(expectedRecertPullArgs(), testRecertImage)...).
		Return("", fmt.Errorf("no route to host")).Times(recertImagePullAttempts)

	pp := NewPostPivot(nil, logrus.New(), mockOps, "", "", "")
	err := pp.ensureRecertImageAvailable(
		&clusterconfig_api.SeedReconfiguration{},
		&seedclusterinfo.SeedClusterInfo{RecertImagePullSpec: testRecertImage})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "precached")
}

// TestEnsureRecertImageAvailable_MissingPullSucceeds proves the connected flow still works: a
// missing image is pulled once and resolution succeeds.
func TestEnsureRecertImageAvailable_MissingPullSucceeds(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ImageExists(testRecertImage).Return(false, nil).Times(1)
	// Connected node (no local image store): the missing image is pulled.
	mockOps.EXPECT().LocalImageStoreEnabled().Return(false).Times(1)
	mockOps.EXPECT().RunInHostNamespace("env", append(expectedRecertPullArgs(), testRecertImage)...).
		Return("", nil).Times(1)

	pp := NewPostPivot(nil, logrus.New(), mockOps, "", "", "")
	err := pp.ensureRecertImageAvailable(
		&clusterconfig_api.SeedReconfiguration{},
		&seedclusterinfo.SeedClusterInfo{RecertImagePullSpec: testRecertImage})
	assert.NoError(t, err)
}

// TestEnsureRecertImageAvailable_ProxyAndImageMetacharactersAreNotShellInterpolated proves the
// pull is executed as an argument vector. Operator-supplied proxy and image values containing
// shell metacharacters must be passed verbatim to env/podman rather than interpreted by a shell.
func TestEnsureRecertImageAvailable_ProxyAndImageMetacharactersAreNotShellInterpolated(t *testing.T) {
	image := "quay.io/example/recert:test;touch,/tmp/pwned"
	proxy := &clusterconfig_api.Proxy{
		HTTPProxy:  "http://user:pass;rm|/tmp@example.com",
		HTTPSProxy: "https://user:pass;rm|/tmp@example.com",
		NoProxy:    ".example.com;rm|/tmp",
	}

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().ImageExists(image).Return(false, nil).Times(1)
	mockOps.EXPECT().LocalImageStoreEnabled().Return(false).Times(1)
	mockOps.EXPECT().RunInHostNamespace(
		"env",
		"HTTP_PROXY="+proxy.HTTPProxy,
		"HTTPS_PROXY="+proxy.HTTPSProxy,
		"NO_PROXY="+proxy.NoProxy,
		"timeout",
		fmt.Sprintf("%d", int(recertImagePullAttemptTimeout.Seconds())),
		"podman",
		"pull",
		"--authfile",
		common.ImageRegistryAuthFile,
		image,
	).Return("", nil).Times(1)

	pp := NewPostPivot(nil, logrus.New(), mockOps, "", "", "")
	err := pp.ensureRecertImageAvailable(
		&clusterconfig_api.SeedReconfiguration{Proxy: proxy},
		&seedclusterinfo.SeedClusterInfo{RecertImagePullSpec: image})
	assert.NoError(t, err)
}
