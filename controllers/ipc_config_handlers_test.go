package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
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
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
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
	if err := ibuv1.AddToScheme(s); err != nil {
		t.Fatalf("failed to add ibu scheme: %v", err)
	}
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

func mkIBU(t *testing.T, stage ibuv1.ImageBasedUpgradeStage, idleConditionTrue bool) *ibuv1.ImageBasedUpgrade {
	t.Helper()
	ibu := &ibuv1.ImageBasedUpgrade{
		ObjectMeta: metav1.ObjectMeta{
			Name:       controllerutils.IBUName,
			Generation: 1,
		},
		Spec: ibuv1.ImageBasedUpgradeSpec{
			Stage: stage,
		},
	}
	if idleConditionTrue {
		controllerutils.ResetStatusConditions(&ibu.Status.Conditions, ibu.Generation)
	}
	return ibu
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

func assertConfigInvalidTransition(t *testing.T, ipc *ipcv1.IPConfig) {
	t.Helper()
	inProg := controllerutils.GetIPInProgressCondition(ipc, ipcv1.IPStages.Config)
	comp := controllerutils.GetIPCompletedCondition(ipc, ipcv1.IPStages.Config)
	if assert.NotNil(t, inProg) {
		assert.Equal(t, metav1.ConditionFalse, inProg.Status)
		assert.Equal(t, string(controllerutils.ConditionReasons.InvalidTransition), inProg.Reason)
	}
	assert.Nil(t, comp, "invalid transition should not set completed condition")
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
		// statusIPsMatchSpec requires status to be populated even if spec is empty.
		ipc.Status.IPv4 = &ipcv1.IPv4Status{}
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

	t.Run("skip healthcheck annotation => bypasses healthcheck failure and continues", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)
		mockReboot := reboot.NewMockRebootIntf(gc)

		ipc := mkConfigIPC(t, true)
		ipc.SetAnnotations(map[string]string{controllerutils.SkipIPConfigPreConfigurationClusterHealthChecksAnnotation: ""})
		// Force statusIPsMatchSpec to return error by leaving status network unpopulated.
		k8sClient := newFakeClientWithStatus(t, scheme, ipc)

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		called := false
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error {
			called = true
			return errors.New("not healthy")
		}

		// If health checks are skipped, we should proceed to copy lca-cli (and fail there).
		mockOps.EXPECT().CopyFile(gomock.Any(), gomock.Any(), gomock.Any()).Return(errors.New("copy failed")).Times(1)

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
		assert.False(t, called, "CheckHealth should not be called when skip annotation is set")
		assert.Equal(t, ctrl.Result{}, res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigFailed(t, updated)
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
		// Intentionally provide only a partial spec; the controller should backfill
		// missing values from status when writing the pre-pivot config file.
		ipc.Spec.IPv4 = &ipcv1.IPv4Config{
			Address: "192.0.2.20",
		}
		ipc.Spec.IPv6 = nil
		ipc.Status.IPv4 = &ipcv1.IPv4Status{
			Address:        "192.0.2.10",
			MachineNetwork: "192.0.2.0/24",
			Gateway:        "192.0.2.1",
		}
		ipc.Status.IPv6 = &ipcv1.IPv6Status{
			Address:        "2001:db8::10",
			MachineNetwork: "2001:db8::/64",
			Gateway:        "2001:db8::1",
		}
		ipc.Status.DNSServers = []string{"192.0.2.53", "2001:db8::53"}
		ipc.Status.VLANID = 123
		ipc.Status.DNSFilterOutFamily = "ipv6"
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
					if filepath.Clean(filename) == filepath.Clean(common.PathOutsideChroot(common.IPConfigPrePivotFlagsFile)) {
						var got common.IPConfigPrePivotConfig
						assert.NoError(t, json.Unmarshal(data, &got))

						// IPv4: address from spec, everything else from status.
						assert.Equal(t, "192.0.2.20", got.IPv4Address)
						assert.Equal(t, "192.0.2.0/24", got.IPv4MachineNetwork)
						assert.Equal(t, "192.0.2.1", got.DesiredIPv4Gateway)
						assert.Equal(t, "192.0.2.1", got.CurrentIPv4Gateway)

						// IPv6: fully backfilled from status (spec omitted IPv6 entirely).
						assert.Equal(t, "2001:db8::10", got.IPv6Address)
						assert.Equal(t, "2001:db8::/64", got.IPv6MachineNetwork)
						assert.Equal(t, "2001:db8::1", got.DesiredIPv6Gateway)
						assert.Equal(t, "2001:db8::1", got.CurrentIPv6Gateway)
						// dnsServers is not backfilled anymore.
						assert.Nil(t, got.DNSServers)

						// VLAN: backfilled from status.
						assert.Equal(t, 123, got.VLANID)
						// dnsFilterOutFamily is not backfilled anymore.
						assert.Equal(t, "", got.DNSFilterOutFamily)
					}
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

	t.Run("skip healthcheck annotation => does not call CheckHealth and proceeds", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)
		mockReboot := reboot.NewMockRebootIntf(gc)

		ipc := mkConfigIPC(t, true)
		// PostPivot now best-effort persists rollback availability expiration; keep it non-zero to avoid filesystem-dependent logic.
		ipc.Status.RollbackAvailabilityExpiration = metav1.Now()
		ipc.SetAnnotations(map[string]string{controllerutils.SkipIPConfigPostConfigurationClusterHealthChecksAnnotation: ""})
		// Make statusIPsMatchSpec succeed (spec empty but status must be populated).
		ipc.Status.IPv4 = &ipcv1.IPv4Status{}

		k8sClient := newFakeClientWithStatus(t, scheme, ipc)

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		called := false
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error {
			called = true
			return errors.New("not healthy")
		}

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
		assert.False(t, called, "CheckHealth should not be called when skip annotation is set")
		assert.Equal(t, doNotRequeue(), res)
	})

	t.Run("pre skip annotation does not skip post-pivot health checks", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)
		mockReboot := reboot.NewMockRebootIntf(gc)

		ipc := mkConfigIPC(t, true)
		// PostPivot now best-effort persists rollback availability expiration; keep it non-zero to avoid filesystem-dependent logic.
		ipc.Status.RollbackAvailabilityExpiration = metav1.Now()
		ipc.SetAnnotations(map[string]string{controllerutils.SkipIPConfigPreConfigurationClusterHealthChecksAnnotation: ""})
		// Make statusIPsMatchSpec succeed (spec empty but status must be populated).
		ipc.Status.IPv4 = &ipcv1.IPv4Status{}

		k8sClient := newFakeClientWithStatus(t, scheme, ipc)

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		called := false
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error {
			called = true
			return nil
		}

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
		assert.True(t, called, "CheckHealth should be called when only pre-skip annotation is set")
		assert.Equal(t, doNotRequeue(), res)
	})

	t.Run("healthcheck failing => updates in-progress and requeues", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)
		mockReboot := reboot.NewMockRebootIntf(gc)

		ipc := mkConfigIPC(t, true)
		// PostPivot now best-effort persists rollback availability expiration; keep it non-zero to avoid filesystem-dependent logic.
		ipc.Status.RollbackAvailabilityExpiration = metav1.Now()
		stuck := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "stuck-imagepullbackoff",
				Namespace: "default",
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: "c",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: controllerutils.PodContainerWaitingReasonImagePullBackOff,
						},
					},
				}},
			},
		}
		notStuck := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "not-stuck",
				Namespace: "default",
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: "c",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "CrashLoopBackOff",
						},
					},
				}},
			},
		}
		// Mirror pod (static pod mirror).
		mirrorStuck := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mirror-stuck",
				Namespace: "openshift-kube-apiserver",
				Annotations: map[string]string{
					corev1.MirrorPodAnnotationKey: "mirror",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: "c",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: controllerutils.PodContainerWaitingReasonImagePullBackOff,
						},
					},
				}},
			},
		}

		k8sClient := newFakeClientWithStatus(t, scheme, ipc, stuck, notStuck, mirrorStuck)

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error { return errors.New("not healthy") }

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
		assert.Equal(t, requeueWithHealthCheckInterval(), res)

		// Stuck ImagePullBackOff pods are deleted best-effort (except static pod mirror pods).
		err = k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "stuck-imagepullbackoff"}, &corev1.Pod{})
		assert.True(t, k8serrors.IsNotFound(err), "expected stuck pod to be deleted")
		assert.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "not-stuck"}, &corev1.Pod{}))
		assert.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "openshift-kube-apiserver", Name: "mirror-stuck"}, &corev1.Pod{}))

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
		// PostPivot now best-effort persists rollback availability expiration; keep it non-zero to avoid filesystem-dependent logic.
		ipc.Status.RollbackAvailabilityExpiration = metav1.Now()
		ipc.Spec.IPv4 = &ipcv1.IPv4Config{
			Address:        "192.0.2.11/24",
			MachineNetwork: "192.0.2.0/24",
			Gateway:        "192.0.2.1",
		}
		ipc.Spec.DNSServers = []ipcv1.IPAddress{"192.0.2.53"}
		ipc.Status.IPv4 = &ipcv1.IPv4Status{
			Address:        "192.0.2.99",   // mismatch
			MachineNetwork: "192.0.2.0/24", // match
			Gateway:        "192.0.2.1",
		}
		ipc.Status.DNSServers = []string{"192.0.2.53"}
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
		// PostPivot now best-effort persists rollback availability expiration; keep it non-zero to avoid filesystem-dependent logic.
		ipc.Status.RollbackAvailabilityExpiration = metav1.Now()
		// Make statusIPsMatchSpec succeed (spec empty but status must be populated).
		ipc.Status.IPv4 = &ipcv1.IPv4Status{}
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
		// PostPivot now best-effort persists rollback availability expiration; keep it non-zero to avoid filesystem-dependent logic.
		ipc.Status.RollbackAvailabilityExpiration = metav1.Now()
		ipc.Status.IPv4 = &ipcv1.IPv4Status{}
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

	t.Run("healthcheck failing with client that would fail deletes => still requeues and does not error", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)
		mockReboot := reboot.NewMockRebootIntf(gc)

		ipc := mkConfigIPC(t, true)
		// PostPivot now best-effort persists rollback availability expiration; keep it non-zero to avoid filesystem-dependent logic.
		ipc.Status.RollbackAvailabilityExpiration = metav1.Now()
		stuck := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "stuck-delete-fails",
				Namespace: "default",
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: "c",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: controllerutils.PodContainerWaitingReasonErrImagePull,
						},
					},
				}},
			},
		}
		baseClient := newFakeClientWithStatus(t, scheme, ipc, stuck)
		k8sClient := &reconcileTestDeleteErrClient{
			Client:   baseClient,
			failName: "stuck-delete-fails",
			failNS:   "default",
			err:      fmt.Errorf("delete failed"),
		}

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error { return errors.New("not healthy") }

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
		assert.Equal(t, requeueWithHealthCheckInterval(), res)

		// Ensure we attempted deletion but still requeue and do not error when deletion fails.
		assert.True(t, k8sClient.called, "expected delete to be attempted")
		assert.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "stuck-delete-fails"}, &corev1.Pod{}))

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigInProgress(t, updated)
		inProg := controllerutils.GetIPInProgressCondition(updated, ipcv1.IPStages.Config)
		if assert.NotNil(t, inProg) {
			assert.Contains(t, inProg.Message, "Waiting for system to stabilize")
		}
	})
}

