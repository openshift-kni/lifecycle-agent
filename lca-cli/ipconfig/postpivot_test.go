package ipconfig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/recert"
	ops "github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

func newPostPivotHandler(t *testing.T) (*IPConfigPostPivotHandler, *ops.MockOps) {
	t.Helper()

	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	mockOps := ops.NewMockOps(ctrl)
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	handler := &IPConfigPostPivotHandler{
		log:          logrus.New(),
		ops:          mockOps,
		recertImage:  "recert-image",
		scheme:       scheme,
		kubeconfig:   filepath.Join(t.TempDir(), "kubeconfig"),
		workspaceDir: t.TempDir(),
	}

	// Create a default nmstate config file for tests that exercise applyNetworkConfiguration().
	nmstatePath := filepath.Join(handler.workspaceDir, common.NmstateConfigFileName)
	nmstate := `interfaces:
- name: ens3
  type: ethernet
  state: up
  ipv4:
    enabled: true
    dhcp: false
    address:
    - ip: 10.1.1.10
      prefix-length: 24
routes:
  config:
  - destination: 0.0.0.0/0
    next-hop-interface: ens3
    next-hop-address: 10.1.1.1
    metric: 48
    table-id: 254
`
	if err := os.WriteFile(nmstatePath, []byte(nmstate), 0o600); err != nil {
		t.Fatalf("failed to write test nmstate config: %v", err)
	}

	return handler, mockOps
}

func kubeconfigForLocalhost() string {
	// Points to localhost:1 so connections fail fast; CreateKubeClient still succeeds.
	return `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://127.0.0.1:1
    insecure-skip-tls-verify: true
users:
- name: u
  user:
    token: dummy
contexts:
- name: ctx
  context:
    cluster: c
    user: u
current-context: ctx
`
}

func TestRunHappyPath(t *testing.T) {
	handler, opsMock := newPostPivotHandler(t)

	gomock.InOrder(
		opsMock.EXPECT().RunInHostNamespace("nmstatectl", "apply", filepath.Join(handler.workspaceDir, common.NmstateConfigFileName)).Return("", nil),
		opsMock.EXPECT().RecertFullFlow(
			handler.recertImage,
			filepath.Join(handler.workspaceDir, filepath.Base(common.IPConfigPullSecretFile)),
			filepath.Join(handler.workspaceDir, recert.RecertConfigFile),
			nil,
			nil,
			"-v", fmt.Sprintf("%s:%s", handler.workspaceDir, handler.workspaceDir),
		).Return(nil),
		opsMock.EXPECT().SystemctlAction("enable", "kubelet", "--now").Return("", nil),
		opsMock.EXPECT().ReadFile(handler.kubeconfig).Return([]byte(kubeconfigForLocalhost()), nil),
		opsMock.EXPECT().SystemctlAction("disable", "ip-configuration.service").Return("", nil),
	)

	// WaitForApi polls until ctx cancellation; keep test fast.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)
	err := handler.Run(ctx)
	assert.NoError(t, err)
}

func TestRunApplyNetworkConfigurationError(t *testing.T) {
	handler, opsMock := newPostPivotHandler(t)

	opsMock.EXPECT().RunInHostNamespace("nmstatectl", "apply", filepath.Join(handler.workspaceDir, common.NmstateConfigFileName)).
		Return("", errors.New("nmstate-fail"))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)
	err := handler.Run(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "apply-network-configuration")
}

func TestRunRecertError(t *testing.T) {
	handler, opsMock := newPostPivotHandler(t)

	gomock.InOrder(
		opsMock.EXPECT().RunInHostNamespace("nmstatectl", "apply", filepath.Join(handler.workspaceDir, common.NmstateConfigFileName)).Return("", nil),
		opsMock.EXPECT().RecertFullFlow(
			handler.recertImage,
			filepath.Join(handler.workspaceDir, filepath.Base(common.IPConfigPullSecretFile)),
			filepath.Join(handler.workspaceDir, recert.RecertConfigFile),
			nil,
			nil,
			"-v", fmt.Sprintf("%s:%s", handler.workspaceDir, handler.workspaceDir),
		).Return(errors.New("recert-fail")),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)
	err := handler.Run(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "run-recert")
}

func TestRunEnableKubeletError(t *testing.T) {
	handler, opsMock := newPostPivotHandler(t)

	gomock.InOrder(
		opsMock.EXPECT().RunInHostNamespace("nmstatectl", "apply", filepath.Join(handler.workspaceDir, common.NmstateConfigFileName)).Return("", nil),
		opsMock.EXPECT().RecertFullFlow(
			handler.recertImage,
			filepath.Join(handler.workspaceDir, filepath.Base(common.IPConfigPullSecretFile)),
			filepath.Join(handler.workspaceDir, recert.RecertConfigFile),
			nil,
			nil,
			"-v", fmt.Sprintf("%s:%s", handler.workspaceDir, handler.workspaceDir),
		).Return(nil),
		opsMock.EXPECT().SystemctlAction("enable", "kubelet", "--now").Return("", errors.New("enable-fail")),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)
	err := handler.Run(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "enable and start kubelet")
}

