package controllers

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"

	ipcv1 "github.com/openshift-kni/lifecycle-agent/api/ipconfig/v1"
	controllerutils "github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type reconcileTestDirEntry struct {
	name  string
	isDir bool
}

func (e reconcileTestDirEntry) Name() string               { return e.name }
func (e reconcileTestDirEntry) IsDir() bool                { return e.isDir }
func (e reconcileTestDirEntry) Type() fs.FileMode          { return 0 }
func (e reconcileTestDirEntry) Info() (fs.FileInfo, error) { return nil, errors.New("not implemented") }

func expectReconcileTestFSDefaults(chrootOps *ops.MockOps) {
	workspaceDir := common.PathOutsideChroot(common.LCAWorkspaceDir)
	chrootOps.EXPECT().MkdirAll(workspaceDir, os.FileMode(0o700)).Return(nil).AnyTimes()
	chrootOps.EXPECT().IsNotExist(os.ErrNotExist).Return(true).AnyTimes()

	// Default: no dnsmasq filter file present on the host.
	filterPath := common.PathOutsideChroot(common.DnsmasqFilterTargetPath)
	chrootOps.EXPECT().ReadFile(filterPath).Return(nil, os.ErrNotExist).AnyTimes()
}

func reconcileTestMinimalNmstateJSON() string {
	// Includes:
	// - br-ex ovs-bridge with an uplink port (ens3) so ExtractBrExUplinkName succeeds
	// - default routes for v4/v6
	// - DNS servers for v4/v6
	return `{
  "interfaces": [
    {
      "name": "br-ex",
      "type": "ovs-bridge",
      "ipv4": { "enabled": true, "address": [ { "ip": "192.0.2.10", "prefix-length": 24 } ] },
      "ipv6": { "enabled": false, "address": [] },
      "bridge": { "port": [ { "name": "br-ex" }, { "name": "ens3" } ] }
    },
    {
      "name": "ens3",
      "type": "ethernet",
      "ipv4": { "enabled": false, "address": [] },
      "ipv6": { "enabled": false, "address": [] }
    }
  ],
  "routes": {
    "running": [
      { "destination": "0.0.0.0/0", "next-hop-address": "192.0.2.1", "next-hop-interface": "br-ex" },
      { "destination": "::/0",      "next-hop-address": "2001:db8::1", "next-hop-interface": "br-ex" }
    ],
    "config": []
  },
  "dns-resolver": {
    "running": { "server": ["192.0.2.53", "2001:db8::53"] },
    "config": { "server": [] }
  }
}`
}

func reconcileTestInstallConfigConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-config-v1",
			Namespace: "kube-system",
		},
		Data: map[string]string{
			"install-config": "networking:\n  machineNetwork:\n  - cidr: 192.0.2.0/24\n",
		},
	}
}

func reconcileTestBaseIPC(stage ipcv1.IPConfigStage) *ipcv1.IPConfig {
	return &ipcv1.IPConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       common.IPConfigName,
			Generation: 5,
			Annotations: map[string]string{
				// Avoid recert caching by making cached==image.
				controllerutils.RecertImageAnnotation:       "quay.io/example/recert:latest",
				controllerutils.RecertCachedImageAnnotation: "quay.io/example/recert:latest",
				// Used by tests to ensure reconcile removes it before calling handlers.
				controllerutils.TriggerReconcileAnnotation: "1",
			},
		},
		Spec: ipcv1.IPConfigSpec{
			Stage: stage,
		},
	}
}

func assertReconcileTestRefreshedNetwork(t *testing.T, ipc *ipcv1.IPConfig) {
	t.Helper()

	if assert.NotNil(t, ipc.Status.IPv4) {
		assert.Equal(t, "192.0.2.10", ipc.Status.IPv4.Address)
		assert.Equal(t, "192.0.2.0/24", ipc.Status.IPv4.MachineNetwork)
		assert.Equal(t, "192.0.2.1", ipc.Status.IPv4.Gateway)
	}

	// No IPv6 in the fixture => should not be set.
	assert.Nil(t, ipc.Status.IPv6)

	assert.Equal(t, []string{"192.0.2.53", "2001:db8::53"}, ipc.Status.DNSServers)

	// No VLAN in nmstate JSON => should not be set.
	assert.Equal(t, 0, ipc.Status.VLANID)

	// When no dnsmasq filter file exists (or it's empty), the controller reports no filtering.
	assert.Equal(t, common.DNSFamilyNone, ipc.Status.DNSFilterOutFamily)
}

type reconcileTestErrReader struct{ err error }

func (e reconcileTestErrReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return e.err
}
func (e reconcileTestErrReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return e.err
}

type reconcileTestStatusErrClient struct {
	client.Client
	err error
}

type reconcileTestStatusErrWriter struct {
	client.SubResourceWriter
	err error
}

func (w *reconcileTestStatusErrWriter) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return w.err
}
func (w *reconcileTestStatusErrWriter) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return w.err
}

