package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	ipcv1 "github.com/openshift-kni/lifecycle-agent/api/ipconfig/v1"
	controllerutils "github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	lcautils "github.com/openshift-kni/lifecycle-agent/utils"
	machineconfigv1 "github.com/openshift/api/machineconfiguration/v1"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// noStatusClient wraps a real controller-runtime client but makes all Status()
// operations no-ops. This avoids optimistic concurrency conflicts in unit tests
// that directly call Status().Update on the same in-memory object multiple
// times (which the fake client treats as "object was modified").
type noStatusClient struct {
	crclient.Client
}

type noopStatusWriter struct{}

func (noStatusClient) Status() crclient.StatusWriter {
	return noopStatusWriter{}
}

func (noopStatusWriter) Create(ctx context.Context, obj crclient.Object, subResource crclient.Object, opts ...crclient.SubResourceCreateOption) error {
	return nil
}

func (noopStatusWriter) Update(ctx context.Context, obj crclient.Object, opts ...crclient.SubResourceUpdateOption) error {
	return nil
}

func (noopStatusWriter) Patch(ctx context.Context, obj crclient.Object, patch crclient.Patch, opts ...crclient.SubResourcePatchOption) error {
	return nil
}

// newTestIPConfig returns a minimal IPConfig with generation set for conditions.
func newTestIPConfig() *ipcv1.IPConfig {
	return &ipcv1.IPConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       common.IPConfigName,
			Generation: 1,
		},
		Spec: ipcv1.IPConfigSpec{
			Stage: ipcv1.IPStages.Config,
		},
	}
}

func newFakeIPClient(t *testing.T, objs ...*ipcv1.IPConfig) *fake.ClientBuilder {
	t.Helper()
	sch := scheme.Scheme
	if err := ipcv1.AddToScheme(sch); err != nil {
		t.Fatalf("failed to add ipcv1 to scheme: %v", err)
	}
	if err := machineconfigv1.AddToScheme(sch); err != nil {
		t.Fatalf("failed to add machineconfigv1 to scheme: %v", err)
	}
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatalf("failed to add corev1 to scheme: %v", err)
	}

	builder := fake.NewClientBuilder().WithScheme(sch)

	// Always include a minimal dnsmasq MachineConfig so validateDNSMasqMCExists succeeds
	mc := &machineconfigv1.MachineConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: common.DnsmasqMachineConfigName,
		},
	}

	// And a single SNO master node so validateSNO() passes in tests.
	masterNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "master-0",
			Labels: map[string]string{
				"node-role.kubernetes.io/master": "",
			},
		},
	}

	objsAny := make([]crclient.Object, 0, len(objs)+2)
	objsAny = append(objsAny, mc, masterNode)
	for _, o := range objs {
		objsAny = append(objsAny, o)
	}

	builder = builder.WithObjects(objsAny...)

	if len(objs) > 0 {
		statusObjs := make([]crclient.Object, 0, len(objs))
		for _, o := range objs {
			statusObjs = append(statusObjs, o)
		}
		builder = builder.WithStatusSubresource(statusObjs...)
	}

	return builder
}

// --- Helper tests for small pure functions ---

func Test_ipEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{name: "equal IPv4 plain", a: "192.0.2.10", b: "192.0.2.10", want: true},
		{name: "equal IPv4 with CIDR", a: "192.0.2.10/24", b: "192.0.2.10", want: true},
		{name: "different IPv4", a: "192.0.2.10", b: "192.0.2.11", want: false},
		{name: "equal IPv6 with brackets", a: "[2001:db8::1]", b: "2001:db8::1", want: true},
		{name: "invalid IPs fallback equal", a: "not-an-ip", b: "not-an-ip", want: true},
		{name: "invalid IPs fallback not equal", a: "not-an-ip", b: "other", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ipEqual(tt.a, tt.b)
			assert.Equal(t, tt.want, got)
		})
	}
}

func Test_cidrEqual_and_parseCIDR(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{name: "equal IPv4 CIDR", a: "192.0.2.0/24", b: "192.0.2.0/24", want: true},
		{name: "equal IPv6 CIDR with brackets", a: "[2001:db8::/32]", b: "2001:db8::/32", want: true},
		{name: "different prefix", a: "192.0.2.0/24", b: "192.0.2.0/25", want: false},
		{name: "invalid fall back to string", a: "invalid", b: "invalid", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cidrEqual(tt.a, tt.b)
			assert.Equal(t, tt.want, got)
		})
	}

	ip, prefix, err := parseCIDR("[10.0.0.0/8]")
	assert.NoError(t, err)
	assert.Equal(t, "10.0.0.0", ip)
	assert.Equal(t, 8, prefix)
}

func Test_statusIPsMatchSpec_and_checkFamilyStatusMatchesSpec(t *testing.T) {
	ipc := &ipcv1.IPConfig{
		Spec: ipcv1.IPConfigSpec{
			DNSResolutionFamily: "ipv4",
			VLAN:                &ipcv1.VLANConfig{ID: 100},
			IPv4: &ipcv1.IPv4Config{
				Address:        "192.0.2.10/24",
				Gateway:        "192.0.2.1",
				DNSServer:      "192.0.2.53",
				MachineNetwork: "192.0.2.0/24",
			},
		},
		Status: ipcv1.IPConfigStatus{
			DNSResolutionFamily: "ipv4",
			Network: &ipcv1.NetworkStatus{
				HostNetwork: &ipcv1.HostNetworkStatus{
					VLANID: 100,
					IPv4: &ipcv1.HostIPStatus{
						Gateway:   "192.0.2.1",
						DNSServer: "192.0.2.53",
					},
				},
				ClusterNetwork: &ipcv1.ClusterNetworkStatus{
					IPv4: &ipcv1.ClusterIPStatus{
						Address:        "192.0.2.10",
						MachineNetwork: "192.0.2.0/24",
					},
				},
			},
		},
	}

	// Matching case
	assert.NoError(t, statusIPsMatchSpec(ipc))

	// Introduce various mismatches to ensure all branches are exercised.
	ipcMismatch := ipc.DeepCopy()
	ipcMismatch.Status.Network.HostNetwork.IPv4.Gateway = "192.0.2.254"
	ipcMismatch.Status.Network.HostNetwork.IPv4.DNSServer = "192.0.2.1"
	ipcMismatch.Status.Network.ClusterNetwork.IPv4.Address = "192.0.2.11"
	ipcMismatch.Status.Network.ClusterNetwork.IPv4.MachineNetwork = "192.0.3.0/24"

	err := statusIPsMatchSpec(ipcMismatch)
	assert.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "gateway mismatch")
	assert.Contains(t, msg, "dns mismatch")
	assert.Contains(t, msg, "cluster ipv4 not observed")
	assert.Contains(t, msg, "cluster ipv4 machineNetwork not observed")
}

