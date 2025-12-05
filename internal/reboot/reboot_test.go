package reboot

import (
	"testing"

	"github.com/go-logr/logr"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"go.uber.org/mock/gomock"
)

func TestIsOrigStaterootBooted(t *testing.T) {

	var (
		mockController      = gomock.NewController(t)
		mockRpmostreeclient = rpmostreeclient.NewMockIClient(mockController)
		mockOstreeclient    = ostreeclient.NewMockIClient(mockController)
		mockOps             = ops.NewMockOps(mockController)
		mockExec            = ops.NewMockExecute(mockController)
	)

	defer func() {
		mockController.Finish()
	}()

	type args struct {
		ibu          *ibuv1.ImageBasedUpgrade
		r            rpmostreeclient.IClient
		ostreeClient ostreeclient.IClient
		ops          *ops.MockOps
		executor     ops.Execute
		log          logr.Logger
	}
	tests := []struct {
		name             string
		args             args
		currentStateRoot string
		want             bool
		wantErr          bool
	}{
		{
			name: "in post pivot when desired stateroot is the same",
			args: args{
				ibu: &ibuv1.ImageBasedUpgrade{Spec: ibuv1.ImageBasedUpgradeSpec{SeedImageRef: ibuv1.SeedImageRef{
					Version: "4.14",
				}}},
				r:            mockRpmostreeclient,
				ostreeClient: mockOstreeclient,
				ops:          mockOps,
				executor:     mockExec,
			},
			currentStateRoot: "rhcos_4.14",
			want:             false,
			wantErr:          false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rebootClient := NewIBURebootClient(&tt.args.log, tt.args.executor, tt.args.r, tt.args.ostreeClient, tt.args.ops)
			mockRpmostreeclient.EXPECT().GetCurrentStaterootName().Return(tt.currentStateRoot, nil).Times(1)
			got, err := rebootClient.IsOrigStaterootBooted(tt.args.ibu.Spec.SeedImageRef.Version)
			if (err != nil) != tt.wantErr {
				t.Errorf("IsOrigStaterootBooted() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("IsOrigStaterootBooted() got = %v, want %v", got, tt.want)
			}
		})
	}
}
