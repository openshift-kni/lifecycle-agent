package controllers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	ipcv1 "github.com/openshift-kni/lifecycle-agent/api/ipconfig/v1"
	controllerutils "github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	machineconfigv1 "github.com/openshift/api/machineconfiguration/v1"
)

func newIPConfigTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := ipcv1.AddToScheme(s); err != nil {
		t.Fatalf("failed to add ipconfig scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("failed to add corev1 scheme: %v", err)
	}
	if err := machineconfigv1.AddToScheme(s); err != nil {
		t.Fatalf("failed to add machineconfig scheme: %v", err)
	}
	return s
}

func newFakeClientWithStatus(t *testing.T, s *runtime.Scheme, objs ...client.Object) client.Client {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...)
	// Ensure status updates work for IPConfig objects.
	for _, o := range objs {
		if _, ok := o.(*ipcv1.IPConfig); ok {
			b = b.WithStatusSubresource(o)
		}
	}
	return b.Build()
}

func mustGetIPCConfig(t *testing.T, c client.Reader, name string) *ipcv1.IPConfig {
	t.Helper()
	got := &ipcv1.IPConfig{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, got); err != nil {
		t.Fatalf("failed to get ipconfig %q: %v", name, err)
	}
	return got
}

func ensureWritableIPConfigPaths(t *testing.T) {
	t.Helper()

	// Tests should mock filesystem I/O via ops.Ops; nothing to do here.
}

func mkConfigIPC(t *testing.T, withHistory bool) *ipcv1.IPConfig {
	t.Helper()
	ipc := &ipcv1.IPConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       common.IPConfigName,
			Generation: 1,
		},
		Spec: ipcv1.IPConfigSpec{
			Stage: ipcv1.IPStages.Config,
		},
		Status: ipcv1.IPConfigStatus{
			ValidNextStages: []ipcv1.IPConfigStage{ipcv1.IPStages.Config, ipcv1.IPStages.Idle},
		},
	}
	if withHistory {
		ipc.Status.History = []*ipcv1.IPHistory{{
			Stage:     ipcv1.IPStages.Config,
			StartTime: metav1.Now(),
			Phases:    []*ipcv1.IPPhase{},
		}}
	}
	return ipc
}

func mkSNOObjects() (*corev1.Node, *machineconfigv1.MachineConfig) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "master-0",
			Labels: map[string]string{
				"node-role.kubernetes.io/master": "",
			},
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{
				Type:    corev1.NodeInternalIP,
				Address: "192.0.2.10",
			}},
		},
	}
	mc := &machineconfigv1.MachineConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: common.DnsmasqMachineConfigName,
		},
	}
	return node, mc
}

func assertConfigInProgress(t *testing.T, ipc *ipcv1.IPConfig) {
	t.Helper()
	inProg := controllerutils.GetIPInProgressCondition(ipc, ipcv1.IPStages.Config)
	if assert.NotNil(t, inProg) {
		assert.Equal(t, metav1.ConditionTrue, inProg.Status)
		assert.Equal(t, string(controllerutils.ConditionReasons.InProgress), inProg.Reason)
	}
}

func assertConfigFailed(t *testing.T, ipc *ipcv1.IPConfig) {
	t.Helper()
	inProg := controllerutils.GetIPInProgressCondition(ipc, ipcv1.IPStages.Config)
	comp := controllerutils.GetIPCompletedCondition(ipc, ipcv1.IPStages.Config)
	if assert.NotNil(t, inProg) {
		assert.Equal(t, metav1.ConditionFalse, inProg.Status)
		assert.Equal(t, string(controllerutils.ConditionReasons.Failed), inProg.Reason)
	}
	if assert.NotNil(t, comp) {
		assert.Equal(t, metav1.ConditionFalse, comp.Status)
		assert.Equal(t, string(controllerutils.ConditionReasons.Failed), comp.Reason)
	}
}