// --- writeIPConfigRunConfig and getRecertImage ---

func Test_writeIPConfigRunConfig_WritesExpectedFields(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	mockOps := ops.NewMockOps(ctrlMock)
	handler := &IPConfigConfigPhasesHandler{
		ChrootOps: mockOps,
	}

	ipc := &ipcv1.IPConfig{
		ObjectMeta: metav1.ObjectMeta{Generation: 1},
		Spec: ipcv1.IPConfigSpec{
			IPv4: &ipcv1.IPv4Config{
				Address:        "192.0.2.10/24",
				Gateway:        "192.0.2.1",
				DNSServer:      "192.0.2.53",
				MachineNetwork: "192.0.2.0/24",
			},
			IPv6: &ipcv1.IPv6Config{
				Address:        "2001:db8::1",
				Gateway:        "2001:db8::ff",
				DNSServer:      "2001:db8::53",
				MachineNetwork: "2001:db8::/64",
			},
			VLAN: &ipcv1.VLANConfig{ID: 42},
		},
	}

	var written common.IPConfigRunConfig
	mockOps.EXPECT().
		WriteFile(common.PathOutsideChroot(common.IPConfigRunFlagsFile), gomock.Any(), os.FileMode(0o600)).
		DoAndReturn(func(_ string, data []byte, _ os.FileMode) error {
			return json.Unmarshal(data, &written)
		})

	err := handler.writeIPConfigRunConfig(ipc)
	assert.NoError(t, err)

	assert.Equal(t, "192.0.2.10", written.IPv4Address)
	assert.Equal(t, "192.0.2.0/24", written.IPv4MachineNetwork)
	assert.Equal(t, "192.0.2.1", written.IPv4Gateway)
	assert.Equal(t, "192.0.2.53", written.IPv4DNSServer)

	assert.Equal(t, "2001:db8::1", written.IPv6Address)
	assert.Equal(t, "2001:db8::/64", written.IPv6MachineNetwork)
	assert.Equal(t, "2001:db8::ff", written.IPv6Gateway)
	assert.Equal(t, "2001:db8::53", written.IPv6DNSServer)

	assert.Equal(t, 42, written.VLANID)
}

func Test_writeIPConfigRunConfig_WriteError(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	mockOps := ops.NewMockOps(ctrlMock)
	handler := &IPConfigConfigPhasesHandler{
		ChrootOps: mockOps,
	}

	mockOps.EXPECT().
		WriteFile(common.PathOutsideChroot(common.IPConfigRunFlagsFile), gomock.Any(), os.FileMode(0o600)).
		Return(fmt.Errorf("write failed"))

	ipc := &ipcv1.IPConfig{
		Spec: ipcv1.IPConfigSpec{
			IPv4: &ipcv1.IPv4Config{Address: "192.0.2.10/24"},
		},
	}

	err := handler.writeIPConfigRunConfig(ipc)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to write ip-config run config")
}

func Test_getRecertImage_PriorityOrder(t *testing.T) {
	ipc := newTestIPConfig()

	// 1. Annotation wins over env/default
	ipc.Annotations = map[string]string{
		controllerutils.RecertImageAnnotation: "from-annotation",
	}
	os.Unsetenv(common.RecertImageEnvKey)
	assert.Equal(t, "from-annotation", getRecertImage(ipc))

	// 2. Env when no annotation
	delete(ipc.Annotations, controllerutils.RecertImageAnnotation)
	os.Setenv(common.RecertImageEnvKey, "from-env")
	defer os.Unsetenv(common.RecertImageEnvKey)
	assert.Equal(t, "from-env", getRecertImage(ipc))

	// 3. Default when neither annotation nor env
	os.Unsetenv(common.RecertImageEnvKey)
	assert.Equal(t, common.DefaultRecertImage, getRecertImage(ipc))
}

// --- RunLcaCliIPConfigRun and runLcaCliIPConfigPrepare ---

func Test_RunLcaCliIPConfigRun_SuccessAndFailure(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	mockOps := ops.NewMockOps(ctrlMock)
	handler := &IPConfigConfigPhasesHandler{
		ChrootOps: mockOps,
	}
	logger := logr.Discard()

	// Success – expect exact arguments used by RunLcaCliIPConfigRun
	mockOps.EXPECT().RunSystemdAction(
		"--wait",
		"--collect",
		"--property", controllerutils.SystemdExitTypeCgroup,
		"--unit", controllerutils.IPConfigRunUnit,
		"--description", controllerutils.IPConfigRunDescription,
		controllerutils.LcaCliBinaryName, "ip-config", "run",
	).Return("ok", nil)

	assert.NoError(t, handler.RunLcaCliIPConfigRun(logger))

	// Failure – allow any args but with the same arity
	mockOps.EXPECT().RunSystemdAction(
		gomock.Any(), gomock.Any(), // --wait, --collect
		gomock.Any(), gomock.Any(), // --property, value
		gomock.Any(), gomock.Any(), // --unit, value
		gomock.Any(), gomock.Any(), // --description, value
		gomock.Any(), gomock.Any(), gomock.Any(), // binary, "ip-config", "run"
	).Return("", fmt.Errorf("systemd error"))
	err := handler.RunLcaCliIPConfigRun(logger)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ip-config run failed")
}

func Test_runLcaCliIPConfigPrepare_SuccessAndFailure(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	mockOps := ops.NewMockOps(ctrlMock)
	logger := logr.Discard()
	ipc := newTestIPConfig()

	// monitorEnabled = true, success
	mockOps.EXPECT().RunSystemdAction(
		"--wait",
		"--collect",
		"--property", controllerutils.SystemdExitTypeCgroup,
		"--unit", controllerutils.IPConfigPrepareUnit,
		"--description", controllerutils.IPConfigPrepareDescription,
		controllerutils.LcaCliBinaryName, "ip-config", "prepare",
		"--install-init-monitor",
		"--new-stateroot-name", gomock.Any(),
	).Return("ok", nil)

	assert.NoError(t, runLcaCliIPConfigPrepare(mockOps, logger, ipc, true))

	// monitorEnabled = false, failure
	mockOps.EXPECT().RunSystemdAction(
		"--wait",
		"--collect",
		"--property", controllerutils.SystemdExitTypeCgroup,
		"--unit", controllerutils.IPConfigPrepareUnit,
		"--description", controllerutils.IPConfigPrepareDescription,
		controllerutils.LcaCliBinaryName, "ip-config", "prepare",
		"--new-stateroot-name", gomock.Any(),
	).Return("", fmt.Errorf("systemd error"))

	err := runLcaCliIPConfigPrepare(mockOps, logger, ipc, false)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ip-config prepare failed")
}

