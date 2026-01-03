package controllers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	v1 "github.com/openshift-kni/lifecycle-agent/api/ipconfig/v1"
	controllerutils "github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1.AddToScheme(s); err != nil {
		t.Fatalf("failed to add ipconfig scheme: %v", err)
	}
	return s
}

func newFakeClientWithIPC(t *testing.T, s *runtime.Scheme, ipc *v1.IPConfig) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ipc).
		WithStatusSubresource(ipc).
		Build()
}

func mustGetIPC(t *testing.T, c client.Reader, name string) *v1.IPConfig {
	t.Helper()
	got := &v1.IPConfig{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, got); err != nil {
		t.Fatalf("failed to get ipconfig %q: %v", name, err)
	}
	return got
}

func mkRollbackIPC(t *testing.T, withHistory bool) *v1.IPConfig {
	t.Helper()
	ipc := &v1.IPConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       common.IPConfigName,
			Generation: 1,
		},
		Spec: v1.IPConfigSpec{
			Stage: v1.IPStages.Rollback,
		},
		Status: v1.IPConfigStatus{
			ValidNextStages: []v1.IPConfigStage{v1.IPStages.Rollback, v1.IPStages.Idle},
		},
	}
	if withHistory {
		ipc.Status.History = []*v1.IPHistory{{
			Stage:     v1.IPStages.Rollback,
			StartTime: metav1.Now(),
			Phases:    []*v1.IPPhase{},
		}}
	}
	return ipc
}

func assertRollbackInProgress(t *testing.T, ipc *v1.IPConfig) {
	t.Helper()
	inProg := controllerutils.GetIPInProgressCondition(ipc, v1.IPStages.Rollback)
	if assert.NotNil(t, inProg) {
		assert.Equal(t, metav1.ConditionTrue, inProg.Status)
		assert.Equal(t, string(controllerutils.ConditionReasons.InProgress), inProg.Reason)
	}
}

func assertRollbackFailed(t *testing.T, ipc *v1.IPConfig) {
	t.Helper()
	inProg := controllerutils.GetIPInProgressCondition(ipc, v1.IPStages.Rollback)
	comp := controllerutils.GetIPCompletedCondition(ipc, v1.IPStages.Rollback)
	if assert.NotNil(t, inProg) {
		assert.Equal(t, metav1.ConditionFalse, inProg.Status)
		assert.Equal(t, string(controllerutils.ConditionReasons.Failed), inProg.Reason)
	}
	if assert.NotNil(t, comp) {
		assert.Equal(t, metav1.ConditionFalse, comp.Status)
		assert.Equal(t, string(controllerutils.ConditionReasons.Failed), comp.Reason)
	}
}

func assertRollbackInvalidTransition(t *testing.T, ipc *v1.IPConfig) {
	t.Helper()
	inProg := controllerutils.GetIPInProgressCondition(ipc, v1.IPStages.Rollback)
	comp := controllerutils.GetIPCompletedCondition(ipc, v1.IPStages.Rollback)
	if assert.NotNil(t, inProg) {
		assert.Equal(t, metav1.ConditionFalse, inProg.Status)
		assert.Equal(t, string(controllerutils.ConditionReasons.InvalidTransition), inProg.Reason)
	}
	assert.Nil(t, comp, "invalid transition should not set completed condition")
}

func assertRollbackCompleted(t *testing.T, ipc *v1.IPConfig) {
	t.Helper()
	inProg := controllerutils.GetIPInProgressCondition(ipc, v1.IPStages.Rollback)
	comp := controllerutils.GetIPCompletedCondition(ipc, v1.IPStages.Rollback)
	if assert.NotNil(t, inProg) {
		assert.Equal(t, metav1.ConditionFalse, inProg.Status)
		assert.Equal(t, string(controllerutils.ConditionReasons.Completed), inProg.Reason)
	}
	if assert.NotNil(t, comp) {
		assert.Equal(t, metav1.ConditionTrue, comp.Status)
		assert.Equal(t, string(controllerutils.ConditionReasons.Completed), comp.Reason)
	}
}