func assertConfigCompleted(t *testing.T, ipc *ipcv1.IPConfig) {
	t.Helper()
	inProg := controllerutils.GetIPInProgressCondition(ipc, ipcv1.IPStages.Config)
	comp := controllerutils.GetIPCompletedCondition(ipc, ipcv1.IPStages.Config)
	if assert.NotNil(t, inProg) {
		assert.Equal(t, metav1.ConditionFalse, inProg.Status)
		assert.Equal(t, string(controllerutils.ConditionReasons.Completed), inProg.Reason)
	}
	if assert.NotNil(t, comp) {
		assert.Equal(t, metav1.ConditionTrue, comp.Status)
		assert.Equal(t, string(controllerutils.ConditionReasons.Completed), comp.Reason)
	}
}

func findConfigPhase(t *testing.T, ipc *ipcv1.IPConfig, phase string) *ipcv1.IPPhase {
	t.Helper()
	for _, h := range ipc.Status.History {
		if h.Stage != ipcv1.IPStages.Config {
			continue
		}
		for _, p := range h.Phases {
			if p.Phase == phase {
				return p
			}
		}
	}
	return nil
}

func findConfigStageHistory(t *testing.T, ipc *ipcv1.IPConfig) *ipcv1.IPHistory {
	t.Helper()
	for _, h := range ipc.Status.History {
		if h.Stage == ipcv1.IPStages.Config {
			return h
		}
	}
	return nil
}

