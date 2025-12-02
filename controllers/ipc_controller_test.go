package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	ipcv1 "github.com/openshift-kni/lifecycle-agent/api/ipconfig/v1"
	controllerutils "github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"github.com/openshift-kni/lifecycle-agent/utils"
	machineconfigv1 "github.com/openshift/api/machineconfiguration/v1"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newTestReconciler builds an IPConfigReconciler wired with fake clients and
// mocked dependencies so that Reconcile can be exercised end‑to‑end without
// touching the real cluster. It also wires mocked stage handlers which tests
// can configure per scenario.
func newTestReconciler(
	t *testing.T,
	ctrlMock *gomock.Controller,
	stage ipcv1.IPConfigStage,
) (*IPConfigReconciler,
	*ipcv1.IPConfig,
	*MockIPConfigStageHandler,
	*MockIPConfigStageHandler,
	*MockIPConfigStageHandler,
	*ops.MockOps,
	*ops.MockOps,
) {
	t.Helper()

	// Make sure our API types are registered in the scheme used by the fake client.
	sch := scheme.Scheme
	if err := ipcv1.AddToScheme(sch); err != nil {
		t.Fatalf("failed to add ipcv1 to scheme: %v", err)
	}
	if err := machineconfigv1.AddToScheme(sch); err != nil {
		t.Fatalf("failed to add machineconfigv1 to scheme: %v", err)
	}

	ipc := newTestIPConfig()
	ipc.Spec.Stage = stage

	// Seed objects needed by refreshStatus() and inferDNSResolutionFamilyFromMC().
	const podName = "ipconfig-controller-pod"
	podNS := common.LcaNamespace

	if err := os.Setenv("MY_POD_NAME", podName); err != nil {
		t.Fatalf("failed to set MY_POD_NAME: %v", err)
	}
	if err := os.Setenv("MY_POD_NAMESPACE", podNS); err != nil {
		t.Fatalf("failed to set MY_POD_NAMESPACE: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Unsetenv("MY_POD_NAME")
		_ = os.Unsetenv("MY_POD_NAMESPACE")
	})

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: podNS,
		},
		Spec: corev1.PodSpec{
			NodeName: "worker-0",
		},
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-0",
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.0.2.10"},
			},
		},
	}

	installCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      common.InstallConfigCM,
			Namespace: common.InstallConfigCMNamespace,
		},
		Data: map[string]string{
			common.InstallConfigCMInstallConfigDataKey: "networking:\n  machineNetwork:\n  - cidr: 192.0.2.0/24\n",
		},
	}

	mc := &machineconfigv1.MachineConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: common.DnsmasqMachineConfigName,
		},
		Spec: machineconfigv1.MachineConfigSpec{
			Config: runtime.RawExtension{
				Raw: []byte(`{}`),
			},
		},
	}

	baseClient := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(ipc, pod, node, installCM, mc).
		WithStatusSubresource(ipc).
		Build()

	// Wrap client so Status().Update is a no‑op, avoiding optimistic‑concurrency
	// issues in unit tests (same pattern as ipc_config_handlers_test.go).
	mockClient := &noStatusClient{Client: baseClient}

	mockChrootOps := ops.NewMockOps(ctrlMock)
	mockNsenterOps := ops.NewMockOps(ctrlMock)

	idleHandler := NewMockIPConfigStageHandler(ctrlMock)
	configHandler := NewMockIPConfigStageHandler(ctrlMock)
	rollbackHandler := NewMockIPConfigStageHandler(ctrlMock)

	reconciler := &IPConfigReconciler{
		Client:          mockClient,
		NoncachedClient: baseClient,
		Scheme:          sch,
		ChrootOps:       mockChrootOps,
		NsenterOps:      mockNsenterOps,
		RebootClient:    nil,
		RPMOstreeClient: nil,
		OstreeClient:    nil,
		Clientset:       nil,
		IdleHandler:     idleHandler,
		ConfigHandler:   configHandler,
		RollbackHandler: rollbackHandler,
		Mux:             nil,
	}

	return reconciler, ipc, idleHandler, configHandler, rollbackHandler, mockChrootOps, mockNsenterOps
}

// expectRefreshStatusSuccess configures the nsenter Ops mock to return a minimal
// but valid nmstate JSON that lets refreshStatus() succeed.
func expectRefreshStatusSuccess(t *testing.T, mockNsenterOps *ops.MockOps) {
	t.Helper()

	nm := utils.NmState{
		Interfaces: []utils.NmIf{
			{
				Name: controllerutils.BridgeExternalName,
				Type: "ovs-bridge",
				Bridge: utils.NmBridge{
					Port: []struct {
						Name string `json:"name"`
					}{
						// Name must not contain the bridge name so that
						// extractBrExUplinkName() treats it as a valid uplink.
						{Name: "enp0s8"},
					},
				},
			},
			{
				// This interface represents the uplink discovered above.
				Name: "enp0s8",
				Type: "vlan",
				VLAN: &utils.NmVLAN{ID: 100},
			},
		},
	}

	data, err := json.Marshal(nm)
	if err != nil {
		t.Fatalf("failed to marshal nmstate: %v", err)
	}

	mockNsenterOps.EXPECT().
		RunInHostNamespace("nmstatectl", "show", "--json", "-q").
		Return(string(data), nil)
}

