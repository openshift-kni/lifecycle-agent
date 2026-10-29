package postpivot

import (
	"fmt"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	clusterconfig_api "github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedclusterinfo"
)

const testRecertImage = "quay.io/openshift/recert@sha256:abc"

// expectedRecertPullCommand is the bash command string ensureRecertImageAvailable builds for a
// no-proxy pull: podman wrapped in coreutils `timeout` so a stalled pull cannot block offline
// reconfigure indefinitely.
func expectedRecertPullCommand() string {
	return fmt.Sprintf("timeout %d podman", int(recertImagePullAttemptTimeout.Seconds()))
}

// TestEnsureRecertImageAvailable_PresentNoPull proves that when the recert image is precached
// no pull (and therefore no network access) is attempted. This is the offline no-network audit
// for the recert image resolution.
func TestEnsureRecertImageAvailable_PresentNoPull(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOps.EXPECT().RunInHostNamespace("podman", "image", "exists", testRecertImage).Return("", nil).Times(1)
	// No pull must be issued.
	mockOps.EXPECT().RunBashInHostNamespace(gomock.Any(), gomock.Any()).Times(0)

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
	mockOps.EXPECT().RunInHostNamespace("podman", "image", "exists", testRecertImage).
		Return("", fmt.Errorf("image not known")).Times(1)
	// A bounded number of pull attempts, then fail fast.
	mockOps.EXPECT().RunBashInHostNamespace(expectedRecertPullCommand(), "pull", gomock.Any(), gomock.Any(), testRecertImage).
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
	mockOps.EXPECT().RunInHostNamespace("podman", "image", "exists", testRecertImage).
		Return("", fmt.Errorf("image not known")).Times(1)
	mockOps.EXPECT().RunBashInHostNamespace(expectedRecertPullCommand(), "pull", gomock.Any(), gomock.Any(), testRecertImage).
		Return("", nil).Times(1)

	pp := NewPostPivot(nil, logrus.New(), mockOps, "", "", "")
	err := pp.ensureRecertImageAvailable(
		&clusterconfig_api.SeedReconfiguration{},
		&seedclusterinfo.SeedClusterInfo{RecertImagePullSpec: testRecertImage})
	assert.NoError(t, err)
}
