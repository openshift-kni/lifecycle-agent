package ostreeclient

import (
	"errors"
	"testing"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestVerifyRepo(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "valid repository"},
		{name: "corrupt repository", err: errors.New("invalid object checksum")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := ops.NewMockExecute(gomock.NewController(t))
			executor.EXPECT().Execute("ostree", "fsck", "--repo", "/seed/ostree").Return("", tc.err)

			err := NewClient(executor, true).VerifyRepo("/seed/ostree")
			if tc.err == nil {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "invalid object checksum")
			}
		})
	}
}

func TestPullLocalVerifiesSourceObjects(t *testing.T) {
	for _, ibi := range []bool{false, true} {
		t.Run(map[bool]string{false: "IBU", true: "IBI"}[ibi], func(t *testing.T) {
			executor := ops.NewMockExecute(gomock.NewController(t))
			args := []any{"ostree", "pull-local", "--untrusted"}
			if ibi {
				args = append(args, "--repo", "/mnt/ostree/repo")
			}
			args = append(args, "/seed/ostree")
			executor.EXPECT().Execute(args[0].(string), args[1:]...).Return("", nil)

			require.NoError(t, NewClient(executor, ibi).PullLocal("/seed/ostree"))
		})
	}
}
