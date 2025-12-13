package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	ipcv1 "github.com/openshift-kni/lifecycle-agent/api/ipconfig/v1"
	controllerutils "github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// --- scheduleIPConfigRollback and PrePivot ---

func Test_scheduleIPConfigRollback_SuccessAndFailure(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	mockOps := ops.NewMockOps(ctrlMock)
	handler := &IPConfigRollbackPhasesHandler{
		Ops: mockOps,
	}
	logger := logr.Discard()

	// Success – expect exact arguments
	mockOps.EXPECT().RunSystemdAction(
		"--property", controllerutils.SystemdExitTypeCgroup,
		"--unit", controllerutils.IPConfigRollbackUnit,
		"--description", controllerutils.IPConfigRollbackDescription,
		controllerutils.LcaCliBinaryName, "ip-config", "rollback",
		"--stateroot", "old-root",
	).Return("ok", nil)

	assert.NoError(t, handler.scheduleIPConfigRollback(logger, "old-root"))

	// Failure – allow any args but with the same arity (10 arguments)
	mockOps.EXPECT().RunSystemdAction(
		gomock.Any(), gomock.Any(), gomock.Any(),
		gomock.Any(), gomock.Any(), gomock.Any(),
		gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(),
	).Return("", fmt.Errorf("systemd error"))

	err := handler.scheduleIPConfigRollback(logger, "another-root")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to schedule ip-config rollback")
}

func Test_IPConfigRollbackPhasesHandler_PrePivot_ErrorPathsAndSuccess(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newTestIPConfig()
	ipc.Spec.Stage = ipcv1.IPStages.Rollback

	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}

	mockOps := ops.NewMockOps(ctrlMock)
	mockRPM := rpmostreeclient.NewMockIClient(ctrlMock)

	handler := &IPConfigRollbackPhasesHandler{
		Client:          mockClient,
		NoncachedClient: mockClient,
		RPMOstreeClient: mockRPM,
		Ops:             mockOps,
	}

	ctx := context.Background()
	logger := logr.Discard()

	// Case 1: GetUnbootedStaterootName fails
	mockRPM.EXPECT().GetUnbootedStaterootName().Return("", fmt.Errorf("stateroot error"))

	res, err := handler.PrePivot(ctx, ipc, logger)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to determine unbooted stateroot")
	assert.Equal(t, ctrl.Result{}.RequeueAfter, res.RequeueAfter)

	// Case 2: RemountSysroot fails
	ipc = newTestIPConfig()
	ipc.Spec.Stage = ipcv1.IPStages.Rollback
	baseClient = newFakeIPClient(t, ipc).Build()
	mockClient = &noStatusClient{Client: baseClient}
	handler.Client = mockClient
	handler.NoncachedClient = mockClient

	mockRPM.EXPECT().GetUnbootedStaterootName().Return("old-root", nil)
	mockOps.EXPECT().RemountSysroot().Return(fmt.Errorf("remount error"))

	res, err = handler.PrePivot(ctx, ipc, logger)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to remount sysroot")
	assert.Equal(t, ctrl.Result{}.RequeueAfter, res.RequeueAfter)

	// Case 3: WriteFile fails
	ipc = newTestIPConfig()
	ipc.Spec.Stage = ipcv1.IPStages.Rollback
	baseClient = newFakeIPClient(t, ipc).Build()
	mockClient = &noStatusClient{Client: baseClient}
	handler.Client = mockClient
	handler.NoncachedClient = mockClient

	mockRPM.EXPECT().GetUnbootedStaterootName().Return("old-root", nil)
	mockOps.EXPECT().RemountSysroot().Return(nil)
	mockOps.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).Return(fmt.Errorf("write error"))

	res, err = handler.PrePivot(ctx, ipc, logger)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to save IPConfig CR before pivot")
	assert.Equal(t, ctrl.Result{}.RequeueAfter, res.RequeueAfter)

	// Case 4: scheduleIPConfigRollback fails
	ipc = newTestIPConfig()
	ipc.Spec.Stage = ipcv1.IPStages.Rollback
	baseClient = newFakeIPClient(t, ipc).Build()
	mockClient = &noStatusClient{Client: baseClient}
	handler.Client = mockClient
	handler.NoncachedClient = mockClient

	mockRPM.EXPECT().GetUnbootedStaterootName().Return("old-root", nil)
	mockOps.EXPECT().RemountSysroot().Return(nil)
	mockOps.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	mockOps.EXPECT().RunSystemdAction(
		"--property", controllerutils.SystemdExitTypeCgroup,
		"--unit", controllerutils.IPConfigRollbackUnit,
		"--description", controllerutils.IPConfigRollbackDescription,
		controllerutils.LcaCliBinaryName, "ip-config", "rollback",
		"--stateroot", gomock.Any(),
	).Return("", fmt.Errorf("systemd error"))

	res, err = handler.PrePivot(ctx, ipc, logger)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to schedule ip-config rollback")
	assert.Equal(t, ctrl.Result{}.RequeueAfter, res.RequeueAfter)

	// Case 5: full success – short requeue
	ipc = newTestIPConfig()
	ipc.Spec.Stage = ipcv1.IPStages.Rollback
	baseClient = newFakeIPClient(t, ipc).Build()
	mockClient = &noStatusClient{Client: baseClient}
	handler.Client = mockClient
	handler.NoncachedClient = mockClient

	mockRPM.EXPECT().GetUnbootedStaterootName().Return("old-root", nil)
	mockOps.EXPECT().RemountSysroot().Return(nil)
	mockOps.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	mockOps.EXPECT().RunSystemdAction(
		"--property", controllerutils.SystemdExitTypeCgroup,
		"--unit", controllerutils.IPConfigRollbackUnit,
		"--description", controllerutils.IPConfigRollbackDescription,
		controllerutils.LcaCliBinaryName, "ip-config", "rollback",
		"--stateroot", gomock.Any(),
	).Return("ok", nil)

	res, err = handler.PrePivot(ctx, ipc, logger)
	assert.NoError(t, err)
	assert.Equal(t, requeueWithShortInterval().RequeueAfter, res.RequeueAfter)
}