// --- enableInitMonitorService / disableNodeipRerunUnit ---

func Test_enableInitMonitorService_VariousScenarios(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()
	mockOps := ops.NewMockOps(ctrlMock)

	handler := &IPConfigConfigPhasesHandler{
		ChrootOps: mockOps,
	}

	// Case 1: service active, stop fails
	mockOps.EXPECT().SystemctlAction("is-active", common.IPCInitMonitorService).Return("", nil)
	mockOps.EXPECT().SystemctlAction("stop", common.IPCInitMonitorService).Return("", fmt.Errorf("stop error"))
	requeue, err := handler.enableInitMonitorService()
	assert.True(t, requeue)
	assert.Error(t, err)

	// Case 2: service already enabled, no further work
	mockOps.EXPECT().SystemctlAction("is-active", common.IPCInitMonitorService).Return("", fmt.Errorf("inactive"))
	mockOps.EXPECT().SystemctlAction("is-enabled", common.IPCInitMonitorService).Return("", nil)
	requeue, err = handler.enableInitMonitorService()
	assert.True(t, requeue)
	assert.NoError(t, err)

	// Case 3: enable returns error
	mockOps.EXPECT().SystemctlAction("is-active", common.IPCInitMonitorService).Return("", fmt.Errorf("inactive"))
	mockOps.EXPECT().SystemctlAction("is-enabled", common.IPCInitMonitorService).Return("", fmt.Errorf("disabled"))
	mockOps.EXPECT().SystemctlAction("enable", common.IPCInitMonitorService).Return("", fmt.Errorf("enable error"))
	requeue, err = handler.enableInitMonitorService()
	assert.True(t, requeue)
	assert.Error(t, err)

	// Case 4: enable succeeds, no requeue requested
	mockOps.EXPECT().SystemctlAction("is-active", common.IPCInitMonitorService).Return("", fmt.Errorf("inactive"))
	mockOps.EXPECT().SystemctlAction("is-enabled", common.IPCInitMonitorService).Return("", fmt.Errorf("disabled"))
	mockOps.EXPECT().SystemctlAction("enable", common.IPCInitMonitorService).Return("", nil)
	requeue, err = handler.enableInitMonitorService()
	assert.False(t, requeue)
	assert.NoError(t, err)
}

func Test_disableNodeipRerunUnit(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()
	mockOps := ops.NewMockOps(ctrlMock)

	handler := &IPConfigConfigPhasesHandler{
		ChrootOps: mockOps,
	}

	// Case 1: unit not enabled -> no calls to disable, no error
	mockOps.EXPECT().SystemctlAction("is-enabled", lcautils.NodeipRerunUnitPath).Return("", fmt.Errorf("not enabled"))
	assert.NoError(t, handler.disableNodeipRerunUnit())

	// Case 2: disable fails
	mockOps.EXPECT().SystemctlAction("is-enabled", lcautils.NodeipRerunUnitPath).Return("", nil)
	mockOps.EXPECT().SystemctlAction("disable", lcautils.NodeipRerunUnitPath).Return("", fmt.Errorf("disable error"))
	err := handler.disableNodeipRerunUnit()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to disable")

	// Case 3: disable succeeds
	mockOps.EXPECT().SystemctlAction("is-enabled", lcautils.NodeipRerunUnitPath).Return("", nil)
	mockOps.EXPECT().SystemctlAction("disable", lcautils.NodeipRerunUnitPath).Return("", nil)
	assert.NoError(t, handler.disableNodeipRerunUnit())
}

// --- PrePivot / PreConfiguration / PostConfiguration ---

func Test_IPConfigConfigPhasesHandler_PrePivot_HealthcheckFailure(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newTestIPConfig()
	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}
	mockReader := mockClient
	mockOps := ops.NewMockOps(ctrlMock)

	handler := &IPConfigConfigPhasesHandler{
		Client:          mockClient,
		NoncachedClient: mockReader,
		ChrootOps:       mockOps,
	}

	ctx := context.Background()
	logger := logr.Discard()

	// Case 1: healthcheck fails
	ipc = newTestIPConfig()
	oldHC := CheckHealth
	CheckHealth = func(_ context.Context, _ crclient.Reader, _ logr.Logger) error {
		return fmt.Errorf("hc error")
	}
	res, err := handler.PrePivot(ctx, ipc, logger)
	CheckHealth = oldHC

	assert.NoError(t, err)
	assert.Equal(t, requeueWithHealthCheckInterval().RequeueAfter, res.RequeueAfter)
	cond := meta.FindStatusCondition(ipc.Status.Conditions, string(controllerutils.ConditionTypes.ConfigInProgress))
	assert.NotNil(t, cond)
	assert.Equal(t, string(controllerutils.ConditionReasons.InProgress), cond.Reason)
}

func Test_IPConfigConfigPhasesHandler_PrePivot_AutoRollbackConfigFailure(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newTestIPConfig()
	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}
	mockReader := mockClient
	mockOps := ops.NewMockOps(ctrlMock)

	handler := &IPConfigConfigPhasesHandler{
		Client:          mockClient,
		NoncachedClient: mockReader,
		ChrootOps:       mockOps,
	}

	ctx := context.Background()
	logger := logr.Discard()

	oldHC := CheckHealth
	defer func() {
		CheckHealth = oldHC
	}()

	CheckHealth = func(_ context.Context, _ crclient.Reader, _ logr.Logger) error {
		return nil
	}
	// First, allow the lca-cli binary copy to succeed.
	mockOps.EXPECT().CopyFile(
		controllerutils.LcaCliBinaryContainerPath,
		common.PathOutsideChroot(controllerutils.LcaCliBinaryHostPath),
		os.FileMode(0o777),
	).Return(nil)
	// Then make WriteIPCAutoRollbackConfigFile fail via the underlying ops.WriteFile.
	mockOps.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).Return(fmt.Errorf("autosave failed"))

	res, err := handler.PrePivot(ctx, ipc, logger)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to write ip-config auto-rollback config")
	assert.Equal(t, ctrl.Result{}.RequeueAfter, res.RequeueAfter)

	condCompleted := meta.FindStatusCondition(ipc.Status.Conditions, string(controllerutils.ConditionTypes.ConfigCompleted))
	assert.NotNil(t, condCompleted)
	assert.Equal(t, string(controllerutils.ConditionReasons.Failed), condCompleted.Reason)
}