// expectRecertCachingSuccess makes cacheRecertImageIfNeeded() succeed by
// returning success from podman pull.
func expectRecertCachingSuccess(mockChrootOps *ops.MockOps) {
	mockChrootOps.EXPECT().
		RunBashInHostNamespace(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return("ok", nil)
}

func Test_IPConfigReconciler_Reconcile_IdleStage_HandlerSuccess(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	r, _, idleHandler, _, _, mockChrootOps, mockNsenterOps :=
		newTestReconciler(t, ctrlMock, ipcv1.IPStages.Idle)

	expectRefreshStatusSuccess(t, mockNsenterOps)
	expectRecertCachingSuccess(mockChrootOps)

	// Idle handler should be invoked and its result returned.
	idleHandler.EXPECT().
		Handle(gomock.Any(), gomock.Any()).
		DoAndReturn(func(ctx context.Context, ipc *ipcv1.IPConfig) (ctrl.Result, error) {
			assert.Equal(t, ipcv1.IPStages.Idle, ipc.Spec.Stage)
			return doNotRequeue(), nil
		})

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: common.IPConfigName}}
	res, err := r.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)
}

func Test_IPConfigReconciler_Reconcile_ConfigStage_HandlerSuccess(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	r, _, _, configHandler, _, mockChrootOps, mockNsenterOps :=
		newTestReconciler(t, ctrlMock, ipcv1.IPStages.Config)

	expectRefreshStatusSuccess(t, mockNsenterOps)
	expectRecertCachingSuccess(mockChrootOps)

	configHandler.EXPECT().
		Handle(gomock.Any(), gomock.Any()).
		DoAndReturn(func(ctx context.Context, ipc *ipcv1.IPConfig) (ctrl.Result, error) {
			// Reconcile should hand the same object through to the handler.
			assert.Equal(t, ipcv1.IPStages.Config, ipc.Spec.Stage)
			return requeueWithShortInterval(), nil
		})

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: common.IPConfigName}}
	res, err := r.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, requeueWithShortInterval().RequeueAfter, res.RequeueAfter)
}

func Test_IPConfigReconciler_Reconcile_RollbackStage_HandlerSuccess(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	r, _, _, _, rollbackHandler, mockChrootOps, mockNsenterOps :=
		newTestReconciler(t, ctrlMock, ipcv1.IPStages.Rollback)

	expectRefreshStatusSuccess(t, mockNsenterOps)
	expectRecertCachingSuccess(mockChrootOps)

	rollbackHandler.EXPECT().
		Handle(gomock.Any(), gomock.Any()).
		Return(doNotRequeue(), nil)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: common.IPConfigName}}
	res, err := r.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)
}

func Test_IPConfigReconciler_Reconcile_ConfigStage_HandlerErrorPropagated(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	r, _, _, configHandler, _, mockChrootOps, mockNsenterOps :=
		newTestReconciler(t, ctrlMock, ipcv1.IPStages.Config)

	expectRefreshStatusSuccess(t, mockNsenterOps)
	expectRecertCachingSuccess(mockChrootOps)

	expectedErr := fmt.Errorf("handler error")
	configHandler.EXPECT().
		Handle(gomock.Any(), gomock.Any()).
		Return(doNotRequeue(), expectedErr)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: common.IPConfigName}}
	res, err := r.Reconcile(context.Background(), req)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "handler error")
	// Result still comes from the handler.
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)
}

func Test_IPConfigReconciler_Reconcile_RefreshStatusError(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	r, _, _, _, _, mockChrootOps, mockNsenterOps :=
		newTestReconciler(t, ctrlMock, ipcv1.IPStages.Config)

	// refreshStatus should fail because nmstatectl invocation fails.
	mockNsenterOps.EXPECT().
		RunInHostNamespace("nmstatectl", "show", "--json", "-q").
		Return("", fmt.Errorf("nmstate error"))
	// cacheRecertImageIfNeeded is never reached in this path.
	mockChrootOps.EXPECT().RunBashInHostNamespace(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Times(0)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: common.IPConfigName}}
	res, err := r.Reconcile(context.Background(), req)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to refresh status")
	// requeueWithError returns the zero-value Result.
	assert.Equal(t, ctrl.Result{}.RequeueAfter, res.RequeueAfter)
}