func TestIPCConfigTwoPhaseHandler_PrePivot(t *testing.T) {
	scheme := newIPConfigTestScheme(t)
	logger := logr.Logger{}

	t.Run("spec and status match => sets completed and does not requeue", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)
		mockReboot := reboot.NewMockRebootIntf(gc)

		ipc := mkConfigIPC(t, true)
		// statusIPsMatchSpec requires these to be non-nil even if spec is empty.
		ipc.Status.Network = &ipcv1.NetworkStatus{
			HostNetwork:    &ipcv1.HostNetworkStatus{},
			ClusterNetwork: &ipcv1.ClusterNetworkStatus{},
		}
		k8sClient := newFakeClientWithStatus(t, scheme, ipc)

		h := &IPCConfigTwoPhaseHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			RPMOstreeClient: mockRPM,
			OstreeClient:    mockOstree,
			ChrootOps:       mockOps,
			RebootClient:    mockReboot,
		}

		res, err := h.PrePivot(context.Background(), ipc, logger)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigCompleted(t, updated)
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Config, ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
		hist := findConfigStageHistory(t, updated)
		if assert.NotNil(t, hist) {
			assert.False(t, hist.StartTime.IsZero())
		}
		// PrePivot always starts the pre-pivot phase, even if it exits early.
		p := findConfigPhase(t, updated, IPConfigPhasePrePivot)
		if assert.NotNil(t, p) {
			assert.False(t, p.StartTime.IsZero())
		}
	})

	t.Run("healthcheck failing => updates in-progress message and requeues", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)
		mockReboot := reboot.NewMockRebootIntf(gc)

		ipc := mkConfigIPC(t, true)
		// Force statusIPsMatchSpec to return error by leaving status network unpopulated.
		k8sClient := newFakeClientWithStatus(t, scheme, ipc)

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error {
			return errors.New("not healthy")
		}

		h := &IPCConfigTwoPhaseHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			RPMOstreeClient: mockRPM,
			OstreeClient:    mockOstree,
			ChrootOps:       mockOps,
			RebootClient:    mockReboot,
		}

		res, err := h.PrePivot(context.Background(), ipc, logger)
		assert.NoError(t, err)
		assert.Equal(t, requeueWithHealthCheckInterval(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigInProgress(t, updated)
		hist := findConfigStageHistory(t, updated)
		if assert.NotNil(t, hist) {
			assert.False(t, hist.StartTime.IsZero())
			assert.True(t, hist.CompletionTime.IsZero())
		}
		inProg := controllerutils.GetIPInProgressCondition(updated, ipcv1.IPStages.Config)
		if assert.NotNil(t, inProg) {
			assert.Contains(t, inProg.Message, "Waiting for system to stabilize")
		}
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Config, ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
	})

	t.Run("copy lca-cli failure => marks failed and returns error", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)
		mockReboot := reboot.NewMockRebootIntf(gc)

		ipc := mkConfigIPC(t, true)
		k8sClient := newFakeClientWithStatus(t, scheme, ipc)

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error { return nil }

		mockOps.EXPECT().
			CopyFile(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(errors.New("copy failed")).
			Times(1)

		h := &IPCConfigTwoPhaseHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			RPMOstreeClient: mockRPM,
			OstreeClient:    mockOstree,
			ChrootOps:       mockOps,
			RebootClient:    mockReboot,
		}

		res, err := h.PrePivot(context.Background(), ipc, logger)
		assert.Error(t, err)
		assert.Equal(t, ctrl.Result{}, res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigFailed(t, updated)
		hist := findConfigStageHistory(t, updated)
		if assert.NotNil(t, hist) {
			assert.False(t, hist.StartTime.IsZero())
		}
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Config, ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
	})

	t.Run("auto-rollback config write failure => marks failed and returns error", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)
		mockReboot := reboot.NewMockRebootIntf(gc)

		ipc := mkConfigIPC(t, true)
		k8sClient := newFakeClientWithStatus(t, scheme, ipc)

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error { return nil }

		mockOps.EXPECT().
			CopyFile(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(nil).
			Times(1)
		mockOps.EXPECT().
			WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(func(filename string, data []byte, perm os.FileMode) error {
				if strings.Contains(filename, filepath.Clean(common.IPCAutoRollbackConfigFile)) {
					return errors.New("write auto-rollback failed")
				}
				// Allow other writes in this path.
				return nil
			}).
			AnyTimes()

		h := &IPCConfigTwoPhaseHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			RPMOstreeClient: mockRPM,
			OstreeClient:    mockOstree,
			ChrootOps:       mockOps,
			RebootClient:    mockReboot,
		}

		_, err := h.PrePivot(context.Background(), ipc, logger)
		assert.Error(t, err)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigFailed(t, updated)
		hist := findConfigStageHistory(t, updated)
		if assert.NotNil(t, hist) {
			assert.False(t, hist.StartTime.IsZero())
		}
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Config, ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
	})

	t.Run("success schedules pre-pivot via systemd-run and starts phase history", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)
		mockReboot := reboot.NewMockRebootIntf(gc)

		ipc := mkConfigIPC(t, true)
		k8sClient := newFakeClientWithStatus(t, scheme, ipc)

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error { return nil }

		mockOps.EXPECT().CopyFile(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(1)

		// reboot.WriteIPCAutoRollbackConfigFile -> ops.WriteFile on IPCAutoRollbackConfigFile
		// writeIPConfigPrePivotConfig -> ops.WriteFile on IPConfigPrePivotFlagsFile
		// writeIPConfigPostPivotConfig -> ops.WriteFile on IPConfigPostPivotFlagsFile
		// exportIPConfigForUncontrolledRollback -> ops.WriteFile on IPCFilePath
		wroteRollbackCopy := false
		mockOps.EXPECT().
			WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(func(filename string, data []byte, perm os.FileMode) error {
				assert.NotEmpty(t, data)
				assert.Equal(t, os.FileMode(0o600), perm)
				switch filepath.Clean(filename) {
				case filepath.Clean(common.PathOutsideChroot(common.IPCAutoRollbackConfigFile)),
					filepath.Clean(common.PathOutsideChroot(common.IPConfigPrePivotFlagsFile)),
					filepath.Clean(common.PathOutsideChroot(common.IPConfigPostPivotFlagsFile)),
					filepath.Clean(common.PathOutsideChroot(common.IPCFilePath)):
					if filepath.Clean(filename) == filepath.Clean(common.PathOutsideChroot(common.IPCFilePath)) {
						wroteRollbackCopy = true
					}
					return nil
				default:
					// Allow any other writes (tests should not be brittle on incidental paths)
					return nil
				}
			}).
			AnyTimes()

		mockOps.EXPECT().
			RunSystemdAction(
				gomock.Any(), gomock.Any(),
				gomock.Any(), gomock.Any(),
				gomock.Any(), gomock.Any(),
				gomock.Any(), gomock.Any(),
				gomock.Any(), gomock.Any(), gomock.Any(),
			).
			DoAndReturn(func(args ...string) (string, error) {
				assert.Contains(t, args, "--unit")
				assert.Contains(t, args, controllerutils.IPConfigPrePivotUnit)
				assert.Contains(t, args, controllerutils.LcaCliBinaryName)
				assert.Contains(t, args, "ip-config")
				assert.Contains(t, args, "pre-pivot")
				return "ok", nil
			}).Times(1)

		h := &IPCConfigTwoPhaseHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			RPMOstreeClient: mockRPM,
			OstreeClient:    mockOstree,
			ChrootOps:       mockOps,
			RebootClient:    mockReboot,
		}

		res, err := h.PrePivot(context.Background(), ipc, logger)
		assert.NoError(t, err)
		assert.Equal(t, requeueWithShortInterval(), res)
		assert.True(t, wroteRollbackCopy, "expected uncontrolled-rollback IPConfig copy to be written")

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Config, ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
		hist := findConfigStageHistory(t, updated)
		if assert.NotNil(t, hist) {
			assert.False(t, hist.StartTime.IsZero())
			assert.True(t, hist.CompletionTime.IsZero())
		}
		p := findConfigPhase(t, updated, IPConfigPhasePrePivot)
		if assert.NotNil(t, p) {
			assert.False(t, p.StartTime.IsZero())
		}
	})

	t.Run("systemd-run failure => marks failed and does not requeue", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)
		mockReboot := reboot.NewMockRebootIntf(gc)

		ipc := mkConfigIPC(t, true)
		k8sClient := newFakeClientWithStatus(t, scheme, ipc)

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error { return nil }

		mockOps.EXPECT().CopyFile(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(1)
		mockOps.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		mockOps.EXPECT().
			RunSystemdAction(
				gomock.Any(), gomock.Any(),
				gomock.Any(), gomock.Any(),
				gomock.Any(), gomock.Any(),
				gomock.Any(), gomock.Any(),
				gomock.Any(), gomock.Any(), gomock.Any(),
			).
			Return("", errors.New("systemd-run failed")).
			Times(1)

		h := &IPCConfigTwoPhaseHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			RPMOstreeClient: mockRPM,
			OstreeClient:    mockOstree,
			ChrootOps:       mockOps,
			RebootClient:    mockReboot,
		}

		res, err := h.PrePivot(context.Background(), ipc, logger)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigFailed(t, updated)
		hist := findConfigStageHistory(t, updated)
		if assert.NotNil(t, hist) {
			assert.False(t, hist.StartTime.IsZero())
		}
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Config, ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
	})
}