func findRollbackPhase(t *testing.T, ipc *v1.IPConfig, phase string) *v1.IPPhase {
	t.Helper()
	for _, h := range ipc.Status.History {
		if h.Stage != v1.IPStages.Rollback {
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

func TestIPCRollbackTwoPhaseHandler_PrePivot(t *testing.T) {
	scheme := newTestScheme(t)
	logger := logr.Logger{}

	t.Run("success schedules rollback and starts phase history", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockOps := ops.NewMockOps(gc)
		mockRpm := rpmostreeclient.NewMockIClient(gc)

		ipc := mkRollbackIPC(t, true)
		k8sClient := newFakeClientWithIPC(t, scheme, ipc)

		mockRpm.EXPECT().GetUnbootedStaterootName().Return("stateroot-old", nil).Times(1)
		mockOps.EXPECT().RemountSysroot().Return(nil).Times(1)
		mockOps.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(func(filename string, data []byte, perm os.FileMode) error {
				assert.True(t, strings.Contains(filename, "stateroot-old"), "expected save path to include stateroot name, got %q", filename)
				assert.True(t, strings.Contains(filename, common.IPCFilePath), "expected save path to include IPCFilePath, got %q", filename)
				assert.NotEmpty(t, data)
				assert.Equal(t, os.FileMode(0o600), perm)
				return nil
			}).Times(1)
		// scheduleIPConfigRollback passes 13 args to RunSystemdAction:
		// --wait --collect --property ExitType=cgroup --unit ... --description ... lca-cli ip-config rollback --stateroot <name>
		mockOps.EXPECT().RunSystemdAction(
			gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(),
			gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(),
			gomock.Any(), gomock.Any(), gomock.Any(),
		).
			DoAndReturn(func(args ...string) (string, error) {
				assert.Contains(t, args, "--unit")
				assert.Contains(t, args, controllerutils.IPConfigRollbackUnit)
				assert.Contains(t, args, controllerutils.LcaCliBinaryName)
				assert.Contains(t, args, "ip-config")
				assert.Contains(t, args, "rollback")
				assert.Contains(t, args, "--stateroot")
				assert.Contains(t, args, "stateroot-old")
				return "ok", nil
			}).Times(1)

		h := &IPCRollbackTwoPhaseHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			RPMOstreeClient: mockRpm,
			Ops:             mockOps,
		}

		res, err := h.PrePivot(context.Background(), ipc, logger)
		assert.NoError(t, err)
		assert.Equal(t, requeueWithShortInterval(), res)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		phase := findRollbackPhase(t, updated, IPConfigRollbackPhasePrepivot)
		if assert.NotNil(t, phase) {
			assert.False(t, phase.StartTime.IsZero())
		}
	})

	t.Run("rpm-ostree failure marks rollback failed and returns error", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockOps := ops.NewMockOps(gc)
		mockRpm := rpmostreeclient.NewMockIClient(gc)

		ipc := mkRollbackIPC(t, true)
		k8sClient := newFakeClientWithIPC(t, scheme, ipc)

		mockRpm.EXPECT().GetUnbootedStaterootName().Return("", errors.New("boom")).Times(1)

		h := &IPCRollbackTwoPhaseHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			RPMOstreeClient: mockRpm,
			Ops:             mockOps,
		}

		_, err := h.PrePivot(context.Background(), ipc, logger)
		assert.Error(t, err)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		assertRollbackFailed(t, updated)
	})

	t.Run("remount failure marks rollback failed and returns error", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockOps := ops.NewMockOps(gc)
		mockRpm := rpmostreeclient.NewMockIClient(gc)

		ipc := mkRollbackIPC(t, true)
		k8sClient := newFakeClientWithIPC(t, scheme, ipc)

		mockRpm.EXPECT().GetUnbootedStaterootName().Return("stateroot-old", nil).Times(1)
		mockOps.EXPECT().RemountSysroot().Return(errors.New("remount failed")).Times(1)

		h := &IPCRollbackTwoPhaseHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			RPMOstreeClient: mockRpm,
			Ops:             mockOps,
		}

		_, err := h.PrePivot(context.Background(), ipc, logger)
		assert.Error(t, err)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		assertRollbackFailed(t, updated)
	})

	t.Run("save failure marks rollback failed and returns error", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockOps := ops.NewMockOps(gc)
		mockRpm := rpmostreeclient.NewMockIClient(gc)

		ipc := mkRollbackIPC(t, true)
		k8sClient := newFakeClientWithIPC(t, scheme, ipc)

		mockRpm.EXPECT().GetUnbootedStaterootName().Return("stateroot-old", nil).Times(1)
		mockOps.EXPECT().RemountSysroot().Return(nil).Times(1)
		mockOps.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).Return(errors.New("write failed")).Times(1)

		h := &IPCRollbackTwoPhaseHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			RPMOstreeClient: mockRpm,
			Ops:             mockOps,
		}

		_, err := h.PrePivot(context.Background(), ipc, logger)
		assert.Error(t, err)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		assertRollbackFailed(t, updated)
	})

	t.Run("schedule failure marks rollback failed and returns error", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockOps := ops.NewMockOps(gc)
		mockRpm := rpmostreeclient.NewMockIClient(gc)

		ipc := mkRollbackIPC(t, true)
		k8sClient := newFakeClientWithIPC(t, scheme, ipc)

		mockRpm.EXPECT().GetUnbootedStaterootName().Return("stateroot-old", nil).Times(1)
		mockOps.EXPECT().RemountSysroot().Return(nil).Times(1)
		mockOps.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(1)
		mockOps.EXPECT().RunSystemdAction(
			gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(),
			gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(),
			gomock.Any(), gomock.Any(), gomock.Any(),
		).
			Return("", errors.New("systemd-run failed")).Times(1)

		h := &IPCRollbackTwoPhaseHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			RPMOstreeClient: mockRpm,
			Ops:             mockOps,
		}

		_, err := h.PrePivot(context.Background(), ipc, logger)
		assert.Error(t, err)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		assertRollbackFailed(t, updated)
	})
}