type reconcileTestDeleteErrClient struct {
	client.Client
	failName string
	failNS   string
	err      error
	called   bool
}

func (c *reconcileTestDeleteErrClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if obj != nil && obj.GetName() == c.failName && obj.GetNamespace() == c.failNS {
		c.called = true
		return c.err
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func TestIPCConfigStageHandler_Handle(t *testing.T) {
	scheme := newIPConfigTestScheme(t)
	ctx := context.Background()

	t.Run("transition requested but invalid next stage => status invalidTransition, then becomes valid and proceeds", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)

		ipc := mkConfigIPC(t, true)
		ipc.Status.ValidNextStages = []ipcv1.IPConfigStage{ipcv1.IPStages.Idle} // exclude config => invalid

		ibu := mkIBU(t, ibuv1.Stages.Idle, true)
		node, mc := mkSNOObjects()
		k8sClient := newFakeClientWithStatus(t, scheme, ipc, ibu, node, mc)
		tph := NewMockIPConfigTwoPhaseHandlerInterface(gc)
		stageHandler := NewIPCConfigStageHandler(k8sClient, k8sClient, mockRPM, mockOps, tph)

		res, err := stageHandler.Handle(ctx, ipc)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigInvalidTransition(t, updated)
		hist := findConfigStageHistory(t, updated)
		if assert.NotNil(t, hist) {
			assert.False(t, hist.StartTime.IsZero())
			assert.True(t, hist.CompletionTime.IsZero())
		}
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)

		// Now the stage becomes valid: allow Config, then ensure the next reconcile proceeds and overwrites
		// the previous InvalidTransition condition with a regular in-progress status.
		updated.Status.ValidNextStages = []ipcv1.IPConfigStage{ipcv1.IPStages.Config}
		assert.NoError(t, k8sClient.Status().Update(ctx, updated))

		mockOps.EXPECT().
			RunInHostNamespace("nmstatectl", "show", "--json", "-q").
			Return(`{"interfaces":[{"name":"br-ex","type":"ovs-interface","bridge":{"port":[{"name":"ens3"},{"name":"patch-br-ex"}]}},{"name":"ens3","type":"ethernet","ipv4":{"enabled":true,"dhcp":false,"address":[{"ip":"192.0.2.10","prefix-length":24}]},"ipv6":{"enabled":false,"dhcp":false,"autoconf":false,"address":[]}}],"routes":{"running":[], "config":[]},"dns-resolver":{"running":{"server":[]},"config":{"server":[]}}}`, nil).
			Times(1)

		mockRPM.EXPECT().IsStaterootBooted("rhcos").Return(false, nil).Times(1)
		mockRPM.EXPECT().GetUnbootedStaterootName().Return("some-unbooted", nil).Times(1)
		tph.EXPECT().
			PrePivot(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(requeueWithShortInterval(), nil).
			Times(1)

		res2, err2 := stageHandler.Handle(ctx, updated)
		assert.NoError(t, err2)
		assert.Equal(t, requeueWithShortInterval(), res2)

		updated2 := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigInProgress(t, updated2)
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
		ibu := mkIBU(t, ibuv1.Stages.Idle, true)
		k8sClient := newFakeClientWithStatus(t, scheme, ipc, ibu, mc)

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

	t.Run("transition requested but static networking validation fails (DHCP enabled) => status failed and no requeue", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRPM := rpmostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)

		ipc := mkConfigIPC(t, true)
		ipc.Status.ValidNextStages = []ipcv1.IPConfigStage{ipcv1.IPStages.Config} // allow stage

		ibu := mkIBU(t, ibuv1.Stages.Idle, true)
		node, mc := mkSNOObjects()
		k8sClient := newFakeClientWithStatus(t, scheme, ipc, ibu, node, mc)

		// DHCP enabled on br-ex uplink => should fail validation.
		mockOps.EXPECT().
			RunInHostNamespace("nmstatectl", "show", "--json", "-q").
			Return(`{"interfaces":[{"name":"br-ex","type":"ovs-interface","bridge":{"port":[{"name":"ens3"},{"name":"patch-br-ex"}]},"ipv4":{"enabled":true,"dhcp":true,"address":[{"ip":"192.0.2.10","prefix-length":24}]}},{"name":"ens3","type":"ethernet","ipv4":{"enabled":true,"dhcp":false,"address":[{"ip":"192.0.2.10","prefix-length":24}]},"ipv6":{"enabled":false,"dhcp":false,"autoconf":false,"address":[]}}],"routes":{"running":[], "config":[]},"dns-resolver":{"running":{"server":[]},"config":{"server":[]}}}`, nil).
			Times(1)

		tph := NewMockIPConfigTwoPhaseHandlerInterface(gc)
		stageHandler := NewIPCConfigStageHandler(k8sClient, k8sClient, mockRPM, mockOps, tph)

		res, err := stageHandler.Handle(ctx, ipc)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assertConfigFailed(t, updated)
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

		ibu := mkIBU(t, ibuv1.Stages.Idle, true)
		node, mc := mkSNOObjects()
		k8sClient := newFakeClientWithStatus(t, scheme, ipc, ibu, node, mc)

		mockOps.EXPECT().
			RunInHostNamespace("nmstatectl", "show", "--json", "-q").
			Return(`{"interfaces":[{"name":"br-ex","type":"ovs-interface","bridge":{"port":[{"name":"ens3"},{"name":"patch-br-ex"}]}},{"name":"ens3","type":"ethernet","ipv4":{"enabled":true,"dhcp":false,"address":[{"ip":"192.0.2.10","prefix-length":24}]},"ipv6":{"enabled":false,"dhcp":false,"autoconf":false,"address":[]}}],"routes":{"running":[], "config":[]},"dns-resolver":{"running":{"server":[]},"config":{"server":[]}}}`, nil).
			Times(1)

		mockRPM.EXPECT().IsStaterootBooted("rhcos").Return(false, nil).Times(1)
		mockRPM.EXPECT().GetUnbootedStaterootName().Return("some-unbooted", nil).Times(1)

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
			assert.Equal(t, string(controllerutils.ConditionReasons.ConfigurationInProgress), idle.Reason)
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
		mockRPM.EXPECT().GetUnbootedStaterootName().Return("some-unbooted", nil).Times(1)

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
		mockRPM.EXPECT().GetUnbootedStaterootName().Return("some-unbooted", nil).Times(1)

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
		mockRPM.EXPECT().GetUnbootedStaterootName().Return("some-unbooted", nil).Times(1)

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

	t.Run("dnsFilterOutFamily mismatch => error includes detail", func(t *testing.T) {
		ipc := mkConfigIPC(t, false)
		ipc.Spec.DNSFilterOutFamily = "ipv4"
		ipc.Status.DNSFilterOutFamily = "ipv6"
		ipc.Status.IPv4 = &ipcv1.IPv4Status{}
		err := statusIPsMatchSpec(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "dnsFilterOutFamily mismatch")
	})

	t.Run("vlan mismatch => error includes detail", func(t *testing.T) {
		ipc := mkConfigIPC(t, false)
		ipc.Spec.VLANID = 100
		ipc.Status.VLANID = 200
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
		}
		ipc.Spec.DNSServers = []ipcv1.IPAddress{"2001:db8::53"}
		ipc.Status.IPv6 = &ipcv1.IPv6Status{
			Address:        "2001:db8::10",
			MachineNetwork: "2001:db8::/64",
			Gateway:        "fe80::1",
		}
		ipc.Status.DNSServers = []string{"2001:db8::53"}
		assert.NoError(t, statusIPsMatchSpec(ipc))
	})
}

func TestValidateAddressChanges(t *testing.T) {
	t.Run("nil status network returns nil", func(t *testing.T) {
		ipc := mkConfigIPC(t, false)
		ipc.Spec.IPv4 = &ipcv1.IPv4Config{Address: "192.0.2.10"}
		assert.NoError(t, validateAddressChanges(ipc))
	})

	t.Run("IPv4 validation error is wrapped", func(t *testing.T) {
		ipc := mkConfigIPC(t, false)
		ipc.Spec.IPv4 = &ipcv1.IPv4Config{
			Address:        "192.0.2.10",
			MachineNetwork: "192.0.3.0/24",
		}
		ipc.Status.IPv4 = &ipcv1.IPv4Status{Address: "192.0.2.10", MachineNetwork: "192.0.2.0/24"}
		err := validateAddressChanges(ipc)
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "failed to validate IPv4 address changes:")
			assert.Contains(t, err.Error(), "machineNetwork can be changed only if address is also changed")
		}
	})

	t.Run("IPv6 validation error is wrapped", func(t *testing.T) {
		ipc := mkConfigIPC(t, false)
		ipc.Spec.IPv6 = &ipcv1.IPv6Config{
			Address: "2001:db8::10",
			Gateway: "fe80::2",
		}
		ipc.Status.IPv6 = &ipcv1.IPv6Status{Address: "2001:db8::10", Gateway: "fe80::1"}
		err := validateAddressChanges(ipc)
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "failed to validate IPv6 address changes:")
		}
	})

	t.Run("both families valid returns nil", func(t *testing.T) {
		ipc := mkConfigIPC(t, false)
		ipc.Spec.IPv4 = &ipcv1.IPv4Config{Address: "192.0.2.11"}
		ipc.Status.IPv4 = &ipcv1.IPv4Status{Address: "192.0.2.10"}
		assert.NoError(t, validateAddressChanges(ipc))
	})

	t.Run("nil IPC returns nil", func(t *testing.T) {
		assert.NoError(t, validateAddressChanges(nil))
	})

	t.Run("equal DNS servers returns nil early", func(t *testing.T) {
		ipc := mkConfigIPC(t, false)
		ipc.Spec.IPv4 = &ipcv1.IPv4Config{Address: "192.0.2.10"}
		ipc.Status.IPv4 = &ipcv1.IPv4Status{Address: "192.0.2.10"}
		ipc.Spec.DNSServers = []ipcv1.IPAddress{"192.0.2.53"}
		ipc.Status.DNSServers = []string{"192.0.2.53"}
		assert.NoError(t, validateAddressChanges(ipc))
	})

	t.Run("IPv6 address change allows DNS server change", func(t *testing.T) {
		ipc := mkConfigIPC(t, false)
		ipc.Spec.IPv6 = &ipcv1.IPv6Config{Address: "2001:db8::11"}
		ipc.Status.IPv6 = &ipcv1.IPv6Status{Address: "2001:db8::10"}
		ipc.Spec.DNSServers = []ipcv1.IPAddress{"2001:db8::53"}
		ipc.Status.DNSServers = []string{"2001:db8::52"}
		assert.NoError(t, validateAddressChanges(ipc))
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
		status := &ipcv1.IPv4Status{
			Address:        "192.0.2.10",
			MachineNetwork: "192.0.2.0/24",
			Gateway:        "192.0.2.1",
		}

		// Address same, machineNetwork change => error
		err := validateFamilyAddressChanges(common.IPv4FamilyName, &ipcv1.IPv4Config{Address: "192.0.2.10", MachineNetwork: "192.0.3.0/24"}, status)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "machineNetwork can be changed only if address is also changed")

		// Address same, gateway change => error
		err = validateFamilyAddressChanges(common.IPv4FamilyName, &ipcv1.IPv4Config{Address: "192.0.2.10", Gateway: "192.0.2.254"}, status)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "gateway can be changed only if address is also changed")

		// Address changed => allowed
		assert.NoError(t, validateFamilyAddressChanges(common.IPv4FamilyName, &ipcv1.IPv4Config{
			Address:        "192.0.2.11",
			MachineNetwork: "192.0.3.0/24",
			Gateway:        "192.0.2.254",
		}, status))
	})

	t.Run("validateAddressChanges blocks dnsServers changes without any address change", func(t *testing.T) {
		ipc := mkConfigIPC(t, false)
		ipc.Spec.IPv4 = &ipcv1.IPv4Config{Address: "192.0.2.10"}
		ipc.Status.IPv4 = &ipcv1.IPv4Status{Address: "192.0.2.10"}
		ipc.Spec.DNSServers = []ipcv1.IPAddress{"192.0.2.54"}
		ipc.Status.DNSServers = []string{"192.0.2.53"}

		err := validateAddressChanges(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "dnsServers can be changed only if address is also changed")

		// Now change address too => allowed
		ipc.Spec.IPv4.Address = "192.0.2.11"
		assert.NoError(t, validateAddressChanges(ipc))
	})
}