// --- PostPivot ---

func Test_IPConfigRollbackPhasesHandler_PostPivot_TargetNotBooted(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newMatchingIPConfig()
	ipc.Spec.Stage = ipcv1.IPStages.Rollback

	sch := scheme.Scheme
	_ = ipcv1.AddToScheme(sch)
	baseClient := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(ipc).
		WithStatusSubresource(ipc).
		Build()
	mockClient := &noStatusClient{Client: baseClient}

	mockRPM := rpmostreeclient.NewMockIClient(ctrlMock)
	mockRPM.EXPECT().IsStaterootBooted(gomock.Any()).Return(false, nil)

	handler := &IPConfigRollbackPhasesHandler{
		Client:          mockClient,
		NoncachedClient: mockClient,
		RPMOstreeClient: mockRPM,
		Ops:             ops.NewMockOps(ctrlMock),
	}

	ctx := context.Background()
	logger := logr.Discard()

	res, err := handler.PostPivot(ctx, ipc, logger)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "old stateroot is not booted")
	assert.Equal(t, ctrl.Result{}.RequeueAfter, res.RequeueAfter)
}

func Test_IPConfigRollbackPhasesHandler_PostPivot_HealthcheckFailureAndSuccess(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newTestIPConfig()
	ipc.Spec.Stage = ipcv1.IPStages.Rollback

	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}

	mockRPM := rpmostreeclient.NewMockIClient(ctrlMock)
	handler := &IPConfigRollbackPhasesHandler{
		Client:          mockClient,
		NoncachedClient: mockClient,
		RPMOstreeClient: mockRPM,
		Ops:             ops.NewMockOps(ctrlMock),
	}

	ctx := context.Background()
	logger := logr.Discard()

	oldHC := CheckHealth
	defer func() {
		CheckHealth = oldHC
	}()

	// Case 1: healthcheck fails
	mockRPM.EXPECT().IsStaterootBooted(gomock.Any()).Return(true, nil)
	CheckHealth = func(_ context.Context, _ client.Reader, _ logr.Logger) error {
		return fmt.Errorf("hc error")
	}

	res, err := handler.PostPivot(ctx, ipc, logger)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "waiting for system to stabilize")
	assert.Equal(t, requeueWithHealthCheckInterval().RequeueAfter, res.RequeueAfter)

	// Case 2: healthcheck succeeds
	mockRPM.EXPECT().IsStaterootBooted(gomock.Any()).Return(true, nil)
	CheckHealth = func(_ context.Context, _ client.Reader, _ logr.Logger) error {
		return nil
	}

	res, err = handler.PostPivot(ctx, ipc, logger)
	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)
}

