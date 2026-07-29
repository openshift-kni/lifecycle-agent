package reboot

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"go.uber.org/mock/gomock"
)

func TestIsOrigStaterootBooted(t *testing.T) {
	tests := []struct {
		name             string
		version          string
		currentStateRoot string
		staterootErr     error
		want             bool
		wantErr          bool
	}{
		{
			name:             "in post pivot when desired stateroot is the same",
			version:          "4.14",
			currentStateRoot: "rhcos_4.14",
			want:             false,
			wantErr:          false,
		},
		{
			name:             "original stateroot is booted when current differs from desired",
			version:          "4.15",
			currentStateRoot: "rhcos_4.14",
			want:             true,
			wantErr:          false,
		},
		{
			name:         "error getting current stateroot name",
			version:      "4.14",
			staterootErr: fmt.Errorf("rpm-ostree status failed"),
			want:         false,
			wantErr:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockController := gomock.NewController(t)
			defer mockController.Finish()

			mockRpmostreeclient := rpmostreeclient.NewMockIClient(mockController)
			mockOstreeclient := ostreeclient.NewMockIClient(mockController)
			mockOps := ops.NewMockOps(mockController)
			mockExec := ops.NewMockExecute(mockController)
			log := logr.Discard()

			rebootClient := NewIBURebootClient(&log, mockExec, mockRpmostreeclient, mockOstreeclient, mockOps)
			mockRpmostreeclient.EXPECT().GetCurrentStaterootName(gomock.Any()).Return(tt.currentStateRoot, tt.staterootErr).Times(1)
			got, err := rebootClient.IsOrigStaterootBooted(context.Background(), tt.version)
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

func TestIPCIsOrigStaterootBooted(t *testing.T) {
	tests := []struct {
		name             string
		version          string
		currentStateRoot string
		staterootErr     error
		want             bool
		wantErr          bool
	}{
		{
			name:             "desired stateroot is booted",
			version:          "4.14",
			currentStateRoot: "rhcos_4.14",
			want:             false,
			wantErr:          false,
		},
		{
			name:             "original stateroot is booted",
			version:          "4.15",
			currentStateRoot: "rhcos_4.14",
			want:             true,
			wantErr:          false,
		},
		{
			name:         "error getting current stateroot",
			version:      "4.14",
			staterootErr: fmt.Errorf("query status failed"),
			want:         false,
			wantErr:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockController := gomock.NewController(t)
			defer mockController.Finish()

			mockRpmostreeclient := rpmostreeclient.NewMockIClient(mockController)
			mockOstreeclient := ostreeclient.NewMockIClient(mockController)
			mockOps := ops.NewMockOps(mockController)
			mockExec := ops.NewMockExecute(mockController)
			log := logr.Discard()

			rebootClient := NewIPCRebootClient(&log, mockExec, mockRpmostreeclient, mockOstreeclient, mockOps)
			mockRpmostreeclient.EXPECT().GetCurrentStaterootName(gomock.Any()).Return(tt.currentStateRoot, tt.staterootErr).Times(1)
			got, err := rebootClient.IsOrigStaterootBooted(context.Background(), tt.version)
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

func TestIBUDisableInitMonitor(t *testing.T) {
	tests := []struct {
		name             string
		isActiveErr      error
		stopErr          error
		isEnabledErr     error
		disableErr       error
		daemonReloadErr  error
		wantErr          bool
		expectStop       bool
		expectDisable    bool
		expectDaemonLoad bool
	}{
		{
			name:             "service active and enabled - stops, disables, and reloads",
			expectStop:       true,
			expectDisable:    true,
			expectDaemonLoad: true,
			wantErr:          false,
		},
		{
			name:             "service not active but enabled - skips stop, disables, and reloads",
			isActiveErr:      fmt.Errorf("inactive"),
			expectStop:       false,
			expectDisable:    true,
			expectDaemonLoad: true,
			wantErr:          false,
		},
		{
			name:             "service active but not enabled - stops, skips disable, and reloads",
			isEnabledErr:     fmt.Errorf("disabled"),
			expectStop:       true,
			expectDisable:    false,
			expectDaemonLoad: true,
			wantErr:          false,
		},
		{
			name:       "stop fails - returns error",
			stopErr:    fmt.Errorf("stop failed"),
			expectStop: true,
			wantErr:    true,
		},
		{
			name:          "disable fails - returns error",
			disableErr:    fmt.Errorf("disable failed"),
			expectStop:    true,
			expectDisable: true,
			wantErr:       true,
		},
		{
			name:             "daemon-reload fails - returns error",
			daemonReloadErr:  fmt.Errorf("daemon-reload failed"),
			expectStop:       true,
			expectDisable:    true,
			expectDaemonLoad: true,
			wantErr:          true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockController := gomock.NewController(t)
			defer mockController.Finish()

			mockExec := ops.NewMockExecute(mockController)
			mockRpmostreeclient := rpmostreeclient.NewMockIClient(mockController)
			mockOstreeclient := ostreeclient.NewMockIClient(mockController)
			mockOps := ops.NewMockOps(mockController)
			log := logr.Discard()

			// is-active check
			isActiveCall := mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "is-active", gomock.Any()).Return("", tt.isActiveErr)

			if tt.isActiveErr == nil {
				// stop call
				stopCall := mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "stop", gomock.Any()).Return("", tt.stopErr).After(isActiveCall)
				if tt.stopErr != nil {
					// Error stops execution
					goto createClient
				}
				_ = stopCall
			}

			{
				// is-enabled check
				isEnabledCall := mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "is-enabled", gomock.Any()).Return("", tt.isEnabledErr)

				if tt.isEnabledErr == nil {
					// disable call
					disableCall := mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "disable", gomock.Any()).Return("", tt.disableErr).After(isEnabledCall)
					if tt.disableErr != nil {
						goto createClient
					}
					_ = disableCall
				}

				// daemon-reload
				if tt.expectDaemonLoad {
					mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "daemon-reload").Return("", tt.daemonReloadErr)
				}
			}

		createClient:
			rebootClient := NewIBURebootClient(&log, mockExec, mockRpmostreeclient, mockOstreeclient, mockOps)
			err := rebootClient.DisableInitMonitor(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("DisableInitMonitor() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestIPCDisableInitMonitor(t *testing.T) {
	tests := []struct {
		name        string
		isActiveErr error
		stopErr     error
		wantErr     bool
	}{
		{
			name:    "service active - stops successfully",
			wantErr: false,
		},
		{
			name:        "service not active - no stop attempt",
			isActiveErr: fmt.Errorf("inactive"),
			wantErr:     false,
		},
		{
			name:    "stop fails - returns error",
			stopErr: fmt.Errorf("stop failed"),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockController := gomock.NewController(t)
			defer mockController.Finish()

			mockExec := ops.NewMockExecute(mockController)
			mockRpmostreeclient := rpmostreeclient.NewMockIClient(mockController)
			mockOstreeclient := ostreeclient.NewMockIClient(mockController)
			mockOps := ops.NewMockOps(mockController)
			log := logr.Discard()

			isActiveCall := mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "is-active", gomock.Any()).Return("", tt.isActiveErr)

			if tt.isActiveErr == nil {
				mockExec.EXPECT().Execute(gomock.Any(), "systemctl", "stop", gomock.Any()).Return("", tt.stopErr).After(isActiveCall)
			}

			rebootClient := NewIPCRebootClient(&log, mockExec, mockRpmostreeclient, mockOstreeclient, mockOps)
			err := rebootClient.DisableInitMonitor(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("DisableInitMonitor() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