func (c *reconcileTestStatusErrClient) Status() client.SubResourceWriter {
	return &reconcileTestStatusErrWriter{SubResourceWriter: c.Client.Status(), err: c.err}
}

func TestIPConfigReconciler_Reconcile_Full(t *testing.T) {
	scheme := newIPConfigTestScheme(t)

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: common.IPConfigName}}

	t.Run("idle success => calls idle handler; refreshes status; resets history; removes trigger annotation before handler", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		node, _ := mkSNOObjects()
		cm := reconcileTestInstallConfigConfigMap()

		ipc := reconcileTestBaseIPC(ipcv1.IPStages.Idle)
		ipc.Status.History = []*ipcv1.IPHistory{{Stage: ipcv1.IPStages.Config, StartTime: metav1.Now()}} // should be reset

		k8sClient := newFakeClientWithStatus(t, scheme, ipc, node, cm)

		nsenterOps := ops.NewMockOps(gc)
		nsenterOps.EXPECT().
			RunInHostNamespace("nmstatectl", "show", "--json", "-q").
			Return(reconcileTestMinimalNmstateJSON(), nil).
			Times(1)

		chrootOps := ops.NewMockOps(gc)
		expectReconcileTestFSDefaults(chrootOps)

		idleHandler := NewMockIPConfigStageHandler(gc)
		idleHandler.EXPECT().
			Handle(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, got *ipcv1.IPConfig) (ctrl.Result, error) {
				anns := got.GetAnnotations()
				assert.NotNil(t, anns)
				assert.Equal(t, "", anns[controllerutils.TriggerReconcileAnnotation])
				return doNotRequeue(), nil
			}).
			Times(1)

		r := &IPConfigReconciler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			Scheme:          scheme,
			NsenterOps:      nsenterOps,
			ChrootOps:       chrootOps,
			IdleHandler:     idleHandler,
			ConfigHandler:   NewMockIPConfigStageHandler(gc),
			RollbackHandler: NewMockIPConfigStageHandler(gc),
		}

		res, err := r.Reconcile(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assert.Equal(t, int64(5), updated.Status.ObservedGeneration)
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
		assertReconcileTestRefreshedNetwork(t, updated)
		assert.Empty(t, updated.Status.History)
	})

	t.Run("config success => starts history; refreshes status; calls config handler", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		node, _ := mkSNOObjects()
		cm := reconcileTestInstallConfigConfigMap()

		ipc := reconcileTestBaseIPC(ipcv1.IPStages.Config)
		// Nil ValidNextStages triggers the early calculation+status update branch.
		ipc.Status.ValidNextStages = nil

		k8sClient := newFakeClientWithStatus(t, scheme, ipc, node, cm)

		nsenterOps := ops.NewMockOps(gc)
		nsenterOps.EXPECT().
			RunInHostNamespace("nmstatectl", "show", "--json", "-q").
			Return(reconcileTestMinimalNmstateJSON(), nil).
			Times(1)

		chrootOps := ops.NewMockOps(gc)
		expectReconcileTestFSDefaults(chrootOps)

		configHandler := NewMockIPConfigStageHandler(gc)
		configHandler.EXPECT().
			Handle(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, got *ipcv1.IPConfig) (ctrl.Result, error) {
				anns := got.GetAnnotations()
				assert.NotNil(t, anns)
				assert.Equal(t, "", anns[controllerutils.TriggerReconcileAnnotation])
				return requeueWithShortInterval(), nil
			}).
			Times(1)

		r := &IPConfigReconciler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			Scheme:          scheme,
			NsenterOps:      nsenterOps,
			ChrootOps:       chrootOps,
			IdleHandler:     NewMockIPConfigStageHandler(gc),
			ConfigHandler:   configHandler,
			RollbackHandler: NewMockIPConfigStageHandler(gc),
		}

		res, err := r.Reconcile(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, requeueWithShortInterval(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assert.Equal(t, int64(5), updated.Status.ObservedGeneration)
		// With no conditions, validNextStages defaults to "Idle" on initial creation.
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
		assertReconcileTestRefreshedNetwork(t, updated)
		if assert.Len(t, updated.Status.History, 1) {
			assert.Equal(t, ipcv1.IPStages.Config, updated.Status.History[0].Stage)
			assert.False(t, updated.Status.History[0].StartTime.IsZero())
		}
	})

	t.Run("rollback success => starts history; refreshes status; calls rollback handler", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		node, _ := mkSNOObjects()
		cm := reconcileTestInstallConfigConfigMap()

		ipc := reconcileTestBaseIPC(ipcv1.IPStages.Rollback)
		ipc.Status.ValidNextStages = []ipcv1.IPConfigStage{ipcv1.IPStages.Rollback}

		k8sClient := newFakeClientWithStatus(t, scheme, ipc, node, cm)

		nsenterOps := ops.NewMockOps(gc)
		nsenterOps.EXPECT().
			RunInHostNamespace("nmstatectl", "show", "--json", "-q").
			Return(reconcileTestMinimalNmstateJSON(), nil).
			Times(1)

		chrootOps := ops.NewMockOps(gc)
		expectReconcileTestFSDefaults(chrootOps)

		rollbackHandler := NewMockIPConfigStageHandler(gc)
		rollbackHandler.EXPECT().
			Handle(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, got *ipcv1.IPConfig) (ctrl.Result, error) {
				// validNextStages() depends on status conditions; simulate what the real rollback handler does.
				controllerutils.SetIPRollbackStatusInProgress(got, "Rollback is in progress")
				return doNotRequeue(), nil
			}).
			Times(1)

		r := &IPConfigReconciler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			Scheme:          scheme,
			NsenterOps:      nsenterOps,
			ChrootOps:       chrootOps,
			IdleHandler:     NewMockIPConfigStageHandler(gc),
			ConfigHandler:   NewMockIPConfigStageHandler(gc),
			RollbackHandler: rollbackHandler,
		}

		res, err := r.Reconcile(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assert.Equal(t, int64(5), updated.Status.ObservedGeneration)
		// In-progress rollback disallows any next stages.
		assert.Empty(t, updated.Status.ValidNextStages)
		assertReconcileTestRefreshedNetwork(t, updated)
		if assert.Len(t, updated.Status.History, 1) {
			assert.Equal(t, ipcv1.IPStages.Rollback, updated.Status.History[0].Stage)
			assert.False(t, updated.Status.History[0].StartTime.IsZero())
		}
	})

	t.Run("invalid stage => does not call any handler and does not requeue", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		node, _ := mkSNOObjects()
		cm := reconcileTestInstallConfigConfigMap()

		ipc := reconcileTestBaseIPC(ipcv1.IPConfigStage("InvalidStage"))
		ipc.Status.ValidNextStages = nil

		k8sClient := newFakeClientWithStatus(t, scheme, ipc, node, cm)

		nsenterOps := ops.NewMockOps(gc)
		nsenterOps.EXPECT().
			RunInHostNamespace("nmstatectl", "show", "--json", "-q").
			Return(reconcileTestMinimalNmstateJSON(), nil).
			Times(1)

		chrootOps := ops.NewMockOps(gc)
		expectReconcileTestFSDefaults(chrootOps)

		r := &IPConfigReconciler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			Scheme:          scheme,
			NsenterOps:      nsenterOps,
			ChrootOps:       chrootOps,
			IdleHandler:     NewMockIPConfigStageHandler(gc),
			ConfigHandler:   NewMockIPConfigStageHandler(gc),
			RollbackHandler: NewMockIPConfigStageHandler(gc),
		}

		res, err := r.Reconcile(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assert.Equal(t, int64(5), updated.Status.ObservedGeneration)
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
		assertReconcileTestRefreshedNetwork(t, updated)
		if assert.Len(t, updated.Status.History, 1) {
			assert.Equal(t, ipcv1.IPConfigStage("InvalidStage"), updated.Status.History[0].Stage)
			assert.False(t, updated.Status.History[0].StartTime.IsZero())
		}
	})

	t.Run("idle handler error => returns error and does not requeue; status updated", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		node, _ := mkSNOObjects()
		cm := reconcileTestInstallConfigConfigMap()

		ipc := reconcileTestBaseIPC(ipcv1.IPStages.Idle)
		k8sClient := newFakeClientWithStatus(t, scheme, ipc, node, cm)

		nsenterOps := ops.NewMockOps(gc)
		nsenterOps.EXPECT().
			RunInHostNamespace("nmstatectl", "show", "--json", "-q").
			Return(reconcileTestMinimalNmstateJSON(), nil).
			Times(1)

		chrootOps := ops.NewMockOps(gc)
		expectReconcileTestFSDefaults(chrootOps)

		idleHandler := NewMockIPConfigStageHandler(gc)
		idleHandler.EXPECT().
			Handle(gomock.Any(), gomock.Any()).
			Return(ctrl.Result{}, fmt.Errorf("boom")).
			Times(1)

		r := &IPConfigReconciler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			Scheme:          scheme,
			NsenterOps:      nsenterOps,
			ChrootOps:       chrootOps,
			IdleHandler:     idleHandler,
			ConfigHandler:   NewMockIPConfigStageHandler(gc),
			RollbackHandler: NewMockIPConfigStageHandler(gc),
		}

		res, err := r.Reconcile(ctx, req)
		assert.Error(t, err)
		assert.Equal(t, ctrl.Result{}, res)
		assert.Contains(t, err.Error(), "idle handler failed")

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assert.Equal(t, int64(5), updated.Status.ObservedGeneration)
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
		assertReconcileTestRefreshedNetwork(t, updated)
	})

	t.Run("config handler error => returns error; status updated; history started", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		node, _ := mkSNOObjects()
		cm := reconcileTestInstallConfigConfigMap()

		ipc := reconcileTestBaseIPC(ipcv1.IPStages.Config)
		k8sClient := newFakeClientWithStatus(t, scheme, ipc, node, cm)

		nsenterOps := ops.NewMockOps(gc)
		nsenterOps.EXPECT().
			RunInHostNamespace("nmstatectl", "show", "--json", "-q").
			Return(reconcileTestMinimalNmstateJSON(), nil).
			Times(1)

		chrootOps := ops.NewMockOps(gc)
		expectReconcileTestFSDefaults(chrootOps)

		configHandler := NewMockIPConfigStageHandler(gc)
		configHandler.EXPECT().
			Handle(gomock.Any(), gomock.Any()).
			Return(ctrl.Result{}, fmt.Errorf("boom")).
			Times(1)

		r := &IPConfigReconciler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			Scheme:          scheme,
			NsenterOps:      nsenterOps,
			ChrootOps:       chrootOps,
			IdleHandler:     NewMockIPConfigStageHandler(gc),
			ConfigHandler:   configHandler,
			RollbackHandler: NewMockIPConfigStageHandler(gc),
		}

		res, err := r.Reconcile(ctx, req)
		assert.Error(t, err)
		assert.Equal(t, ctrl.Result{}, res)
		assert.Contains(t, err.Error(), "config handler failed")

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assert.Equal(t, int64(5), updated.Status.ObservedGeneration)
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
		assertReconcileTestRefreshedNetwork(t, updated)
		assert.NotEmpty(t, updated.Status.History)
	})

	t.Run("rollback handler error => returns error; status updated; history started", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		node, _ := mkSNOObjects()
		cm := reconcileTestInstallConfigConfigMap()

		ipc := reconcileTestBaseIPC(ipcv1.IPStages.Rollback)
		ipc.Status.ValidNextStages = []ipcv1.IPConfigStage{ipcv1.IPStages.Rollback}
		k8sClient := newFakeClientWithStatus(t, scheme, ipc, node, cm)

		nsenterOps := ops.NewMockOps(gc)
		nsenterOps.EXPECT().
			RunInHostNamespace("nmstatectl", "show", "--json", "-q").
			Return(reconcileTestMinimalNmstateJSON(), nil).
			Times(1)

		chrootOps := ops.NewMockOps(gc)
		expectReconcileTestFSDefaults(chrootOps)

		rollbackHandler := NewMockIPConfigStageHandler(gc)
		rollbackHandler.EXPECT().
			Handle(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, got *ipcv1.IPConfig) (ctrl.Result, error) {
				// validNextStages() depends on status conditions; simulate what the real rollback handler does.
				controllerutils.SetIPRollbackStatusInProgress(got, "Rollback is in progress")
				return ctrl.Result{}, fmt.Errorf("boom")
			}).
			Times(1)

		r := &IPConfigReconciler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			Scheme:          scheme,
			NsenterOps:      nsenterOps,
			ChrootOps:       chrootOps,
			IdleHandler:     NewMockIPConfigStageHandler(gc),
			ConfigHandler:   NewMockIPConfigStageHandler(gc),
			RollbackHandler: rollbackHandler,
		}

		res, err := r.Reconcile(ctx, req)
		assert.Error(t, err)
		assert.Equal(t, ctrl.Result{}, res)
		assert.Contains(t, err.Error(), "rollback handler failed")

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assert.Equal(t, int64(5), updated.Status.ObservedGeneration)
		// In-progress rollback disallows any next stages.
		assert.Empty(t, updated.Status.ValidNextStages)
		assertReconcileTestRefreshedNetwork(t, updated)
		assert.NotEmpty(t, updated.Status.History)
	})

	t.Run("cache recert image failure is logged but reconcile continues", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		node, _ := mkSNOObjects()
		cm := reconcileTestInstallConfigConfigMap()

		ipc := reconcileTestBaseIPC(ipcv1.IPStages.Idle)
		// Force cacheRecertImageIfNeeded to attempt secret lookup and fail (no such secret).
		ipc.Annotations[controllerutils.RecertPullSecretAnnotation] = "missing"
		delete(ipc.Annotations, controllerutils.RecertCachedImageAnnotation)

		k8sClient := newFakeClientWithStatus(t, scheme, ipc, node, cm)

		nsenterOps := ops.NewMockOps(gc)
		nsenterOps.EXPECT().
			RunInHostNamespace("nmstatectl", "show", "--json", "-q").
			Return(reconcileTestMinimalNmstateJSON(), nil).
			Times(1)

		chrootOps := ops.NewMockOps(gc)
		expectReconcileTestFSDefaults(chrootOps)

		idleHandler := NewMockIPConfigStageHandler(gc)
		idleHandler.EXPECT().
			Handle(gomock.Any(), gomock.Any()).
			Return(doNotRequeue(), nil).
			Times(1)

		r := &IPConfigReconciler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			Scheme:          scheme,
			NsenterOps:      nsenterOps,
			ChrootOps:       chrootOps,
			IdleHandler:     idleHandler,
			ConfigHandler:   NewMockIPConfigStageHandler(gc),
			RollbackHandler: NewMockIPConfigStageHandler(gc),
		}

		res, err := r.Reconcile(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, doNotRequeue(), res)

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		// Since caching failed, cached annotation should remain unset.
		assert.Equal(t, "", updated.GetAnnotations()[controllerutils.RecertCachedImageAnnotation])
		assert.Equal(t, int64(5), updated.Status.ObservedGeneration)
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}, updated.Status.ValidNextStages)
		assertReconcileTestRefreshedNetwork(t, updated)
	})

	t.Run("workspace mkdir failure => returns error early", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		workspacePath := common.PathOutsideChroot(common.LCAWorkspaceDir)

		chrootOps := ops.NewMockOps(gc)
		chrootOps.EXPECT().MkdirAll(workspacePath, os.FileMode(0o700)).Return(errors.New("mkdir failed")).Times(1)

		r := &IPConfigReconciler{
			Client:          newFakeClientWithStatus(t, scheme),
			NoncachedClient: reconcileTestErrReader{err: errors.New("should not be reached")},
			Scheme:          scheme,
			NsenterOps:      ops.NewMockOps(gc),
			ChrootOps:       chrootOps,
			IdleHandler:     NewMockIPConfigStageHandler(gc),
			ConfigHandler:   NewMockIPConfigStageHandler(gc),
			RollbackHandler: NewMockIPConfigStageHandler(gc),
		}

		res, err := r.Reconcile(ctx, req)
		assert.Error(t, err)
		assert.Equal(t, ctrl.Result{}, res)
		assert.Contains(t, err.Error(), "failed to create workspace dir")
	})

	t.Run("getIPConfig error => returns error early", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		chrootOps := ops.NewMockOps(gc)
		expectReconcileTestFSDefaults(chrootOps)

		r := &IPConfigReconciler{
			Client:          newFakeClientWithStatus(t, scheme),
			NoncachedClient: reconcileTestErrReader{err: errors.New("get failed")},
			Scheme:          scheme,
			NsenterOps:      ops.NewMockOps(gc),
			ChrootOps:       chrootOps,
			IdleHandler:     NewMockIPConfigStageHandler(gc),
			ConfigHandler:   NewMockIPConfigStageHandler(gc),
			RollbackHandler: NewMockIPConfigStageHandler(gc),
		}

		res, err := r.Reconcile(ctx, req)
		assert.Error(t, err)
		assert.Equal(t, ctrl.Result{}, res)
		assert.Contains(t, err.Error(), "failed to get IPConfig")
	})

	t.Run("ipconfig not found => InitIPConfig creates it, but reconcile fails updating status for the unfetched object", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		node, _ := mkSNOObjects()
		cm := reconcileTestInstallConfigConfigMap()

		// No IPConfig object initially. InitIPConfig should create one.
		k8sClient := newFakeClientWithStatus(t, scheme, node, cm)

		chrootOps := ops.NewMockOps(gc)
		expectReconcileTestFSDefaults(chrootOps)

		nsenterOps := ops.NewMockOps(gc)
		nsenterOps.EXPECT().
			RunInHostNamespace("nmstatectl", "show", "--json", "-q").
			Return(reconcileTestMinimalNmstateJSON(), nil).
			AnyTimes()

		r := &IPConfigReconciler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			Scheme:          scheme,
			NsenterOps:      nsenterOps,
			ChrootOps:       chrootOps,
			IdleHandler:     NewMockIPConfigStageHandler(gc),
			ConfigHandler:   NewMockIPConfigStageHandler(gc),
			RollbackHandler: NewMockIPConfigStageHandler(gc),
		}

		res, err := r.Reconcile(ctx, req)
		assert.Error(t, err)
		assert.Equal(t, ctrl.Result{}, res)
		assert.Contains(t, err.Error(), "failed to update ipconfig status")

		created := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assert.Equal(t, ipcv1.IPStages.Idle, created.Spec.Stage)
	})

	t.Run("validNextStages calculation error (nil rpmOstreeClient with config in-progress) => returns error and ValidNextStages stays nil", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		node, _ := mkSNOObjects()
		cm := reconcileTestInstallConfigConfigMap()

		ipc := reconcileTestBaseIPC(ipcv1.IPStages.Config)
		controllerutils.SetIPConfigStatusInProgress(ipc, "in progress")
		ipc.Status.ValidNextStages = nil

		k8sClient := newFakeClientWithStatus(t, scheme, ipc, node, cm)

		chrootOps := ops.NewMockOps(gc)
		expectReconcileTestFSDefaults(chrootOps)

		nsenterOps := ops.NewMockOps(gc)
		nsenterOps.EXPECT().
			RunInHostNamespace("nmstatectl", "show", "--json", "-q").
			Return(reconcileTestMinimalNmstateJSON(), nil).
			AnyTimes()

		r := &IPConfigReconciler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			Scheme:          scheme,
			NsenterOps:      nsenterOps,
			ChrootOps:       chrootOps,
			RPMOstreeClient: nil, // forces error in isTargetStaterootBooted
			IdleHandler:     NewMockIPConfigStageHandler(gc),
			ConfigHandler:   NewMockIPConfigStageHandler(gc),
			RollbackHandler: NewMockIPConfigStageHandler(gc),
		}

		res, err := r.Reconcile(ctx, req)
		assert.Error(t, err)
		assert.Equal(t, ctrl.Result{}, res)
		assert.Contains(t, err.Error(), "failed to get valid next stages")

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assert.Equal(t, int64(5), updated.Status.ObservedGeneration)
		assert.Nil(t, updated.Status.ValidNextStages)
	})

	t.Run("refreshStatus error (invalid nmstate JSON) => returns error; network status not populated", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		node, _ := mkSNOObjects()
		cm := reconcileTestInstallConfigConfigMap()

		ipc := reconcileTestBaseIPC(ipcv1.IPStages.Idle)
		ipc.Status.ValidNextStages = []ipcv1.IPConfigStage{ipcv1.IPStages.Idle} // skip early status update

		k8sClient := newFakeClientWithStatus(t, scheme, ipc, node, cm)

		nsenterOps := ops.NewMockOps(gc)
		nsenterOps.EXPECT().
			RunInHostNamespace("nmstatectl", "show", "--json", "-q").
			Return("{invalid json", nil).
			Times(1)

		chrootOps := ops.NewMockOps(gc)
		expectReconcileTestFSDefaults(chrootOps)

		r := &IPConfigReconciler{
			Client:          k8sClient,
			NoncachedClient: k8sClient,
			Scheme:          scheme,
			NsenterOps:      nsenterOps,
			ChrootOps:       chrootOps,
			IdleHandler:     NewMockIPConfigStageHandler(gc),
			ConfigHandler:   NewMockIPConfigStageHandler(gc),
			RollbackHandler: NewMockIPConfigStageHandler(gc),
		}

		res, err := r.Reconcile(ctx, req)
		assert.Error(t, err)
		assert.Equal(t, ctrl.Result{}, res)
		assert.Contains(t, err.Error(), "failed to refresh status")

		updated := mustGetIPCConfig(t, k8sClient, common.IPConfigName)
		assert.Equal(t, int64(5), updated.Status.ObservedGeneration)
		assert.Nil(t, updated.Status.IPv4)
		assert.Nil(t, updated.Status.IPv6)
		assert.Equal(t, 0, updated.Status.VLANID)
	})

	t.Run("status update error after refreshStatus => returns error (and defer also fails to update status)", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()

		node, _ := mkSNOObjects()
		cm := reconcileTestInstallConfigConfigMap()

		ipc := reconcileTestBaseIPC(ipcv1.IPStages.Config)
		ipc.Status.ValidNextStages = []ipcv1.IPConfigStage{ipcv1.IPStages.Idle} // avoid early Status().Update path

		baseClient := newFakeClientWithStatus(t, scheme, ipc, node, cm)
		failingClient := &reconcileTestStatusErrClient{Client: baseClient, err: errors.New("status update failed")}

		nsenterOps := ops.NewMockOps(gc)
		nsenterOps.EXPECT().
			RunInHostNamespace("nmstatectl", "show", "--json", "-q").
			Return(reconcileTestMinimalNmstateJSON(), nil).
			Times(1)

		mockRPM := rpmostreeclient.NewMockIClient(gc)

		chrootOps := ops.NewMockOps(gc)
		expectReconcileTestFSDefaults(chrootOps)

		r := &IPConfigReconciler{
			Client:          failingClient,
			NoncachedClient: baseClient,
			Scheme:          scheme,
			NsenterOps:      nsenterOps,
			ChrootOps:       chrootOps,
			RPMOstreeClient: mockRPM,
			IdleHandler:     NewMockIPConfigStageHandler(gc),
			ConfigHandler:   NewMockIPConfigStageHandler(gc),
			RollbackHandler: NewMockIPConfigStageHandler(gc),
		}

		res, err := r.Reconcile(ctx, req)
		assert.Error(t, err)
		assert.Equal(t, ctrl.Result{}, res)
		assert.Contains(t, err.Error(), "failed to update ipconfig status")
		assert.Contains(t, err.Error(), "also failed to update ipconfig status")
	})
}