func TestIPCConfigTwoPhaseHandler_PostPivot(t *testing.T) {
	scheme := newIPConfigTestScheme(t)
	logger := logr.Logger{}

	t.Run("healthcheck failing => updates in-progress and requeues", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)
		mockReboot := reboot.NewMockRebootIntf(gc)

		ipc := mkConfigIPC(t, true)
		k8sClient := newFakeClientWithStatus(t, scheme, ipc)

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error { return errors.New("not healthy") }

		h := &IPCConfigTwoPhaseHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			RPMOstreeClient: mockRPM,
			OstreeClient:    mockOstree,
			ChrootOps:       mockOps,
			RebootClient:    mockReboot,
		}

		res, err := h.PostPivot(context.Background(), ipc, logger)
		assert.NoError(t, err)
		assert.Equal(t, requeueWithHealthCheckInterval(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigInProgress(t, updated)
		hist := findConfigStageHistory(t, updated)
		if assert.NotNil(t, hist) {
			assert.False(t, hist.StartTime.IsZero())
			assert.True(t, hist.CompletionTime.IsZero())
		}
		inProg := controllerutils.GetIPInProgressCondition(updated, ipcv1.IPStages.Config)
		if assert.NotNil(t, inProg) {
			assert.Contains(t, inProg.Message, "Waiting for system to stabilize")
		}
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Config, ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
	})

	t.Run("healthy but status does not match spec => requeues waiting for status match", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)
		mockReboot := reboot.NewMockRebootIntf(gc)

		ipc := mkConfigIPC(t, true)
		ipc.Spec.IPv4 = &ipcv1.IPv4Config{
			Address:        "192.0.2.11/24",
			MachineNetwork: "192.0.2.0/24",
			Gateway:        "192.0.2.1",
			DNSServer:      "192.0.2.53",
		}
		ipc.Status.Network = &ipcv1.NetworkStatus{
			HostNetwork: &ipcv1.HostNetworkStatus{
				IPv4: &ipcv1.HostIPStatus{
					Gateway:   "192.0.2.1",
					DNSServer: "192.0.2.53",
				},
			},
			ClusterNetwork: &ipcv1.ClusterNetworkStatus{
				IPv4: &ipcv1.ClusterIPStatus{
					Address:        "192.0.2.99",   // mismatch
					MachineNetwork: "192.0.2.0/24", // match
				},
			},
		}
		k8sClient := newFakeClientWithStatus(t, scheme, ipc)

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error { return nil }

		h := &IPCConfigTwoPhaseHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			RPMOstreeClient: mockRPM,
			OstreeClient:    mockOstree,
			ChrootOps:       mockOps,
			RebootClient:    mockReboot,
		}

		res, err := h.PostPivot(context.Background(), ipc, logger)
		assert.NoError(t, err)
		assert.Equal(t, requeueWithHealthCheckInterval(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigInProgress(t, updated)
		hist := findConfigStageHistory(t, updated)
		if assert.NotNil(t, hist) {
			assert.False(t, hist.StartTime.IsZero())
			assert.True(t, hist.CompletionTime.IsZero())
		}
		inProg := controllerutils.GetIPInProgressCondition(updated, ipcv1.IPStages.Config)
		if assert.NotNil(t, inProg) {
			assert.Contains(t, inProg.Message, "Waiting for current IPs to match spec")
		}
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Config, ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
	})

	t.Run("disable init monitor failure => marks failed and returns error", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)
		mockReboot := reboot.NewMockRebootIntf(gc)

		ipc := mkConfigIPC(t, true)
		// Make statusIPsMatchSpec succeed (spec empty but status must be populated).
		ipc.Status.Network = &ipcv1.NetworkStatus{
			HostNetwork:    &ipcv1.HostNetworkStatus{},
			ClusterNetwork: &ipcv1.ClusterNetworkStatus{},
		}
		k8sClient := newFakeClientWithStatus(t, scheme, ipc)

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error { return nil }

		mockReboot.EXPECT().DisableInitMonitor().Return(errors.New("disable failed")).Times(1)

		h := &IPCConfigTwoPhaseHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			RPMOstreeClient: mockRPM,
			OstreeClient:    mockOstree,
			ChrootOps:       mockOps,
			RebootClient:    mockReboot,
		}

		_, err := h.PostPivot(context.Background(), ipc, logger)
		assert.Error(t, err)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigFailed(t, updated)
		hist := findConfigStageHistory(t, updated)
		if assert.NotNil(t, hist) {
			assert.False(t, hist.StartTime.IsZero())
		}
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Config, ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
	})

	t.Run("success stops postpivot phase and does not requeue", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)
		mockReboot := reboot.NewMockRebootIntf(gc)

		ipc := mkConfigIPC(t, true)
		ipc.Status.Network = &ipcv1.NetworkStatus{
			HostNetwork:    &ipcv1.HostNetworkStatus{},
			ClusterNetwork: &ipcv1.ClusterNetworkStatus{},
		}
		k8sClient := newFakeClientWithStatus(t, scheme, ipc)

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error { return nil }

		mockReboot.EXPECT().DisableInitMonitor().Return(nil).Times(1)

		h := &IPCConfigTwoPhaseHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			RPMOstreeClient: mockRPM,
			OstreeClient:    mockOstree,
			ChrootOps:       mockOps,
			RebootClient:    mockReboot,
		}

		res, err := h.PostPivot(context.Background(), ipc, logger)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		phase := findConfigPhase(t, updated, IPConfigPhasePostPivot)
		if assert.NotNil(t, phase) {
			assert.False(t, phase.StartTime.IsZero())
			assert.False(t, phase.CompletionTime.IsZero())
		}
		hist := findConfigStageHistory(t, updated)
		if assert.NotNil(t, hist) {
			assert.False(t, hist.StartTime.IsZero())
			assert.True(t, hist.CompletionTime.IsZero())
		}
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Config, ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
	})
}

