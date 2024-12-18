// Code generated by MockGen. DO NOT EDIT.
// Source: ops.go
//
// Generated by this command:
//
//	mockgen -source=ops.go -package=ops -destination=mock_ops.go
//
// Package ops is a generated GoMock package.
package ops

import (
	reflect "reflect"

	logrus "github.com/sirupsen/logrus"
	gomock "go.uber.org/mock/gomock"
)

// MockOps is a mock of Ops interface.
type MockOps struct {
	ctrl     *gomock.Controller
	recorder *MockOpsMockRecorder
}

// MockOpsMockRecorder is the mock recorder for MockOps.
type MockOpsMockRecorder struct {
	mock *MockOps
}

// NewMockOps creates a new mock instance.
func NewMockOps(ctrl *gomock.Controller) *MockOps {
	mock := &MockOps{ctrl: ctrl}
	mock.recorder = &MockOpsMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockOps) EXPECT() *MockOpsMockRecorder {
	return m.recorder
}

// Chroot mocks base method.
func (m *MockOps) Chroot(chrootPath string) (func() error, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Chroot", chrootPath)
	ret0, _ := ret[0].(func() error)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// Chroot indicates an expected call of Chroot.
func (mr *MockOpsMockRecorder) Chroot(chrootPath any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Chroot", reflect.TypeOf((*MockOps)(nil).Chroot), chrootPath)
}

// CreateExtraPartition mocks base method.
func (m *MockOps) CreateExtraPartition(installationDisk, extraPartitionLabel, extraPartitionStart string, extraPartitionNumber uint) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "CreateExtraPartition", installationDisk, extraPartitionLabel, extraPartitionStart, extraPartitionNumber)
	ret0, _ := ret[0].(error)
	return ret0
}

// CreateExtraPartition indicates an expected call of CreateExtraPartition.
func (mr *MockOpsMockRecorder) CreateExtraPartition(installationDisk, extraPartitionLabel, extraPartitionStart, extraPartitionNumber any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "CreateExtraPartition", reflect.TypeOf((*MockOps)(nil).CreateExtraPartition), installationDisk, extraPartitionLabel, extraPartitionStart, extraPartitionNumber)
}

// CreateIsoWithEmbeddedIgnition mocks base method.
func (m *MockOps) CreateIsoWithEmbeddedIgnition(log logrus.FieldLogger, ignitionBytes []byte, baseIsoPath, outputIsoPath string) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "CreateIsoWithEmbeddedIgnition", log, ignitionBytes, baseIsoPath, outputIsoPath)
	ret0, _ := ret[0].(error)
	return ret0
}

// CreateIsoWithEmbeddedIgnition indicates an expected call of CreateIsoWithEmbeddedIgnition.
func (mr *MockOpsMockRecorder) CreateIsoWithEmbeddedIgnition(log, ignitionBytes, baseIsoPath, outputIsoPath any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "CreateIsoWithEmbeddedIgnition", reflect.TypeOf((*MockOps)(nil).CreateIsoWithEmbeddedIgnition), log, ignitionBytes, baseIsoPath, outputIsoPath)
}

// ExtractTarWithSELinux mocks base method.
func (m *MockOps) ExtractTarWithSELinux(srcPath, destPath string) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "ExtractTarWithSELinux", srcPath, destPath)
	ret0, _ := ret[0].(error)
	return ret0
}

// ExtractTarWithSELinux indicates an expected call of ExtractTarWithSELinux.
func (mr *MockOpsMockRecorder) ExtractTarWithSELinux(srcPath, destPath any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "ExtractTarWithSELinux", reflect.TypeOf((*MockOps)(nil).ExtractTarWithSELinux), srcPath, destPath)
}

// ForceExpireSeedCrypto mocks base method.
func (m *MockOps) ForceExpireSeedCrypto(recertContainerImage, authFile string, hasKubeAdminPassword bool) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "ForceExpireSeedCrypto", recertContainerImage, authFile, hasKubeAdminPassword)
	ret0, _ := ret[0].(error)
	return ret0
}