// --- IPConfigRollbackStageHandler.Handle with mocked phases handler ---

func Test_IPConfigRollbackStageHandler_Handle_ValidationFails(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	sch := scheme.Scheme
	_ = ipcv1.AddToScheme(sch)
	ipc := newTestIPConfig()
	ipc.Spec.Stage = ipcv1.IPStages.Rollback
	client := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(ipc).
		WithStatusSubresource(ipc).
		Build()

	handler := &IPConfigRollbackStageHandler{
		Client:        client,
		ChrootOps:     ops.NewMockOps(ctrlMock),
		PhasesHandler: NewMockIPConfigRollbackPhasesHandlerInterface(ctrlMock),
	}

	res, err := handler.Handle(context.Background(), ipc)
	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)
	assert.True(t, controllerutils.IsIPStageFailed(ipc, ipcv1.IPStages.Rollback))
}

func Test_IPConfigRollbackStageHandler_Handle_StatusReadError(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	sch := scheme.Scheme
	_ = ipcv1.AddToScheme(sch)
	ipc := newTestIPConfig()
	ipc.Spec.Stage = ipcv1.IPStages.Rollback

	client := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(ipc).
		WithStatusSubresource(ipc).
		Build()

	// Mark Rollback stage as completed so isIPTransitionRequested is false and validation is skipped.
	controllerutils.SetIPRollbackStatusCompleted(ipc, "done")

	mockOps := ops.NewMockOps(ctrlMock)
	mockOps.EXPECT().
		ReadFile(common.PathOutsideChroot(common.IPConfigRollbackStatusFile)).
		Return(nil, fmt.Errorf("read error"))
	mockOps.EXPECT().IsNotExist(gomock.Any()).Return(false)

	handler := &IPConfigRollbackStageHandler{
		Client:        client,
		ChrootOps:     mockOps,
		PhasesHandler: NewMockIPConfigRollbackPhasesHandlerInterface(ctrlMock),
	}

	_, err := handler.Handle(context.Background(), ipc)
	assert.Error(t, err)
}

func Test_IPConfigRollbackStageHandler_Handle_Unknown_DelegatesPrePivot(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	// ChrootOps used by ReadIPConfigStatus
	mockOps := ops.NewMockOps(ctrlMock)
	status := common.IPConfigStatus{Phase: common.IPConfigStatusUnknown}
	data, _ := json.Marshal(status)
	mockOps.EXPECT().
		ReadFile(common.PathOutsideChroot(common.IPConfigRollbackStatusFile)).
		Return(data, nil)
	mockOps.EXPECT().IsNotExist(gomock.Any()).Return(false).AnyTimes()

	ctx := context.Background()
	ipc := newTestIPConfig()
	ipc.Spec.Stage = ipcv1.IPStages.Rollback
	// Mark Rollback stage already completed so that isIPTransitionRequested(ipc) is false
	controllerutils.SetIPRollbackStatusCompleted(ipc, "done")

	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}

	phasesMock := NewMockIPConfigRollbackPhasesHandlerInterface(ctrlMock)

	handler := &IPConfigRollbackStageHandler{
		Client:        mockClient,
		ChrootOps:     mockOps,
		PhasesHandler: phasesMock,
	}

	phasesMock.EXPECT().PrePivot(ctx, ipc, gomock.Any()).Return(doNotRequeue(), nil)

	res, err := handler.Handle(ctx, ipc)
	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)
}

