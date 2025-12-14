package controllers

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	ipcv1 "github.com/openshift-kni/lifecycle-agent/api/ipconfig/v1"
	controllerutils "github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type fakeFileInfo struct{}

func (fakeFileInfo) Name() string       { return "x" }
func (fakeFileInfo) Size() int64        { return 0 }
func (fakeFileInfo) Mode() os.FileMode  { return 0o600 }
func (fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fakeFileInfo) IsDir() bool        { return false }
func (fakeFileInfo) Sys() any           { return nil }

type errUpdateClient struct {
	client.Client
	err error
}

func (c *errUpdateClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return c.err
}

type errStatusWriter struct {
	err error
}

func (w errStatusWriter) Create(ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	return w.err
}
func (w errStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	return w.err
}
func (w errStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	return w.err
}

type errStatusClient struct {
	client.Client
	err error
}

func (c errStatusClient) Status() client.StatusWriter {
	return errStatusWriter{err: c.err}
}

func mkIPCForIdle(t *testing.T) *ipcv1.IPConfig {
	t.Helper()
	return &ipcv1.IPConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       common.IPConfigName,
			Generation: 7,
		},
		Spec: ipcv1.IPConfigSpec{
			Stage: ipcv1.IPStages.Idle,
		},
		Status: ipcv1.IPConfigStatus{
			ObservedGeneration: 99,
			ValidNextStages:    []ipcv1.IPConfigStage{ipcv1.IPStages.Idle, ipcv1.IPStages.Config},
			DNSFilterOutFamily: "ipv4",
			History: []*ipcv1.IPHistory{{
				Stage:     ipcv1.IPStages.Config,
				StartTime: metav1.Now(),
				Phases: []*ipcv1.IPPhase{{
					Phase:     "some-phase",
					StartTime: metav1.Now(),
				}},
			}},
		},
	}
}

func assertIdleCond(t *testing.T, ipc *ipcv1.IPConfig, wantStatus metav1.ConditionStatus, wantReason controllerutils.ConditionReason, msgContains string) {
	t.Helper()
	idle := meta.FindStatusCondition(ipc.Status.Conditions, string(controllerutils.ConditionTypes.Idle))
	if assert.NotNil(t, idle) {
		assert.Equal(t, wantStatus, idle.Status)
		assert.Equal(t, string(wantReason), idle.Reason)
		if msgContains != "" {
			assert.Contains(t, idle.Message, msgContains)
		}
	}
}

func normalizeMetav1TimeForCompare(t metav1.Time) metav1.Time {
	// Fake client reads/writes and deepcopy flows can strip (or change) Go's monotonic
	// clock reading embedded in time.Time. Strip it explicitly so tests can use
	// deep-equality on metav1.Time.
	if t.Time.IsZero() {
		return t
	}
	// Round-tripping through JSON yields a stable representation (and drops monotonic bits)
	// that matches what the fake client typically stores/returns.
	b, err := t.MarshalJSON()
	if err != nil {
		// Best-effort fallback: drop monotonic via UnixNano rebuild.
		return metav1.NewTime(time.Unix(0, t.Time.UnixNano()).UTC())
	}
	var out metav1.Time
	if err := out.UnmarshalJSON(b); err != nil {
		// Best-effort fallback: drop monotonic via UnixNano rebuild.
		return metav1.NewTime(time.Unix(0, t.Time.UnixNano()).UTC())
	}
	return out
}

func normalizeHistoryForCompare(in []*ipcv1.IPHistory) []*ipcv1.IPHistory {
	if in == nil {
		return nil
	}

	out := make([]*ipcv1.IPHistory, 0, len(in))
	for _, h := range in {
		if h == nil {
			out = append(out, nil)
			continue
		}
		cp := h.DeepCopy()
		cp.StartTime = normalizeMetav1TimeForCompare(cp.StartTime)
		cp.CompletionTime = normalizeMetav1TimeForCompare(cp.CompletionTime)
		for _, p := range cp.Phases {
			if p == nil {
				continue
			}
			p.StartTime = normalizeMetav1TimeForCompare(p.StartTime)
			p.CompletionTime = normalizeMetav1TimeForCompare(p.CompletionTime)
		}
		out = append(out, cp)
	}
	return out
}