// ForceExpireSeedCrypto indicates an expected call of ForceExpireSeedCrypto.
func (mr *MockOpsMockRecorder) ForceExpireSeedCrypto(recertContainerImage, authFile, hasKubeAdminPassword any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "ForceExpireSeedCrypto", reflect.TypeOf((*MockOps)(nil).ForceExpireSeedCrypto), recertContainerImage, authFile, hasKubeAdminPassword)
}

// GetContainerStorageTarget mocks base method.
func (m *MockOps) GetContainerStorageTarget() (string, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetContainerStorageTarget")
	ret0, _ := ret[0].(string)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// GetContainerStorageTarget indicates an expected call of GetContainerStorageTarget.
func (mr *MockOpsMockRecorder) GetContainerStorageTarget() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetContainerStorageTarget", reflect.TypeOf((*MockOps)(nil).GetContainerStorageTarget))
}

// GetHostname mocks base method.
func (m *MockOps) GetHostname() (string, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetHostname")
	ret0, _ := ret[0].(string)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// GetHostname indicates an expected call of GetHostname.
func (mr *MockOpsMockRecorder) GetHostname() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetHostname", reflect.TypeOf((*MockOps)(nil).GetHostname))
}

// ImageExists mocks base method.
func (m *MockOps) ImageExists(img string) (bool, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "ImageExists", img)
	ret0, _ := ret[0].(bool)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// ImageExists indicates an expected call of ImageExists.
func (mr *MockOpsMockRecorder) ImageExists(img any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "ImageExists", reflect.TypeOf((*MockOps)(nil).ImageExists), img)
}

// IsImageMounted mocks base method.
func (m *MockOps) IsImageMounted(img string) (bool, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "IsImageMounted", img)
	ret0, _ := ret[0].(bool)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// IsImageMounted indicates an expected call of IsImageMounted.
func (mr *MockOpsMockRecorder) IsImageMounted(img any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "IsImageMounted", reflect.TypeOf((*MockOps)(nil).IsImageMounted), img)
}

// ListBlockDevices mocks base method.
func (m *MockOps) ListBlockDevices() ([]BlockDevice, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "ListBlockDevices")
	ret0, _ := ret[0].([]BlockDevice)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// ListBlockDevices indicates an expected call of ListBlockDevices.
func (mr *MockOpsMockRecorder) ListBlockDevices() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "ListBlockDevices", reflect.TypeOf((*MockOps)(nil).ListBlockDevices))
}

// Mount mocks base method.
func (m *MockOps) Mount(deviceName, mountFolder string) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Mount", deviceName, mountFolder)
	ret0, _ := ret[0].(error)
	return ret0
}

// Mount indicates an expected call of Mount.
func (mr *MockOpsMockRecorder) Mount(deviceName, mountFolder any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Mount", reflect.TypeOf((*MockOps)(nil).Mount), deviceName, mountFolder)
}

// MountImage mocks base method.
func (m *MockOps) MountImage(img string) (string, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "MountImage", img)
	ret0, _ := ret[0].(string)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// MountImage indicates an expected call of MountImage.
func (mr *MockOpsMockRecorder) MountImage(img any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "MountImage", reflect.TypeOf((*MockOps)(nil).MountImage), img)
}

// RecertFullFlow mocks base method.
func (m *MockOps) RecertFullFlow(recertContainerImage, authFile, configFile string, preRecertOperations, postRecertOperations func() error, additionalPodmanParams ...string) error {
	m.ctrl.T.Helper()
	varargs := []any{recertContainerImage, authFile, configFile, preRecertOperations, postRecertOperations}
	for _, a := range additionalPodmanParams {
		varargs = append(varargs, a)
	}
	ret := m.ctrl.Call(m, "RecertFullFlow", varargs...)
	ret0, _ := ret[0].(error)
	return ret0
}