func TestRunCreateKubeClientError(t *testing.T) {
	handler, opsMock := newPostPivotHandler(t)
	// force CreateKubeClient to fail by returning an invalid kubeconfig payload
	opsMock.EXPECT().ReadFile(handler.kubeconfig).Return([]byte("{not a kubeconfig"), nil)

	gomock.InOrder(
		opsMock.EXPECT().RunInHostNamespace("nmstatectl", "apply", filepath.Join(handler.workspaceDir, common.NmstateConfigFileName)).Return("", nil),
		opsMock.EXPECT().RecertFullFlow(
			handler.recertImage,
			filepath.Join(handler.workspaceDir, filepath.Base(common.IPConfigPullSecretFile)),
			filepath.Join(handler.workspaceDir, recert.RecertConfigFile),
			nil,
			nil,
			"-v", fmt.Sprintf("%s:%s", handler.workspaceDir, handler.workspaceDir),
		).Return(nil),
		opsMock.EXPECT().SystemctlAction("enable", "kubelet", "--now").Return("", nil),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)
	err := handler.Run(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "create k8s client")
}

func TestRunDNSIPFamilyIsIgnoredInPostPivot(t *testing.T) {
	handler, opsMock := newPostPivotHandler(t)

	gomock.InOrder(
		opsMock.EXPECT().RunInHostNamespace("nmstatectl", "apply", filepath.Join(handler.workspaceDir, common.NmstateConfigFileName)).Return("", nil),
		opsMock.EXPECT().RecertFullFlow(
			handler.recertImage,
			filepath.Join(handler.workspaceDir, filepath.Base(common.IPConfigPullSecretFile)),
			filepath.Join(handler.workspaceDir, recert.RecertConfigFile),
			nil,
			nil,
			"-v", fmt.Sprintf("%s:%s", handler.workspaceDir, handler.workspaceDir),
		).Return(nil),
		opsMock.EXPECT().SystemctlAction("enable", "kubelet", "--now").Return("", nil),
		opsMock.EXPECT().ReadFile(handler.kubeconfig).Return([]byte(kubeconfigForLocalhost()), nil),
		opsMock.EXPECT().SystemctlAction("disable", "ip-configuration.service").Return("", nil),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)
	err := handler.Run(ctx)
	assert.NoError(t, err)
}

func TestRunDisableServiceError(t *testing.T) {
	handler, opsMock := newPostPivotHandler(t)

	gomock.InOrder(
		opsMock.EXPECT().RunInHostNamespace("nmstatectl", "apply", filepath.Join(handler.workspaceDir, common.NmstateConfigFileName)).Return("", nil),
		opsMock.EXPECT().RecertFullFlow(
			handler.recertImage,
			filepath.Join(handler.workspaceDir, filepath.Base(common.IPConfigPullSecretFile)),
			filepath.Join(handler.workspaceDir, recert.RecertConfigFile),
			nil,
			nil,
			"-v", fmt.Sprintf("%s:%s", handler.workspaceDir, handler.workspaceDir),
		).Return(nil),
		opsMock.EXPECT().SystemctlAction("enable", "kubelet", "--now").Return("", nil),
		opsMock.EXPECT().ReadFile(handler.kubeconfig).Return([]byte(kubeconfigForLocalhost()), nil),
		opsMock.EXPECT().SystemctlAction("disable", "ip-configuration.service").Return("", errors.New("disable-fail")),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)
	err := handler.Run(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "disable ip-configuration.service")
}

func TestRunRecert(t *testing.T) {
	handler, opsMock := newPostPivotHandler(t)

	opsMock.EXPECT().RecertFullFlow(
		handler.recertImage,
		filepath.Join(handler.workspaceDir, filepath.Base(common.IPConfigPullSecretFile)),
		filepath.Join(handler.workspaceDir, recert.RecertConfigFile),
		nil,
		nil,
		"-v",
		fmt.Sprintf("%s:%s", handler.workspaceDir, handler.workspaceDir),
	).Return(nil)
	assert.NoError(t, handler.runRecert())

	opsMock.EXPECT().RecertFullFlow(
		handler.recertImage,
		filepath.Join(handler.workspaceDir, filepath.Base(common.IPConfigPullSecretFile)),
		filepath.Join(handler.workspaceDir, recert.RecertConfigFile),
		nil,
		nil,
		"-v",
		fmt.Sprintf("%s:%s", handler.workspaceDir, handler.workspaceDir),
	).Return(errors.New("boom"))
	assert.Error(t, handler.runRecert())
}

func TestApplyNetworkConfiguration(t *testing.T) {
	handler, opsMock := newPostPivotHandler(t)

	nmstateConfigFile := filepath.Join(handler.workspaceDir, common.NmstateConfigFileName)
	opsMock.EXPECT().RunInHostNamespace("nmstatectl", "apply", nmstateConfigFile).Return("", nil)
	assert.NoError(t, handler.applyNetworkConfiguration())

	opsMock.EXPECT().RunInHostNamespace("nmstatectl", "apply", nmstateConfigFile).Return("", errors.New("apply-fail"))
	assert.Error(t, handler.applyNetworkConfiguration())
}

func TestEnableAndStartKubelet(t *testing.T) {
	handler, opsMock := newPostPivotHandler(t)

	opsMock.EXPECT().SystemctlAction("enable", "kubelet", "--now").Return("", nil)
	assert.NoError(t, handler.enableAndStartKubelet())

	opsMock.EXPECT().SystemctlAction("enable", "kubelet", "--now").Return("", errors.New("enable-fail"))
	assert.Error(t, handler.enableAndStartKubelet())
}