func TestIPCConfigStageHandler_Handle(t *testing.T) {
	scheme := newIPConfigTestScheme(t)
	ctx := context.Background()

	t.Run("transition requested but invalid next stage => status failed and no requeue", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)

		ipc := mkConfigIPC(t, true)
		ipc.Status.ValidNextStages = []ipcv1.IPConfigStage{ipcv1.IPStages.Idle} // exclude config => invalid

		k8sClient := newFakeClientWithStatus(t, scheme, ipc)
		tph := NewMockIPConfigTwoPhaseHandlerInterface(gc)
		stageHandler := NewIPCConfigStageHandler(k8sClient, k8sClient, mockRPM, mockOps, tph)

		res, err := stageHandler.Handle(ctx, ipc)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigFailed(t, updated)
		hist := findConfigStageHistory(t, updated)
		if assert.NotNil(t, hist) {
			assert.False(t, hist.StartTime.IsZero())
			assert.True(t, hist.CompletionTime.IsZero())
		}
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
	})

	t.Run("transition requested but SNO validation fails => status failed and no requeue", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)

		ipc := mkConfigIPC(t, true)
		ipc.Status.ValidNextStages = []ipcv1.IPConfigStage{ipcv1.IPStages.Config} // allow stage

		// No master node => validateSNO should fail.
		mc := &machineconfigv1.MachineConfig{ObjectMeta: metav1.ObjectMeta{Name: common.DnsmasqMachineConfigName}}
		k8sClient := newFakeClientWithStatus(t, scheme, ipc, mc)

		tph := NewMockIPConfigTwoPhaseHandlerInterface(gc)
		stageHandler := NewIPCConfigStageHandler(k8sClient, k8sClient, mockRPM, mockOps, tph)

		res, err := stageHandler.Handle(ctx, ipc)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigFailed(t, updated)
		hist := findConfigStageHistory(t, updated)
		if assert.NotNil(t, hist) {
			assert.False(t, hist.StartTime.IsZero())
			assert.True(t, hist.CompletionTime.IsZero())
		}
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Config}, updated.Status.ValidNextStages)
	})

	t.Run("stage not in progress => do not requeue", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)

		ipc := mkConfigIPC(t, true)
		controllerutils.SetIPConfigStatusCompleted(ipc, "done")
		k8sClient := newFakeClientWithStatus(t, scheme, ipc)

		tph := NewMockIPConfigTwoPhaseHandlerInterface(gc)
		stageHandler := NewIPCConfigStageHandler(k8sClient, k8sClient, mockRPM, mockOps, tph)

		res, err := stageHandler.Handle(ctx, ipc)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigCompleted(t, updated)
		hist := findConfigStageHistory(t, updated)
		if assert.NotNil(t, hist) {
			assert.False(t, hist.StartTime.IsZero())
		}
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Config, ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
	})

	t.Run("transition requested and validations pass (before pivot) => sets in-progress + idle false and runs prepivot", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)

		ipc := mkConfigIPC(t, true)
		ipc.Status.ValidNextStages = []ipcv1.IPConfigStage{ipcv1.IPStages.Config}

		node, mc := mkSNOObjects()
		k8sClient := newFakeClientWithStatus(t, scheme, ipc, node, mc)

		mockRPM.EXPECT().IsStaterootBooted("rhcos").Return(false, nil).Times(1)

		tph := NewMockIPConfigTwoPhaseHandlerInterface(gc)
		tph.EXPECT().
			PrePivot(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(requeueWithShortInterval(), nil).
			Times(1)

		stageHandler := NewIPCConfigStageHandler(k8sClient, k8sClient, mockRPM, mockOps, tph)
		res, err := stageHandler.Handle(ctx, ipc)
		assert.NoError(t, err)
		assert.Equal(t, requeueWithShortInterval(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigInProgress(t, updated)
		idle := controllerutils.GetIPInProgressCondition(updated, ipcv1.IPStages.Idle)
		if assert.NotNil(t, idle) {
			assert.Equal(t, metav1.ConditionFalse, idle.Status)
			assert.Equal(t, string(controllerutils.ConditionReasons.InProgress), idle.Reason)
		}
		hist := findConfigStageHistory(t, updated)
		if assert.NotNil(t, hist) {
			assert.False(t, hist.StartTime.IsZero())
			assert.True(t, hist.CompletionTime.IsZero())
		}
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Config}, updated.Status.ValidNextStages)
	})

	t.Run("booted target stateroot is false (before pivot) => runs prepivot handler", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)

		ipc := mkConfigIPC(t, true)
		controllerutils.SetIPConfigStatusInProgress(ipc, "Configuration is in progress")
		k8sClient := newFakeClientWithStatus(t, scheme, ipc)

		mockRPM.EXPECT().IsStaterootBooted("rhcos").Return(false, nil).Times(1)

		tph := NewMockIPConfigTwoPhaseHandlerInterface(gc)
		tph.EXPECT().
			PrePivot(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(requeueWithShortInterval(), nil).
			Times(1)

		stageHandler := NewIPCConfigStageHandler(k8sClient, k8sClient, mockRPM, mockOps, tph)
		res, err := stageHandler.Handle(ctx, ipc)
		assert.NoError(t, err)
		assert.Equal(t, requeueWithShortInterval(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigInProgress(t, updated)
		hist := findConfigStageHistory(t, updated)
		if assert.NotNil(t, hist) {
			assert.False(t, hist.StartTime.IsZero())
			assert.True(t, hist.CompletionTime.IsZero())
		}
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Config, ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
	})

	t.Run("after pivot => stops prepivot phase, runs postpivot, completes stage and config status", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)

		ipc := mkConfigIPC(t, true)
		// Pretend prepivot phase started earlier so StopIPPhase can close it.
		ipc.Status.History[0].Phases = []*ipcv1.IPPhase{{
			Phase:     IPConfigPhasePrePivot,
			StartTime: metav1.Now(),
		}}
		controllerutils.SetIPConfigStatusInProgress(ipc, "Configuration is in progress")

		node, mc := mkSNOObjects()
		k8sClient := newFakeClientWithStatus(t, scheme, ipc, node, mc)

		// Transition requested should still be false since we're already in progress; only boot check should be called.
		mockRPM.EXPECT().IsStaterootBooted("rhcos").Return(true, nil).Times(1)

		tph := NewMockIPConfigTwoPhaseHandlerInterface(gc)
		tph.EXPECT().
			PostPivot(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(doNotRequeue(), nil).
			Times(1)

		stageHandler := NewIPCConfigStageHandler(k8sClient, k8sClient, mockRPM, mockOps, tph)
		res, err := stageHandler.Handle(ctx, ipc)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigCompleted(t, updated)
		hist := findConfigStageHistory(t, updated)
		if assert.NotNil(t, hist) {
			assert.False(t, hist.StartTime.IsZero())
			assert.False(t, hist.CompletionTime.IsZero())
		}
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Config, ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)

		// prepivot phase should be marked completed
		phase := findConfigPhase(t, updated, IPConfigPhasePrePivot)
		if assert.NotNil(t, phase) {
			assert.False(t, phase.CompletionTime.IsZero())
		}
		// stage completion time should be set
		if assert.Len(t, updated.Status.History, 1) {
			assert.Equal(t, ipcv1.IPStages.Config, updated.Status.History[0].Stage)
			assert.False(t, updated.Status.History[0].CompletionTime.IsZero())
		}
	})

	t.Run("prepivot error bubbles up as error with returned result", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)

		ipc := mkConfigIPC(t, true)
		controllerutils.SetIPConfigStatusInProgress(ipc, "Configuration is in progress")
		k8sClient := newFakeClientWithStatus(t, scheme, ipc)

		mockRPM.EXPECT().IsStaterootBooted("rhcos").Return(false, nil).Times(1)

		tph := NewMockIPConfigTwoPhaseHandlerInterface(gc)
		tph.EXPECT().
			PrePivot(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(requeueWithShortInterval(), fmt.Errorf("prepivot failed")).
			Times(1)

		stageHandler := NewIPCConfigStageHandler(k8sClient, k8sClient, mockRPM, mockOps, tph)
		res, err := stageHandler.Handle(ctx, ipc)
		assert.Error(t, err)
		assert.Equal(t, requeueWithShortInterval(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigInProgress(t, updated)
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Config, ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
	})
}