// RecertFullFlow indicates an expected call of RecertFullFlow.
func (mr *MockOpsMockRecorder) RecertFullFlow(recertContainerImage, authFile, configFile, preRecertOperations, postRecertOperations any, additionalPodmanParams ...any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	varargs := append([]any{recertContainerImage, authFile, configFile, preRecertOperations, postRecertOperations}, additionalPodmanParams...)
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "RecertFullFlow", reflect.TypeOf((*MockOps)(nil).RecertFullFlow), varargs...)
}

// RemountSysroot mocks base method.
func (m *MockOps) RemountSysroot() error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "RemountSysroot")
	ret0, _ := ret[0].(error)
	return ret0
}

// RemountSysroot indicates an expected call of RemountSysroot.
func (mr *MockOpsMockRecorder) RemountSysroot() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "RemountSysroot", reflect.TypeOf((*MockOps)(nil).RemountSysroot))
}

// RestoreOriginalSeedCrypto mocks base method.
func (m *MockOps) RestoreOriginalSeedCrypto(recertContainerImage, authFile string) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "RestoreOriginalSeedCrypto", recertContainerImage, authFile)
	ret0, _ := ret[0].(error)
	return ret0
}

// RestoreOriginalSeedCrypto indicates an expected call of RestoreOriginalSeedCrypto.
func (mr *MockOpsMockRecorder) RestoreOriginalSeedCrypto(recertContainerImage, authFile any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "RestoreOriginalSeedCrypto", reflect.TypeOf((*MockOps)(nil).RestoreOriginalSeedCrypto), recertContainerImage, authFile)
}

// RunBashInHostNamespace mocks base method.
func (m *MockOps) RunBashInHostNamespace(command string, args ...string) (string, error) {
	m.ctrl.T.Helper()
	varargs := []any{command}
	for _, a := range args {
		varargs = append(varargs, a)
	}
	ret := m.ctrl.Call(m, "RunBashInHostNamespace", varargs...)
	ret0, _ := ret[0].(string)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// RunBashInHostNamespace indicates an expected call of RunBashInHostNamespace.
func (mr *MockOpsMockRecorder) RunBashInHostNamespace(command any, args ...any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	varargs := append([]any{command}, args...)
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "RunBashInHostNamespace", reflect.TypeOf((*MockOps)(nil).RunBashInHostNamespace), varargs...)
}

// RunInHostNamespace mocks base method.
func (m *MockOps) RunInHostNamespace(command string, args ...string) (string, error) {
	m.ctrl.T.Helper()
	varargs := []any{command}
	for _, a := range args {
		varargs = append(varargs, a)
	}
	ret := m.ctrl.Call(m, "RunInHostNamespace", varargs...)
	ret0, _ := ret[0].(string)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// RunInHostNamespace indicates an expected call of RunInHostNamespace.
func (mr *MockOpsMockRecorder) RunInHostNamespace(command any, args ...any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	varargs := append([]any{command}, args...)
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "RunInHostNamespace", reflect.TypeOf((*MockOps)(nil).RunInHostNamespace), varargs...)
}

// RunListOfCommands mocks base method.
func (m *MockOps) RunListOfCommands(cmds []*CMD) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "RunListOfCommands", cmds)
	ret0, _ := ret[0].(error)
	return ret0
}

// RunListOfCommands indicates an expected call of RunListOfCommands.
func (mr *MockOpsMockRecorder) RunListOfCommands(cmds any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "RunListOfCommands", reflect.TypeOf((*MockOps)(nil).RunListOfCommands), cmds)
}