func TestInferDNSFilterOutFamily(t *testing.T) {
	filterPath := common.PathOutsideChroot(common.DnsmasqFilterTargetPath)

	t.Run("missing file => none", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()
		chrootOps := ops.NewMockOps(gc)
		r := &IPConfigReconciler{ChrootOps: chrootOps}

		chrootOps.EXPECT().ReadFile(filterPath).Return(nil, os.ErrNotExist).Times(1)
		chrootOps.EXPECT().IsNotExist(os.ErrNotExist).Return(true).Times(1)

		fam, err := r.inferDNSFilterOutFamily()
		assert.NoError(t, err)
		assert.Equal(t, common.DNSFamilyNone, *fam)
	})

	t.Run("ipv4 filter => ipv4", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()
		chrootOps := ops.NewMockOps(gc)
		r := &IPConfigReconciler{ChrootOps: chrootOps}

		chrootOps.EXPECT().ReadFile(filterPath).Return([]byte(common.DnsmasqFilterManagedByIPConfigHeader+common.DnsmasqFilterOutIPv4+"\n"), nil).Times(1)

		fam, err := r.inferDNSFilterOutFamily()
		assert.NoError(t, err)
		assert.Equal(t, common.IPv4FamilyName, *fam)
	})

	t.Run("ipv6 filter => ipv6", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()
		chrootOps := ops.NewMockOps(gc)
		r := &IPConfigReconciler{ChrootOps: chrootOps}

		chrootOps.EXPECT().ReadFile(filterPath).Return([]byte(common.DnsmasqFilterManagedByIPConfigHeader+common.DnsmasqFilterOutIPv6+"\n"), nil).Times(1)

		fam, err := r.inferDNSFilterOutFamily()
		assert.NoError(t, err)
		assert.Equal(t, common.IPv6FamilyName, *fam)
	})

	t.Run("unknown contents => error", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()
		chrootOps := ops.NewMockOps(gc)
		r := &IPConfigReconciler{ChrootOps: chrootOps}

		chrootOps.EXPECT().ReadFile(filterPath).Return([]byte("garbage=1\n"), nil).Times(1)

		_, err := r.inferDNSFilterOutFamily()
		if !assert.Error(t, err) {
			return
		}
		assert.Contains(t, err.Error(), "unknown filter file contents")
	})
}