func TestValidateClusterAndNetworkSpecCompatability_DNSServerFamilyChecks(t *testing.T) {
	scheme := newIPConfigTestScheme(t)
	ctx := context.Background()

	t.Run("single-stack IPv4 cluster allows IPv4 dnsServers", func(t *testing.T) {
		nodeV4 := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "master-0", Labels: map[string]string{"node-role.kubernetes.io/master": ""}},
			Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.10"}}},
		}
		mc := &machineconfigv1.MachineConfig{ObjectMeta: metav1.ObjectMeta{Name: common.DnsmasqMachineConfigName}}

		ipc := mkConfigIPC(t, false)
		ipc.Spec.IPv4 = &ipcv1.IPv4Config{Address: "192.0.2.20"}
		ipc.Spec.DNSServers = []ipcv1.IPAddress{"192.0.2.53"}

		k8sClient := newFakeClientWithStatus(t, scheme, ipc, nodeV4, mc)
		h := &IPCConfigStageHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
		}

		assert.NoError(t, h.validateClusterAndNetworkSpecCompatability(ctx, ipc))
	})

	t.Run("single-stack IPv4 cluster rejects IPv6 dnsServers", func(t *testing.T) {
		nodeV4 := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "master-0", Labels: map[string]string{"node-role.kubernetes.io/master": ""}},
			Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.10"}}},
		}
		mc := &machineconfigv1.MachineConfig{ObjectMeta: metav1.ObjectMeta{Name: common.DnsmasqMachineConfigName}}

		ipc := mkConfigIPC(t, false)
		ipc.Spec.IPv4 = &ipcv1.IPv4Config{Address: "192.0.2.20"}
		ipc.Spec.DNSServers = []ipcv1.IPAddress{"2001:db8::53"}

		k8sClient := newFakeClientWithStatus(t, scheme, ipc, nodeV4, mc)
		h := &IPCConfigStageHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
		}

		err := h.validateClusterAndNetworkSpecCompatability(ctx, ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "cluster does not have IPv6")
	})
}