func Test_IPConfigConfigPhasesHandler_PrePivot_RunPrepareFailureAndSuccess(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newTestIPConfig()
	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}
	mockReader := mockClient
	mockOps := ops.NewMockOps(ctrlMock)

	handler := &IPConfigConfigPhasesHandler{
		Client:          mockClient,
		NoncachedClient: mockReader,
		ChrootOps:       mockOps,
	}

	ctx := context.Background()
	logger := logr.Discard()

	oldHC := CheckHealth
	defer func() {
		CheckHealth = oldHC
	}()

	CheckHealth = func(_ context.Context, _ crclient.Reader, _ logr.Logger) error {
		return nil
	}
	// Allow the lca-cli binary copy and auto-rollback config write to succeed.
	mockOps.EXPECT().CopyFile(
		controllerutils.LcaCliBinaryContainerPath,
		common.PathOutsideChroot(controllerutils.LcaCliBinaryHostPath),
		os.FileMode(0o777),
	).Return(nil).AnyTimes()
	mockOps.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	// Case 1: runLcaCliIPConfigPrepare fails -> status failed and no requeue requested.
	mockOps.EXPECT().RunSystemdAction(
		"--wait",
		"--collect",
		"--property", controllerutils.SystemdExitTypeCgroup,
		"--unit", controllerutils.IPConfigPrepareUnit,
		"--description", controllerutils.IPConfigPrepareDescription,
		controllerutils.LcaCliBinaryName, "ip-config", "prepare",
		"--install-init-monitor",
		"--new-stateroot-name", gomock.Any(),
	).Return("", fmt.Errorf("systemd error"))

	res, err := handler.PrePivot(ctx, ipc, logger)
	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)
	condFailed := meta.FindStatusCondition(ipc.Status.Conditions, string(controllerutils.ConditionTypes.ConfigCompleted))
	assert.NotNil(t, condFailed)
	assert.Equal(t, string(controllerutils.ConditionReasons.Failed), condFailed.Reason)

	// Case 2: runLcaCliIPConfigPrepare succeeds -> short requeue.
	ipc = newTestIPConfig()
	baseClient = newFakeIPClient(t, ipc).Build()
	mockClient = &noStatusClient{Client: baseClient}
	handler.Client = mockClient
	handler.NoncachedClient = mockClient

	mockOps.EXPECT().RunSystemdAction(
		"--wait",
		"--collect",
		"--property", controllerutils.SystemdExitTypeCgroup,
		"--unit", controllerutils.IPConfigPrepareUnit,
		"--description", controllerutils.IPConfigPrepareDescription,
		controllerutils.LcaCliBinaryName, "ip-config", "prepare",
		"--install-init-monitor",
		"--new-stateroot-name", gomock.Any(),
	).Return("ok", nil)

	res, err = handler.PrePivot(ctx, ipc, logger)
	assert.NoError(t, err)
	assert.Equal(t, requeueWithShortInterval().RequeueAfter, res.RequeueAfter)
}

func Test_IPConfigConfigPhasesHandler_PreConfiguration_Flow(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newTestIPConfig()
	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}
	mockReader := mockClient
	mockOps := ops.NewMockOps(ctrlMock)

	handler := &IPConfigConfigPhasesHandler{
		Client:          mockClient,
		NoncachedClient: mockReader,
		ChrootOps:       mockOps,
	}

	ctx := context.Background()
	logger := logr.Discard()

	// Case 1: healthcheck fails
	ipc = newTestIPConfig()
	oldHC := CheckHealth
	CheckHealth = func(_ context.Context, _ crclient.Reader, _ logr.Logger) error {
		return errors.New("hc error")
	}
	res, err := handler.PreConfiguration(ctx, ipc, logger)
	CheckHealth = oldHC

	assert.NoError(t, err)
	assert.Equal(t, requeueWithHealthCheckInterval().RequeueAfter, res.RequeueAfter)

	// Case 2: enableInitMonitorService requests requeue
	ipc = newTestIPConfig()
	CheckHealth = func(_ context.Context, _ crclient.Reader, _ logr.Logger) error { return nil }
	// Simulate path where service already enabled
	mockOps.EXPECT().SystemctlAction("is-active", common.IPCInitMonitorService).Return("", fmt.Errorf("inactive"))
	mockOps.EXPECT().SystemctlAction("is-enabled", common.IPCInitMonitorService).Return("", nil)

	res, err = handler.PreConfiguration(ctx, ipc, logger)
	CheckHealth = oldHC
	assert.NoError(t, err)
	assert.Equal(t, 3*time.Second, res.RequeueAfter)

	// Case 3: full happy path reaching run
	ipc = newTestIPConfig()
	CheckHealth = func(_ context.Context, _ crclient.Reader, _ logr.Logger) error { return nil }

	// enableInitMonitorService -> enable call
	mockOps.EXPECT().SystemctlAction("is-active", common.IPCInitMonitorService).Return("", fmt.Errorf("inactive"))
	mockOps.EXPECT().SystemctlAction("is-enabled", common.IPCInitMonitorService).Return("", fmt.Errorf("disabled"))
	mockOps.EXPECT().SystemctlAction("enable", common.IPCInitMonitorService).Return("", nil)

	// writeIPConfigRunConfig
	mockOps.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	// RunLcaCliIPConfigRun
	mockOps.EXPECT().RunSystemdAction(
		gomock.Any(), gomock.Any(),
		gomock.Any(), gomock.Any(),
		gomock.Any(), gomock.Any(),
		gomock.Any(), gomock.Any(),
		gomock.Any(), gomock.Any(), gomock.Any(),
	).Return("ok", nil)

	res, err = handler.PreConfiguration(ctx, ipc, logger)
	CheckHealth = oldHC
	assert.NoError(t, err)
	assert.Equal(t, requeueWithShortInterval().RequeueAfter, res.RequeueAfter)
}