func TestValidNextStages(t *testing.T) {
	t.Run("rollback in progress => none", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()
		mockRPM := rpmostreeclient.NewMockIClient(gc)

		ipc := &ipcv1.IPConfig{}
		controllerutils.SetIPRollbackStatusInProgress(ipc, "in progress")

		stages, err := validNextStages(ipc, mockRPM)
		assert.NoError(t, err)
		assert.Empty(t, stages)
	})

	t.Run("rollback failed => none", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()
		mockRPM := rpmostreeclient.NewMockIClient(gc)

		ipc := &ipcv1.IPConfig{}
		controllerutils.SetIPRollbackStatusFailed(ipc, "failed")

		stages, err := validNextStages(ipc, mockRPM)
		assert.NoError(t, err)
		assert.Empty(t, stages)
	})

	t.Run("config in progress and both target booted + unbooted available => rollback allowed", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()
		mockRPM := rpmostreeclient.NewMockIClient(gc)

		ipc := &ipcv1.IPConfig{}
		ipc.Spec.IPv4 = &ipcv1.IPv4Config{Address: "192.0.2.10"}
		ipc.Spec.DNSFilterOutFamily = common.IPv6FamilyName
		controllerutils.SetIPConfigStatusInProgress(ipc, "in progress")

		mockRPM.EXPECT().IsStaterootBooted(buildIPConfigStaterootName(ipc)).Return(true, nil).Times(1)
		mockRPM.EXPECT().GetUnbootedStaterootName().Return("some-unbooted", nil).Times(1)

		stages, err := validNextStages(ipc, mockRPM)
		assert.NoError(t, err)
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Rollback}, stages)
	})

	t.Run("config in progress and not booted => only idle allowed (no rollback)", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()
		mockRPM := rpmostreeclient.NewMockIClient(gc)

		ipc := &ipcv1.IPConfig{}
		ipc.Spec.IPv4 = &ipcv1.IPv4Config{Address: "192.0.2.10"}
		controllerutils.SetIPConfigStatusInProgress(ipc, "in progress")

		mockRPM.EXPECT().IsStaterootBooted(buildIPConfigStaterootName(ipc)).Return(false, nil).Times(1)
		mockRPM.EXPECT().GetUnbootedStaterootName().Return("some-unbooted", nil).Times(1)

		stages, err := validNextStages(ipc, mockRPM)
		assert.NoError(t, err)
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}, stages)
	})

	t.Run("config completed => idle and rollback", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()
		mockRPM := rpmostreeclient.NewMockIClient(gc)

		ipc := &ipcv1.IPConfig{}
		controllerutils.SetIPConfigStatusCompleted(ipc, "done")

		stages, err := validNextStages(ipc, mockRPM)
		assert.NoError(t, err)
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Idle, ipcv1.IPStages.Rollback}, stages)
	})

	t.Run("rollback completed => idle", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()
		mockRPM := rpmostreeclient.NewMockIClient(gc)

		ipc := &ipcv1.IPConfig{}
		controllerutils.SetIPRollbackStatusCompleted(ipc, "done")

		stages, err := validNextStages(ipc, mockRPM)
		assert.NoError(t, err)
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}, stages)
	})

	t.Run("initial creation (no idle condition) => idle", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()
		mockRPM := rpmostreeclient.NewMockIClient(gc)

		ipc := &ipcv1.IPConfig{
			Status: ipcv1.IPConfigStatus{
				Conditions: []metav1.Condition{},
			},
		}
		stages, err := validNextStages(ipc, mockRPM)
		assert.NoError(t, err)
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}, stages)
	})

	t.Run("config in progress with nil rpm client => error", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{}
		ipc.Spec.IPv4 = &ipcv1.IPv4Config{Address: "192.0.2.10"}
		controllerutils.SetIPConfigStatusInProgress(ipc, "in progress")

		_, err := validNextStages(ipc, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "rpmOstreeClient is nil")
	})

	t.Run("config in progress and unbooted not available => idle only", func(t *testing.T) {
		gc := gomock.NewController(t)
		defer gc.Finish()
		mockRPM := rpmostreeclient.NewMockIClient(gc)

		ipc := &ipcv1.IPConfig{}
		ipc.Spec.IPv4 = &ipcv1.IPv4Config{Address: "192.0.2.10"}
		controllerutils.SetIPConfigStatusInProgress(ipc, "in progress")

		mockRPM.EXPECT().IsStaterootBooted(buildIPConfigStaterootName(ipc)).Return(true, nil).Times(1)
		mockRPM.EXPECT().GetUnbootedStaterootName().Return("", nil).Times(1)

		stages, err := validNextStages(ipc, mockRPM)
		assert.NoError(t, err)
		assert.Equal(t, []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}, stages)
	})
}