func TestValidateClusterAndNetworkSpecCompatability_DNSFilterOutFamilyRequiresDualStack(t *testing.T) {
	scheme := newIPConfigTestScheme(t)
	ctx := context.Background()

	t.Run("dnsFilterOutFamily set on single-stack => error", func(t *testing.T) {
		nodeV4 := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "master-0", Labels: map[string]string{"node-role.kubernetes.io/master": ""}},
			Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.10"}}},
		}
		mc := &machineconfigv1.MachineConfig{ObjectMeta: metav1.ObjectMeta{Name: common.DnsmasqMachineConfigName}}

		ipc := mkConfigIPC(t, false)
		ipc.Spec.IPv4 = &ipcv1.IPv4Config{Address: "192.0.2.20"}
		ipc.Spec.DNSFilterOutFamily = common.IPv4FamilyName

		k8sClient := newFakeClientWithStatus(t, scheme, ipc, nodeV4, mc)
		h := &IPCConfigStageHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
		}

		err := h.validateClusterAndNetworkSpecCompatability(ctx, ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "dual-stack")
	})
}

func Test_ipEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical v4", "192.168.1.1", "192.168.1.1", true},
		{"different v4", "192.168.1.1", "192.168.1.2", false},
		{"identical v6", "fd00::1", "fd00::1", true},
		{"v6 normalized vs expanded", "fd00::1", "fd00:0000:0000:0000:0000:0000:0000:0001", true},
		{"different v6", "fd00::1", "fd00::2", false},
		{"v4 with CIDR stripped", "192.168.1.1/24", "192.168.1.1", true},
		{"both with CIDR", "10.0.0.1/16", "10.0.0.1/24", true},
		{"different IPs with CIDR", "10.0.0.1/24", "10.0.0.2/24", false},
		{"unparseable falls back to string match", "not-an-ip", "not-an-ip", true},
		{"unparseable different", "not-an-ip", "other", false},
		{"empty strings", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ipEqual(tt.a, tt.b))
		})
	}
}