func Test_IPConfigConfigPhasesHandler_PreConfiguration_ErrorPaths(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newTestIPConfig()
	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}
	mockReader := mockClient
	mockOps := ops.NewMockOps(ctrlMock)

	handler := &IPConfigConfigPhasesHandler{
		Client:          mockClient,
		NoncachedClient: mockReader,
		ChrootOps:       mockOps,
	}

	ctx := context.Background()
	logger := logr.Discard()

	// Case 1: enableInitMonitorService returns error (stop failure)
	ipc = newTestIPConfig()
	oldHC := CheckHealth
	CheckHealth = func(_ context.Context, _ crclient.Reader, _ logr.Logger) error { return nil }
	mockOps.EXPECT().SystemctlAction("is-active", common.IPCInitMonitorService).Return("", nil)
	mockOps.EXPECT().SystemctlAction("stop", common.IPCInitMonitorService).Return("", fmt.Errorf("stop error"))

	res, err := handler.PreConfiguration(ctx, ipc, logger)
	CheckHealth = oldHC

	assert.Error(t, err)
	assert.Equal(t, ctrl.Result{}.RequeueAfter, res.RequeueAfter)
	condFailed := meta.FindStatusCondition(ipc.Status.Conditions, string(controllerutils.ConditionTypes.ConfigCompleted))
	assert.NotNil(t, condFailed)
	assert.Equal(t, string(controllerutils.ConditionReasons.Failed), condFailed.Reason)

	// Case 2: writeIPConfigRunConfig fails
	ipc = newTestIPConfig()
	CheckHealth = func(_ context.Context, _ crclient.Reader, _ logr.Logger) error { return nil }
	// enableInitMonitorService path -> enable call succeeds
	mockOps.EXPECT().SystemctlAction("is-active", common.IPCInitMonitorService).Return("", fmt.Errorf("inactive"))
	mockOps.EXPECT().SystemctlAction("is-enabled", common.IPCInitMonitorService).Return("", fmt.Errorf("disabled"))
	mockOps.EXPECT().SystemctlAction("enable", common.IPCInitMonitorService).Return("", nil)
	// writeIPConfigRunConfig error
	mockOps.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).Return(fmt.Errorf("write failed"))

	res, err = handler.PreConfiguration(ctx, ipc, logger)
	CheckHealth = oldHC

	assert.Error(t, err)
	assert.Equal(t, ctrl.Result{}.RequeueAfter, res.RequeueAfter)
	condFailed = meta.FindStatusCondition(ipc.Status.Conditions, string(controllerutils.ConditionTypes.ConfigCompleted))
	assert.NotNil(t, condFailed)
	assert.Equal(t, string(controllerutils.ConditionReasons.Failed), condFailed.Reason)

	// Case 3: RunLcaCliIPConfigRun fails
	ipc = newTestIPConfig()
	CheckHealth = func(_ context.Context, _ crclient.Reader, _ logr.Logger) error { return nil }
	// enableInitMonitorService path -> enable call succeeds
	mockOps.EXPECT().SystemctlAction("is-active", common.IPCInitMonitorService).Return("", fmt.Errorf("inactive"))
	mockOps.EXPECT().SystemctlAction("is-enabled", common.IPCInitMonitorService).Return("", fmt.Errorf("disabled"))
	mockOps.EXPECT().SystemctlAction("enable", common.IPCInitMonitorService).Return("", nil)
	// writeIPConfigRunConfig succeeds
	mockOps.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	// RunLcaCliIPConfigRun failure
	mockOps.EXPECT().RunSystemdAction(
		gomock.Any(), gomock.Any(),
		gomock.Any(), gomock.Any(),
		gomock.Any(), gomock.Any(),
		gomock.Any(), gomock.Any(),
		gomock.Any(), gomock.Any(), gomock.Any(),
	).Return("", fmt.Errorf("systemd error"))

	res, err = handler.PreConfiguration(ctx, ipc, logger)
	CheckHealth = oldHC

	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)
	condFailed = meta.FindStatusCondition(ipc.Status.Conditions, string(controllerutils.ConditionTypes.ConfigCompleted))
	assert.NotNil(t, condFailed)
	assert.Equal(t, string(controllerutils.ConditionReasons.Failed), condFailed.Reason)
}

func Test_IPConfigConfigPhasesHandler_PostConfiguration_Flows(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newTestIPConfig()
	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}
	mockReader := mockClient
	mockOps := ops.NewMockOps(ctrlMock)
	mockReboot := reboot.NewMockRebootIntf(ctrlMock)

	handler := &IPConfigConfigPhasesHandler{
		Client:          mockClient,
		NoncachedClient: mockReader,
		ChrootOps:       mockOps,
		RebootClient:    mockReboot,
	}

	ctx := context.Background()
	logger := logr.Discard()

	// Case 1: healthcheck fails
	ipc = newTestIPConfig()
	oldHC := CheckHealth
	CheckHealth = func(_ context.Context, _ crclient.Reader, _ logr.Logger) error {
		return fmt.Errorf("hc error")
	}
	res, err := handler.PostConfiguration(ctx, ipc, logger)
	CheckHealth = oldHC

	assert.NoError(t, err)
	assert.Equal(t, requeueWithHealthCheckInterval().RequeueAfter, res.RequeueAfter)

	// Case 2: statusIPsMatchSpec mismatch -> requeue
	ipc = newTestIPConfig()
	ipc.Status.Network = nil
	CheckHealth = func(_ context.Context, _ crclient.Reader, _ logr.Logger) error { return nil }
	res, err = handler.PostConfiguration(ctx, ipc, logger)
	CheckHealth = oldHC
	assert.NoError(t, err)
	assert.Equal(t, requeueWithHealthCheckInterval().RequeueAfter, res.RequeueAfter)

	// Case 3: DisableInitMonitor fails
	ipc = newTestIPConfig()
	ipc.Status.Network = &ipcv1.NetworkStatus{
		HostNetwork:    &ipcv1.HostNetworkStatus{},
		ClusterNetwork: &ipcv1.ClusterNetworkStatus{},
	}
	CheckHealth = func(_ context.Context, _ crclient.Reader, _ logr.Logger) error { return nil }
	mockReboot.EXPECT().DisableInitMonitor().Return(fmt.Errorf("disable error"))
	res, err = handler.PostConfiguration(ctx, ipc, logger)
	CheckHealth = oldHC
	assert.Error(t, err)
	// requeueWithError returns the zero-value Result; ensure that's what we get.
	assert.Equal(t, ctrl.Result{}.RequeueAfter, res.RequeueAfter)

	// Case 4: full success
	ipc = newTestIPConfig()
	ipc.Status.Network = &ipcv1.NetworkStatus{
		HostNetwork:    &ipcv1.HostNetworkStatus{},
		ClusterNetwork: &ipcv1.ClusterNetworkStatus{},
	}
	CheckHealth = func(_ context.Context, _ crclient.Reader, _ logr.Logger) error { return nil }
	mockReboot.EXPECT().DisableInitMonitor().Return(nil)
	mockOps.EXPECT().SystemctlAction("is-enabled", lcautils.NodeipRerunUnitPath).Return("", fmt.Errorf("not enabled"))

	res, err = handler.PostConfiguration(ctx, ipc, logger)
	CheckHealth = oldHC
	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)

	cond := meta.FindStatusCondition(ipc.Status.Conditions, string(controllerutils.ConditionTypes.ConfigCompleted))
	assert.NotNil(t, cond)
	assert.Equal(t, string(controllerutils.ConditionReasons.Completed), cond.Reason)
}

