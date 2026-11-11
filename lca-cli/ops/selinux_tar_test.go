package ops

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
)

func TestPrepareSELinuxTar(t *testing.T) {
	if _, err := os.Stat("/usr/bin/tar"); err != nil {
		t.Skip("requires /usr/bin/tar")
	}

	testCases := []struct {
		name     string
		chconErr error
	}{
		{name: "returns the labeled copy"},
		{name: "removes the copy when chcon fails", chconErr: errors.New("operation not permitted")},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mockExec := NewMockExecute(gomock.NewController(t))
			o := newTestOps(t, 0)
			o.hostCommandsExecutor = mockExec
			var labeled string
			mockExec.EXPECT().Execute("chcon", "-t", "install_exec_t", gomock.Any()).
				DoAndReturn(func(_ string, args ...string) (string, error) {
					labeled = args[2]
					return "", tc.chconErr
				})

			tarCopy, err := o.prepareSELinuxTar()
			if !assert.NotEmpty(t, labeled) {
				return
			}
			labeledOutside := common.PathOutsideChroot(labeled)
			t.Cleanup(func() { _ = os.Remove(labeledOutside) })

			if tc.chconErr != nil {
				assert.ErrorIs(t, err, tc.chconErr)
				assert.NoFileExists(t, labeledOutside, "the temporary tar copy must be removed")
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, labeledOutside, tarCopy, "chcon labels the returned copy")
			assert.FileExists(t, tarCopy, "the labeled tar copy is kept for the caller")
		})
	}
}
