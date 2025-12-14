package ipconfig

import (
	"errors"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	ostreemock "github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	opsmock "github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
)

func newRollbackHandler(t *testing.T) (*RollbackHandler, *opsmock.MockOps, *ostreemock.MockIClient, *rpmostreeclient.MockIClient) {
	t.Helper()

	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	mockOps := opsmock.NewMockOps(ctrl)
	mockOstree := ostreemock.NewMockIClient(ctrl)
	mockRPM := rpmostreeclient.NewMockIClient(ctrl)

	return NewRollbackHandler(logrus.New(), mockOps, mockOstree, mockRPM), mockOps, mockOstree, mockRPM
}

func TestRollbackRunFeatureEnabledSuccess(t *testing.T) {
	handler, _, ostree, rpm := newRollbackHandler(t)

	ostree.EXPECT().IsOstreeAdminSetDefaultFeatureEnabled().Return(true)
	rpm.EXPECT().GetDeploymentIndex("new").Return(3, nil)
	ostree.EXPECT().SetDefaultDeployment(3).Return(nil)

	assert.NoError(t, handler.Run("new"))
}

func TestRollbackRunGetDeploymentIndexError(t *testing.T) {
	handler, _, ostree, rpm := newRollbackHandler(t)

	ostree.EXPECT().IsOstreeAdminSetDefaultFeatureEnabled().Return(true)
	rpm.EXPECT().GetDeploymentIndex("new").Return(0, errors.New("idx-fail"))
	ostree.EXPECT().SetDefaultDeployment(gomock.Any()).Times(0)

	err := handler.Run("new")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get deployment index")
}

func TestRollbackRunSetDefaultDeploymentError(t *testing.T) {
	handler, _, ostree, rpm := newRollbackHandler(t)

	ostree.EXPECT().IsOstreeAdminSetDefaultFeatureEnabled().Return(true)
	rpm.EXPECT().GetDeploymentIndex("new").Return(1, nil)
	ostree.EXPECT().SetDefaultDeployment(1).Return(errors.New("set-fail"))

	err := handler.Run("new")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to set default deployment")
}

func TestRollbackRunFeatureDisabledNoop(t *testing.T) {
	handler, _, ostree, rpm := newRollbackHandler(t)

	ostree.EXPECT().IsOstreeAdminSetDefaultFeatureEnabled().Return(false)
	rpm.EXPECT().GetDeploymentIndex(gomock.Any()).Times(0)
	ostree.EXPECT().SetDefaultDeployment(gomock.Any()).Times(0)

	assert.NoError(t, handler.Run("new"))
}