// RunRecert mocks base method.
func (m *MockOps) RunRecert(recertContainerImage, authFile, recertConfigFile string, additionalPodmanParams ...string) error {
	m.ctrl.T.Helper()
	varargs := []any{recertContainerImage, authFile, recertConfigFile}
	for _, a := range additionalPodmanParams {
		varargs = append(varargs, a)
	}
	ret := m.ctrl.Call(m, "RunRecert", varargs...)
	ret0, _ := ret[0].(error)
	return ret0
}

// RunRecert indicates an expected call of RunRecert.
func (mr *MockOpsMockRecorder) RunRecert(recertContainerImage, authFile, recertConfigFile any, additionalPodmanParams ...any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	varargs := append([]any{recertContainerImage, authFile, recertConfigFile}, additionalPodmanParams...)
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "RunRecert", reflect.TypeOf((*MockOps)(nil).RunRecert), varargs...)
}

// RunUnauthenticatedEtcdServer mocks base method.
func (m *MockOps) RunUnauthenticatedEtcdServer(authFile, name string) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "RunUnauthenticatedEtcdServer", authFile, name)
	ret0, _ := ret[0].(error)
	return ret0
}

// RunUnauthenticatedEtcdServer indicates an expected call of RunUnauthenticatedEtcdServer.
func (mr *MockOpsMockRecorder) RunUnauthenticatedEtcdServer(authFile, name any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "RunUnauthenticatedEtcdServer", reflect.TypeOf((*MockOps)(nil).RunUnauthenticatedEtcdServer), authFile, name)
}

// SetupContainersFolderCommands mocks base method.
func (m *MockOps) SetupContainersFolderCommands() error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "SetupContainersFolderCommands")
	ret0, _ := ret[0].(error)
	return ret0
}

// SetupContainersFolderCommands indicates an expected call of SetupContainersFolderCommands.
func (mr *MockOpsMockRecorder) SetupContainersFolderCommands() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "SetupContainersFolderCommands", reflect.TypeOf((*MockOps)(nil).SetupContainersFolderCommands))
}

// SystemctlAction mocks base method.
func (m *MockOps) SystemctlAction(action string, args ...string) (string, error) {
	m.ctrl.T.Helper()
	varargs := []any{action}
	for _, a := range args {
		varargs = append(varargs, a)
	}
	ret := m.ctrl.Call(m, "SystemctlAction", varargs...)
	ret0, _ := ret[0].(string)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// SystemctlAction indicates an expected call of SystemctlAction.
func (mr *MockOpsMockRecorder) SystemctlAction(action any, args ...any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	varargs := append([]any{action}, args...)
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "SystemctlAction", reflect.TypeOf((*MockOps)(nil).SystemctlAction), varargs...)
}

// Umount mocks base method.
func (m *MockOps) Umount(deviceName string) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Umount", deviceName)
	ret0, _ := ret[0].(error)
	return ret0
}

// Umount indicates an expected call of Umount.
func (mr *MockOpsMockRecorder) Umount(deviceName any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Umount", reflect.TypeOf((*MockOps)(nil).Umount), deviceName)
}

// UnmountAndRemoveImage mocks base method.
func (m *MockOps) UnmountAndRemoveImage(img string) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "UnmountAndRemoveImage", img)
	ret0, _ := ret[0].(error)
	return ret0
}

// UnmountAndRemoveImage indicates an expected call of UnmountAndRemoveImage.
func (mr *MockOpsMockRecorder) UnmountAndRemoveImage(img any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "UnmountAndRemoveImage", reflect.TypeOf((*MockOps)(nil).UnmountAndRemoveImage), img)
}

// waitForEtcd mocks base method.
func (m *MockOps) waitForEtcd(healthzEndpoint string) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "waitForEtcd", healthzEndpoint)
	ret0, _ := ret[0].(error)
	return ret0
}

// waitForEtcd indicates an expected call of waitForEtcd.
func (mr *MockOpsMockRecorder) waitForEtcd(healthzEndpoint any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "waitForEtcd", reflect.TypeOf((*MockOps)(nil).waitForEtcd), healthzEndpoint)
}