func Test_cidrEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical v4 CIDR", "192.168.1.0/24", "192.168.1.0/24", true},
		{"same network different notation", "10.0.0.0/8", "10.0.0.0/8", true},
		{"different prefix length", "192.168.1.0/24", "192.168.1.0/16", false},
		{"different network", "10.0.0.0/24", "10.0.1.0/24", false},
		{"v6 CIDR identical", "fd00::/64", "fd00::/64", true},
		{"v6 CIDR different prefix", "fd00::/64", "fd00::/48", false},
		{"unparseable falls back to string", "bogus", "bogus", true},
		{"unparseable different", "bogus", "other", false},
		{"one parseable one not", "192.168.1.0/24", "bogus", false},
		{"whitespace trimmed", " 10.0.0.0/24 ", "10.0.0.0/24", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, cidrEqual(tt.a, tt.b))
		})
	}
}

func Test_parseCIDR(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantIP  string
		wantLen int
		wantErr bool
	}{
		{"v4 /24", "192.168.1.0/24", "192.168.1.0", 24, false},
		{"v4 /16", "10.0.0.0/16", "10.0.0.0", 16, false},
		{"v4 host masked", "192.168.1.100/24", "192.168.1.0", 24, false},
		{"v6 /64", "fd00::/64", "fd00::", 64, false},
		{"v6 /128", "::1/128", "::1", 128, false},
		{"whitespace", "  10.0.0.0/8  ", "10.0.0.0", 8, false},
		{"invalid", "not-cidr", "", 0, true},
		{"ip without prefix", "192.168.1.1", "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip, prefixLen, err := parseCIDR(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.wantIP, ip)
			assert.Equal(t, tt.wantLen, prefixLen)
		})
	}
}