func TestIsIPTransitionRequested(t *testing.T) {
	t.Run("idle stage requested when neither completed nor in-progress", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{}
		ipc.Spec.Stage = ipcv1.IPStages.Idle
		assert.True(t, isIPTransitionRequested(ipc))
	})

	t.Run("idle stage not requested when completed", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{}
		ipc.Generation = 1
		ipc.Spec.Stage = ipcv1.IPStages.Idle
		// For Idle, "completed" is represented by the Idle condition itself being True.
		ipc.Status.Conditions = []metav1.Condition{{
			Type:               string(controllerutils.ConditionTypes.Idle),
			Status:             metav1.ConditionTrue,
			Reason:             string(controllerutils.ConditionReasons.Idle),
			Message:            "Idle",
			ObservedGeneration: ipc.Generation,
		}}
		assert.False(t, isIPTransitionRequested(ipc))
	})

	t.Run("config stage not requested when failed", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{}
		ipc.Spec.Stage = ipcv1.IPStages.Config
		controllerutils.SetIPConfigStatusFailed(ipc, "failed")
		assert.False(t, isIPTransitionRequested(ipc))
	})

	t.Run("config stage requested when no conditions", func(t *testing.T) {
		ipc := &ipcv1.IPConfig{}
		ipc.Spec.Stage = ipcv1.IPStages.Config
		assert.True(t, isIPTransitionRequested(ipc))
	})
}

