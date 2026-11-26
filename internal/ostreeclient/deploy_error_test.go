package ostreeclient

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

func TestDeployErrorNamesDeployCommand(t *testing.T) {
	errExit := errors.New("exit status 1")
	mockExec := ops.NewMockExecute(gomock.NewController(t))
	c := NewClient(mockExec, true)
	mockExec.EXPECT().Execute("bash", "-c", gomock.Cond(func(cmd string) bool { return strings.HasPrefix(cmd, "ostree admin deploy") })).Return("", errExit)

	err := c.Deploy("rhcos_4.20", "abc123", nil, nil, true)
	assert.ErrorContains(t, err, "failed to run ostree admin deploy")
	assert.NotContains(t, err.Error(), "OSInit")
	assert.ErrorIs(t, err, errExit)
}