func Test_statusIPsMatchSpec(t *testing.T) {
	t.Run("status not populated returns error", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{}
		err := statusIPsMatchSpec(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not yet populated")
	})

	t.Run("matching v4 spec and status", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				IPv4: &ipcv1.IPv4Config{
					Address:        "192.168.1.10/24",
					Gateway:        "192.168.1.1",
					MachineNetwork: "192.168.1.0/24",
				},
			},
			Status: ipcv1.IPConfigStatus{
				IPv4: &ipcv1.IPv4Status{
					Address:        "192.168.1.10",
					Gateway:        "192.168.1.1",
					MachineNetwork: "192.168.1.0/24",
				},
			},
		}
		assert.NoError(t, statusIPsMatchSpec(ipc))
	})

	t.Run("v4 address mismatch", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				IPv4: &ipcv1.IPv4Config{Address: "192.168.1.10"},
			},
			Status: ipcv1.IPConfigStatus{
				IPv4: &ipcv1.IPv4Status{Address: "192.168.1.20"},
			},
		}
		err := statusIPsMatchSpec(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "ipv4 address mismatch")
	})

	t.Run("v4 status nil", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				IPv4: &ipcv1.IPv4Config{Address: "192.168.1.10"},
			},
			Status: ipcv1.IPConfigStatus{
				IPv6: &ipcv1.IPv6Status{Address: "fd00::1"},
			},
		}
		err := statusIPsMatchSpec(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "ipv4 missing from status")
	})

	t.Run("dns filter out family mismatch", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				DNSFilterOutFamily: "ipv6",
				IPv4:               &ipcv1.IPv4Config{Address: "10.0.0.1"},
			},
			Status: ipcv1.IPConfigStatus{
				DNSFilterOutFamily: "ipv4",
				IPv4:               &ipcv1.IPv4Status{Address: "10.0.0.1"},
			},
		}
		err := statusIPsMatchSpec(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "dnsFilterOutFamily mismatch")
	})

	t.Run("vlan mismatch", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				VLANID: 100,
				IPv4:   &ipcv1.IPv4Config{Address: "10.0.0.1"},
			},
			Status: ipcv1.IPConfigStatus{
				VLANID: 200,
				IPv4:   &ipcv1.IPv4Status{Address: "10.0.0.1"},
			},
		}
		err := statusIPsMatchSpec(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "vlan mismatch")
	})

	t.Run("dns servers mismatch", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				DNSServers: []ipcv1.IPAddress{"8.8.8.8"},
				IPv4:       &ipcv1.IPv4Config{Address: "10.0.0.1"},
			},
			Status: ipcv1.IPConfigStatus{
				DNSServers: []string{"1.1.1.1"},
				IPv4:       &ipcv1.IPv4Status{Address: "10.0.0.1"},
			},
		}
		err := statusIPsMatchSpec(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "dnsServers mismatch")
	})

	t.Run("v6 gateway mismatch", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				IPv6: &ipcv1.IPv6Config{
					Address: "fd00::10",
					Gateway: "fd00::1",
				},
			},
			Status: ipcv1.IPConfigStatus{
				IPv6: &ipcv1.IPv6Status{
					Address: "fd00::10",
					Gateway: "fd00::ff",
				},
			},
		}
		err := statusIPsMatchSpec(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "ipv6 gateway mismatch")
	})

	t.Run("v6 machineNetwork mismatch", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				IPv6: &ipcv1.IPv6Config{
					Address:        "fd00::10",
					MachineNetwork: "fd00::/64",
				},
			},
			Status: ipcv1.IPConfigStatus{
				IPv6: &ipcv1.IPv6Status{
					Address:        "fd00::10",
					MachineNetwork: "fd01::/64",
				},
			},
		}
		err := statusIPsMatchSpec(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "ipv6 machineNetwork mismatch")
	})

	t.Run("v6 machineNetwork not observed", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				IPv6: &ipcv1.IPv6Config{
					Address:        "fd00::10",
					MachineNetwork: "fd00::/64",
				},
			},
			Status: ipcv1.IPConfigStatus{
				IPv6: &ipcv1.IPv6Status{
					Address: "fd00::10",
				},
			},
		}
		err := statusIPsMatchSpec(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "ipv6 machineNetwork not observed")
	})

	t.Run("v6 address missing from status", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				IPv6: &ipcv1.IPv6Config{Address: "fd00::10"},
			},
			Status: ipcv1.IPConfigStatus{
				IPv6: &ipcv1.IPv6Status{},
			},
		}
		err := statusIPsMatchSpec(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "ipv6 address missing from status")
	})

	t.Run("v4 machineNetwork not observed", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				IPv4: &ipcv1.IPv4Config{
					Address:        "192.168.1.10",
					MachineNetwork: "192.168.1.0/24",
				},
			},
			Status: ipcv1.IPConfigStatus{
				IPv4: &ipcv1.IPv4Status{
					Address: "192.168.1.10",
				},
			},
		}
		err := statusIPsMatchSpec(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "machineNetwork not observed")
	})

	t.Run("full dual-stack match", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				IPv4: &ipcv1.IPv4Config{
					Address:        "192.168.1.10",
					Gateway:        "192.168.1.1",
					MachineNetwork: "192.168.1.0/24",
				},
				IPv6: &ipcv1.IPv6Config{
					Address:        "fd00::10",
					Gateway:        "fd00::1",
					MachineNetwork: "fd00::/64",
				},
				VLANID:             100,
				DNSFilterOutFamily: "none",
				DNSServers:         []ipcv1.IPAddress{"8.8.8.8", "8.8.4.4"},
			},
			Status: ipcv1.IPConfigStatus{
				IPv4: &ipcv1.IPv4Status{
					Address:        "192.168.1.10",
					Gateway:        "192.168.1.1",
					MachineNetwork: "192.168.1.0/24",
				},
				IPv6: &ipcv1.IPv6Status{
					Address:        "fd00::10",
					Gateway:        "fd00::1",
					MachineNetwork: "fd00::/64",
				},
				VLANID:             100,
				DNSFilterOutFamily: "none",
				DNSServers:         []string{"8.8.8.8", "8.8.4.4"},
			},
		}
		assert.NoError(t, statusIPsMatchSpec(ipc))
	})
}