func TestIPCRollbackTwoPhaseHandler_PostPivot(t *testing.T) {
	scheme := newTestScheme(t)
	logger := logr.Logger{}

	t.Run("skip healthcheck annotation => does not call CheckHealth and completes postpivot", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockOps := ops.NewMockOps(gc)
		mockRpm := rpmostreeclient.NewMockIClient(gc)

		ipc := mkRollbackIPC(t, true)
		ipc.SetAnnotations(map[string]string{controllerutils.SkipIPConfigPreConfigurationClusterHealthChecksAnnotation: ""})

		k8sClient := newFakeClientWithIPC(t, scheme, ipc)
		h := &IPCRollbackTwoPhaseHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			RPMOstreeClient: mockRpm,
			Ops:             mockOps,
		}

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		called := false
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error {
			called = true
			return errors.New("not healthy")
		}

		res, err := h.PostPivot(context.Background(), ipc, logger)
		assert.NoError(t, err)
		assert.False(t, called, "CheckHealth should not be called when skip annotation is set")
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		phase := findRollbackPhase(t, updated, IPConfigRollbackPhasePostpivot)
		if assert.NotNil(t, phase) {
			assert.False(t, phase.StartTime.IsZero())
			assert.False(t, phase.CompletionTime.IsZero())
		}
	})

	t.Run("healthcheck failing updates rollback in-progress message and requeues", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockOps := ops.NewMockOps(gc)
		mockRpm := rpmostreeclient.NewMockIClient(gc)

		ipc := mkRollbackIPC(t, true)

		k8sClient := newFakeClientWithIPC(t, scheme, ipc)
		h := &IPCRollbackTwoPhaseHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			RPMOstreeClient: mockRpm,
			Ops:             mockOps,
		}

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error {
			return errors.New("not healthy")
		}

		res, err := h.PostPivot(context.Background(), ipc, logger)
		assert.NoError(t, err)
		assert.Equal(t, requeueWithHealthCheckInterval(), res)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		assertRollbackInProgress(t, updated)
		inProg := controllerutils.GetIPInProgressCondition(updated, v1.IPStages.Rollback)
		if assert.NotNil(t, inProg) {
			assert.Contains(t, inProg.Message, "Waiting for system to stabilize")
		}
	})

	t.Run("healthy stops postpivot phase and does not requeue", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockOps := ops.NewMockOps(gc)
		mockRpm := rpmostreeclient.NewMockIClient(gc)

		ipc := mkRollbackIPC(t, true)

		k8sClient := newFakeClientWithIPC(t, scheme, ipc)
		h := &IPCRollbackTwoPhaseHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			RPMOstreeClient: mockRpm,
			Ops:             mockOps,
		}

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error { return nil }

		res, err := h.PostPivot(context.Background(), ipc, logger)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		phase := findRollbackPhase(t, updated, IPConfigRollbackPhasePostpivot)
		if assert.NotNil(t, phase) {
			assert.False(t, phase.StartTime.IsZero())
			assert.False(t, phase.CompletionTime.IsZero())
		}
	})
}