func TestStatusIPsMatchSpec(t *testing.T) {
	t.Run("missing network status => error", func(t *testing.T) {
		ipc := mkConfigIPC(t, false)
		err := statusIPsMatchSpec(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not yet populated")
	})

	t.Run("dnsResolutionFamily mismatch => error includes detail", func(t *testing.T) {
		ipc := mkConfigIPC(t, false)
		ipc.Spec.DNSResolutionFamily = "ipv4"
		ipc.Status.DNSResolutionFamily = "ipv6"
		ipc.Status.Network = &ipcv1.NetworkStatus{
			HostNetwork:    &ipcv1.HostNetworkStatus{},
			ClusterNetwork: &ipcv1.ClusterNetworkStatus{},
		}
		err := statusIPsMatchSpec(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "dnsResolutionFamily mismatch")
	})

	t.Run("vlan mismatch => error includes detail", func(t *testing.T) {
		ipc := mkConfigIPC(t, false)
		ipc.Spec.VLAN = &ipcv1.VLANConfig{ID: 100}
		ipc.Status.Network = &ipcv1.NetworkStatus{
			HostNetwork:    &ipcv1.HostNetworkStatus{VLANID: 200},
			ClusterNetwork: &ipcv1.ClusterNetworkStatus{},
		}
		err := statusIPsMatchSpec(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "vlan mismatch")
	})

	t.Run("ipv6 match => no error", func(t *testing.T) {
		ipc := mkConfigIPC(t, false)
		ipc.Spec.IPv6 = &ipcv1.IPv6Config{
			Address:        "2001:db8::10/64",
			MachineNetwork: "2001:db8::/64",
			Gateway:        "fe80::1",
			DNSServer:      "2001:db8::53",
		}
		ipc.Status.Network = &ipcv1.NetworkStatus{
			HostNetwork: &ipcv1.HostNetworkStatus{
				IPv6: &ipcv1.HostIPStatus{
					Gateway:   "fe80::1",
					DNSServer: "2001:db8::53",
				},
			},
			ClusterNetwork: &ipcv1.ClusterNetworkStatus{
				IPv6: &ipcv1.ClusterIPStatus{
					Address:        "2001:db8::10",
					MachineNetwork: "2001:db8::/64",
				},
			},
		}
		assert.NoError(t, statusIPsMatchSpec(ipc))
	})
}

func TestIPAndCIDRHelpers(t *testing.T) {
	t.Run("ipEqual normalizes CIDR", func(t *testing.T) {
		assert.True(t, ipEqual("192.0.2.10/24", "192.0.2.10"))
		assert.True(t, ipEqual("2001:db8::1", "2001:db8::1"))
		assert.False(t, ipEqual("192.0.2.10", "192.0.2.11"))
	})

	t.Run("cidrEqual compares prefixes and IPs", func(t *testing.T) {
		assert.True(t, cidrEqual("192.0.2.0/24", "192.0.2.0/24"))
		assert.False(t, cidrEqual("192.0.2.0/24", "192.0.2.0/25"))
		assert.True(t, cidrEqual("2001:db8::/64", "2001:db8::/64"))
	})

	t.Run("validateFamilyAddressChanges blocks dependent changes without address change", func(t *testing.T) {
		host := &ipcv1.HostIPStatus{Gateway: "192.0.2.1", DNSServer: "192.0.2.53"}
		cluster := &ipcv1.ClusterIPStatus{Address: "192.0.2.10", MachineNetwork: "192.0.2.0/24"}

		// Address same, machineNetwork change => error
		err := validateFamilyAddressChanges("ipv4", "192.0.2.10", "192.0.3.0/24", "", "", host, cluster)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "machineNetwork can be changed only if address is also changed")

		// Address same, gateway change => error
		err = validateFamilyAddressChanges("ipv4", "192.0.2.10", "", "192.0.2.254", "", host, cluster)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "gateway can be changed only if address is also changed")

		// Address same, dns change => error
		err = validateFamilyAddressChanges("ipv4", "192.0.2.10", "", "", "192.0.2.54", host, cluster)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "dnsServer can be changed only if address is also changed")

		// Address changed => allowed
		assert.NoError(t, validateFamilyAddressChanges("ipv4", "192.0.2.11", "192.0.3.0/24", "192.0.2.254", "192.0.2.54", host, cluster))
	})
}