func assertStatusInvariants(t *testing.T, got *ipcv1.IPConfig, want *ipcv1.IPConfig) {
	t.Helper()
	assert.Equal(t, want.Status.ObservedGeneration, got.Status.ObservedGeneration)
	assert.Equal(t, want.Status.ValidNextStages, got.Status.ValidNextStages)
	assert.Equal(t, want.Status.IPv4, got.Status.IPv4)
	assert.Equal(t, want.Status.IPv6, got.Status.IPv6)
	assert.Equal(t, want.Status.VLANID, got.Status.VLANID)
	assert.Equal(t, want.Status.DNSFilterOutFamily, got.Status.DNSFilterOutFamily)
	assert.Equal(t, normalizeHistoryForCompare(want.Status.History), normalizeHistoryForCompare(got.Status.History))
}

func TestIPCIdleStageHandler_Handle(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme(t)

	t.Run("transition requested but invalid next stage => status idle=false invalidTransition, no requeue", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockOps := ops.NewMockOps(gc)
		mockRpm := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)

		ipc := mkIPCForIdle(t)
		ipc.Spec.Stage = ipcv1.IPStages.Config
		ipc.Status.ValidNextStages = []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}

		k8sClient := newFakeClientWithIPC(t, scheme, ipc)
		h := &IPCIdleStageHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			ChrootOps:       mockOps,
			OstreeClient:    mockOstree,
			RPMOstreeClient: mockRpm,
		}

		res, err := h.Handle(ctx, ipc)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		assertIdleCond(t, updated, metav1.ConditionFalse, controllerutils.ConditionReasons.InvalidTransition, "invalid IPConfig stage")
		assertStatusInvariants(t, updated, ipc)
	})

	t.Run("invalid next stage but status update fails => returns requeueWithError and does not persist changes", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockOps := ops.NewMockOps(gc)
		mockRpm := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)

		ipc := mkIPCForIdle(t)
		ipc.Spec.Stage = ipcv1.IPStages.Config
		ipc.Status.ValidNextStages = []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}

		baseClient := newFakeClientWithIPC(t, scheme, ipc)
		wrapped := errStatusClient{Client: baseClient, err: errors.New("status update failed")}

		h := &IPCIdleStageHandler{
			Client:          wrapped,
			NoncachedClient: baseClient,
			ChrootOps:       mockOps,
			OstreeClient:    mockOstree,
			RPMOstreeClient: mockRpm,
		}

		res, err := h.Handle(ctx, ipc)
		assert.Error(t, err)
		wantRes, _ := requeueWithError(err)
		assert.Equal(t, wantRes, res)
		assert.Contains(t, err.Error(), "failed to update ipconfig status")
		// Note: Handle currently wraps the *stage validation* error, not the update error.
		assert.Contains(t, err.Error(), "invalid IPConfig stage")

		unchanged := mustGetIPC(t, baseClient, common.IPConfigName)
		assert.Nil(t, meta.FindStatusCondition(unchanged.Status.Conditions, string(controllerutils.ConditionTypes.Idle)))
		assertStatusInvariants(t, unchanged, ipc)
	})

	t.Run("healthcheck failing => idle=false stabilizing, requeueWithHealthCheckInterval and error", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockOps := ops.NewMockOps(gc)
		mockRpm := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)

		ipc := mkIPCForIdle(t)
		// Ensure transition is considered requested so validateIPConfigStage is exercised (and passes).
		ipc.Status.Conditions = nil
		ipc.Status.ValidNextStages = []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}

		k8sClient := newFakeClientWithIPC(t, scheme, ipc)
		h := &IPCIdleStageHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			ChrootOps:       mockOps,
			OstreeClient:    mockOstree,
			RPMOstreeClient: mockRpm,
		}

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error { return errors.New("not healthy") }

		res, err := h.Handle(ctx, ipc)
		assert.NoError(t, err)
		assert.Equal(t, requeueWithHealthCheckInterval(), res)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		assertIdleCond(t, updated, metav1.ConditionFalse, controllerutils.ConditionReasons.Stabilizing, "Waiting for system to stabilize")
		assertStatusInvariants(t, updated, ipc)
	})

	t.Run("skip healthcheck annotation => does not call CheckHealth and continues to cleanup", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockOps := ops.NewMockOps(gc)
		mockRpm := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)

		ipc := mkIPCForIdle(t)
		ipc.Status.Conditions = nil
		ipc.Status.ValidNextStages = []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}
		ipc.SetAnnotations(map[string]string{controllerutils.SkipIPConfigClusterHealthChecksAnnotation: ""})

		k8sClient := newFakeClientWithIPC(t, scheme, ipc)
		h := &IPCIdleStageHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			ChrootOps:       mockOps,
			OstreeClient:    mockOstree,
			RPMOstreeClient: mockRpm,
		}

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		called := false
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error {
			called = true
			return errors.New("not healthy")
		}

		// If health checks are skipped, handler should proceed to cleanup and hit RemountSysroot.
		mockOps.EXPECT().RemountSysroot().Return(errors.New("remount failed")).Times(1)

		res, err := h.Handle(ctx, ipc)
		assert.Error(t, err)
		assert.False(t, called, "CheckHealth should not be called when skip annotation is set")
		wantRes, _ := requeueWithError(err)
		assert.Equal(t, wantRes, res)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		assertIdleCond(t, updated, metav1.ConditionFalse, controllerutils.ConditionReasons.Failed, "failed to cleanup")
		assertStatusInvariants(t, updated, ipc)
	})

	t.Run("cleanup failing at remount sysroot => idle=false failed and requeueWithError", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockOps := ops.NewMockOps(gc)
		mockRpm := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)

		ipc := mkIPCForIdle(t)
		ipc.Status.Conditions = nil
		ipc.Status.ValidNextStages = []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}

		k8sClient := newFakeClientWithIPC(t, scheme, ipc)
		h := &IPCIdleStageHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			ChrootOps:       mockOps,
			OstreeClient:    mockOstree,
			RPMOstreeClient: mockRpm,
		}

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error { return nil }

		mockOps.EXPECT().RemountSysroot().Return(errors.New("remount failed")).Times(1)

		res, err := h.Handle(ctx, ipc)
		assert.Error(t, err)
		wantRes, _ := requeueWithError(err)
		assert.Equal(t, wantRes, res)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		assertIdleCond(t, updated, metav1.ConditionFalse, controllerutils.ConditionReasons.Failed, controllerutils.ManualCleanupAnnotation)
		assertStatusInvariants(t, updated, ipc)
	})

	t.Run("cleanup failing at getStaterootsToRemove (rpm QueryStatus error) => idle=false failed and requeueWithError", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockOps := ops.NewMockOps(gc)
		mockRpm := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)

		ipc := mkIPCForIdle(t)
		ipc.Status.Conditions = nil
		ipc.Status.ValidNextStages = []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}

		k8sClient := newFakeClientWithIPC(t, scheme, ipc)
		h := &IPCIdleStageHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			ChrootOps:       mockOps,
			OstreeClient:    mockOstree,
			RPMOstreeClient: mockRpm,
		}

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error { return nil }

		mockOps.EXPECT().RemountSysroot().Return(nil).Times(1)
		mockRpm.EXPECT().QueryStatus().Return(nil, errors.New("rpm error")).Times(1)

		res, err := h.Handle(ctx, ipc)
		assert.Error(t, err)
		wantRes, _ := requeueWithError(err)
		assert.Equal(t, wantRes, res)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		assertIdleCond(t, updated, metav1.ConditionFalse, controllerutils.ConditionReasons.Failed, "failed to clean up unbooted stateroots")
		assertStatusInvariants(t, updated, ipc)
	})

	t.Run("cleanup failing at workspace cleanup (RemoveAllFiles error) => idle=false failed and requeueWithError", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockOps := ops.NewMockOps(gc)
		mockRpm := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)

		ipc := mkIPCForIdle(t)
		ipc.Status.Conditions = nil
		ipc.Status.ValidNextStages = []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}

		k8sClient := newFakeClientWithIPC(t, scheme, ipc)
		h := &IPCIdleStageHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			ChrootOps:       mockOps,
			OstreeClient:    mockOstree,
			RPMOstreeClient: mockRpm,
		}

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error { return nil }

		status := &rpmostreeclient.Status{
			Deployments: []rpmostreeclient.Deployment{{OSName: "rhcos", Booted: true}},
		}

		mockOps.EXPECT().RemountSysroot().Return(nil).Times(1)
		// getStaterootsToRemove + CleanupUnbootedStateroots
		mockRpm.EXPECT().QueryStatus().Return(status, nil).Times(2)
		mockOps.EXPECT().RemountBoot().Return(nil).Times(1)
		mockOps.EXPECT().ReadDir(gomock.Any()).Return([]os.DirEntry{}, nil).Times(1)
		mockRpm.EXPECT().RpmOstreeCleanup().Return(nil).Times(1)
		mockOps.EXPECT().StatFile(gomock.Any()).Return(fakeFileInfo{}, nil).Times(1)
		mockOps.EXPECT().RemoveAllFiles(gomock.Any()).Return(errors.New("rm failed")).Times(1)

		res, err := h.Handle(ctx, ipc)
		assert.Error(t, err)
		wantRes, _ := requeueWithError(err)
		assert.Equal(t, wantRes, res)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		assertIdleCond(t, updated, metav1.ConditionFalse, controllerutils.ConditionReasons.Failed, "failed to cleanup")
		assertStatusInvariants(t, updated, ipc)
	})

	t.Run("success => resets conditions to only Idle=true and does not change history/validNextStages", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockOps := ops.NewMockOps(gc)
		mockRpm := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)

		ipc := mkIPCForIdle(t)
		// Add non-idle conditions to ensure ResetStatusConditions removes them.
		ipc.Status.Conditions = []metav1.Condition{
			{
				Type:               string(controllerutils.ConditionTypes.ConfigInProgress),
				Status:             metav1.ConditionTrue,
				Reason:             string(controllerutils.ConditionReasons.InProgress),
				Message:            "config running",
				ObservedGeneration: ipc.Generation,
			},
			{
				Type:               string(controllerutils.ConditionTypes.Idle),
				Status:             metav1.ConditionFalse,
				Reason:             string(controllerutils.ConditionReasons.Stabilizing),
				Message:            "old idle condition",
				ObservedGeneration: ipc.Generation,
			},
		}
		// Ensure transition is requested so validateIPConfigStage runs (and passes).
		ipc.Status.ValidNextStages = []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}

		k8sClient := newFakeClientWithIPC(t, scheme, ipc)
		h := &IPCIdleStageHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			ChrootOps:       mockOps,
			OstreeClient:    mockOstree,
			RPMOstreeClient: mockRpm,
		}

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error { return nil }

		status := &rpmostreeclient.Status{
			Deployments: []rpmostreeclient.Deployment{{OSName: "rhcos", Booted: true}},
		}

		mockOps.EXPECT().RemountSysroot().Return(nil).Times(1)
		mockRpm.EXPECT().QueryStatus().Return(status, nil).Times(2) // getStaterootsToRemove + CleanupUnbootedStateroots
		mockOps.EXPECT().RemountBoot().Return(nil).Times(1)
		mockOps.EXPECT().ReadDir(gomock.Any()).Return([]os.DirEntry{}, nil).Times(1)
		mockRpm.EXPECT().RpmOstreeCleanup().Return(nil).Times(1)
		mockOps.EXPECT().StatFile(gomock.Any()).Return(nil, errors.New("not exist")).Times(1)
		mockOps.EXPECT().RemoveAllFiles(gomock.Any()).Times(0)

		res, err := h.Handle(ctx, ipc)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		// After successful cleanup, conditions should be reset to Idle=true only.
		assert.Len(t, updated.Status.Conditions, 1)
		idle := meta.FindStatusCondition(updated.Status.Conditions, string(controllerutils.ConditionTypes.Idle))
		if assert.NotNil(t, idle) {
			assert.Equal(t, metav1.ConditionTrue, idle.Status)
			assert.Equal(t, string(controllerutils.ConditionReasons.Idle), idle.Reason)
			assert.Equal(t, "Idle", idle.Message)
		}
		assertStatusInvariants(t, updated, ipc)
	})

	t.Run("manual cleanup annotation present when idle previously failed => removes annotation and continues", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockOps := ops.NewMockOps(gc)
		mockRpm := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)

		ipc := mkIPCForIdle(t)
		ipc.SetAnnotations(map[string]string{controllerutils.ManualCleanupAnnotation: ""})
		ipc.Status.Conditions = []metav1.Condition{{
			Type:               string(controllerutils.ConditionTypes.Idle),
			Status:             metav1.ConditionFalse,
			Reason:             string(controllerutils.ConditionReasons.Failed),
			Message:            "failed earlier",
			ObservedGeneration: ipc.Generation,
		}}
		// Avoid validate stage branch in this test; focus on manual cleanup + health check.
		ipc.Status.ValidNextStages = nil

		k8sClient := newFakeClientWithIPC(t, scheme, ipc)
		h := &IPCIdleStageHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			ChrootOps:       mockOps,
			OstreeClient:    mockOstree,
			RPMOstreeClient: mockRpm,
		}

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error { return errors.New("still unhealthy") }

		res, err := h.Handle(ctx, ipc)
		assert.NoError(t, err)
		assert.Equal(t, requeueWithHealthCheckInterval(), res)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		_, annPresent := updated.GetAnnotations()[controllerutils.ManualCleanupAnnotation]
		assert.False(t, annPresent, "manual cleanup annotation should be removed")
		assertIdleCond(t, updated, metav1.ConditionFalse, controllerutils.ConditionReasons.Stabilizing, "Waiting for system to stabilize")
		assertStatusInvariants(t, updated, ipc)
	})

	t.Run("manual cleanup annotation removal update fails => returns requeueWithError and does not proceed to healthcheck", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockOps := ops.NewMockOps(gc)
		mockRpm := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)

		ipc := mkIPCForIdle(t)
		ipc.SetAnnotations(map[string]string{controllerutils.ManualCleanupAnnotation: ""})
		ipc.Status.Conditions = []metav1.Condition{{
			Type:               string(controllerutils.ConditionTypes.Idle),
			Status:             metav1.ConditionFalse,
			Reason:             string(controllerutils.ConditionReasons.Failed),
			Message:            "failed earlier",
			ObservedGeneration: ipc.Generation,
		}}

		baseClient := newFakeClientWithIPC(t, scheme, ipc)
		wrapped := &errUpdateClient{Client: baseClient, err: errors.New("update failed")}

		h := &IPCIdleStageHandler{
			Client:          wrapped,
			NoncachedClient: baseClient,
			ChrootOps:       mockOps,
			OstreeClient:    mockOstree,
			RPMOstreeClient: mockRpm,
		}

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error {
			t.Fatalf("CheckHealth should not be called when manual cleanup update fails")
			return nil
		}

		res, err := h.Handle(ctx, ipc)
		assert.Error(t, err)
		wantRes, _ := requeueWithError(err)
		assert.Equal(t, wantRes, res)
		assert.Contains(t, err.Error(), "failed to handle manual cleanup if failed")

		unchanged := mustGetIPC(t, baseClient, common.IPConfigName)
		_, annPresent := unchanged.GetAnnotations()[controllerutils.ManualCleanupAnnotation]
		assert.True(t, annPresent, "annotation should remain if update failed")
		assertStatusInvariants(t, unchanged, ipc)
	})

	t.Run("transition not requested but invalid stage in spec => still runs healthcheck/cleanup and can succeed", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		mockOps := ops.NewMockOps(gc)
		mockRpm := rpmostreeclient.NewMockIClient(gc)
		mockOstree := ostreeclient.NewMockIClient(gc)

		ipc := mkIPCForIdle(t)
		ipc.Spec.Stage = ipcv1.IPStages.Config
		// Transition not requested: set a completed condition for Config stage.
		controllerutils.SetIPConfigStatusCompleted(ipc, "done")
		ipc.Status.ValidNextStages = []ipcv1.IPConfigStage{ipcv1.IPStages.Idle} // would be invalid if validate ran

		k8sClient := newFakeClientWithIPC(t, scheme, ipc)
		h := &IPCIdleStageHandler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			ChrootOps:       mockOps,
			OstreeClient:    mockOstree,
			RPMOstreeClient: mockRpm,
		}

		oldHC := CheckHealth
		defer func() { CheckHealth = oldHC }()
		CheckHealth = func(ctx context.Context, c client.Reader, l logr.Logger) error { return nil }

		status := &rpmostreeclient.Status{
			Deployments: []rpmostreeclient.Deployment{{OSName: "rhcos", Booted: true}},
		}

		mockOps.EXPECT().RemountSysroot().Return(nil).Times(1)
		mockRpm.EXPECT().QueryStatus().Return(status, nil).Times(2)
		mockOps.EXPECT().RemountBoot().Return(nil).Times(1)
		mockOps.EXPECT().ReadDir(gomock.Any()).Return([]os.DirEntry{}, nil).Times(1)
		mockRpm.EXPECT().RpmOstreeCleanup().Return(nil).Times(1)
		mockOps.EXPECT().StatFile(gomock.Any()).Return(nil, errors.New("not exist")).Times(1)

		res, err := h.Handle(ctx, ipc)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPC(t, k8sClient, common.IPConfigName)
		assert.Len(t, updated.Status.Conditions, 1)
		assertIdleCond(t, updated, metav1.ConditionTrue, controllerutils.ConditionReasons.Idle, "")
		assertStatusInvariants(t, updated, ipc)
	})
}