func Test_IPConfigConfigPhasesHandler_PostConfiguration_DisableNodeipRerunUnitError(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newTestIPConfig()
	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}
	mockReader := mockClient
	mockOps := ops.NewMockOps(ctrlMock)
	mockReboot := reboot.NewMockRebootIntf(ctrlMock)

	handler := &IPConfigConfigPhasesHandler{
		Client:          mockClient,
		NoncachedClient: mockReader,
		ChrootOps:       mockOps,
		RebootClient:    mockReboot,
	}

	ctx := context.Background()
	logger := logr.Discard()

	ipc = newTestIPConfig()
	ipc.Status.Network = &ipcv1.NetworkStatus{
		HostNetwork:    &ipcv1.HostNetworkStatus{},
		ClusterNetwork: &ipcv1.ClusterNetworkStatus{},
	}

	oldHC := CheckHealth
	CheckHealth = func(_ context.Context, _ crclient.Reader, _ logr.Logger) error { return nil }
	defer func() { CheckHealth = oldHC }()

	mockReboot.EXPECT().DisableInitMonitor().Return(nil)
	mockOps.EXPECT().SystemctlAction("is-enabled", lcautils.NodeipRerunUnitPath).Return("", nil)
	mockOps.EXPECT().SystemctlAction("disable", lcautils.NodeipRerunUnitPath).Return("", fmt.Errorf("disable error"))

	res, err := handler.PostConfiguration(ctx, ipc, logger)

	assert.Error(t, err)
	assert.Equal(t, ctrl.Result{}.RequeueAfter, res.RequeueAfter)
	condFailed := meta.FindStatusCondition(ipc.Status.Conditions, string(controllerutils.ConditionTypes.ConfigCompleted))
	assert.NotNil(t, condFailed)
	assert.Equal(t, string(controllerutils.ConditionReasons.Failed), condFailed.Reason)
}

// --- IPConfigConfigStageHandler.Handle with mocked phases handler ---

func Test_IPConfigConfigStageHandler_Handle_PrepareUnknown_PrePivotCalled(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	// ChrootOps used by ReadIPConfigStatus
	mockOps := ops.NewMockOps(ctrlMock)
	// prepare status Unknown -> Unknown
	status := common.IPConfigStatus{Phase: common.IPConfigStatusUnknown}
	data, _ := json.Marshal(status)
	mockOps.EXPECT().ReadFile(common.PathOutsideChroot(common.IPConfigPrepareStatusFile)).Return(data, nil)
	mockOps.EXPECT().IsNotExist(gomock.Any()).Return(false).AnyTimes()

	ctx := context.Background()
	ipc := newTestIPConfig()
	// Mark Config stage already completed so that isIPTransitionRequested(ipc) is false
	// and validateStageTransition is skipped in these Handle() tests.
	controllerutils.SetIPConfigStatusCompleted(ipc, "done")
	ipc.Status.Network = nil // force statusIPsMatchSpec mismatch

	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}
	mockReader := mockClient

	phasesMock := NewMockIPConfigConfigPhasesHandlerInterface(ctrlMock)

	handler := &IPConfigConfigStageHandler{
		Client:          mockClient,
		NoncachedClient: mockReader,
		RPMOstreeClient: rpmostreeclient.NewMockIClient(ctrlMock),
		ChrootOps:       mockOps,
		PhasesHandler:   phasesMock,
	}

	phasesMock.EXPECT().PrePivot(ctx, ipc, gomock.Any()).Return(doNotRequeue(), nil)

	res, err := handler.Handle(ctx, ipc)
	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)
}

// --- Additional Handle() tests covering all prepare/run status branches ---

// Build an IPConfig whose spec/status already match, so statusIPsMatchSpec returns nil.
func newMatchingIPConfig() *ipcv1.IPConfig {
	return &ipcv1.IPConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       common.IPConfigName,
			Generation: 1,
		},
		Spec: ipcv1.IPConfigSpec{
			Stage: ipcv1.IPStages.Config,
			IPv4: &ipcv1.IPv4Config{
				Address:        "192.0.2.10",
				Gateway:        "192.0.2.1",
				DNSServer:      "192.0.2.53",
				MachineNetwork: "192.0.2.0/24",
			},
		},
		Status: ipcv1.IPConfigStatus{
			Network: &ipcv1.NetworkStatus{
				HostNetwork: &ipcv1.HostNetworkStatus{
					IPv4: &ipcv1.HostIPStatus{
						Gateway:   "192.0.2.1",
						DNSServer: "192.0.2.53",
					},
				},
				ClusterNetwork: &ipcv1.ClusterNetworkStatus{
					IPv4: &ipcv1.ClusterIPStatus{
						Address:        "192.0.2.10",
						MachineNetwork: "192.0.2.0/24",
					},
				},
			},
		},
	}
}

func Test_IPConfigConfigStageHandler_Handle_ValidationFails(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newTestIPConfig()
	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}

	// leave ValidNextStages nil so validateIPConfigStage fails
	handler := &IPConfigConfigStageHandler{
		Client:          mockClient,
		NoncachedClient: mockClient,
		RPMOstreeClient: rpmostreeclient.NewMockIClient(ctrlMock),
		ChrootOps:       ops.NewMockOps(ctrlMock),
		PhasesHandler:   NewMockIPConfigConfigPhasesHandlerInterface(ctrlMock),
	}

	res, err := handler.Handle(context.Background(), ipc)
	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)
	assert.True(t, controllerutils.IsIPStageFailed(ipc, ipcv1.IPStages.Config))
}

func Test_IPConfigConfigStageHandler_Handle_PrepareStatusReadError(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newTestIPConfig()
	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}

	// Mark stage as completed so isIPTransitionRequested is false and validation is skipped.
	controllerutils.SetIPConfigStatusCompleted(ipc, "done")

	mockOps := ops.NewMockOps(ctrlMock)
	mockOps.EXPECT().
		ReadFile(common.PathOutsideChroot(common.IPConfigPrepareStatusFile)).
		Return(nil, fmt.Errorf("read error"))
	mockOps.EXPECT().IsNotExist(gomock.Any()).Return(false)

	handler := &IPConfigConfigStageHandler{
		Client:          mockClient,
		NoncachedClient: mockClient,
		RPMOstreeClient: rpmostreeclient.NewMockIClient(ctrlMock),
		ChrootOps:       mockOps,
		PhasesHandler:   NewMockIPConfigConfigPhasesHandlerInterface(ctrlMock),
	}

	_, err := handler.Handle(context.Background(), ipc)
	assert.Error(t, err)
	assert.True(t, controllerutils.IsIPStageFailed(ipc, ipcv1.IPStages.Config))
}