func TestIPCRollbackStageHandler_Handle(t *testing.T) {
	scheme := newTestScheme(t)
	ctx := context.Background()

	t.Run("transition requested but invalid next stage => status invalidTransition, then becomes valid and proceeds", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRpm := rpmostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)

		ipc := mkRollbackIPC(t, true)
		ipc.Status.ValidNextStages = []v1.IPConfigStage{v1.IPStages.Idle} // exclude rollback => invalid

		k8sClient := newFakeClientWithIPC(t, scheme, ipc)

		tph := NewMockIPConfigTwoPhaseHandlerInterface(gc)
		stageHandler := NewIPCRollbackStageHandler(k8sClient, mockRpm, mockOps, tph)

		res, err := stageHandler.Handle(ctx, ipc)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		assertRollbackInvalidTransition(t, updated)
		// validate we didn't clobber validNextStages
		assert.Equal(t, []v1.IPConfigStage{v1.IPStages.Idle}, updated.Status.ValidNextStages)

		// Now the stage becomes valid: allow Rollback, then ensure the next reconcile proceeds and overwrites
		// the previous InvalidTransition condition with a regular in-progress status.
		updated.Status.ValidNextStages = []v1.IPConfigStage{v1.IPStages.Rollback}
		assert.NoError(t, k8sClient.Status().Update(ctx, updated))

		mockRpm.EXPECT().GetUnbootedStaterootName().Return("some-unbooted", nil).Times(1)
		mockRpm.EXPECT().IsStaterootBooted(gomock.Any()).Return(true, nil).Times(1)
		tph.EXPECT().
			PrePivot(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(requeueWithShortInterval(), nil).
			Times(1)

		res2, err2 := stageHandler.Handle(ctx, updated)
		assert.NoError(t, err2)
		assert.Equal(t, requeueWithShortInterval(), res2)

		updated2 := mustGetIPC(t, k8sClient, common.IPConfigName)
		assertRollbackInProgress(t, updated2)
	})

	t.Run("transition requested but unbooted stateroot not available => status failed and no requeue", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRpm := rpmostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)

		ipc := mkRollbackIPC(t, true)
		// Transition requested: no rollback conditions yet, and rollback is allowed
		ipc.Status.ValidNextStages = []v1.IPConfigStage{v1.IPStages.Rollback}

		k8sClient := newFakeClientWithIPC(t, scheme, ipc)

		mockRpm.EXPECT().GetUnbootedStaterootName().Return("", errors.New("no unbooted stateroot")).Times(1)
		mockRpm.EXPECT().IsStaterootBooted(gomock.Any()).Times(0)

		tph := NewMockIPConfigTwoPhaseHandlerInterface(gc)
		stageHandler := NewIPCRollbackStageHandler(k8sClient, mockRpm, mockOps, tph)
		res, err := stageHandler.Handle(ctx, ipc)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		assertRollbackFailed(t, updated)
	})

	t.Run("stage not in progress => do not requeue", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRpm := rpmostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)

		ipc := mkRollbackIPC(t, true)
		controllerutils.SetIPRollbackStatusCompleted(ipc, "done")

		k8sClient := newFakeClientWithIPC(t, scheme, ipc)

		tph := NewMockIPConfigTwoPhaseHandlerInterface(gc)
		stageHandler := NewIPCRollbackStageHandler(k8sClient, mockRpm, mockOps, tph)
		res, err := stageHandler.Handle(ctx, ipc)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)
	})

	t.Run("booted target stateroot => runs prepivot handler", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRpm := rpmostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)

		ipc := mkRollbackIPC(t, true)
		k8sClient := newFakeClientWithIPC(t, scheme, ipc)

		mockRpm.EXPECT().GetUnbootedStaterootName().Return("stateroot-old", nil).AnyTimes()
		mockRpm.EXPECT().IsStaterootBooted("rhcos").Return(true, nil).Times(1)

		tph := NewMockIPConfigTwoPhaseHandlerInterface(gc)
		tph.EXPECT().
			PrePivot(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(requeueWithShortInterval(), nil).
			Times(1)

		stageHandler := NewIPCRollbackStageHandler(k8sClient, mockRpm, mockOps, tph)
		res, err := stageHandler.Handle(ctx, ipc)
		assert.NoError(t, err)
		assert.Equal(t, requeueWithShortInterval(), res)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		assertRollbackInProgress(t, updated)
	})

	t.Run("after pivot => stops prepivot phase, runs postpivot, completes stage and rollback status", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRpm := rpmostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)

		ipc := mkRollbackIPC(t, true)
		// Pretend prepivot phase started earlier so StopIPPhase can close it.
		ipc.Status.History[0].Phases = []*v1.IPPhase{{
			Phase:     IPConfigRollbackPhasePrepivot,
			StartTime: metav1.Now(),
		}}
		controllerutils.SetIPRollbackStatusInProgress(ipc, "Rollback is in progress")

		k8sClient := newFakeClientWithIPC(t, scheme, ipc)

		// With PostPivot mocked, only Handle() performs the isTargetStaterootBooted check.
		mockRpm.EXPECT().IsStaterootBooted("rhcos").Return(false, nil).Times(1)

		tph := NewMockIPConfigTwoPhaseHandlerInterface(gc)
		tph.EXPECT().
			PostPivot(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(doNotRequeue(), nil).
			Times(1)

		stageHandler := NewIPCRollbackStageHandler(k8sClient, mockRpm, mockOps, tph)
		res, err := stageHandler.Handle(ctx, ipc)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		assertRollbackCompleted(t, updated)

		// prepivot phase should be marked completed
		phase := findRollbackPhase(t, updated, IPConfigRollbackPhasePrepivot)
		if assert.NotNil(t, phase) {
			assert.False(t, phase.CompletionTime.IsZero())
		}

		// stage completion time should be set
		if assert.Len(t, updated.Status.History, 1) {
			assert.Equal(t, v1.IPStages.Rollback, updated.Status.History[0].Stage)
			assert.False(t, updated.Status.History[0].CompletionTime.IsZero())
		}
	})

	t.Run("prepivot error bubbles up as error with returned result", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockRpm := rpmostreeclient.NewMockIClient(gc)
		mockOps := ops.NewMockOps(gc)

		ipc := mkRollbackIPC(t, true)
		k8sClient := newFakeClientWithIPC(t, scheme, ipc)

		mockRpm.EXPECT().GetUnbootedStaterootName().Return("stateroot-old", nil).AnyTimes()
		mockRpm.EXPECT().IsStaterootBooted("rhcos").Return(true, nil).Times(1)

		tph := NewMockIPConfigTwoPhaseHandlerInterface(gc)
		tph.EXPECT().
			PrePivot(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(requeueWithShortInterval(), fmt.Errorf("prepivot failed")).
			Times(1)

		stageHandler := NewIPCRollbackStageHandler(k8sClient, mockRpm, mockOps, tph)
		res, err := stageHandler.Handle(ctx, ipc)
		assert.Error(t, err)
		assert.Equal(t, requeueWithShortInterval(), res)
	})
}