func Test_validateAddressChanges(t *testing.T) {
	t.Run("nil ipc returns nil", func(t *testing.T) {
		assert.NoError(t, validateAddressChanges(nil))
	})

	t.Run("no spec families returns nil", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{}
		assert.NoError(t, validateAddressChanges(ipc))
	})

	t.Run("dns change without address change rejected", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				IPv4:       &ipcv1.IPv4Config{Address: "10.0.0.1"},
				DNSServers: []ipcv1.IPAddress{"8.8.8.8"},
			},
			Status: ipcv1.IPConfigStatus{
				IPv4:       &ipcv1.IPv4Status{Address: "10.0.0.1"},
				DNSServers: []string{"1.1.1.1"},
			},
		}
		err := validateAddressChanges(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "dnsServers can be changed only if address is also changed")
	})

	t.Run("dns change with address change allowed", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				IPv4:       &ipcv1.IPv4Config{Address: "10.0.0.2"},
				DNSServers: []ipcv1.IPAddress{"8.8.8.8"},
			},
			Status: ipcv1.IPConfigStatus{
				IPv4:       &ipcv1.IPv4Status{Address: "10.0.0.1"},
				DNSServers: []string{"1.1.1.1"},
			},
		}
		assert.NoError(t, validateAddressChanges(ipc))
	})

	t.Run("v6 gateway change without address change rejected", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				IPv6: &ipcv1.IPv6Config{Address: "fd00::1", Gateway: "fd00::ff"},
			},
			Status: ipcv1.IPConfigStatus{
				IPv6: &ipcv1.IPv6Status{Address: "fd00::1", Gateway: "fd00::1"},
			},
		}
		err := validateAddressChanges(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "ipv6 gateway can be changed only if address is also changed")
	})

	t.Run("v6 machineNetwork change without address change rejected", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				IPv6: &ipcv1.IPv6Config{Address: "fd00::1", MachineNetwork: "fd01::/64"},
			},
			Status: ipcv1.IPConfigStatus{
				IPv6: &ipcv1.IPv6Status{Address: "fd00::1", MachineNetwork: "fd00::/64"},
			},
		}
		err := validateAddressChanges(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "ipv6 machineNetwork can be changed only if address is also changed")
	})

	t.Run("v6 machineNetwork change with address change allowed", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				IPv6: &ipcv1.IPv6Config{Address: "fd00::2", MachineNetwork: "fd01::/64"},
			},
			Status: ipcv1.IPConfigStatus{
				IPv6: &ipcv1.IPv6Status{Address: "fd00::1", MachineNetwork: "fd00::/64"},
			},
		}
		assert.NoError(t, validateAddressChanges(ipc))
	})

	t.Run("v4 gateway change without address change rejected", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				IPv4: &ipcv1.IPv4Config{Address: "10.0.0.1", Gateway: "10.0.0.254"},
			},
			Status: ipcv1.IPConfigStatus{
				IPv4: &ipcv1.IPv4Status{Address: "10.0.0.1", Gateway: "10.0.0.1"},
			},
		}
		err := validateAddressChanges(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "ipv4 gateway can be changed only if address is also changed")
	})

	t.Run("v4 machineNetwork change without address change rejected", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				IPv4: &ipcv1.IPv4Config{Address: "10.0.0.1", MachineNetwork: "10.0.1.0/24"},
			},
			Status: ipcv1.IPConfigStatus{
				IPv4: &ipcv1.IPv4Status{Address: "10.0.0.1", MachineNetwork: "10.0.0.0/24"},
			},
		}
		err := validateAddressChanges(ipc)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "ipv4 machineNetwork can be changed only if address is also changed")
	})

	t.Run("nil spec and status returns nil", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{
			Spec: ipcv1.IPConfigSpec{
				IPv4: &ipcv1.IPv4Config{Address: "10.0.0.1"},
			},
			Status: ipcv1.IPConfigStatus{},
		}
		assert.NoError(t, validateAddressChanges(ipc))
	})
}