type idleCleanupTestDirEntry struct {
	name  string
	isDir bool
}

func (e idleCleanupTestDirEntry) Name() string      { return e.name }
func (e idleCleanupTestDirEntry) IsDir() bool       { return e.isDir }
func (e idleCleanupTestDirEntry) Type() fs.FileMode { return 0 }
func (e idleCleanupTestDirEntry) Info() (fs.FileInfo, error) {
	return nil, errors.New("not implemented")
}

func TestGetStaterootsToRemove(t *testing.T) {
	gc := gomock.NewController(t)
	defer gc.Finish()

	mockRPM := rpmostreeclient.NewMockIClient(gc)

	mockRPM.EXPECT().QueryStatus().Return(&rpmostreeclient.Status{
		Deployments: []rpmostreeclient.Deployment{
			{OSName: "booted", Booted: true},
			{OSName: "old-1", Booted: false},
			{OSName: "old-2", Booted: false},
		},
	}, nil).Times(1)

	got, err := getStaterootsToRemove(mockRPM)
	assert.NoError(t, err)
	// Reverse order because code walks deployments from end to start.
	assert.Equal(t, []string{"old-2", "old-1"}, got)
}

func TestRemoveBootDirsByStaterootPrefixes(t *testing.T) {
	gc := gomock.NewController(t)
	defer gc.Finish()

	mockOps := ops.NewMockOps(gc)
	logger := logr.Logger{}

	bootOstreePath := common.PathOutsideChroot("/boot/ostree")
	entries := []os.DirEntry{
		idleCleanupTestDirEntry{name: "rhcos-aaa", isDir: true},
		idleCleanupTestDirEntry{name: "rhcos", isDir: true},      // no dash suffix => should not match
		idleCleanupTestDirEntry{name: "other-bbb", isDir: true},  // different stateroot
		idleCleanupTestDirEntry{name: "rhcos-ccc", isDir: false}, // not a dir => ignored
		idleCleanupTestDirEntry{name: "rhcos-zzz", isDir: true},  // should match
		idleCleanupTestDirEntry{name: "rhcos2-111", isDir: true}, // should not match "rhcos-"
	}

	mockOps.EXPECT().ReadDir(bootOstreePath).Return(entries, nil).Times(1)

	mockOps.EXPECT().RemoveAllFiles(filepath.Join(bootOstreePath, "rhcos-aaa")).Return(nil).Times(1)
	mockOps.EXPECT().RemoveAllFiles(filepath.Join(bootOstreePath, "rhcos-zzz")).Return(nil).Times(1)

	err := removeBootDirsByStaterootPrefixes(logger, mockOps, []string{"rhcos"})
	assert.NoError(t, err)
}

func TestRemoveBootDirsByStaterootPrefixes_NoBootDir(t *testing.T) {
	gc := gomock.NewController(t)
	defer gc.Finish()

	mockOps := ops.NewMockOps(gc)
	logger := logr.Logger{}

	bootOstreePath := common.PathOutsideChroot("/boot/ostree")
	mockOps.EXPECT().ReadDir(bootOstreePath).Return(nil, os.ErrNotExist).Times(1)
	mockOps.EXPECT().IsNotExist(os.ErrNotExist).Return(true).Times(1)

	err := removeBootDirsByStaterootPrefixes(logger, mockOps, []string{"rhcos"})
	assert.NoError(t, err)
}