func Test_IPConfigConfigStageHandler_Handle_PrepareRunning(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newTestIPConfig()
	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}

	controllerutils.SetIPConfigStatusCompleted(ipc, "done")

	status := common.IPConfigStatus{Phase: common.IPConfigStatusRunning}
	data, _ := json.Marshal(status)

	mockOps := ops.NewMockOps(ctrlMock)
	mockOps.EXPECT().
		ReadFile(common.PathOutsideChroot(common.IPConfigPrepareStatusFile)).
		Return(data, nil)
	mockOps.EXPECT().IsNotExist(gomock.Any()).Return(false).AnyTimes()

	handler := &IPConfigConfigStageHandler{
		Client:          mockClient,
		NoncachedClient: mockClient,
		RPMOstreeClient: rpmostreeclient.NewMockIClient(ctrlMock),
		ChrootOps:       mockOps,
		PhasesHandler:   NewMockIPConfigConfigPhasesHandlerInterface(ctrlMock),
	}

	res, err := handler.Handle(context.Background(), ipc)
	assert.NoError(t, err)
	assert.Equal(t, requeueWithShortInterval().RequeueAfter, res.RequeueAfter)
}

func Test_IPConfigConfigStageHandler_Handle_PrepareFailed(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newTestIPConfig()
	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}

	controllerutils.SetIPConfigStatusCompleted(ipc, "done")

	status := common.IPConfigStatus{Phase: common.IPConfigStatusFailed, Message: "prepare failed"}
	data, _ := json.Marshal(status)

	mockOps := ops.NewMockOps(ctrlMock)
	mockOps.EXPECT().
		ReadFile(common.PathOutsideChroot(common.IPConfigPrepareStatusFile)).
		Return(data, nil)
	mockOps.EXPECT().IsNotExist(gomock.Any()).Return(false).AnyTimes()

	handler := &IPConfigConfigStageHandler{
		Client:          mockClient,
		NoncachedClient: mockClient,
		RPMOstreeClient: rpmostreeclient.NewMockIClient(ctrlMock),
		ChrootOps:       mockOps,
		PhasesHandler:   NewMockIPConfigConfigPhasesHandlerInterface(ctrlMock),
	}

	res, err := handler.Handle(context.Background(), ipc)
	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)
	assert.True(t, controllerutils.IsIPStageFailed(ipc, ipcv1.IPStages.Config))
}

func Test_IPConfigConfigStageHandler_Handle_PrepareUnknown_StatusMatchesSpec(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newMatchingIPConfig()
	controllerutils.SetIPConfigStatusCompleted(ipc, "done")
	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}

	status := common.IPConfigStatus{Phase: common.IPConfigStatusUnknown}
	data, _ := json.Marshal(status)

	mockOps := ops.NewMockOps(ctrlMock)
	mockOps.EXPECT().
		ReadFile(common.PathOutsideChroot(common.IPConfigPrepareStatusFile)).
		Return(data, nil)
	mockOps.EXPECT().IsNotExist(gomock.Any()).Return(false).AnyTimes()

	handler := &IPConfigConfigStageHandler{
		Client:          mockClient,
		NoncachedClient: mockClient,
		RPMOstreeClient: rpmostreeclient.NewMockIClient(ctrlMock),
		ChrootOps:       mockOps,
		PhasesHandler:   NewMockIPConfigConfigPhasesHandlerInterface(ctrlMock),
	}

	res, err := handler.Handle(context.Background(), ipc)
	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)
	assert.True(t, controllerutils.IsIPStageCompleted(ipc, ipcv1.IPStages.Config))
}

func Test_IPConfigConfigStageHandler_Handle_PrepareSucceeded_TargetNotBooted(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newTestIPConfig()
	controllerutils.SetIPConfigStatusCompleted(ipc, "done")
	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}

	prepStatus := common.IPConfigStatus{Phase: common.IPConfigStatusSucceeded}
	prepData, _ := json.Marshal(prepStatus)
	runStatus := common.IPConfigStatus{Phase: common.IPConfigStatusUnknown}
	runData, _ := json.Marshal(runStatus)

	mockOps := ops.NewMockOps(ctrlMock)
	gomock.InOrder(
		mockOps.EXPECT().
			ReadFile(common.PathOutsideChroot(common.IPConfigPrepareStatusFile)).
			Return(prepData, nil),
		mockOps.EXPECT().
			ReadFile(common.PathOutsideChroot(common.IPConfigRunStatusFile)).
			Return(runData, nil),
	)
	mockOps.EXPECT().IsNotExist(gomock.Any()).Return(false).AnyTimes()

	mockRpm := rpmostreeclient.NewMockIClient(ctrlMock)
	mockRpm.EXPECT().IsStaterootBooted(gomock.Any()).Return(false, nil)

	handler := &IPConfigConfigStageHandler{
		Client:          mockClient,
		NoncachedClient: mockClient,
		RPMOstreeClient: mockRpm,
		ChrootOps:       mockOps,
		PhasesHandler:   NewMockIPConfigConfigPhasesHandlerInterface(ctrlMock),
	}

	res, err := handler.Handle(context.Background(), ipc)
	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)
	assert.True(t, controllerutils.IsIPStageFailed(ipc, ipcv1.IPStages.Config))
}

func Test_IPConfigConfigStageHandler_Handle_PrepareSucceeded_RunStatusReadError(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newTestIPConfig()
	controllerutils.SetIPConfigStatusCompleted(ipc, "done")
	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}

	prepStatus := common.IPConfigStatus{Phase: common.IPConfigStatusSucceeded}
	prepData, _ := json.Marshal(prepStatus)

	mockOps := ops.NewMockOps(ctrlMock)
	gomock.InOrder(
		mockOps.EXPECT().
			ReadFile(common.PathOutsideChroot(common.IPConfigPrepareStatusFile)).
			Return(prepData, nil),
		mockOps.EXPECT().
			ReadFile(common.PathOutsideChroot(common.IPConfigRunStatusFile)).
			Return(nil, fmt.Errorf("run read error")),
	)
	mockOps.EXPECT().IsNotExist(gomock.Any()).Return(false).AnyTimes()

	mockRpm := rpmostreeclient.NewMockIClient(ctrlMock)

	handler := &IPConfigConfigStageHandler{
		Client:          mockClient,
		NoncachedClient: mockClient,
		RPMOstreeClient: mockRpm,
		ChrootOps:       mockOps,
		PhasesHandler:   NewMockIPConfigConfigPhasesHandlerInterface(ctrlMock),
	}

	_, err := handler.Handle(context.Background(), ipc)
	assert.Error(t, err)
	assert.True(t, controllerutils.IsIPStageFailed(ipc, ipcv1.IPStages.Config))
}