func TestBuildIPConfigStaterootName_Stable(t *testing.T) {
	// Ensure it does not panic and returns non-empty name for typical inputs.
	ipc := &ipcv1.IPConfig{
		Spec: ipcv1.IPConfigSpec{
			IPv4:               &ipcv1.IPv4Config{Address: "192.0.2.10/24"},
			IPv6:               &ipcv1.IPv6Config{Address: "2001:db8::10/64"},
			VLANID:             123,
			DNSFilterOutFamily: common.IPv6FamilyName,
		},
	}
	name := buildIPConfigStaterootName(ipc)
	assert.NotEmpty(t, name)
}

func TestShouldSkipIPClusterHealthChecks(t *testing.T) {
	assert.False(t, shouldSkipIPClusterHealthChecks(nil))
	assert.False(t, shouldSkipIPClusterHealthChecks(&ipcv1.IPConfig{}))

	ipc := &ipcv1.IPConfig{}
	ipc.SetAnnotations(map[string]string{controllerutils.SkipIPConfigClusterHealthChecksAnnotation: "any"})
	assert.True(t, shouldSkipIPClusterHealthChecks(ipc))
}

func TestValidNextStages_DoesNotMutateIPC(t *testing.T) {
	gc := gomock.NewController(t)
	defer gc.Finish()

	mockRPM := rpmostreeclient.NewMockIClient(gc)

	ipc := &ipcv1.IPConfig{}
	controllerutils.SetIPConfigStatusCompleted(ipc, "done")
	before := ipc.DeepCopy()

	_, err := validNextStages(ipc, mockRPM)
	assert.NoError(t, err)
	assert.Equal(t, before, ipc, "validNextStages should be pure and not mutate IPC")
}

func TestBuildNetworkStatus_SelectsFirstV4V6AndVLAN(t *testing.T) {
	v4, v6, vlan := buildNetworkStatus(
		"192.0.2.1",
		"2001:db8::1",
		[]string{"192.0.2.10", "192.0.2.11", "2001:db8::10"},
		[]string{"192.0.2.0/24", "2001:db8::/64"},
		func() *int { i := 100; return &i }(),
	)
	if assert.NotNil(t, v4) {
		assert.Equal(t, "192.0.2.10", v4.Address)
	}
	if assert.NotNil(t, v6) {
		assert.Equal(t, "2001:db8::10", v6.Address)
	}
	assert.Equal(t, 100, vlan)
}