func Test_IPConfigReconciler_Reconcile_RecertCachingError_DoesNotBlock(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	r, _, idleHandler, _, _, mockChrootOps, mockNsenterOps :=
		newTestReconciler(t, ctrlMock, ipcv1.IPStages.Idle)

	expectRefreshStatusSuccess(t, mockNsenterOps)

	// Make cacheRecertImageIfNeeded fail; Reconcile should log but continue and
	// still call the stage handler.
	mockChrootOps.EXPECT().
		RunBashInHostNamespace(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return("", fmt.Errorf("podman pull error"))

	idleHandler.EXPECT().
		Handle(gomock.Any(), gomock.Any()).
		Return(doNotRequeue(), nil)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: common.IPConfigName}}
	res, err := r.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)
}

func Test_IPConfigReconciler_Reconcile_TriggerReconcileAnnotationCleared(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	r, ipc, idleHandler, _, _, mockChrootOps, mockNsenterOps :=
		newTestReconciler(t, ctrlMock, ipcv1.IPStages.Idle)

	annotations := ipc.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[controllerutils.TriggerReconcileAnnotation] = "true"
	ipc.SetAnnotations(annotations)

	expectRefreshStatusSuccess(t, mockNsenterOps)
	expectRecertCachingSuccess(mockChrootOps)

	idleHandler.EXPECT().
		Handle(gomock.Any(), gomock.Any()).
		DoAndReturn(func(ctx context.Context, obj *ipcv1.IPConfig) (ctrl.Result, error) {
			return doNotRequeue(), nil
		})

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: common.IPConfigName}}
	ctx := context.Background()
	_, err := r.Reconcile(ctx, req)
	assert.NoError(t, err)

	// Trigger annotation should be cleared by Reconcile on the persisted object.
	latest := &ipcv1.IPConfig{}
	err = r.Client.Get(ctx, types.NamespacedName{Name: common.IPConfigName}, latest)
	assert.NoError(t, err)
	finalAnnotations := latest.GetAnnotations()
	_, exists := finalAnnotations[controllerutils.TriggerReconcileAnnotation]
	assert.False(t, exists)
}

func Test_IPConfigReconciler_Reconcile_InvalidStage(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	r, ipc, _, _, _, mockChrootOps, mockNsenterOps :=
		newTestReconciler(t, ctrlMock, ipcv1.IPStages.Idle)

	// Set an invalid stage value to exercise the default branch in Reconcile.
	ipc.Spec.Stage = ipcv1.IPConfigStage("invalid-stage")

	// Persist the updated stage in the fake client so Reconcile observes it.
	ctx := context.Background()
	err := r.Client.Update(ctx, ipc)
	assert.NoError(t, err)

	expectRefreshStatusSuccess(t, mockNsenterOps)
	expectRecertCachingSuccess(mockChrootOps)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: common.IPConfigName}}
	res, err := r.Reconcile(ctx, req)

	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)
}

// errorReader implements client.Reader and always fails. It is used to force
// getIPConfig to fail so that the corresponding Reconcile error path is tested.
type errorReader struct{}

func (errorReader) Get(_ context.Context, _ crclient.ObjectKey, _ crclient.Object, _ ...crclient.GetOption) error {
	return fmt.Errorf("boom")
}

func (errorReader) List(_ context.Context, _ crclient.ObjectList, _ ...crclient.ListOption) error {
	return fmt.Errorf("boom")
}

func Test_IPConfigReconciler_Reconcile_GetIPConfigError(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	// Build a reconciler with a broken NoncachedClient while keeping other
	// dependencies minimal, since Reconcile should return before using them.
	sch := scheme.Scheme
	if err := ipcv1.AddToScheme(sch); err != nil {
		t.Fatalf("failed to add ipcv1 to scheme: %v", err)
	}

	ipc := newTestIPConfig()
	baseClient := fake.NewClientBuilder().WithScheme(sch).WithObjects(ipc).WithStatusSubresource(ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}

	r := &IPConfigReconciler{
		Client:          mockClient,
		NoncachedClient: errorReader{},
		Scheme:          sch,
		ChrootOps:       nil,
		NsenterOps:      nil,
		RebootClient:    nil,
		RPMOstreeClient: nil,
		OstreeClient:    nil,
		Clientset:       nil,
		IdleHandler:     NewMockIPConfigStageHandler(ctrlMock),
		ConfigHandler:   NewMockIPConfigStageHandler(ctrlMock),
		RollbackHandler: NewMockIPConfigStageHandler(ctrlMock),
		Mux:             nil,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: common.IPConfigName}}
	res, err := r.Reconcile(context.Background(), req)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get IPConfig")
	// requeueWithError returns the zero-value Result.
	assert.Equal(t, ctrl.Result{}.RequeueAfter, res.RequeueAfter)
}