func Test_IPConfigRollbackStageHandler_Handle_Running_Requeues(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	mockOps := ops.NewMockOps(ctrlMock)
	status := common.IPConfigStatus{Phase: common.IPConfigStatusRunning}
	data, _ := json.Marshal(status)
	mockOps.EXPECT().
		ReadFile(common.PathOutsideChroot(common.IPConfigRollbackStatusFile)).
		Return(data, nil)
	mockOps.EXPECT().IsNotExist(gomock.Any()).Return(false).AnyTimes()

	ctx := context.Background()
	ipc := newTestIPConfig()
	ipc.Spec.Stage = ipcv1.IPStages.Rollback
	controllerutils.SetIPRollbackStatusCompleted(ipc, "done")

	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}

	handler := &IPConfigRollbackStageHandler{
		Client:        mockClient,
		ChrootOps:     mockOps,
		PhasesHandler: NewMockIPConfigRollbackPhasesHandlerInterface(ctrlMock),
	}

	res, err := handler.Handle(ctx, ipc)
	assert.NoError(t, err)
	assert.Equal(t, requeueWithShortInterval().RequeueAfter, res.RequeueAfter)
}

func Test_IPConfigRollbackStageHandler_Handle_Failed_SetsStatusFailed(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	mockOps := ops.NewMockOps(ctrlMock)
	status := common.IPConfigStatus{Phase: common.IPConfigStatusFailed, Message: "rollback failed"}
	data, _ := json.Marshal(status)
	mockOps.EXPECT().
		ReadFile(common.PathOutsideChroot(common.IPConfigRollbackStatusFile)).
		Return(data, nil)
	mockOps.EXPECT().IsNotExist(gomock.Any()).Return(false).AnyTimes()

	ctx := context.Background()
	ipc := newTestIPConfig()
	ipc.Spec.Stage = ipcv1.IPStages.Rollback
	controllerutils.SetIPRollbackStatusCompleted(ipc, "done")

	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}

	handler := &IPConfigRollbackStageHandler{
		Client:        mockClient,
		ChrootOps:     mockOps,
		PhasesHandler: NewMockIPConfigRollbackPhasesHandlerInterface(ctrlMock),
	}

	res, err := handler.Handle(ctx, ipc)
	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)
	assert.True(t, controllerutils.IsIPStageFailed(ipc, ipcv1.IPStages.Rollback))
}

func Test_IPConfigRollbackStageHandler_Handle_Succeeded_DelegatesPostPivot(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	mockOps := ops.NewMockOps(ctrlMock)
	status := common.IPConfigStatus{Phase: common.IPConfigStatusSucceeded}
	data, _ := json.Marshal(status)
	mockOps.EXPECT().
		ReadFile(common.PathOutsideChroot(common.IPConfigRollbackStatusFile)).
		Return(data, nil)
	mockOps.EXPECT().IsNotExist(gomock.Any()).Return(false).AnyTimes()

	ctx := context.Background()
	ipc := newTestIPConfig()
	ipc.Spec.Stage = ipcv1.IPStages.Rollback
	controllerutils.SetIPRollbackStatusCompleted(ipc, "done")

	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}

	phasesMock := NewMockIPConfigRollbackPhasesHandlerInterface(ctrlMock)
	phasesMock.EXPECT().
		PostPivot(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(doNotRequeue(), nil)

	handler := &IPConfigRollbackStageHandler{
		Client:        mockClient,
		ChrootOps:     mockOps,
		PhasesHandler: phasesMock,
	}

	res, err := handler.Handle(ctx, ipc)
	assert.NoError(t, err)
	assert.Equal(t, requeueWithShortInterval().RequeueAfter, res.RequeueAfter)
	assert.True(t, controllerutils.IsIPStageCompleted(ipc, ipcv1.IPStages.Rollback))
}