func Test_IPConfigConfigStageHandler_Handle_PrepareSucceeded_RunRunning(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newTestIPConfig()
	controllerutils.SetIPConfigStatusCompleted(ipc, "done")
	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}

	prepStatus := common.IPConfigStatus{Phase: common.IPConfigStatusSucceeded}
	prepData, _ := json.Marshal(prepStatus)
	runStatus := common.IPConfigStatus{Phase: common.IPConfigStatusRunning}
	runData, _ := json.Marshal(runStatus)

	mockOps := ops.NewMockOps(ctrlMock)
	gomock.InOrder(
		mockOps.EXPECT().
			ReadFile(common.PathOutsideChroot(common.IPConfigPrepareStatusFile)).
			Return(prepData, nil),
		mockOps.EXPECT().
			ReadFile(common.PathOutsideChroot(common.IPConfigRunStatusFile)).
			Return(runData, nil),
	)
	mockOps.EXPECT().IsNotExist(gomock.Any()).Return(false).AnyTimes()

	mockRpm := rpmostreeclient.NewMockIClient(ctrlMock)

	handler := &IPConfigConfigStageHandler{
		Client:          mockClient,
		NoncachedClient: mockClient,
		RPMOstreeClient: mockRpm,
		ChrootOps:       mockOps,
		PhasesHandler:   NewMockIPConfigConfigPhasesHandlerInterface(ctrlMock),
	}

	res, err := handler.Handle(context.Background(), ipc)
	assert.NoError(t, err)
	assert.Equal(t, requeueWithShortInterval().RequeueAfter, res.RequeueAfter)
}

func Test_IPConfigConfigStageHandler_Handle_PrepareSucceeded_RunUnknown_DelegatesPreConfiguration(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newTestIPConfig()
	controllerutils.SetIPConfigStatusCompleted(ipc, "done")
	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}

	prepStatus := common.IPConfigStatus{Phase: common.IPConfigStatusSucceeded}
	prepData, _ := json.Marshal(prepStatus)
	runStatus := common.IPConfigStatus{Phase: common.IPConfigStatusUnknown}
	runData, _ := json.Marshal(runStatus)

	mockOps := ops.NewMockOps(ctrlMock)
	gomock.InOrder(
		mockOps.EXPECT().
			ReadFile(common.PathOutsideChroot(common.IPConfigPrepareStatusFile)).
			Return(prepData, nil),
		mockOps.EXPECT().
			ReadFile(common.PathOutsideChroot(common.IPConfigRunStatusFile)).
			Return(runData, nil),
	)
	mockOps.EXPECT().IsNotExist(gomock.Any()).Return(false).AnyTimes()

	mockRpm := rpmostreeclient.NewMockIClient(ctrlMock)
	mockRpm.EXPECT().IsStaterootBooted(gomock.Any()).Return(true, nil)

	phasesMock := NewMockIPConfigConfigPhasesHandlerInterface(ctrlMock)
	phasesMock.EXPECT().
		PreConfiguration(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(doNotRequeue(), nil)

	handler := &IPConfigConfigStageHandler{
		Client:          mockClient,
		NoncachedClient: mockClient,
		RPMOstreeClient: mockRpm,
		ChrootOps:       mockOps,
		PhasesHandler:   phasesMock,
	}

	res, err := handler.Handle(context.Background(), ipc)
	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)
}

func Test_IPConfigConfigStageHandler_Handle_PrepareSucceeded_RunFailed(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newTestIPConfig()
	controllerutils.SetIPConfigStatusCompleted(ipc, "done")
	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}

	prepStatus := common.IPConfigStatus{Phase: common.IPConfigStatusSucceeded}
	prepData, _ := json.Marshal(prepStatus)
	runStatus := common.IPConfigStatus{Phase: common.IPConfigStatusFailed, Message: "run failed"}
	runData, _ := json.Marshal(runStatus)

	mockOps := ops.NewMockOps(ctrlMock)
	gomock.InOrder(
		mockOps.EXPECT().
			ReadFile(common.PathOutsideChroot(common.IPConfigPrepareStatusFile)).
			Return(prepData, nil),
		mockOps.EXPECT().
			ReadFile(common.PathOutsideChroot(common.IPConfigRunStatusFile)).
			Return(runData, nil),
	)
	mockOps.EXPECT().IsNotExist(gomock.Any()).Return(false).AnyTimes()

	mockRpm := rpmostreeclient.NewMockIClient(ctrlMock)

	handler := &IPConfigConfigStageHandler{
		Client:          mockClient,
		NoncachedClient: mockClient,
		RPMOstreeClient: mockRpm,
		ChrootOps:       mockOps,
		PhasesHandler:   NewMockIPConfigConfigPhasesHandlerInterface(ctrlMock),
	}

	res, err := handler.Handle(context.Background(), ipc)
	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)
	assert.True(t, controllerutils.IsIPStageFailed(ipc, ipcv1.IPStages.Config))
}

func Test_IPConfigConfigStageHandler_Handle_PrepareSucceeded_RunSucceeded_DelegatesPostConfiguration(t *testing.T) {
	ctrlMock := gomock.NewController(t)
	defer ctrlMock.Finish()

	ipc := newTestIPConfig()
	controllerutils.SetIPConfigStatusCompleted(ipc, "done")
	baseClient := newFakeIPClient(t, ipc).Build()
	mockClient := &noStatusClient{Client: baseClient}

	prepStatus := common.IPConfigStatus{Phase: common.IPConfigStatusSucceeded}
	prepData, _ := json.Marshal(prepStatus)
	runStatus := common.IPConfigStatus{Phase: common.IPConfigStatusSucceeded}
	runData, _ := json.Marshal(runStatus)

	mockOps := ops.NewMockOps(ctrlMock)
	gomock.InOrder(
		mockOps.EXPECT().
			ReadFile(common.PathOutsideChroot(common.IPConfigPrepareStatusFile)).
			Return(prepData, nil),
		mockOps.EXPECT().
			ReadFile(common.PathOutsideChroot(common.IPConfigRunStatusFile)).
			Return(runData, nil),
	)
	mockOps.EXPECT().IsNotExist(gomock.Any()).Return(false).AnyTimes()

	mockRpm := rpmostreeclient.NewMockIClient(ctrlMock)

	phasesMock := NewMockIPConfigConfigPhasesHandlerInterface(ctrlMock)
	phasesMock.EXPECT().
		PostConfiguration(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(doNotRequeue(), nil)

	handler := &IPConfigConfigStageHandler{
		Client:          mockClient,
		NoncachedClient: mockClient,
		RPMOstreeClient: mockRpm,
		ChrootOps:       mockOps,
		PhasesHandler:   phasesMock,
	}

	res, err := handler.Handle(context.Background(), ipc)
	assert.NoError(t, err)
	assert.Equal(t, doNotRequeue().RequeueAfter, res.RequeueAfter)
}
